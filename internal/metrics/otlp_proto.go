package metrics

import (
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// Protobuf decode for /v1/metrics, mirroring internal/tracing/otlp_proto.go.
// OTLP/HTTP exporters send application/x-protobuf by default, so JSON-only
// ingest rejected the out-of-the-box configuration of every OTel SDK and the
// Collector. Decoded into the same ExportMetricsRequest the JSON path builds,
// so ingest, validation and storage remain single-sourced.

func decodeProtoMetrics(body []byte) (ExportMetricsRequest, error) {
	var pb metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &pb); err != nil {
		return ExportMetricsRequest{}, fmt.Errorf("invalid protobuf: %w", err)
	}

	out := ExportMetricsRequest{ResourceMetrics: make([]ResourceMetrics, 0, len(pb.GetResourceMetrics()))}
	for _, rm := range pb.GetResourceMetrics() {
		res := ResourceMetrics{
			Resource:     Resource{Attributes: protoAttrs(rm.GetResource().GetAttributes())},
			ScopeMetrics: make([]ScopeMetrics, 0, len(rm.GetScopeMetrics())),
		}
		for _, sm := range rm.GetScopeMetrics() {
			scope := ScopeMetrics{
				Scope: InstrumentationScope{
					Name:    sm.GetScope().GetName(),
					Version: sm.GetScope().GetVersion(),
				},
				Metrics: make([]OTLPMetric, 0, len(sm.GetMetrics())),
			}
			for _, m := range sm.GetMetrics() {
				scope.Metrics = append(scope.Metrics, protoMetric(m))
			}
			res.ScopeMetrics = append(res.ScopeMetrics, scope)
		}
		out.ResourceMetrics = append(out.ResourceMetrics, res)
	}
	return out, nil
}

// protoMetric maps the three data shapes the JSON path models. Summary and
// exponential histograms are not represented on the JSON side either, so they
// arrive as a metric with no data points rather than being mis-mapped.
func protoMetric(m *otlpmetrics.Metric) OTLPMetric {
	out := OTLPMetric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit()}
	switch d := m.GetData().(type) {
	case *otlpmetrics.Metric_Gauge:
		out.Gauge = &Gauge{DataPoints: protoNumberPoints(d.Gauge.GetDataPoints())}
	case *otlpmetrics.Metric_Sum:
		out.Sum = &Sum{
			DataPoints:             protoNumberPoints(d.Sum.GetDataPoints()),
			AggregationTemporality: int(d.Sum.GetAggregationTemporality()),
			IsMonotonic:            d.Sum.GetIsMonotonic(),
		}
	case *otlpmetrics.Metric_Histogram:
		out.Histogram = &Histogram{
			DataPoints:             protoHistogramPoints(d.Histogram.GetDataPoints()),
			AggregationTemporality: int(d.Histogram.GetAggregationTemporality()),
		}
	}
	return out
}

func protoNumberPoints(pts []*otlpmetrics.NumberDataPoint) []NumberDataPoint {
	out := make([]NumberDataPoint, 0, len(pts))
	for _, p := range pts {
		dp := NumberDataPoint{
			Attributes:   protoAttrs(p.GetAttributes()),
			TimeUnixNano: fmt.Sprintf("%d", p.GetTimeUnixNano()),
		}
		switch v := p.GetValue().(type) {
		case *otlpmetrics.NumberDataPoint_AsDouble:
			dp.AsDouble = v.AsDouble
		case *otlpmetrics.NumberDataPoint_AsInt:
			dp.AsInt = jsonInt(fmt.Sprintf("%d", v.AsInt))
		}
		out = append(out, dp)
	}
	return out
}

func protoHistogramPoints(pts []*otlpmetrics.HistogramDataPoint) []HistogramDataPoint {
	out := make([]HistogramDataPoint, 0, len(pts))
	for _, p := range pts {
		dp := HistogramDataPoint{
			Attributes:     protoAttrs(p.GetAttributes()),
			TimeUnixNano:   fmt.Sprintf("%d", p.GetTimeUnixNano()),
			Count:          jsonInt(fmt.Sprintf("%d", p.GetCount())),
			Sum:            p.GetSum(),
			ExplicitBounds: p.GetExplicitBounds(),
		}
		for _, c := range p.GetBucketCounts() {
			dp.BucketCounts = append(dp.BucketCounts, jsonInt(fmt.Sprintf("%d", c)))
		}
		out = append(out, dp)
	}
	return out
}

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
		return AnyValue{StringValue: v.String()}
	}
}
