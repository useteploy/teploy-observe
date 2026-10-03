package metrics

import (
	"encoding/json"
	"reflect"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// false, 0, 0.0 and "" are values a producer set; they must not vanish.
func TestAttrsToMap_JSONPreservesZeroValues(t *testing.T) {
	body := `[{"key":"s","value":{"stringValue":""}},{"key":"i","value":{"intValue":"0"}},
	{"key":"b","value":{"boolValue":false}},{"key":"d","value":{"doubleValue":0}},
	{"key":"t","value":{"boolValue":true}},{"key":"none","value":{}}]`
	var kvs []KeyValue
	if err := json.Unmarshal([]byte(body), &kvs); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"s": "", "i": "0", "b": "false", "d": "0", "t": "true"}
	if got := AttrsToMap(kvs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestAttrsToMap_ProtoPreservesZeroValues(t *testing.T) {
	pb := []*commonpb.KeyValue{
		{Key: "s", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}},
		{Key: "i", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 0}}},
		{Key: "b", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		{Key: "d", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0}}},
	}
	want := map[string]string{"s": "", "i": "0", "b": "false", "d": "0"}
	if got := AttrsToMap(protoAttrs(pb)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
