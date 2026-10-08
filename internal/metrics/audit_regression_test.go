package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/useteploy/teploy-observe/internal/ingest"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"net/http/httptest"
	"testing"
)

func gaugeRequest(points int) ExportMetricsRequest {
	d := make([]NumberDataPoint, points)
	for i := range d {
		d[i].Attributes = []KeyValue{{Key: "point", Value: stringAny(string(rune('a' + i)))}}
	}
	return ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{ScopeMetrics: []ScopeMetrics{{Metrics: []OTLPMetric{{Name: "m", Gauge: &Gauge{DataPoints: d}}}}}}}}
}
func TestOBS05IdentitySurvivesIngestAndSeparatesRates(t *testing.T) {
	svc := NewService(nil)
	var stored []metricPointRow
	svc.writePointRows = func(_ context.Context, _ string, rows []metricPointRow) (int, error) {
		stored = append(stored, rows...)
		return len(rows), nil
	}
	req := ExportMetricsRequest{}
	for i, instance := range []string{"a", "b"} {
		vals := []NumberDataPoint{{TimeUnixNano: "0", AsDouble: 0}, {TimeUnixNano: "10000000000", AsDouble: 10}}
		if i == 1 {
			vals = []NumberDataPoint{{TimeUnixNano: "1000000000", AsDouble: 1000}, {TimeUnixNano: "11000000000", AsDouble: 1010}}
		}
		req.ResourceMetrics = append(req.ResourceMetrics, ResourceMetrics{Resource: Resource{Attributes: []KeyValue{{Key: "service.name", Value: stringAny("same-service")}, {Key: "service.instance.id", Value: stringAny(instance)}}}, ScopeMetrics: []ScopeMetrics{{Scope: InstrumentationScope{Name: "scope"}, Metrics: []OTLPMetric{{Name: "counter", Unit: "1", Sum: &Sum{IsMonotonic: true, AggregationTemporality: 2, DataPoints: vals}}}}}})
	}
	r, err := svc.Ingest(context.Background(), "site", req)
	if err != nil || r.Points != 4 {
		t.Fatalf("ingest: %+v %v", r, err)
	}
	rows := []pointRow{}
	for _, p := range stored {
		rows = append(rows, pointRow{StreamIdentity: p.streamIdentity, ServiceName: p.service, Kind: p.kind, Monotonic: p.monotonic, Temporality: p.temporality, Attributes: p.attrsJSON, Value: p.value, TsNs: p.tsNs})
	}
	// Interleave the two streams exactly as a timestamp-ordered DB read.
	rows = []pointRow{rows[0], rows[2], rows[1], rows[3]}
	out := aggregateSeries(rows, AggRate, 60000)
	if len(out) != 1 || out[0].Value != 2 {
		t.Fatalf("streams merged: %+v", out)
	}
	k1, _ := groupKey(map[string]string{"a": "x\x1fb=y"}, []string{"a"})
	k2, _ := groupKey(map[string]string{"a": "x", "b": "y"}, []string{"a", "b"})
	if k1 == k2 {
		t.Fatal("label framing collision")
	}
	legacy := []pointRow{cumRow(0, 0, ""), cumRow(1, 1000, ""), cumRow(10, 10, ""), cumRow(11, 1010, "")}
	legacy[0].ServiceName = "a"
	legacy[2].ServiceName = "a"
	legacy[1].ServiceName = "b"
	legacy[3].ServiceName = "b"
	out = aggregateSeries(legacy, AggRate, 60000)
	if len(out) != 1 || out[0].Value != 2 {
		t.Fatalf("legacy service identity discarded: %+v", out)
	}
}
func TestOBS06PartialSuccessHTTPAndGRPC(t *testing.T) {
	for _, protobuf := range []bool{false, true} {
		svc := NewService(nil).WithCardinalityLimits(CardinalityLimits{MaxSeriesPerSite: 1})
		svc.writePointRows = func(_ context.Context, _ string, rows []metricPointRow) (int, error) { return len(rows), nil }
		var body []byte
		var err error
		if protobuf {
			body, err = proto.Marshal(&metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*otlpmetrics.ResourceMetrics{{ScopeMetrics: []*otlpmetrics.ScopeMetrics{{Metrics: []*otlpmetrics.Metric{{Name: "a", Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{DataPoints: []*otlpmetrics.NumberDataPoint{{}}}}}, {Name: "b", Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{DataPoints: []*otlpmetrics.NumberDataPoint{{}}}}}}}}}}})
		} else {
			body, err = json.Marshal(gaugeRequest(2))
		}
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(body))
		req = req.WithContext(ingest.WithSiteID(req.Context(), "site"))
		req.Header.Set("Content-Type", "application/json")
		if protobuf {
			req.Header.Set("Content-Type", "application/x-protobuf")
		}
		rr := httptest.NewRecorder()
		NewOTLPHandler(svc).ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var resp metricspb.ExportMetricsServiceResponse
		if protobuf {
			err = proto.Unmarshal(rr.Body.Bytes(), &resp)
		} else {
			err = protojson.Unmarshal(rr.Body.Bytes(), &resp)
		}
		if err != nil || resp.GetPartialSuccess().GetRejectedDataPoints() != 1 {
			t.Fatalf("lost partial response: %v %v", &resp, err)
		}
	}
	svc := NewService(nil).WithCardinalityLimits(CardinalityLimits{MaxSeriesPerSite: 1})
	req := &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*otlpmetrics.ResourceMetrics{{ScopeMetrics: []*otlpmetrics.ScopeMetrics{{Metrics: []*otlpmetrics.Metric{{Name: "summary", Data: &otlpmetrics.Metric_Summary{Summary: &otlpmetrics.Summary{DataPoints: []*otlpmetrics.SummaryDataPoint{{}, {}}}}}}}}}}}
	n, err := IngestOTLPProtoResult(context.Background(), svc, "site", req)
	if err != nil || n != 2 {
		t.Fatalf("unsupported gRPC points acknowledged: %d %v", n, err)
	}
}
func TestOBS06BatchLimitCountsBeforeFiltering(t *testing.T) {
	svc := NewService(nil).WithCardinalityLimits(CardinalityLimits{MaxPointsPerRequest: 1, MaxSeriesPerSite: 1})
	if _, err := svc.Ingest(context.Background(), "site", gaugeRequest(2)); err == nil {
		t.Fatal("oversized batch passed through drop filter")
	}
	if len(svc.guard.sites) != 0 {
		t.Fatal("oversized batch mutated admission registry")
	}
	attrs := svc.guard.truncateAttrs("site", map[string]string{string(bytes.Repeat([]byte{'k'}, 10000)): "v"})
	if len(attrs) != 0 {
		t.Fatal("oversized key retained")
	}
}
func TestOBS102InvalidHistogramsRejectedAndLegacyReadsSafe(t *testing.T) {
	valid := HistogramDataPoint{Count: "2", ExplicitBounds: []float64{10}, BucketCounts: []jsonInt{"1", "1"}}
	cases := []HistogramDataPoint{valid, {Count: "1", ExplicitBounds: []float64{10}, BucketCounts: []jsonInt{"1"}}, {Count: "1", ExplicitBounds: []float64{10}, BucketCounts: []jsonInt{"-1", "2"}}, {Count: "2", ExplicitBounds: []float64{10, 10}, BucketCounts: []jsonInt{"1", "1", "0"}}, {Count: "1", ExplicitBounds: []float64{10}, BucketCounts: []jsonInt{"1", "1"}}}
	for i, dp := range cases {
		if validHistogramPoint(dp) != (i == 0) {
			t.Fatalf("invalid classification case %d", i)
		}
	}
	req := ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{ScopeMetrics: []ScopeMetrics{{Metrics: []OTLPMetric{{Name: "hist", Histogram: &Histogram{DataPoints: cases}}}}}}}}
	svc := NewService(nil)
	svc.writePointRows = func(_ context.Context, _ string, r []metricPointRow) (int, error) { return len(r), nil }
	r, err := svc.Ingest(context.Background(), "site", req)
	if err != nil || r.Points != 1 || r.Rejected != 4 {
		t.Fatalf("bad histogram admission: %+v %v", r, err)
	}
	rows := []pointRow{{Kind: "histogram", Histogram: `{"bounds":[10],"counts":[1],"count":1}`, TsNs: 0}, {Kind: "histogram", Histogram: MarshalHistogram(valid), TsNs: ns(1)}}
	if got := quantileGroupReduce(splitSeries(rows), .95, 60000); len(got) != 0 {
		t.Fatalf("malformed legacy baseline counted: %+v", got)
	}
}
func packedHistogramExport(fragments int, exponential bool) []byte {
	wrap := func(num protowire.Number, body []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), body)
	}
	var packed []byte
	field := protowire.Number(6)
	if exponential {
		field = 2
	}
	for i := 0; i < fragments; i++ {
		packed = append(packed, wrap(field, []byte{1, 0, 0, 0, 0, 0, 0, 0})...)
	}
	metricField := protowire.Number(9)
	if exponential {
		metricField = 10
		packed = wrap(8, packed)
	}
	return wrap(1, wrap(2, wrap(2, wrap(metricField, wrap(1, packed)))))
}
func TestGRPC02WireAdmissionBeforeFastDecoder(t *testing.T) {
	for _, exp := range []bool{false, true} {
		for _, n := range []int{1, 16, 17, 1000} {
			body := packedHistogramExport(n, exp)
			err := ValidateProtoWire(body)
			if (err == nil) != (n <= 16) {
				t.Fatalf("fragments=%d exp=%v: %v", n, exp, err)
			}
			if n > 16 {
				if _, err := decodeProtoMetrics(body); err == nil {
					t.Fatal("HTTP decoder bypassed preflight")
				}
			}
		}
	}
	if err := ValidateProtoWire([]byte{0x0a, 0xff}); err == nil {
		t.Fatal("accepted truncated wire")
	}
}
