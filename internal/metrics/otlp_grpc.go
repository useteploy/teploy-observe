package metrics

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
)

// ErrInvalidOTLP marks a payload that could not be decoded (HTTP 400, gRPC
// InvalidArgument).
var ErrInvalidOTLP = errors.New("invalid otlp payload")

// IngestOTLPProto is the OTLP/gRPC entry point; see tracing.IngestOTLPProto.
// It reuses decodeProtoMetrics and Service.Ingest, so the cardinality refusal
// (*ErrTooManyPoints) is returned as-is for the caller to classify.
func IngestOTLPProto(ctx context.Context, svc metricIngester, siteID string, req *metricspb.ExportMetricsServiceRequest) error {
	_, err := IngestOTLPProtoResult(ctx, svc, siteID, req)
	return err
}

// IngestOTLPProtoResult returns permanent point rejections for OTLP partial success.
func IngestOTLPProtoResult(ctx context.Context, svc metricIngester, siteID string, req *metricspb.ExportMetricsServiceRequest) (int, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	decoded, err := decodeProtoMetrics(body)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	result, err := svc.Ingest(ctx, siteID, decoded)
	return result.Rejected, err
}
