package metrics

import (
	"encoding/json"
	"testing"
)

// Real OTLP exporters may emit int64 fields (asInt, histogram count/bucket
// counts, attribute intValue) as bare JSON numbers rather than the spec's
// quoted strings. Both forms must unmarshal, or a single numeric field 400s
// the whole export batch.
func TestMetrics_NumericInt64Fields(t *testing.T) {
	cases := map[string]string{
		"numbers": `{"resourceMetrics":[{"resource":{"attributes":[]},"scopeMetrics":[{"scope":{"name":"m"},"metrics":[
			{"name":"c","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[{"timeUnixNano":"1","asInt":5,"attributes":[{"key":"code","value":{"intValue":401}}]}]}},
			{"name":"h","histogram":{"aggregationTemporality":2,"dataPoints":[{"timeUnixNano":"1","count":10,"sum":3.5,"bucketCounts":[4,6],"explicitBounds":[1]}]}}
		]}]}]}`,
		"strings": `{"resourceMetrics":[{"resource":{"attributes":[]},"scopeMetrics":[{"scope":{"name":"m"},"metrics":[
			{"name":"c","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[{"timeUnixNano":"1","asInt":"5","attributes":[{"key":"code","value":{"intValue":"401"}}]}]}},
			{"name":"h","histogram":{"aggregationTemporality":2,"dataPoints":[{"timeUnixNano":"1","count":"10","sum":3.5,"bucketCounts":["4","6"],"explicitBounds":[1]}]}}
		]}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var req ExportMetricsRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
			ms := req.ResourceMetrics[0].ScopeMetrics[0].Metrics
			if got := ms[0].Sum.DataPoints[0].AsInt; got != "5" {
				t.Fatalf("%s asInt: got %q want 5", name, got)
			}
			if got := AttrsToMap(ms[0].Sum.DataPoints[0].Attributes)["code"]; got != "401" {
				t.Fatalf("%s intValue attr: got %q want 401", name, got)
			}
			if got := ms[1].Histogram.DataPoints[0].Count; got != "10" {
				t.Fatalf("%s count: got %q want 10", name, got)
			}
		})
	}
}
