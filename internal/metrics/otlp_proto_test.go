package metrics

import (
	"testing"

	"google.golang.org/protobuf/proto"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestDecodeProtoMetrics_AllThreeShapes(t *testing.T) {
	req := &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpmetrics.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}},
			}}},
			ScopeMetrics: []*otlpmetrics.ScopeMetrics{{
				Scope: &commonpb.InstrumentationScope{Name: "runtime"},
				Metrics: []*otlpmetrics.Metric{
					{
						Name: "heap.used", Unit: "By",
						Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{
							DataPoints: []*otlpmetrics.NumberDataPoint{{
								TimeUnixNano: 1700000000000000000,
								Value:        &otlpmetrics.NumberDataPoint_AsInt{AsInt: 4096},
							}},
						}},
					},
					{
						Name: "requests.total",
						Data: &otlpmetrics.Metric_Sum{Sum: &otlpmetrics.Sum{
							IsMonotonic:            true,
							AggregationTemporality: otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
							DataPoints: []*otlpmetrics.NumberDataPoint{{
								TimeUnixNano: 1700000000000000000,
								Value:        &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: 12.5},
							}},
						}},
					},
					{
						Name: "latency",
						Data: &otlpmetrics.Metric_Histogram{Histogram: &otlpmetrics.Histogram{
							AggregationTemporality: otlpmetrics.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
							DataPoints: []*otlpmetrics.HistogramDataPoint{{
								TimeUnixNano:   1700000000000000000,
								Count:          3,
								Sum:            proto.Float64(9.0),
								BucketCounts:   []uint64{1, 2},
								ExplicitBounds: []float64{5},
							}},
						}},
					},
				},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeProtoMetrics(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ms := got.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(ms) != 3 {
		t.Fatalf("metrics = %d, want 3", len(ms))
	}

	if ms[0].Gauge == nil || ms[0].Gauge.DataPoints[0].AsInt != "4096" {
		t.Errorf("gauge = %+v", ms[0].Gauge)
	}
	if ms[0].Unit != "By" {
		t.Errorf("unit = %q", ms[0].Unit)
	}
	if ms[1].Sum == nil || ms[1].Sum.DataPoints[0].AsDouble != 12.5 {
		t.Errorf("sum = %+v", ms[1].Sum)
	}
	if !ms[1].Sum.IsMonotonic || ms[1].Sum.AggregationTemporality != 2 {
		t.Errorf("sum flags: monotonic=%v temporality=%d", ms[1].Sum.IsMonotonic, ms[1].Sum.AggregationTemporality)
	}
	h := ms[2].Histogram
	if h == nil || h.DataPoints[0].Count != "3" || h.DataPoints[0].Sum != 9.0 {
		t.Fatalf("histogram = %+v", h)
	}
	if len(h.DataPoints[0].BucketCounts) != 2 || h.DataPoints[0].BucketCounts[0] != "1" {
		t.Errorf("bucketCounts = %+v", h.DataPoints[0].BucketCounts)
	}
	if h.AggregationTemporality != 1 {
		t.Errorf("delta temporality = %d, want 1", h.AggregationTemporality)
	}
}

func TestDecodeProtoMetrics_RejectsGarbage(t *testing.T) {
	if _, err := decodeProtoMetrics([]byte("definitely not protobuf, just prose")); err == nil {
		t.Fatal("expected an error")
	}
}
