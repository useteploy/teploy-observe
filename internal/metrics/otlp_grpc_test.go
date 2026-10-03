package metrics

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestIngestOTLPProtoMatchesHTTPDecode(t *testing.T) {
	req := &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*otlpmetrics.ResourceMetrics{{
		ScopeMetrics: []*otlpmetrics.ScopeMetrics{{Metrics: []*otlpmetrics.Metric{{
			Name: "m",
			Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{DataPoints: []*otlpmetrics.NumberDataPoint{
				{TimeUnixNano: 1, Value: &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: 2.5}},
			}}},
		}}}},
	}}}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeProtoMetrics(body)
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

func TestIngestOTLPProtoReturnsCardinalityRefusalAsIs(t *testing.T) {
	refusal := &ErrTooManyPoints{Points: 10, Limit: 5}
	err := IngestOTLPProto(context.Background(), &recordingIngester{err: refusal}, "s", &metricspb.ExportMetricsServiceRequest{})
	var got *ErrTooManyPoints
	if !errors.As(err, &got) {
		t.Fatalf("want *ErrTooManyPoints, got %v", err)
	}
}
