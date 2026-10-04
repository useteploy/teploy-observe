package logs

import (
	"context"
	"errors"
	"testing"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
)

func grpcLogsReq(n int) *logspb.ExportLogsServiceRequest {
	recs := make([]*otlplogs.LogRecord, n)
	for i := range recs {
		recs[i] = &otlplogs.LogRecord{
			SeverityNumber: otlplogs.SeverityNumber_SEVERITY_NUMBER_ERROR,
			Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom"}},
		}
	}
	return &logspb.ExportLogsServiceRequest{ResourceLogs: []*otlplogs.ResourceLogs{{
		ScopeLogs: []*otlplogs.ScopeLogs{{LogRecords: recs}},
	}}}
}

func TestIngestOTLPProtoChunksAndReportsRejected(t *testing.T) {
	stub := &recordingIngester{result: LogBatchResult{Accepted: 1, Rejected: 1}}
	n := maxLogBatchSize + 5 // forces two chunks
	rej, err := IngestOTLPProto(context.Background(), stub, "site-1", grpcLogsReq(n))
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 2 || len(stub.inputs) != n {
		t.Fatalf("calls=%d inputs=%d", stub.calls, len(stub.inputs))
	}
	if rej != 2 { // 1 rejected per chunk from the stub
		t.Fatalf("rejected=%d", rej)
	}
	if stub.inputs[0].SiteID != "site-1" || stub.inputs[0].Level != "error" || stub.inputs[0].Message != "boom" {
		t.Fatalf("bad decode: %+v", stub.inputs[0])
	}
}

func TestIngestOTLPProtoPropagatesStorageError(t *testing.T) {
	boom := errors.New("db down")
	_, err := IngestOTLPProto(context.Background(), &recordingIngester{err: boom}, "s", grpcLogsReq(1))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
