package observe

import (
	"net/url"
	"os"
	"strings"
)

// applyEnv fills options the caller left empty from the environment, so an app
// a platform has configured needs no endpoint or key in code. An explicit option
// always wins, then OBSERVE_*, then the OpenTelemetry standard names.
//
// A credential only goes where it was issued for:
//
//   - An explicit Endpoint takes OBSERVE_API_KEY only if OBSERVE_ENDPOINT is unset
//     or names the same place. Otherwise that key belongs to another instance.
//   - OTEL_EXPORTER_OTLP_ENDPOINT is used only when OTEL_EXPORTER_OTLP_HEADERS
//     carries x-api-key and no key was passed in code. Without that header the
//     endpoint is some other vendor's collector, so New keeps returning "Endpoint
//     is required" rather than sending telemetry there. With a key in code,
//     using it would send that key there.
//
// OTEL_EXPORTER_OTLP_PROTOCOL=grpc disables the OTLP endpoint and headers: this
// client speaks HTTP, and that endpoint would be a gRPC port. For http/protobuf
// and http/json the endpoint is usable as is, because Observe accepts both
// encodings on /v1/* and this client sends JSON.
func applyEnv(opts *Options) {
	observeEndpoint := os.Getenv("OBSERVE_ENDPOINT")
	switch {
	case opts.Endpoint != "":
		if opts.APIKey == "" && (observeEndpoint == "" || sameEndpoint(observeEndpoint, opts.Endpoint)) {
			opts.APIKey = os.Getenv("OBSERVE_API_KEY")
		}
	case observeEndpoint != "":
		opts.Endpoint = observeEndpoint
		if opts.APIKey == "" {
			opts.APIKey = os.Getenv("OBSERVE_API_KEY")
		}
	case opts.APIKey == "" && otlpHTTPConfigured():
		key := parseOTELList(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), true)["x-api-key"]
		if key != "" {
			opts.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
			opts.APIKey = key
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

func sameEndpoint(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
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
