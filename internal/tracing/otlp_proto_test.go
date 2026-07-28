package tracing

import (
	"testing"

	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

// A protobuf export must land on exactly the shape the JSON path produces —
// the two formats share one ingest path, so any divergence here shows up as
// spans that store differently depending on which wire format was used.
func TestDecodeProtoTraces_MatchesJSONShape(t *testing.T) {
	traceID := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	parentID := []byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28}

	req := &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*otlptrace.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
			}}},
			ScopeSpans: []*otlptrace.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "manual", Version: "1.2.3"},
				Spans: []*otlptrace.Span{{
					TraceId:           traceID,
					SpanId:            spanID,
					ParentSpanId:      parentID,
					Name:              "GET /cart",
					Kind:              otlptrace.Span_SPAN_KIND_SERVER,
					StartTimeUnixNano: 1700000000000000000,
					EndTimeUnixNano:   1700000000500000000,
					Status:            &otlptrace.Status{Code: otlptrace.Status_STATUS_CODE_ERROR, Message: "boom"},
					Attributes: []*commonpb.KeyValue{
						{Key: "http.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 500}}},
						{Key: "http.ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
						{Key: "http.duration", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}},
					},
					Events: []*otlptrace.Span_Event{{Name: "retry", TimeUnixNano: 1700000000250000000}},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := decodeProtoTraces(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ResourceSpans) != 1 || len(got.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	rs := got.ResourceSpans[0]
	if len(rs.Resource.Attributes) != 1 || rs.Resource.Attributes[0].Value.StringValue != "checkout" {
		t.Errorf("resource attrs = %+v", rs.Resource.Attributes)
	}
	ss := rs.ScopeSpans[0]
	if ss.Scope.Name != "manual" || ss.Scope.Version != "1.2.3" {
		t.Errorf("scope = %+v", ss.Scope)
	}
	if len(ss.Spans) != 1 {
		t.Fatalf("spans = %d", len(ss.Spans))
	}
	s := ss.Spans[0]

	// IDs are hex on the JSON side; storage and every trace UI expect that.
	if s.TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("traceId = %q", s.TraceID)
	}
	if s.SpanID != "1112131415161718" {
		t.Errorf("spanId = %q", s.SpanID)
	}
	if s.ParentSpanID != "2122232425262728" {
		t.Errorf("parentSpanId = %q", s.ParentSpanID)
	}
	if s.Name != "GET /cart" || s.Kind != 2 {
		t.Errorf("name/kind = %q/%d", s.Name, s.Kind)
	}
	if s.StartTimeUnixNano != "1700000000000000000" || s.EndTimeUnixNano != "1700000000500000000" {
		t.Errorf("times = %q..%q", s.StartTimeUnixNano, s.EndTimeUnixNano)
	}
	if s.Status.Code != 2 || s.Status.Message != "boom" {
		t.Errorf("status = %+v", s.Status)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "retry" {
		t.Errorf("events = %+v", s.Events)
	}

	byKey := map[string]AnyValue{}
	for _, kv := range s.Attributes {
		byKey[kv.Key] = kv.Value
	}
	if byKey["http.status_code"].IntValue != "500" {
		t.Errorf("intValue = %q, want \"500\" (string form, as OTLP/JSON encodes int64)", byKey["http.status_code"].IntValue)
	}
	if byKey["http.duration"].DoubleValue != 1.5 {
		t.Errorf("doubleValue = %v", byKey["http.duration"].DoubleValue)
	}
	if byKey["http.ok"].BoolValue {
		t.Errorf("boolValue should be false")
	}
}

// A body that is not protobuf must be a 400-shaped error, not a panic.
func TestDecodeProtoTraces_RejectsGarbage(t *testing.T) {
	if _, err := decodeProtoTraces([]byte("this is not protobuf at all, not even close")); err == nil {
		t.Fatal("expected an error for a non-protobuf body")
	}
}

// An empty export is valid and must decode to zero resource spans, not fail —
// SDKs flush empty batches.
func TestDecodeProtoTraces_EmptyExport(t *testing.T) {
	body, err := proto.Marshal(&tracepb.ExportTraceServiceRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeProtoTraces(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ResourceSpans) != 0 {
		t.Errorf("resourceSpans = %d, want 0", len(got.ResourceSpans))
	}
}
