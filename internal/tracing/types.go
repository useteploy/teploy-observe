package tracing

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// OTLP JSON types for ExportTraceServiceRequest.
// These match the OpenTelemetry protobuf-to-JSON mapping.

// jsonInt is an OTLP int64 attribute value. The OTLP/JSON spec encodes int64
// as a quoted string, but real-world exporters (@vercel/otel + Next.js, which
// set numeric attributes like http.status_code) emit it as a bare JSON number.
// Accept both, storing the canonical decimal string — otherwise a single
// numeric attribute fails json.Unmarshal for the whole batch and the export
// 400s with "cannot unmarshal number ... intValue of type string".
type jsonInt string

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*j = ""
		return nil
	}
	*j = jsonInt(strings.Trim(s, `"`)) // strip quotes when string-encoded
	return nil
}

type ExportTraceRequest struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

type ScopeSpans struct {
	Scope InstrumentationScope `json:"scope"`
	Spans []OTLPSpan           `json:"spans"`
}

type InstrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type OTLPSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	ParentSpanID      string      `json:"parentSpanId"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []KeyValue  `json:"attributes"`
	Status            SpanStatus  `json:"status"`
	Events            []SpanEvent `json:"events"`
	Links             []SpanLink  `json:"links"`
}

// SpanLink is an OTLP span link: a causal reference from this span to a span
// in the same or another trace (batch consumers, fan-in, retries).
type SpanLink struct {
	TraceID                string     `json:"traceId"`
	SpanID                 string     `json:"spanId"`
	TraceState             string     `json:"traceState"`
	Attributes             []KeyValue `json:"attributes"`
	DroppedAttributesCount int        `json:"droppedAttributesCount"`
}

type SpanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type SpanEvent struct {
	Name         string     `json:"name"`
	TimeUnixNano string     `json:"timeUnixNano"`
	Attributes   []KeyValue `json:"attributes"`
}

type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// valueKind records WHICH AnyValue field the producer actually set. The four
// scalar fields cannot say that on their own: false, 0, 0.0 and "" are the Go
// zero values, so a presence check on the value drops exactly the attributes
// (http.error=false, retry.count=0) an operator most wants to filter on.
type valueKind uint8

const (
	kindUnset valueKind = iota // zero AnyValue, or built by a literal that predates kind
	kindString
	kindInt
	kindBool
	kindDouble
)

// AnyValue is an OTLP attribute value. Decoders (UnmarshalJSON, the protobuf
// translator) record which field was present in an unexported kind tag, so
// AttrsToMap can keep false/0/0.0/"". A literal built outside the decoders
// (the seeder) has no tag and falls back to non-zero detection.
//
// OTLP array, kvlist and bytes values are folded into StringValue at decode
// time as deterministic text (compact JSON with sorted keys; lowercase hex for
// bytes) - there is no richer representation in the string-map storage.
type AnyValue struct {
	StringValue string  `json:"stringValue,omitempty"`
	IntValue    jsonInt `json:"intValue,omitempty"`
	BoolValue   bool    `json:"boolValue,omitempty"`
	DoubleValue float64 `json:"doubleValue,omitempty"`

	kind valueKind
}

func stringAny(s string) AnyValue  { return AnyValue{StringValue: s, kind: kindString} }
func intAny(s string) AnyValue     { return AnyValue{IntValue: jsonInt(s), kind: kindInt} }
func boolAny(b bool) AnyValue      { return AnyValue{BoolValue: b, kind: kindBool} }
func doubleAny(f float64) AnyValue { return AnyValue{DoubleValue: f, kind: kindDouble} }

// maxAnyDepth bounds array/kvlist nesting when folding to text; deeper levels
// are replaced by a marker so a hostile payload cannot drive the recursion.
const maxAnyDepth = 16

// anyWire is the OTLP/JSON shape with pointer/raw fields so presence survives.
type anyWire struct {
	StringValue *string         `json:"stringValue"`
	IntValue    *jsonInt        `json:"intValue"`
	BoolValue   *bool           `json:"boolValue"`
	DoubleValue json.RawMessage `json:"doubleValue"`
	ArrayValue  json.RawMessage `json:"arrayValue"`
	KvlistValue json.RawMessage `json:"kvlistValue"`
	BytesValue  *string         `json:"bytesValue"`
}

func (a *AnyValue) UnmarshalJSON(b []byte) error {
	return a.unmarshalDepth(b, 0)
}

func (a *AnyValue) unmarshalDepth(b []byte, depth int) error {
	*a = AnyValue{}
	if s := strings.TrimSpace(string(b)); s == "null" || s == "" {
		return nil
	}
	var w anyWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	switch {
	case w.StringValue != nil:
		*a = stringAny(*w.StringValue)
	case w.IntValue != nil:
		*a = intAny(string(*w.IntValue))
	case w.BoolValue != nil:
		*a = boolAny(*w.BoolValue)
	case isPresent(w.DoubleValue):
		f, err := parseJSONDouble(w.DoubleValue)
		if err != nil {
			return err
		}
		*a = doubleAny(f)
	case isPresent(w.ArrayValue) || isPresent(w.KvlistValue):
		n, err := containerNative(w, depth)
		if err != nil {
			return err
		}
		*a = stringAny(renderNative(n))
	case w.BytesValue != nil:
		// OTLP/JSON bytes are base64; the protobuf path renders hex, so
		// normalise to hex to keep both wire formats byte-identical.
		raw, err := base64.StdEncoding.DecodeString(*w.BytesValue)
		if err != nil {
			*a = stringAny(*w.BytesValue)
		} else {
			*a = stringAny(hex.EncodeToString(raw))
		}
	}
	return nil
}

// parseJSONDouble accepts a JSON number or the protobuf-JSON string forms
// ("1.5", "NaN", "Infinity", "-Infinity").
func parseJSONDouble(raw json.RawMessage) (float64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		return math.Inf(1), nil
	case "-Infinity":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

func isPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// containerNative converts an OTLP/JSON array or kvlist into nested plain Go
// values (slices / maps), so nesting renders as real JSON rather than as
// escaped strings inside strings.
func containerNative(w anyWire, depth int) (any, error) {
	if isPresent(w.ArrayValue) {
		var arr struct {
			Values []json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(w.ArrayValue, &arr); err != nil {
			return nil, err
		}
		list := make([]any, 0, len(arr.Values))
		for _, raw := range arr.Values {
			list = append(list, jsonNative(raw, depth+1))
		}
		return list, nil
	}
	var kv struct {
		Values []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"values"`
	}
	if err := json.Unmarshal(w.KvlistValue, &kv); err != nil {
		return nil, err
	}
	m := make(map[string]any, len(kv.Values))
	for _, e := range kv.Values {
		m[e.Key] = jsonNative(e.Value, depth+1)
	}
	return m, nil
}

// jsonNative converts a nested OTLP/JSON AnyValue into a plain Go value for
// deterministic rendering.
func jsonNative(raw json.RawMessage, depth int) any {
	if depth > maxAnyDepth {
		return "..."
	}
	var w anyWire
	if s := strings.TrimSpace(string(raw)); s == "null" || s == "" || json.Unmarshal(raw, &w) != nil {
		return nil
	}
	if isPresent(w.ArrayValue) || isPresent(w.KvlistValue) {
		n, err := containerNative(w, depth)
		if err != nil {
			return nil
		}
		return n
	}
	var v AnyValue
	if err := v.unmarshalDepth(raw, depth); err != nil {
		return nil
	}
	return v.native()
}

// native returns the typed Go value of a scalar AnyValue (nested containers
// are already folded to text and therefore come back as strings).
func (a AnyValue) native() any {
	switch a.effectiveKind() {
	case kindString:
		return a.StringValue
	case kindInt:
		if _, err := strconv.ParseInt(string(a.IntValue), 10, 64); err == nil {
			return json.Number(a.IntValue)
		}
		return string(a.IntValue)
	case kindBool:
		return a.BoolValue
	case kindDouble:
		if math.IsNaN(a.DoubleValue) || math.IsInf(a.DoubleValue, 0) {
			return formatDouble(a.DoubleValue)
		}
		return json.Number(formatDouble(a.DoubleValue))
	}
	return nil
}

// renderNative is compact JSON; encoding/json sorts map keys, so equal inputs
// render byte-identical regardless of wire format or producer key order.
func renderNative(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

func formatDouble(f float64) string { return fmt.Sprintf("%g", f) }

// effectiveKind is the recorded kind, or - for a literal with no tag - the
// first non-zero field (the pre-tag behaviour).
func (a AnyValue) effectiveKind() valueKind {
	if a.kind != kindUnset {
		return a.kind
	}
	switch {
	case a.StringValue != "":
		return kindString
	case a.IntValue != "":
		return kindInt
	case a.BoolValue:
		return kindBool
	case a.DoubleValue != 0:
		return kindDouble
	}
	return kindUnset
}

// text renders a value for the string-map attribute store; ok is false when
// the producer set no value at all.
func (a AnyValue) text() (string, bool) {
	switch a.effectiveKind() {
	case kindString:
		return a.StringValue, true
	case kindInt:
		return string(a.IntValue), true
	case kindBool:
		return strconv.FormatBool(a.BoolValue), true
	case kindDouble:
		return formatDouble(a.DoubleValue), true
	}
	return "", false
}

// MarshalJSON emits the field the producer set, including false/0/0.0/"",
// so span events (stored as marshalled JSON) keep those values too.
func (a AnyValue) MarshalJSON() ([]byte, error) {
	switch a.effectiveKind() {
	case kindString:
		return json.Marshal(struct {
			V string `json:"stringValue"`
		}{a.StringValue})
	case kindInt:
		return json.Marshal(struct {
			V jsonInt `json:"intValue"`
		}{a.IntValue})
	case kindBool:
		return json.Marshal(struct {
			V bool `json:"boolValue"`
		}{a.BoolValue})
	case kindDouble:
		if math.IsNaN(a.DoubleValue) || math.IsInf(a.DoubleValue, 0) {
			return json.Marshal(struct {
				V string `json:"doubleValue"`
			}{formatDouble(a.DoubleValue)})
		}
		return json.Marshal(struct {
			V float64 `json:"doubleValue"`
		}{a.DoubleValue})
	}
	return []byte("{}"), nil
}

// SpanKind maps OTLP integer to string.
func SpanKind(kind int) string {
	switch kind {
	case 1:
		return "internal"
	case 2:
		return "server"
	case 3:
		return "client"
	case 4:
		return "producer"
	case 5:
		return "consumer"
	default:
		return "unset"
	}
}

// StatusCode maps OTLP status code to string.
func StatusCode(code int) string {
	switch code {
	case 0:
		return "unset"
	case 1:
		return "ok"
	case 2:
		return "error"
	default:
		return "unset"
	}
}

// ExtractServiceName finds service.name in resource attributes.
func ExtractServiceName(attrs []KeyValue) string {
	for _, kv := range attrs {
		if kv.Key == "service.name" {
			if kv.Value.StringValue != "" {
				return kv.Value.StringValue
			}
		}
	}
	return "unknown"
}

// AttrsToMap converts OTLP KeyValue slice to a string map for JSONB storage.
// A key whose value the producer set is always kept - including false, 0,
// 0.0 and the empty string - and only a value with no field set is skipped.
func AttrsToMap(attrs []KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if v, ok := kv.Value.text(); ok {
			m[kv.Key] = v
		}
	}
	return m
}
