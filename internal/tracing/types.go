package tracing

import "fmt"

// OTLP JSON types for ExportTraceServiceRequest.
// These match the OpenTelemetry protobuf-to-JSON mapping.

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
	TraceID            string     `json:"traceId"`
	SpanID             string     `json:"spanId"`
	ParentSpanID       string     `json:"parentSpanId"`
	Name               string     `json:"name"`
	Kind               int        `json:"kind"`
	StartTimeUnixNano  string     `json:"startTimeUnixNano"`
	EndTimeUnixNano    string     `json:"endTimeUnixNano"`
	Attributes         []KeyValue `json:"attributes"`
	Status             SpanStatus `json:"status"`
	Events             []SpanEvent `json:"events"`
}

type SpanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type SpanEvent struct {
	Name               string     `json:"name"`
	TimeUnixNano       string     `json:"timeUnixNano"`
	Attributes         []KeyValue `json:"attributes"`
}

type KeyValue struct {
	Key   string    `json:"key"`
	Value AnyValue  `json:"value"`
}

type AnyValue struct {
	StringValue string `json:"stringValue,omitempty"`
	IntValue    string `json:"intValue,omitempty"`
	BoolValue   bool   `json:"boolValue,omitempty"`
	DoubleValue float64 `json:"doubleValue,omitempty"`
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
func AttrsToMap(attrs []KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		switch {
		case kv.Value.StringValue != "":
			m[kv.Key] = kv.Value.StringValue
		case kv.Value.IntValue != "":
			m[kv.Key] = kv.Value.IntValue
		case kv.Value.BoolValue:
			m[kv.Key] = "true"
		case kv.Value.DoubleValue != 0:
			m[kv.Key] = fmt.Sprintf("%g", kv.Value.DoubleValue)
		}
	}
	return m
}
