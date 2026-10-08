package otlpgrpc

import (
	"context"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
)

func TestOBS06MetricsTransportReportsPartialSuccess(t *testing.T) {
	s := &Server{deps: Deps{Ingester: Ingesters{MetricsResult: func(context.Context, string, *metricspb.ExportMetricsServiceRequest) (int, error) { return 3, nil }}}}
	resp, err := (&metricsService{s: s}).Export(context.Background(), &metricspb.ExportMetricsServiceRequest{})
	if err != nil || resp.GetPartialSuccess().GetRejectedDataPoints() != 3 {
		t.Fatalf("partial rejection lost: %+v %v", resp, err)
	}
}
func TestGRPC02CodecRejectsFragmentationBeforeDecode(t *testing.T) {
	wrap := func(n protowire.Number, b []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
	}
	var packed []byte
	for i := 0; i < 1000; i++ {
		packed = append(packed, wrap(6, []byte{1, 0, 0, 0, 0, 0, 0, 0})...)
	}
	raw := wrap(1, wrap(2, wrap(2, wrap(9, wrap(1, packed)))))
	var req metricspb.ExportMetricsServiceRequest
	if err := (admissionCodec{}).Unmarshal(raw, &req); err == nil {
		t.Fatal("codec decoded fragmented histogram")
	}
	if len(req.ResourceMetrics) != 0 {
		t.Fatal("preflight rejected after allocating request")
	}
}
