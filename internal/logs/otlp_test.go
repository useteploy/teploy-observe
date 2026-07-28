package logs

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Both wire formats must produce identical LogInputs — same store, same
// pipelines, so a record must not depend on how it arrived.
func TestOTLPLogs_JSONAndProtobufAgree(t *testing.T) {
	pb := &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*otlplogs.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "worker"}},
			}}},
			ScopeLogs: []*otlplogs.ScopeLogs{{
				LogRecords: []*otlplogs.LogRecord{{
					SeverityNumber: otlplogs.SeverityNumber_SEVERITY_NUMBER_ERROR,
					Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "job failed"}},
					TraceId:        []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
					SpanId:         []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
					Attributes: []*commonpb.KeyValue{{
						Key:   "job.id",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "j-7"}},
					}},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fromProto, err := protoLogInputs(body, "site-1")
	if err != nil {
		t.Fatalf("proto decode: %v", err)
	}

	// The equivalent OTLP/JSON payload — note ids are hex here, which is why
	// this path is hand-decoded rather than run through protojson.
	jsonBody := []byte(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"worker"}}]},
	  "scopeLogs":[{"logRecords":[{"severityNumber":17,"body":{"stringValue":"job failed"},
	  "traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1112131415161718",
	  "attributes":[{"key":"job.id","value":{"stringValue":"j-7"}}]}]}]}]}`)
	fromJSON, err := jsonLogInputs(jsonBody, "site-1")
	if err != nil {
		t.Fatalf("json decode: %v", err)
	}

	if len(fromProto) != 1 || len(fromJSON) != 1 {
		t.Fatalf("counts: proto=%d json=%d", len(fromProto), len(fromJSON))
	}
	a, _ := json.Marshal(fromProto[0])
	b, _ := json.Marshal(fromJSON[0])
	if string(a) != string(b) {
		t.Errorf("wire formats disagree:\n proto=%s\n json =%s", a, b)
	}

	got := fromProto[0]
	if got.Level != "error" {
		t.Errorf("level = %q, want error (severityNumber 17)", got.Level)
	}
	if got.Message != "job failed" || got.ServiceName != "worker" {
		t.Errorf("message/service = %q/%q", got.Message, got.ServiceName)
	}
	if got.TraceID != "0102030405060708090a0b0c0d0e0f10" || got.SpanID != "1112131415161718" {
		t.Errorf("ids = %q/%q", got.TraceID, got.SpanID)
	}
	if got.Attributes["job.id"] != "j-7" || got.Attributes["service.name"] != "worker" {
		t.Errorf("attributes = %+v (resource attrs should be merged in)", got.Attributes)
	}
}

func TestSeverityToLevel(t *testing.T) {
	cases := []struct {
		number int
		text   string
		want   string
	}{
		{0, "", "info"},   // unspecified
		{1, "", "trace"},
		{5, "", "debug"},
		{9, "", "info"},
		{13, "", "warn"},
		{17, "", "error"},
		{21, "", "fatal"},
		{24, "", "fatal"},
		{9, "NOTICE", "notice"}, // producer's own text wins
		{17, "Warn", "warn"},    // and is lowercased
	}
	for _, c := range cases {
		if got := severityToLevel(c.number, c.text); got != c.want {
			t.Errorf("severityToLevel(%d, %q) = %q, want %q", c.number, c.text, got, c.want)
		}
	}
}
