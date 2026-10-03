package tracing

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

// mixedAttrsJSON is a real OTLP/JSON attribute list covering every scalar kind
// at its zero value plus array, kvlist and bytes.
const mixedAttrsJSON = `[
 {"key":"s.text","value":{"stringValue":"hello"}},
 {"key":"s.empty","value":{"stringValue":""}},
 {"key":"i.num","value":{"intValue":"42"}},
 {"key":"i.bare","value":{"intValue":7}},
 {"key":"i.zero","value":{"intValue":"0"}},
 {"key":"b.true","value":{"boolValue":true}},
 {"key":"b.false","value":{"boolValue":false}},
 {"key":"d.pi","value":{"doubleValue":3.5}},
 {"key":"d.zero","value":{"doubleValue":0}},
 {"key":"a.list","value":{"arrayValue":{"values":[{"stringValue":"x"},{"intValue":"1"},{"boolValue":false},{"doubleValue":0}]}}},
 {"key":"k.map","value":{"kvlistValue":{"values":[{"key":"z","value":{"intValue":"2"}},{"key":"a","value":{"stringValue":"b"}}]}}},
 {"key":"y.bytes","value":{"bytesValue":"AQID"}},
 {"key":"n.none","value":{}}
]`

// n.none carries no value at all and is skipped.
var mixedAttrsGolden = map[string]string{
	"s.text":  "hello",
	"s.empty": "",
	"i.num":   "42",
	"i.bare":  "7",
	"i.zero":  "0",
	"b.true":  "true",
	"b.false": "false",
	"d.pi":    "3.5",
	"d.zero":  "0",
	"a.list":  `["x",1,false,0]`,
	"k.map":   `{"a":"b","z":2}`,
	"y.bytes": "010203",
}

func mixedAttrsProto() []*commonpb.KeyValue {
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	i64 := func(n int64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
	}
	bl := func(b bool) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
	}
	dbl := func(f float64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	}
	return []*commonpb.KeyValue{
		{Key: "s.text", Value: str("hello")},
		{Key: "s.empty", Value: str("")},
		{Key: "i.num", Value: i64(42)},
		{Key: "i.bare", Value: i64(7)},
		{Key: "i.zero", Value: i64(0)},
		{Key: "b.true", Value: bl(true)},
		{Key: "b.false", Value: bl(false)},
		{Key: "d.pi", Value: dbl(3.5)},
		{Key: "d.zero", Value: dbl(0)},
		{Key: "a.list", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
			Values: []*commonpb.AnyValue{str("x"), i64(1), bl(false), dbl(0)}}}}},
		{Key: "k.map", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{{Key: "z", Value: i64(2)}, {Key: "a", Value: str("b")}}}}}},
		{Key: "y.bytes", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1, 2, 3}}}},
		{Key: "n.none", Value: &commonpb.AnyValue{}},
	}
}

func TestAttrsToMap_JSONPreservesZeroValues(t *testing.T) {
	var kvs []KeyValue
	if err := json.Unmarshal([]byte(mixedAttrsJSON), &kvs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := AttrsToMap(kvs)
	if !reflect.DeepEqual(got, mixedAttrsGolden) {
		t.Fatalf("json attrs mismatch\n got: %#v\nwant: %#v", got, mixedAttrsGolden)
	}
}

func TestAttrsToMap_ProtobufPreservesZeroValues(t *testing.T) {
	req := &tracepb.ExportTraceServiceRequest{ResourceSpans: []*otlptrace.ResourceSpans{{
		ScopeSpans: []*otlptrace.ScopeSpans{{Spans: []*otlptrace.Span{{
			TraceId: []byte{1}, SpanId: []byte{2}, Attributes: mixedAttrsProto(),
		}}}},
	}}}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProtoTraces(body)
	if err != nil {
		t.Fatal(err)
	}
	got := AttrsToMap(decoded.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes)
	if !reflect.DeepEqual(got, mixedAttrsGolden) {
		t.Fatalf("protobuf attrs mismatch\n got: %#v\nwant: %#v", got, mixedAttrsGolden)
	}
}

// The stored span-event JSON goes through AnyValue.MarshalJSON; false/0 must
// survive that too, and round-trip back through the decoder.
func TestAnyValue_MarshalRoundTripKeepsZeroValues(t *testing.T) {
	var kvs []KeyValue
	if err := json.Unmarshal([]byte(mixedAttrsJSON), &kvs); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(kvs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"boolValue":false`, `"intValue":"0"`, `"doubleValue":0`, `"stringValue":""`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("marshalled attrs missing %s: %s", want, raw)
		}
	}
	var back []KeyValue
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := AttrsToMap(back); !reflect.DeepEqual(got, mixedAttrsGolden) {
		t.Fatalf("round trip changed attrs: %#v", got)
	}
}

// A literal built without a kind (the seeder) keeps the legacy behaviour.
func TestAttrsToMap_KindlessLiteralFallback(t *testing.T) {
	got := AttrsToMap([]KeyValue{
		{Key: "a", Value: AnyValue{StringValue: "x"}},
		{Key: "b", Value: AnyValue{}},
	})
	if got["a"] != "x" {
		t.Fatalf("a = %q", got["a"])
	}
	if _, ok := got["b"]; ok {
		t.Fatalf("empty literal should be skipped: %#v", got)
	}
}

func TestAnyValue_DoubleStringForms(t *testing.T) {
	var kvs []KeyValue
	body := `[{"key":"n","value":{"doubleValue":"NaN"}},{"key":"i","value":{"doubleValue":"-Infinity"}},{"key":"s","value":{"doubleValue":"2.5"}}]`
	if err := json.Unmarshal([]byte(body), &kvs); err != nil {
		t.Fatal(err)
	}
	got := AttrsToMap(kvs)
	if got["n"] != "NaN" || got["i"] != "-Inf" || got["s"] != "2.5" {
		t.Fatalf("got %#v", got)
	}
}

func TestAnyValue_DeepNestingBounded(t *testing.T) {
	body := `{"stringValue":"leaf"}`
	for i := 0; i < 200; i++ {
		body = `{"arrayValue":{"values":[` + body + `]}}`
	}
	var v AnyValue
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.StringValue, `"..."`) {
		t.Fatalf("deep nesting not truncated: %.80s", v.StringValue)
	}
}
