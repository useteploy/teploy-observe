package observe

import (
	"net/url"
	"os"
	"strings"
)

// applyEnv fills options the caller left empty from the environment, so an app
// a platform has configured needs no endpoint or key in code. An explicit
// option always wins, then OBSERVE_*, then the OpenTelemetry standard names.
//
// The credential follows the endpoint it was issued alongside: OBSERVE_API_KEY
// is only sent to an explicit or OBSERVE_ENDPOINT endpoint, and the x-api-key
// from OTEL_EXPORTER_OTLP_HEADERS only to OTEL_EXPORTER_OTLP_ENDPOINT. An app
// whose OTEL_* point at some other vendor's collector must not have its Observe
// key sent there, nor that vendor's key sent to Observe.
//
// OTEL_EXPORTER_OTLP_PROTOCOL=grpc disables the OTLP endpoint and headers: this
// client speaks HTTP, and that endpoint would be a gRPC port. For http/protobuf
// and http/json the endpoint is usable as is, because Observe accepts both
// encodings on /v1/* and this client sends JSON.
func applyEnv(opts *Options) {
	explicitEndpoint := opts.Endpoint != ""
	if opts.Endpoint == "" {
		opts.Endpoint = os.Getenv("OBSERVE_ENDPOINT")
	}
	if opts.Endpoint != "" {
		if opts.APIKey == "" {
			opts.APIKey = os.Getenv("OBSERVE_API_KEY")
		}
	} else if !explicitEndpoint && otlpHTTPConfigured() {
		opts.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		if opts.APIKey == "" {
			opts.APIKey = parseOTELList(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), true)["x-api-key"]
		}
	}

	attrs := parseOTELList(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), false)
	if opts.ServiceName == "" {
		opts.ServiceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if opts.ServiceName == "" {
		opts.ServiceName = attrs["service.name"]
	}
	if opts.Release == "" {
		opts.Release = attrs["service.version"]
	}
	if opts.Environment == "" {
		opts.Environment = attrs["deployment.environment.name"]
	}
	if opts.Environment == "" {
		opts.Environment = attrs["deployment.environment"]
	}
}

func otlpHTTPConfigured() bool {
	switch strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")) {
	case "", "http/protobuf", "http/json":
		return true
	}
	return false
}

// parseOTELList reads the W3C-baggage-style list both OTEL_EXPORTER_OTLP_HEADERS
// and OTEL_RESOURCE_ATTRIBUTES use: comma-separated key=value with percent
// encoding. A malformed entry is skipped, as the OTel spec asks. Header names
// are case-insensitive, so lowerKeys folds them.
func parseOTELList(raw string, lowerKeys bool) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if lowerKeys {
			key = strings.ToLower(key)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		out[key] = decoded
	}
	return out
}
