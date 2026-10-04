package tracing

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// ErrInvalidOTLP marks a payload that could not be decoded. Transports map it
// to their "client error, do not retry" status (HTTP 400, gRPC
// InvalidArgument).
var ErrInvalidOTLP = errors.New("invalid otlp payload")

// IngestOTLPProto is the OTLP/gRPC entry point. The gRPC server hands over an
// already-unmarshalled request; it is re-encoded and pushed through the same
// decodeProtoTraces + Ingest path the HTTP receiver uses, so there is a single
// protobuf-to-ingest translation. A storage failure is returned unwrapped from
// svc.Ingest so the caller can classify it as retryable.
func IngestOTLPProto(ctx context.Context, svc traceIngester, siteID string, req *tracepb.ExportTraceServiceRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	decoded, err := decodeProtoTraces(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	_, err = svc.Ingest(ctx, siteID, decoded)
	return err
}
