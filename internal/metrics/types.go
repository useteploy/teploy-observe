package metrics

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// OTLP JSON types for ExportMetricsServiceRequest.
// These mirror the OpenTelemetry protobuf-to-JSON mapping for the metrics
// signal. Only the fields Observe actually persists are decoded — anything
// extra in the wire payload is silently ignored, matching the tracing
// package's tolerance policy.

// jsonInt is an OTLP int64 field (data-point asInt, histogram count/bucket
// counts, attribute intValue). The OTLP/JSON spec encodes int64 as a quoted
// string, but real-world exporters emit bare JSON numbers. Accept both,
// storing the canonical decimal string — otherwise a single numeric field
// fails json.Unmarshal for the whole batch and the export 400s. Mirrors
// tracing.jsonInt.
type jsonInt string

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*j = ""
		return nil
	}
	*j = jsonInt(strings.Trim(s, `"`))
	return nil
}

type ExportMetricsRequest struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics"`
}

type ResourceMetrics struct {
	SchemaURL    string         `json:"schemaUrl"`
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

type ScopeMetrics struct {
	SchemaURL string               `json:"schemaUrl"`
	Scope     InstrumentationScope `json:"scope"`
	Metrics   []OTLPMetric         `json:"metrics"`
}

type InstrumentationScope struct {
	Attributes []KeyValue `json:"attributes"`
	Name       string     `json:"name"`
	Version    string     `json:"version"`
}

// OTLPMetric is a single metric of one of three kinds. Exactly one of
// Gauge / Sum / Histogram is populated per OTLP spec. The unmarshaller
// inspects the populated field to pick the kind.
type OTLPMetric struct {
	Name                 string             `json:"name"`
	Description          string             `json:"description"`
	Unit                 string             `json:"unit"`
	Gauge                *Gauge             `json:"gauge,omitempty"`
	Sum                  *Sum               `json:"sum,omitempty"`
	Histogram            *Histogram         `json:"histogram,omitempty"`
	Summary              *UnsupportedMetric `json:"summary,omitempty"`
	ExponentialHistogram *UnsupportedMetric `json:"exponentialHistogram,omitempty"`
}

type UnsupportedMetric struct {
	DataPoints []json.RawMessage `json:"dataPoints"`
}

type Gauge struct {
	DataPoints []NumberDataPoint `json:"dataPoints"`
}

type Sum struct {
	DataPoints             []NumberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type Histogram struct {
	DataPoints             []HistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

// NumberDataPoint covers gauge + sum points. Per OTLP spec exactly one of
// AsDouble / AsInt is populated; we coerce to float64 in Go because that's
// the only type the storage layer keeps.
type NumberDataPoint struct {
	Attributes   []KeyValue `json:"attributes"`
	TimeUnixNano string     `json:"timeUnixNano"`
	AsDouble     float64    `json:"asDouble,omitempty"`
	AsInt        jsonInt    `json:"asInt,omitempty"`
}

// HistogramDataPoint mirrors the OTLP histogram shape: parallel arrays
// of bucket bounds and counts, plus an explicit sum + count.
type HistogramDataPoint struct {
	Attributes     []KeyValue `json:"attributes"`
	TimeUnixNano   string     `json:"timeUnixNano"`
	Count          jsonInt    `json:"count"`
	Sum            float64    `json:"sum"`
	BucketCounts   []jsonInt  `json:"bucketCounts"`
	ExplicitBounds []float64  `json:"explicitBounds"`
}

type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

type ArrayValue struct {
	Values []AnyValue `json:"values"`
}
type KeyValueList struct {
	Values []KeyValue `json:"values"`
}

type AnyValue struct {
	ArrayValue  *ArrayValue   `json:"arrayValue,omitempty"`
	KVListValue *KeyValueList `json:"kvlistValue,omitempty"`
	BytesValue  string        `json:"bytesValue,omitempty"`
	StringValue string        `json:"stringValue,omitempty"`
	IntValue    jsonInt       `json:"intValue,omitempty"`
	BoolValue   bool          `json:"boolValue,omitempty"`
	DoubleValue float64       `json:"doubleValue,omitempty"`

	// kind records which field the decoder saw set. The scalar fields cannot
	// say that themselves (false, 0, 0.0 and "" are Go zero values), so
	// without it AttrsToMap dropped those attributes. A literal with no kind
	// falls back to non-zero detection.
	kind valueKind
}

type valueKind uint8

const (
	kindUnset valueKind = iota
	kindString
	kindInt
	kindBool
	kindDouble
	kindArray
	kindKVList
	kindBytes
)

func stringAny(s string) AnyValue  { return AnyValue{StringValue: s, kind: kindString} }
func intAny(s string) AnyValue     { return AnyValue{IntValue: jsonInt(s), kind: kindInt} }
func boolAny(b bool) AnyValue      { return AnyValue{BoolValue: b, kind: kindBool} }
func doubleAny(f float64) AnyValue { return AnyValue{DoubleValue: f, kind: kindDouble} }

func (a *AnyValue) UnmarshalJSON(b []byte) error {
	*a = AnyValue{}
	var w struct {
		ArrayValue  *ArrayValue   `json:"arrayValue"`
		KVListValue *KeyValueList `json:"kvlistValue"`
		BytesValue  *string       `json:"bytesValue"`
		StringValue *string       `json:"stringValue"`
		IntValue    *jsonInt      `json:"intValue"`
		BoolValue   *bool         `json:"boolValue"`
		DoubleValue *float64      `json:"doubleValue"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	switch {
	case w.ArrayValue != nil:
		a.ArrayValue, a.kind = w.ArrayValue, kindArray
	case w.KVListValue != nil:
		a.KVListValue, a.kind = w.KVListValue, kindKVList
	case w.BytesValue != nil:
		a.BytesValue, a.kind = *w.BytesValue, kindBytes
	case w.StringValue != nil:
		*a = stringAny(*w.StringValue)
	case w.IntValue != nil:
		*a = intAny(string(*w.IntValue))
	case w.BoolValue != nil:
		*a = boolAny(*w.BoolValue)
	case w.DoubleValue != nil:
		*a = doubleAny(*w.DoubleValue)
	}
	return nil
}

// text renders the value for the string-map label store; ok is false when the
// producer set no value.
func (a AnyValue) text() (string, bool) {
	k := a.kind
	if k == kindUnset {
		switch {
		case a.StringValue != "":
			k = kindString
		case a.IntValue != "":
			k = kindInt
		case a.BoolValue:
			k = kindBool
		case a.DoubleValue != 0:
			k = kindDouble
		}
	}
	switch k {
	case kindArray:
		values := []any{}
		if a.ArrayValue != nil {
			for _, v := range a.ArrayValue.Values {
				text, _ := v.text()
				values = append(values, []any{v.kind, text})
			}
		}
		raw, _ := json.Marshal(values)
		return string(raw), true
	case kindKVList:
		values := map[string]any{}
		if a.KVListValue != nil {
			for _, kv := range a.KVListValue.Values {
				text, _ := kv.Value.text()
				values[kv.Key] = []any{kv.Value.kind, text}
			}
		}
		raw, _ := json.Marshal(values)
		return string(raw), true
	case kindBytes:
		return a.BytesValue, true
	case kindString:
		return a.StringValue, true
	case kindInt:
		return string(a.IntValue), true
	case kindBool:
		return strconv.FormatBool(a.BoolValue), true
	case kindDouble:
		return fmt.Sprintf("%g", a.DoubleValue), true
	}
	return "", false
}

// AggregationTemporality maps the OTLP enum to the column value.
//
//	1 = delta, 2 = cumulative.
//
// Anything else collapses to "cumulative" — matches the OTLP default
// for SDKs that omit the field.
func AggregationTemporality(t int) string {
	switch t {
	case 1:
		return "delta"
	case 2:
		return "cumulative"
	default:
		return "cumulative"
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
	return ""
}
