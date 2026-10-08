// Package otlpgrpc serves the three OTLP/gRPC Export services (traces,
// metrics, logs). It is a transport shim only: authentication, rate limiting
// and the status-code mapping live here, while decoding and ingest are the
// same code the OTLP/HTTP receivers run (see the IngestOTLPProto functions in
// internal/tracing, internal/metrics and internal/logs).
//
// The listener is opt-in (OBSERVE_OTLP_GRPC_ADDR) and plaintext unless a
// certificate is configured; see cmd/observe/otlpgrpc_wiring.go.
package otlpgrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/authguard"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

const (
	// DefaultMaxRecvMsgSize is the per-message cap. grpc applies it to the
	// decompressed message as well, so it also bounds gzip expansion. It is
	// below the OTLP/HTTP cap (10 MiB) on purpose: a batch exporter's default
	// is far smaller, and every byte up to the cap is buffered per RPC.
	DefaultMaxRecvMsgSize = 4 << 20
	// MaxRecvMsgSizeLimit is the ceiling an operator may raise it to (the old
	// default, matching OTLP/HTTP's otlpMaxBodyBytes).
	MaxRecvMsgSizeLimit = 10 << 20
	// DefaultMaxConcurrentStreams bounds in-flight RPCs per connection.
	DefaultMaxConcurrentStreams = 128
	// DefaultMaxHeaderListSize bounds the uncompressed request header list
	// (grpc's own default is 16 MiB).
	DefaultMaxHeaderListSize = 16 << 10
	// DefaultMaxConnections / DefaultMaxConnsPerIP cap concurrent TCP
	// connections in total and per remote address.
	DefaultMaxConnections = 1024
	DefaultMaxConnsPerIP  = 64
	// authTimeout bounds the key lookup run from the tap handle (which runs
	// on the connection's reader goroutine, so it must not block for long).
	authTimeout = 3 * time.Second

	// retryDelay is advertised in RetryInfo on retryable refusals.
	retryDelay = 5 * time.Second
	// rateLimitRetryDelay is the (short) back-off for a token-bucket refusal.
	rateLimitRetryDelay = time.Second
)

// KeyValidator is the slice of *auth.AuthService the interceptor needs.
type KeyValidator interface {
	ValidateAPIKey(ctx context.Context, key string) (auth.ValidatedKey, error)
}

// Limiter is the per-(site, ip) admission check; *ingest.RateLimiter.
type Limiter interface {
	Allow(siteID, ip string) bool
}

// Ingesters are the per-signal sinks. Each returns nil on success. Errors are
// classified by the server: wrap permanent client errors with Invalid;
// everything else is treated as retryable storage failure (Unavailable).
type Ingesters struct {
	// MetricsResult reports rejected points; preferred over the legacy Metrics sink.
	MetricsResult func(ctx context.Context, siteID string, req *metricspb.ExportMetricsServiceRequest) (int, error)
	Traces        func(ctx context.Context, siteID string, req *tracepb.ExportTraceServiceRequest) error
	Metrics       func(ctx context.Context, siteID string, req *metricspb.ExportMetricsServiceRequest) error
	// Logs also returns the number of records the service rejected, reported
	// as partial_success.rejected_log_records.
	Logs func(ctx context.Context, siteID string, req *logspb.ExportLogsServiceRequest) (rejected int, err error)
}

// Config configures a Server. Zero values take the defaults above.
type Config struct {
	Addr                 string
	TLSCertFile          string
	TLSKeyFile           string
	MaxRecvMsgSize       int // clamped to MaxRecvMsgSizeLimit
	MaxConcurrentStreams uint32
	MaxHeaderListSize    uint32
	MaxConnections       int // < 0 disables
	MaxConnsPerIP        int // < 0 disables
	Logger               *slog.Logger
	// OnServeError is called if the accept loop dies after a healthy start.
	OnServeError func(error)
}

// Deps are the collaborators.
type Deps struct {
	Keys     KeyValidator
	Limiter  Limiter // optional
	Ingester Ingesters
}

// invalidError marks a permanent client error (not retryable).
type invalidError struct{ err error }

func (e *invalidError) Error() string { return e.err.Error() }
func (e *invalidError) Unwrap() error { return e.err }

// Invalid wraps err so the server answers InvalidArgument (OTLP: do not retry).
func Invalid(err error) error { return &invalidError{err: err} }

// Server is an OTLP/gRPC listener.
type Server struct {
	cfg  Config
	deps Deps
	gs   *grpc.Server
	log  *slog.Logger
	ln   net.Listener
}

// New builds the server without binding. It fails on a half-configured TLS
// pair or a missing key validator, so misconfiguration stops startup.
func New(cfg Config, deps Deps) (*Server, error) {
	if deps.Keys == nil {
		return nil, errors.New("otlpgrpc: key validator is required")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, errors.New("otlpgrpc: TLS cert and key must be set together")
	}
	if cfg.MaxRecvMsgSize <= 0 {
		cfg.MaxRecvMsgSize = DefaultMaxRecvMsgSize
	}
	if cfg.MaxRecvMsgSize > MaxRecvMsgSizeLimit {
		cfg.MaxRecvMsgSize = MaxRecvMsgSizeLimit
	}
	if cfg.MaxConcurrentStreams == 0 {
		cfg.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if cfg.MaxHeaderListSize == 0 {
		cfg.MaxHeaderListSize = DefaultMaxHeaderListSize
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = DefaultMaxConnections
	}
	if cfg.MaxConnsPerIP == 0 {
		cfg.MaxConnsPerIP = DefaultMaxConnsPerIP
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, deps: deps, log: cfg.Logger}

	opts := []grpc.ServerOption{
		grpc.ForceServerCodec(admissionCodec{}),
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize),
		grpc.MaxConcurrentStreams(cfg.MaxConcurrentStreams),
		grpc.MaxHeaderListSize(cfg.MaxHeaderListSize),
		// Authenticate from the request headers BEFORE the message is read
		// and decoded: an unauthenticated peer's payload is never buffered.
		grpc.InTapHandle(s.tapAuth),
		grpc.ChainUnaryInterceptor(s.recoverInterceptor, s.authInterceptor),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: false,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 10 * time.Minute,
			Time:              2 * time.Minute,
			Timeout:           20 * time.Second,
		}),
		grpc.ConnectionTimeout(20 * time.Second),
	}
	if cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("otlpgrpc: load TLS keypair: %w", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})))
	}
	s.gs = grpc.NewServer(opts...)
	tracepb.RegisterTraceServiceServer(s.gs, &traceService{s: s})
	metricspb.RegisterMetricsServiceServer(s.gs, &metricsService{s: s})
	logspb.RegisterLogsServiceServer(s.gs, &logsService{s: s})
	return s, nil
}

// Start binds cfg.Addr synchronously (a port collision fails startup) and
// serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("bind otlp grpc listener %s: %w", s.cfg.Addr, err)
	}
	s.Serve(ln)
	return nil
}

// Serve serves on an existing listener in the background (tests use this).
func (s *Server) Serve(ln net.Listener) {
	s.ln = ln
	if s.cfg.MaxConnections > 0 || s.cfg.MaxConnsPerIP > 0 {
		ln = newLimitListener(ln, s.cfg.MaxConnections, s.cfg.MaxConnsPerIP)
	}
	go func() {
		if err := s.gs.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			if s.cfg.OnServeError != nil {
				s.cfg.OnServeError(err)
			}
		}
	}()
}

// Addr is the bound address (nil before Start/Serve).
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Shutdown stops accepting, lets in-flight RPCs finish, and falls back to a
// hard stop when ctx expires first.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Stop closes every connection and cancels handler contexts; it is not
		// waited on further, so a sink that ignores its context cannot hold
		// shutdown past the deadline.
		s.gs.Stop()
		return ctx.Err()
	}
}

type siteKey struct{}

func siteFrom(ctx context.Context) string {
	v, _ := ctx.Value(siteKey{}).(string)
	return v
}

func (s *Server) recoverInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("otlp grpc handler panic", "method", info.FullMethod, "panic", r)
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return h(ctx, req)
}

// apiKeyFrom extracts the key from `x-api-key` or `authorization: Bearer`.
func apiKeyFrom(md metadata.MD) string {
	if v := md.Get("x-api-key"); len(v) > 0 {
		if k := strings.TrimSpace(v[0]); k != "" {
			return k
		}
	}
	for _, v := range md.Get("authorization") {
		v = strings.TrimSpace(v)
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	return ""
}

// ipKeyValidator is implemented by *authguard.Guard: key validation that also
// takes the client IP for failed-attempt limiting.
type ipKeyValidator interface {
	Validate(ctx context.Context, key, clientIP string) (auth.ValidatedKey, error)
}

// tapAuth applies the same rules as auth.APIKeyAuthMiddleware (key valid, site
// exists, telemetry scope) followed by ingest.RateLimiter's (site, ip)
// admission, mirroring the otlpChain order in cmd/observe. It runs as a
// grpc.InTapHandle, i.e. on the request HEADERS, before the message is read,
// decompressed or unmarshalled, so a caller without a valid key cannot make
// the server buffer or decode its payload. The validated site travels in the
// returned context; authInterceptor refuses any RPC that arrives without it.
func (s *Server) tapAuth(ctx context.Context, info *tap.Info) (context.Context, error) {
	key := apiKeyFrom(info.Header)
	if key == "" {
		return nil, status.Error(codes.Unauthenticated, "missing API key")
	}
	vctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	var validated auth.ValidatedKey
	var err error
	if gk, ok := s.deps.Keys.(ipKeyValidator); ok {
		validated, err = gk.Validate(vctx, key, peerIP(ctx))
	} else {
		validated, err = s.deps.Keys.ValidateAPIKey(vctx, key)
	}
	if err != nil {
		if errors.Is(err, authguard.ErrTooManyAttempts) {
			return nil, retryable(codes.ResourceExhausted, "too many failed authentication attempts", retryDelay)
		}
		if errors.Is(err, auth.ErrAuthUnavailable) {
			return nil, retryable(codes.Unavailable, "authentication temporarily unavailable", retryDelay)
		}
		// The validator's message names the failure (invalid / revoked); it
		// never contains the key.
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if !auth.HasScope(validated.Scopes, auth.ScopeTelemetry) {
		return nil, status.Error(codes.PermissionDenied, "api key lacks the telemetry capability")
	}
	if s.deps.Limiter != nil && !s.deps.Limiter.Allow(validated.SiteID, peerIP(ctx)) {
		return nil, retryable(codes.ResourceExhausted, "rate limited", rateLimitRetryDelay)
	}
	return context.WithValue(ctx, siteKey{}, validated.SiteID), nil
}

// authInterceptor is the backstop for tapAuth: no authenticated site in the
// context means the tap did not run for this RPC, which must never reach a
// sink.
func (s *Server) authInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if siteFrom(ctx) == "" {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return h(ctx, req)
}

func peerIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}

// mapIngestErr converts a sink error to a gRPC status. Storage failures are
// Unavailable (+RetryInfo) so exporters retry instead of dropping the batch
// (audit F13 posture, as on the HTTP 503 path).
func mapIngestErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var inv *invalidError
	if errors.As(err, &inv) {
		return status.Error(codes.InvalidArgument, inv.Error())
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return retryable(codes.Unavailable, "storage unavailable", retryDelay)
}

type traceService struct {
	tracepb.UnimplementedTraceServiceServer
	s *Server
}

func (t *traceService) Export(ctx context.Context, req *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	if err := mapIngestErr(ctx, t.s.deps.Ingester.Traces(ctx, siteFrom(ctx), req)); err != nil {
		return nil, err
	}
	// Tracing ingest is all-or-nothing: no partial_success, as on HTTP.
	return &tracepb.ExportTraceServiceResponse{}, nil
}

type metricsService struct {
	metricspb.UnimplementedMetricsServiceServer
	s *Server
}

func (m *metricsService) Export(ctx context.Context, req *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	var rejected int
	var err error
	if sink := m.s.deps.Ingester.MetricsResult; sink != nil {
		rejected, err = sink(ctx, siteFrom(ctx), req)
	} else {
		err = m.s.deps.Ingester.Metrics(ctx, siteFrom(ctx), req)
	}
	if err := mapIngestErr(ctx, err); err != nil {
		return nil, err
	}
	resp := &metricspb.ExportMetricsServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &metricspb.ExportMetricsPartialSuccess{RejectedDataPoints: int64(rejected), ErrorMessage: "points rejected by admission policy"}
	}
	return resp, nil
}

type logsService struct {
	logspb.UnimplementedLogsServiceServer
	s *Server
}

func (l *logsService) Export(ctx context.Context, req *logspb.ExportLogsServiceRequest) (*logspb.ExportLogsServiceResponse, error) {
	rejected, err := l.s.deps.Ingester.Logs(ctx, siteFrom(ctx), req)
	if err := mapIngestErr(ctx, err); err != nil {
		return nil, err
	}
	resp := &logspb.ExportLogsServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &logspb.ExportLogsPartialSuccess{RejectedLogRecords: int64(rejected)}
	}
	return resp, nil
}

// Compile-time check that the real limiter satisfies Limiter.
var _ Limiter = (*ingest.RateLimiter)(nil)
