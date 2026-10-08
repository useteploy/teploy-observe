package otlpgrpc

import (
	"fmt"
	"github.com/useteploy/teploy-observe/internal/metrics"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Admission runs after tap authentication but before protobuf decoding.
type admissionCodec struct{}

func (admissionCodec) Name() string { return "proto" }
func (admissionCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("invalid protobuf message")
	}
	return proto.Marshal(m)
}
func (admissionCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("invalid protobuf message")
	}
	if _, ok := v.(*metricspb.ExportMetricsServiceRequest); ok {
		if err := metrics.ValidateProtoWire(data); err != nil {
			return err
		}
	}
	return proto.Unmarshal(data, m)
}
