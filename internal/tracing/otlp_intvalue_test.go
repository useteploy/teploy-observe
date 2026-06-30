package tracing

import (
	"encoding/json"
	"testing"
)

// @vercel/otel + Next.js emit numeric attributes (http.status_code, etc.) as
// a bare JSON number in intValue, not the OTLP/JSON-spec quoted string. Both
// forms must unmarshal, or the whole export batch 400s.
func TestAnyValue_IntValue_NumberOrString(t *testing.T) {
	cases := map[string]string{
		"number": `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"scope":{"name":"next.js"},"spans":[{"traceId":"a","spanId":"b","name":"GET","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2","attributes":[{"key":"http.status_code","value":{"intValue":401}}],"status":{"code":0}}]}]}]}`,
		"string": `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"scope":{"name":"next.js"},"spans":[{"traceId":"a","spanId":"b","name":"GET","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2","attributes":[{"key":"http.status_code","value":{"intValue":"401"}}],"status":{"code":0}}]}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var req ExportTraceRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("unmarshal %s intValue: %v", name, err)
			}
			attrs := AttrsToMap(req.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes)
			if attrs["http.status_code"] != "401" {
				t.Fatalf("%s: got %q want 401", name, attrs["http.status_code"])
			}
		})
	}
}
