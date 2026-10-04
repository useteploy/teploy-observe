package logs

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

// ErrInvalidOTLP marks a payload that could not be decoded (HTTP 400, gRPC
// InvalidArgument).
var ErrInvalidOTLP = errors.New("invalid otlp payload")

// IngestOTLPProto is the OTLP/gRPC entry point; see tracing.IngestOTLPProto.
// It reuses protoLogInputs and ingestExport (chunking included) and returns
// the number of rejected records, which the caller reports as partial_success
// exactly as the HTTP receiver does.
func IngestOTLPProto(ctx context.Context, svc logIngester, siteID string, req *logspb.ExportLogsServiceRequest) (rejected int, err error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	inputs, err := protoLogInputs(body, siteID)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidOTLP, err)
	}
	res, err := ingestExport(ctx, svc, inputs)
	return res.Rejected, err
}
