package tracing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestIngestOTLPProtoMatchesHTTPDecode(t *testing.T) {
	req := &tracepb.ExportTraceServiceRequest{ResourceSpans: []*otlptrace.ResourceSpans{{
		ScopeSpans: []*otlptrace.ScopeSpans{{Spans: []*otlptrace.Span{{
			TraceId: []byte("0123456789abcdef"), SpanId: []byte("01234567"), Name: "op",
			Attributes: []*commonpb.KeyValue{{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v"}}}},
		}}}},
	}}}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeProtoTraces(body)
	if err != nil {
		t.Fatal(err)
	}
	stub := &recordingIngester{}
	if err := IngestOTLPProto(context.Background(), stub, "site-1", req); err != nil {
		t.Fatal(err)
	}
	if stub.siteID != "site-1" || !reflect.DeepEqual(stub.req, want) {
		t.Fatalf("grpc path diverged from HTTP decode: site=%q", stub.siteID)
	}
}

func TestIngestOTLPProtoPropagatesStorageError(t *testing.T) {
	boom := errors.New("db down")
	err := IngestOTLPProto(context.Background(), &recordingIngester{err: boom}, "s", &tracepb.ExportTraceServiceRequest{})
	if !errors.Is(err, boom) || errors.Is(err, ErrInvalidOTLP) {
		t.Fatalf("storage error must pass through unclassified, got %v", err)
	}
}
