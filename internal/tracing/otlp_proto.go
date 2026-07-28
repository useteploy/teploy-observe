package tracing

import (
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Protobuf is the wire format OTLP/HTTP exporters use by default — the OTel
// SDKs and the Collector all send application/x-protobuf unless told otherwise.
// Accepting only JSON meant a stock exporter pointed at Observe got a 415 and
// exported nothing, so "OTLP compatible" was not true in practice.
//
// Rather than carry a second ingest path, protobuf is decoded and translated
// into the same ExportTraceRequest the JSON path produces. The ingest service,
// its validation, and its storage stay untouched and single-sourced.

// decodeProtoTraces turns an OTLP/protobuf ExportTraceServiceRequest into the
// JSON-shaped request the ingest service already understands.
func decodeProtoTraces(body []byte) (ExportTraceRequest, error) {
	var pb tracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &pb); err != nil {
		return ExportTraceRequest{}, fmt.Errorf("invalid protobuf: %w", err)
	}

	out := ExportTraceRequest{ResourceSpans: make([]ResourceSpans, 0, len(pb.GetResourceSpans()))}
	for _, rs := range pb.GetResourceSpans() {
		res := ResourceSpans{
			Resource:   Resource{Attributes: protoAttrs(rs.GetResource().GetAttributes())},
			ScopeSpans: make([]ScopeSpans, 0, len(rs.GetScopeSpans())),
		}
		for _, ss := range rs.GetScopeSpans() {
			scope := ScopeSpans{
				Scope: InstrumentationScope{
					Name:    ss.GetScope().GetName(),
					Version: ss.GetScope().GetVersion(),
				},
				Spans: make([]OTLPSpan, 0, len(ss.GetSpans())),
			}
			for _, s := range ss.GetSpans() {
				scope.Spans = append(scope.Spans, protoSpan(s))
			}
			res.ScopeSpans = append(res.ScopeSpans, scope)
		}
		out.ResourceSpans = append(out.ResourceSpans, res)
	}
	return out, nil
}

func protoSpan(s *otlptrace.Span) OTLPSpan {
	span := OTLPSpan{
		// IDs are raw bytes on the wire and lowercase hex in OTLP/JSON. The
		// storage layer expects the hex form, which is also what every trace
		// UI and every other backend uses.
		TraceID:           hex.EncodeToString(s.GetTraceId()),
		SpanID:            hex.EncodeToString(s.GetSpanId()),
		ParentSpanID:      hex.EncodeToString(s.GetParentSpanId()),
		Name:              s.GetName(),
		Kind:              int(s.GetKind()),
		StartTimeUnixNano: fmt.Sprintf("%d", s.GetStartTimeUnixNano()),
		EndTimeUnixNano:   fmt.Sprintf("%d", s.GetEndTimeUnixNano()),
		Attributes:        protoAttrs(s.GetAttributes()),
		Status: SpanStatus{
			Code:    int(s.GetStatus().GetCode()),
			Message: s.GetStatus().GetMessage(),
		},
	}
	for _, e := range s.GetEvents() {
		span.Events = append(span.Events, SpanEvent{
			Name:         e.GetName(),
			TimeUnixNano: fmt.Sprintf("%d", e.GetTimeUnixNano()),
			Attributes:   protoAttrs(e.GetAttributes()),
		})
	}
	return span
}

// protoAttrs flattens OTLP attribute values into the same shape the JSON path
// unmarshals into. Array, kvlist and bytes values are rendered as their string
// form rather than dropped — a resource attribute that arrives as a list is
// still worth keeping, and the JSON path has no richer representation either.
func protoAttrs(kvs []*commonpb.KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, KeyValue{Key: kv.GetKey(), Value: protoAnyValue(kv.GetValue())})
	}
	return out
}

func protoAnyValue(v *commonpb.AnyValue) AnyValue {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return AnyValue{StringValue: val.StringValue}
	case *commonpb.AnyValue_BoolValue:
		return AnyValue{BoolValue: val.BoolValue}
	case *commonpb.AnyValue_IntValue:
		return AnyValue{IntValue: jsonInt(fmt.Sprintf("%d", val.IntValue))}
	case *commonpb.AnyValue_DoubleValue:
		return AnyValue{DoubleValue: val.DoubleValue}
	case *commonpb.AnyValue_BytesValue:
		return AnyValue{StringValue: hex.EncodeToString(val.BytesValue)}
	case nil:
		return AnyValue{}
	default:
		// Arrays and kvlists: keep the protobuf text form rather than lose the
		// attribute entirely.
		return AnyValue{StringValue: v.String()}
	}
}
