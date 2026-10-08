package tracing

import (
	"strconv"
	"testing"
)

func TestOBS108EqualTimesNeverAdvanceFunnel(t *testing.T) {
	for _, ops := range [][]string{{"a", "b"}, {"a", "a"}} {
		rows := []funnelSpan{{TraceID: "t", OpName: ops[0], StartTime: "1000"}, {TraceID: "t", OpName: ops[1], StartTime: "1000"}}
		for i := 0; i < 2; i++ {
			got := computeFunnel(rows, ops)
			if got.Steps[0].Count != 1 || got.Steps[1].Count != 0 {
				t.Fatalf("equal timestamps advanced: %+v", got)
			}
			rows[0], rows[1] = rows[1], rows[0]
		}
	}
}
func TestOBS107SpanAttributeIdentityIncludesTrace(t *testing.T) {
	req := ExportTraceRequest{ResourceSpans: []ResourceSpans{{ScopeSpans: []ScopeSpans{{Spans: []OTLPSpan{{TraceID: "a", SpanID: "same", Attributes: []KeyValue{{Key: "db.system", Value: AnyValue{StringValue: "pg"}}}}, {TraceID: "b", SpanID: "same", Attributes: []KeyValue{{Key: "http.method", Value: AnyValue{StringValue: "GET"}}}}}}}}}}
	flat := []flatSpan{{TraceID: "a", SpanID: "same"}, {TraceID: "b", SpanID: "same"}}
	got := flatToDetectorSpans(req, flat)
	if got[0].Attributes["db.system"] != "pg" || got[0].Attributes["http.method"] != "" || got[1].Attributes["http.method"] != "GET" {
		t.Fatalf("trace attributes crossed: %+v", got)
	}
}

func TestOBS108SubMillisecondInputDoesNotInventStrictOrder(t *testing.T) {
	req := ExportTraceRequest{ResourceSpans: []ResourceSpans{{ScopeSpans: []ScopeSpans{{Spans: []OTLPSpan{
		{TraceID: "t", SpanID: "a", Name: "a", StartTimeUnixNano: "1000000001", EndTimeUnixNano: "1000000002"},
		{TraceID: "t", SpanID: "b", Name: "b", StartTimeUnixNano: "1000000002", EndTimeUnixNano: "1000000003"},
	}}}}}}
	flat := flattenSpans(req)
	rows := []funnelSpan{}
	for _, s := range flat {
		rows = append(rows, funnelSpan{TraceID: s.TraceID, OpName: s.OperationName, StartTime: strconv.FormatInt(s.StartMs, 10)})
	}
	got := computeFunnel(rows, []string{"a", "b"})
	if got.Steps[1].Count != 0 {
		t.Fatalf("invented nanosecond precision: %+v", got)
	}
}
