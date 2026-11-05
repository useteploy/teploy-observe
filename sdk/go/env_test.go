package observe

import "testing"

func clearObserveEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OBSERVE_ENDPOINT", "OBSERVE_API_KEY", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(k, "")
	}
}

func TestApplyEnvObserveNames(t *testing.T) {
	clearObserveEnv(t)
	t.Setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
	t.Setenv("OBSERVE_API_KEY", "obs_key")
	var o Options
	applyEnv(&o)
	if o.Endpoint != "https://ingest.example.com" || o.APIKey != "obs_key" {
		t.Fatalf("%+v", o)
	}
}

func TestApplyEnvExplicitOptionsWin(t *testing.T) {
	clearObserveEnv(t)
	t.Setenv("OBSERVE_ENDPOINT", "https://env.example.com")
	t.Setenv("OBSERVE_API_KEY", "obs_env")
	t.Setenv("OTEL_SERVICE_NAME", "env-name")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=9,deployment.environment=env")
	o := Options{Endpoint: "https://code.example.com", APIKey: "obs_code", ServiceName: "code", Release: "1", Environment: "code"}
	applyEnv(&o)
	if o.Endpoint != "https://code.example.com" || o.APIKey != "obs_code" ||
		o.ServiceName != "code" || o.Release != "1" || o.Environment != "code" {
		t.Fatalf("got %+v", o)
	}
}

func TestApplyEnvOTELContract(t *testing.T) {
	clearObserveEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://ingest.example.com")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "other=1, X-Api-Key=obs%5Fkey")
	t.Setenv("OTEL_SERVICE_NAME", "shop")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=ignored,service.version=1.2.3,deployment.environment.name=prod")
	var o Options
	applyEnv(&o)
	if o.Endpoint != "https://ingest.example.com" || o.APIKey != "obs_key" ||
		o.ServiceName != "shop" || o.Release != "1.2.3" || o.Environment != "prod" {
		t.Fatalf("%+v", o)
	}
}

func TestApplyEnvOTELGRPCIsNotUsable(t *testing.T) {
	clearObserveEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.com:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=obs_key")
	var o Options
	applyEnv(&o)
	if o.Endpoint != "" || o.APIKey != "" {
		t.Fatalf("a gRPC endpoint must not be used: %+v", o)
	}
}

// A credential must not travel to an endpoint it was not issued alongside.
func TestApplyEnvCredentialFollowsItsEndpoint(t *testing.T) {
	clearObserveEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
	t.Setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
	var o Options
	applyEnv(&o)
	if o.Endpoint != "https://ingest.example.com" || o.APIKey != "" {
		t.Fatalf("vendor key reached Observe: %+v", o)
	}

	clearObserveEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
	t.Setenv("OBSERVE_API_KEY", "obs_key")
	o = Options{}
	applyEnv(&o)
	if o.Endpoint != "https://other-vendor.example.com" || o.APIKey != "" {
		t.Fatalf("Observe key reached another vendor: %+v", o)
	}

	clearObserveEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
	o = Options{Endpoint: "https://code.example.com"}
	applyEnv(&o)
	if o.APIKey != "" {
		t.Fatalf("vendor key reached an explicit endpoint: %+v", o)
	}
}

func TestNewReadsEnvAndStillRequiresAnEndpoint(t *testing.T) {
	clearObserveEnv(t)
	if _, err := New(Options{}); err == nil {
		t.Fatal("no endpoint anywhere must still be an error")
	}
	t.Setenv("OBSERVE_ENDPOINT", "http://127.0.0.1:1")
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.opts.Endpoint != "http://127.0.0.1:1" {
		t.Fatalf("endpoint = %q", c.opts.Endpoint)
	}
}

func TestParseOTELListSkipsMalformed(t *testing.T) {
	got := parseOTELList("a=1,broken,=2,b=%zz,c=x%3Dy", false)
	if len(got) != 2 || got["a"] != "1" || got["c"] != "x=y" {
		t.Fatalf("%v", got)
	}
}
