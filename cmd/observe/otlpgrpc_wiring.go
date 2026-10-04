package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/logs"
	"github.com/useteploy/teploy-observe/internal/metrics"
	"github.com/useteploy/teploy-observe/internal/otlpgrpc"
	"github.com/useteploy/teploy-observe/internal/tracing"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// ─── OTLP/gRPC receiver ─────────────────────────────────────────────────────
//
// OBSERVE_OTLP_GRPC_ADDR (default empty = disabled) starts a gRPC listener
// serving the OTLP Export services for traces, metrics and logs. OTLP/HTTP on
// the main listener is unchanged. Optional OBSERVE_OTLP_GRPC_TLS_CERT /
// OBSERVE_OTLP_GRPC_TLS_KEY enable TLS (both or neither); without them the
// listener is plaintext h2c and should sit behind a TLS-terminating proxy or a
// tailnet. Auth is the same API key as the HTTP receivers, sent as gRPC
// metadata `x-api-key: <key>` or `authorization: Bearer <key>`.
//
// The lifecycle hook is registered in the neutron options (before the app
// dependencies exist) and configured later by configureOTLPGRPC, so it stops
// before the ingest buffers it feeds.

// otlpGRPC is the process-wide handle the lifecycle hook and the configure
// call share.
var otlpGRPC otlpGRPCRunner

type otlpGRPCRunner struct {
	srv *otlpgrpc.Server
	err error
}

// otlpGRPCHook is the lifecycle hook for main.go's neutron.WithLifecycle.
func otlpGRPCHook(logger *slog.Logger) neutron.LifecycleHook {
	return neutron.LifecycleHook{
		Name: "otlp-grpc-listener",
		OnStart: func(ctx context.Context) error {
			if otlpGRPC.err != nil {
				return otlpGRPC.err
			}
			if otlpGRPC.srv == nil {
				return nil
			}
			if err := otlpGRPC.srv.Start(); err != nil {
				return err
			}
			logger.Info("otlp grpc listener starting", "addr", otlpGRPC.srv.Addr().String())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if otlpGRPC.srv == nil {
				return nil
			}
			return otlpGRPC.srv.Shutdown(ctx)
		},
	}
}

// configureOTLPGRPC builds the server when OBSERVE_OTLP_GRPC_ADDR is set. A
// configuration error is held and returned from the hook's OnStart so it fails
// startup cleanly.
func configureOTLPGRPC(logger *slog.Logger, authSvc *auth.AuthService, limiter *ingest.RateLimiter,
	traceIngest *tracing.IngestService, metricsSvc *metrics.Service, logSvc *logs.LogService) {
	addr := strings.TrimSpace(os.Getenv("OBSERVE_OTLP_GRPC_ADDR"))
	if addr == "" {
		return
	}
	cert := os.Getenv("OBSERVE_OTLP_GRPC_TLS_CERT")
	if cert == "" {
		logger.Warn("otlp grpc listener is plaintext; keep it behind a TLS proxy or tailnet, or set OBSERVE_OTLP_GRPC_TLS_CERT/KEY")
	}
	srv, err := otlpgrpc.New(otlpgrpc.Config{
		Addr:        addr,
		TLSCertFile: cert,
		TLSKeyFile:  os.Getenv("OBSERVE_OTLP_GRPC_TLS_KEY"),
		Logger:      logger,
		OnServeError: func(err error) {
			// Same posture as the ingest listener: a dead accept loop means
			// telemetry is unreachable while health stays green.
			logger.Error("otlp grpc listener terminated - exiting so the supervisor restarts the service", "err", err)
			os.Exit(1)
		},
	}, otlpgrpc.Deps{
		Keys:    authSvc,
		Limiter: limiter,
		Ingester: otlpgrpc.Ingesters{
			Traces: func(ctx context.Context, siteID string, req *tracepb.ExportTraceServiceRequest) error {
				return classifyOTLPErr(tracing.IngestOTLPProto(ctx, traceIngest, siteID, req))
			},
			Metrics: func(ctx context.Context, siteID string, req *metricspb.ExportMetricsServiceRequest) error {
				return classifyOTLPErr(metrics.IngestOTLPProto(ctx, metricsSvc, siteID, req))
			},
			Logs: func(ctx context.Context, siteID string, req *logspb.ExportLogsServiceRequest) (int, error) {
				n, err := logs.IngestOTLPProto(ctx, logSvc, siteID, req)
				return n, classifyOTLPErr(err)
			},
		},
	})
	otlpGRPC = otlpGRPCRunner{srv: srv, err: err}
}

// classifyOTLPErr marks the permanent (non-retryable) refusals the HTTP
// receivers answer with 4xx; everything else stays a retryable storage error.
func classifyOTLPErr(err error) error {
	var tooMany *metrics.ErrTooManyPoints
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tracing.ErrInvalidOTLP), errors.Is(err, metrics.ErrInvalidOTLP),
		errors.Is(err, logs.ErrInvalidOTLP), errors.Is(err, logs.ErrBatchTooLarge),
		errors.As(err, &tooMany):
		return otlpgrpc.Invalid(err)
	}
	return err
}
