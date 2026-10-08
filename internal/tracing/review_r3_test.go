package tracing

import "testing"

func TestR3TraceIdentityAttributesAndDependencyRollups(t *testing.T) {
	spans := []OTLPSpan{
		{TraceID: "a", SpanID: "same", Attributes: []KeyValue{{Key: "db.statement", Value: AnyValue{StringValue: "SELECT a"}}}},
		{TraceID: "b", SpanID: "same", Attributes: []KeyValue{{Key: "db.statement", Value: AnyValue{StringValue: "SELECT b"}}}},
	}
	req := ExportTraceRequest{ResourceSpans: []ResourceSpans{{ScopeSpans: []ScopeSpans{{Spans: spans}}}}}
	flat := []flatSpan{{TraceID: "a", SpanID: "same", ServiceName: "parent-a"}, {TraceID: "b", SpanID: "same", ServiceName: "parent-b"}, {TraceID: "a", SpanID: "child", ParentSpanID: "same", ServiceName: "db-a"}, {TraceID: "b", SpanID: "child", ParentSpanID: "same", ServiceName: "db-b"}}
	detector := flatToDetectorSpans(req, flat)
	if detector[0].Attributes["db.statement"] != "SELECT a" || detector[1].Attributes["db.statement"] != "SELECT b" {
		t.Fatal("reused span ID crossed trace attributes")
	}
	_, edges := aggregateRollups(flat)
	if len(edges) != 2 || edges[ServiceEdge{Src: "parent-a", Dst: "db-a"}] == nil || edges[ServiceEdge{Src: "parent-b", Dst: "db-b"}] == nil {
		t.Fatalf("cross-trace dependency: %+v", edges)
	}
}
