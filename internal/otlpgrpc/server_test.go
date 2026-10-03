package otlpgrpc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/useteploy/teploy-observe/internal/auth"
)

// The vendored grpc has no test/bufconn package, so these tests serve on a
// loopback listener (127.0.0.1:0); the transport is still real gRPC/HTTP2.

type fakeKeys struct {
	keys map[string]auth.ValidatedKey
	err  error
}

func (f *fakeKeys) ValidateAPIKey(_ context.Context, key string) (auth.ValidatedKey, error) {
	if f.err != nil {
		return auth.ValidatedKey{}, f.err
	}
	v, ok := f.keys[key]
	if !ok {
		return auth.ValidatedKey{}, errors.New("auth: invalid api key")
	}
	return v, nil
}

type fakeLimiter struct {
	allow atomic.Bool
	site  atomic.Value
}

func (f *fakeLimiter) Allow(siteID, ip string) bool {
	f.site.Store(siteID + "|" + ip)
	return f.allow.Load()
}

// sink records what reached the ingesters.
type sink struct {
	mu       sync.Mutex
	sites    []string
	err      error
	rejected int
	block    chan struct{} // when set, ingesters wait on it
	entered  chan struct{}
}

func (s *sink) record(ctx context.Context, site string) error {
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sites = append(s.sites, site)
	return s.err
}

func (s *sink) ingesters() Ingesters {
	return Ingesters{
		Traces: func(ctx context.Context, site string, _ *tracepb.ExportTraceServiceRequest) error {
			return s.record(ctx, site)
		},
		Metrics: func(ctx context.Context, site string, _ *metricspb.ExportMetricsServiceRequest) error {
			return s.record(ctx, site)
		},
		Logs: func(ctx context.Context, site string, _ *logspb.ExportLogsServiceRequest) (int, error) {
			err := s.record(ctx, site)
			return s.rejected, err
		},
	}
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sites)
}

type harness struct {
	srv     *Server
	conn    *grpc.ClientConn
	sink    *sink
	limiter *fakeLimiter
	keys    *fakeKeys
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{
		sink:    &sink{},
		limiter: &fakeLimiter{},
		keys: &fakeKeys{keys: map[string]auth.ValidatedKey{
			"obs_good":    {SiteID: "site-a", Scopes: "telemetry"},
			"obs_publish": {SiteID: "site-a", Scopes: "publish"},
		}},
	}
	h.limiter.allow.Store(true)
	cfg := Config{Addr: "127.0.0.1:0"}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg, Deps{Keys: h.keys, Limiter: h.limiter, Ingester: h.sink.ingesters()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	srv.Serve(ln)
	h.srv = srv
	h.conn, err = grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return h
}

func withKey(key string) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	return metadata.AppendToOutgoingContext(ctx, "x-api-key", key)
}

func code(err error) codes.Code { return status.Code(err) }

func retryDelayOf(t *testing.T, err error) time.Duration {
	t.Helper()
	st := status.Convert(err)
	for _, d := range st.Proto().GetDetails() {
		if d.GetTypeUrl() != retryInfoTypeURL {
			continue
		}
		v := d.GetValue()
		if len(v) < 2 || v[0] != 0x0a {
			t.Fatalf("malformed RetryInfo bytes % x", v)
		}
		var dur durationpb.Duration
		if err := proto.Unmarshal(v[2:], &dur); err != nil {
			t.Fatal(err)
		}
		return dur.AsDuration()
	}
	t.Fatalf("no RetryInfo detail in %v", err)
	return 0
}

func logsReq(msg string) *logspb.ExportLogsServiceRequest {
	return &logspb.ExportLogsServiceRequest{ResourceLogs: []*otlplogs.ResourceLogs{{
		ScopeLogs: []*otlplogs.ScopeLogs{{LogRecords: []*otlplogs.LogRecord{{
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: msg}},
		}}}},
	}}}
}

func TestAllThreeServicesSucceedWithSiteFromKey(t *testing.T) {
	h := newHarness(t, nil)
	ctx := withKey("obs_good")
	if _, err := tracepb.NewTraceServiceClient(h.conn).Export(ctx, &tracepb.ExportTraceServiceRequest{}); err != nil {
		t.Fatalf("traces: %v", err)
	}
	if _, err := metricspb.NewMetricsServiceClient(h.conn).Export(ctx, &metricspb.ExportMetricsServiceRequest{}); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	resp, err := logspb.NewLogsServiceClient(h.conn).Export(ctx, logsReq("hi"))
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if resp.GetPartialSuccess() != nil {
		t.Fatalf("unexpected partial_success %v", resp.GetPartialSuccess())
	}
	if h.sink.count() != 3 {
		t.Fatalf("sink saw %d calls", h.sink.count())
	}
	for _, s := range h.sink.sites {
		if s != "site-a" {
			t.Fatalf("site must come from the validated key, got %q", s)
		}
	}
	got, _ := h.limiter.site.Load().(string)
	if !strings.HasPrefix(got, "site-a|127.0.0.1") {
		t.Fatalf("limiter key %q", got)
	}
}

func TestBearerAuthorizationAccepted(t *testing.T) {
	h := newHarness(t, nil)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer obs_good")
	if _, err := tracepb.NewTraceServiceClient(h.conn).Export(ctx, &tracepb.ExportTraceServiceRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestLogsPartialSuccess(t *testing.T) {
	h := newHarness(t, nil)
	h.sink.rejected = 3
	resp, err := logspb.NewLogsServiceClient(h.conn).Export(withKey("obs_good"), logsReq("x"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPartialSuccess().GetRejectedLogRecords() != 3 {
		t.Fatalf("partial_success=%v", resp.GetPartialSuccess())
	}
}

func TestAuthFailures(t *testing.T) {
	h := newHarness(t, nil)
	tr := tracepb.NewTraceServiceClient(h.conn)
	mr := metricspb.NewMetricsServiceClient(h.conn)
	lr := logspb.NewLogsServiceClient(h.conn)

	cases := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"missing", context.Background(), codes.Unauthenticated},
		{"empty key", withKey(" "), codes.Unauthenticated},
		{"invalid", withKey("obs_nope"), codes.Unauthenticated},
		{"non-bearer scheme", metadata.AppendToOutgoingContext(context.Background(), "authorization", "Basic obs_good"), codes.Unauthenticated},
		{"wrong scope", withKey("obs_publish"), codes.PermissionDenied},
	}
	for _, c := range cases {
		if _, err := tr.Export(c.ctx, &tracepb.ExportTraceServiceRequest{}); code(err) != c.want {
			t.Errorf("%s traces: got %v want %v", c.name, err, c.want)
		}
		if _, err := mr.Export(c.ctx, &metricspb.ExportMetricsServiceRequest{}); code(err) != c.want {
			t.Errorf("%s metrics: got %v want %v", c.name, err, c.want)
		}
		if _, err := lr.Export(c.ctx, logsReq("x")); code(err) != c.want {
			t.Errorf("%s logs: got %v want %v", c.name, err, c.want)
		}
	}
	if h.sink.count() != 0 {
		t.Fatalf("rejected requests reached ingest (%d)", h.sink.count())
	}
}

func TestAuthErrorDoesNotEchoKey(t *testing.T) {
	h := newHarness(t, nil)
	_, err := tracepb.NewTraceServiceClient(h.conn).Export(withKey("obs_supersecret_value"), &tracepb.ExportTraceServiceRequest{})
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("key leaked in error: %v", err)
	}
}

func TestAuthStoreOutageIsUnavailableWithRetryInfo(t *testing.T) {
	h := newHarness(t, nil)
	h.keys.err = errors.Join(auth.ErrAuthUnavailable, errors.New("conn refused"))
	_, err := tracepb.NewTraceServiceClient(h.conn).Export(withKey("obs_good"), &tracepb.ExportTraceServiceRequest{})
	if code(err) != codes.Unavailable {
		t.Fatalf("got %v", err)
	}
	if d := retryDelayOf(t, err); d != retryDelay {
		t.Fatalf("retry delay %v", d)
	}
}

func TestOversizeMessageRejected(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRecvMsgSize = 1024 })
	big := logsReq(strings.Repeat("a", 4096))
	_, err := logspb.NewLogsServiceClient(h.conn).Export(withKey("obs_good"), big)
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("got %v", err)
	}
	if h.sink.count() != 0 {
		t.Fatal("oversize message reached ingest")
	}
}

func TestGzipCompressedRequestAcceptedAndBombBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRecvMsgSize = 64 << 10 })
	lc := logspb.NewLogsServiceClient(h.conn)

	if _, err := lc.Export(withKey("obs_good"), logsReq("small"), grpc.UseCompressor("gzip")); err != nil {
		t.Fatalf("gzip export: %v", err)
	}
	// ~1 MiB of 'a' gzips to a few KiB (well under the cap on the wire) but
	// expands far past MaxRecvMsgSize.
	bomb := logsReq(string(bytes.Repeat([]byte("a"), 1<<20)))
	_, err := lc.Export(withKey("obs_good"), bomb, grpc.UseCompressor("gzip"))
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("gzip bomb: got %v", err)
	}
	if h.sink.count() != 1 {
		t.Fatalf("sink=%d, bomb must not be ingested", h.sink.count())
	}
}

func TestRateLimitedIsResourceExhaustedWithRetryInfo(t *testing.T) {
	h := newHarness(t, nil)
	h.limiter.allow.Store(false)
	_, err := metricspb.NewMetricsServiceClient(h.conn).Export(withKey("obs_good"), &metricspb.ExportMetricsServiceRequest{})
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("got %v", err)
	}
	if d := retryDelayOf(t, err); d != rateLimitRetryDelay {
		t.Fatalf("retry delay %v", d)
	}
	if h.sink.count() != 0 {
		t.Fatal("rate-limited request reached ingest")
	}
}

func TestStorageFailureIsUnavailableWithRetryInfo(t *testing.T) {
	h := newHarness(t, nil)
	h.sink.err = errors.New("nucleus down")
	ctx := withKey("obs_good")
	calls := []func() error{
		func() error {
			_, err := tracepb.NewTraceServiceClient(h.conn).Export(ctx, &tracepb.ExportTraceServiceRequest{})
			return err
		},
		func() error {
			_, err := metricspb.NewMetricsServiceClient(h.conn).Export(ctx, &metricspb.ExportMetricsServiceRequest{})
			return err
		},
		func() error { _, err := logspb.NewLogsServiceClient(h.conn).Export(ctx, logsReq("x")); return err },
	}
	for i, call := range calls {
		err := call()
		if code(err) != codes.Unavailable {
			t.Fatalf("service %d: got %v", i, err)
		}
		if retryDelayOf(t, err) != retryDelay {
			t.Fatalf("service %d: retry delay", i)
		}
		if strings.Contains(err.Error(), "nucleus down") {
			t.Fatalf("internal error text leaked: %v", err)
		}
	}
}

func TestInvalidErrorIsInvalidArgument(t *testing.T) {
	h := newHarness(t, nil)
	h.sink.err = Invalid(errors.New("too many points"))
	_, err := metricspb.NewMetricsServiceClient(h.conn).Export(withKey("obs_good"), &metricspb.ExportMetricsServiceRequest{})
	if code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func TestPanicInSinkBecomesInternal(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.deps.Ingester.Traces = func(context.Context, string, *tracepb.ExportTraceServiceRequest) error { panic("boom") }
	_, err := tracepb.NewTraceServiceClient(h.conn).Export(withKey("obs_good"), &tracepb.ExportTraceServiceRequest{})
	if code(err) != codes.Internal {
		t.Fatalf("got %v", err)
	}
}

func TestGracefulShutdownDrainsInFlightThenRefuses(t *testing.T) {
	h := newHarness(t, nil)
	h.sink.block = make(chan struct{})
	h.sink.entered = make(chan struct{}, 1)

	errCh := make(chan error, 1)
	go func() {
		_, err := tracepb.NewTraceServiceClient(h.conn).Export(withKey("obs_good"), &tracepb.ExportTraceServiceRequest{})
		errCh <- err
	}()
	select {
	case <-h.sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rpc never reached the sink")
	}

	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { shutdownDone <- h.srv.Shutdown(ctx) }()

	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while an RPC was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(h.sink.block)
	if err := <-errCh; err != nil {
		t.Fatalf("in-flight rpc must complete during graceful stop: %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// After shutdown new RPCs fail fast rather than hang.
	fctx, fcancel := context.WithTimeout(withKey("obs_good"), 2*time.Second)
	defer fcancel()
	if _, err := tracepb.NewTraceServiceClient(h.conn).Export(fctx, &tracepb.ExportTraceServiceRequest{}); err == nil {
		t.Fatal("rpc succeeded after shutdown")
	}
}

func TestShutdownForcesStopWhenDeadlineExpires(t *testing.T) {
	h := newHarness(t, nil)
	h.sink.block = make(chan struct{})
	h.sink.entered = make(chan struct{}, 1)
	defer close(h.sink.block)

	go func() {
		_, _ = tracepb.NewTraceServiceClient(h.conn).Export(withKey("obs_good"), &tracepb.ExportTraceServiceRequest{})
	}()
	<-h.sink.entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded after forced stop, got %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	keys := &fakeKeys{}
	if _, err := New(Config{Addr: ":0"}, Deps{}); err == nil {
		t.Fatal("missing validator must fail")
	}
	if _, err := New(Config{Addr: ":0", TLSCertFile: "x.pem"}, Deps{Keys: keys}); err == nil {
		t.Fatal("cert without key must fail")
	}
	if _, err := New(Config{Addr: ":0", TLSCertFile: "/nonexistent.pem", TLSKeyFile: "/nonexistent.key"}, Deps{Keys: keys}); err == nil {
		t.Fatal("unreadable keypair must fail")
	}
}

func TestStartBindFailureIsReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	srv, err := New(Config{Addr: ln.Addr().String()}, Deps{Keys: &fakeKeys{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		t.Fatal("binding a taken port must fail startup")
	}
}
