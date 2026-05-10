package metrics

// OTLP JSON types for ExportMetricsServiceRequest.
// These mirror the OpenTelemetry protobuf-to-JSON mapping for the metrics
// signal. Only the fields Observe actually persists are decoded — anything
// extra in the wire payload is silently ignored, matching the tracing
// package's tolerance policy.

type ExportMetricsRequest struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics"`
}

type ResourceMetrics struct {
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

type ScopeMetrics struct {
	Scope   InstrumentationScope `json:"scope"`
	Metrics []OTLPMetric         `json:"metrics"`
}

type InstrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// OTLPMetric is a single metric of one of three kinds. Exactly one of
// Gauge / Sum / Histogram is populated per OTLP spec. The unmarshaller
// inspects the populated field to pick the kind.
type OTLPMetric struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Unit        string     `json:"unit"`
	Gauge       *Gauge     `json:"gauge,omitempty"`
	Sum         *Sum       `json:"sum,omitempty"`
	Histogram   *Histogram `json:"histogram,omitempty"`
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
	AsInt        string     `json:"asInt,omitempty"`
}

// HistogramDataPoint mirrors the OTLP histogram shape: parallel arrays
// of bucket bounds and counts, plus an explicit sum + count.
type HistogramDataPoint struct {
	Attributes     []KeyValue `json:"attributes"`
	TimeUnixNano   string     `json:"timeUnixNano"`
	Count          string     `json:"count"`
	Sum            float64    `json:"sum"`
	BucketCounts   []string   `json:"bucketCounts"`
	ExplicitBounds []float64  `json:"explicitBounds"`
}

type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

type AnyValue struct {
	StringValue string  `json:"stringValue,omitempty"`
	IntValue    string  `json:"intValue,omitempty"`
	BoolValue   bool    `json:"boolValue,omitempty"`
	DoubleValue float64 `json:"doubleValue,omitempty"`
}

// AggregationTemporality maps the OTLP enum to the column value.
//   1 = delta, 2 = cumulative.
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
