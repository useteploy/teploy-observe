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

// An explicit endpoint takes the env key only when the env does not say that key
// belongs somewhere else.
func TestApplyEnvExplicitEndpointAndEnvKey(t *testing.T) {
	for _, tc := range []struct {
		name, observeEndpoint, wantKey string
	}{
		{"OBSERVE_ENDPOINT unset", "", "obs_key"},
		{"OBSERVE_ENDPOINT equal", "https://code.example.com/", "obs_key"},
		{"OBSERVE_ENDPOINT differs", "https://other.example.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearObserveEnv(t)
			t.Setenv("OBSERVE_API_KEY", "obs_key")
			t.Setenv("OBSERVE_ENDPOINT", tc.observeEndpoint)
			o := Options{Endpoint: "https://code.example.com"}
			applyEnv(&o)
			if o.Endpoint != "https://code.example.com" || o.APIKey != tc.wantKey {
				t.Fatalf("%+v", o)
			}
		})
	}
}

// The OTEL endpoint needs its own x-api-key. Anything else is another vendor's
// collector, and New must keep failing rather than send telemetry there.
func TestOTELEndpointRequiresItsOwnKey(t *testing.T) {
	const vendor = "https://other-vendor.example.com"
	t.Run("empty options, no header", func(t *testing.T) {
		clearObserveEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", vendor)
		if _, err := New(Options{}); err == nil {
			t.Fatal("a vendor endpoint with no x-api-key must be an error")
		}
	})
	t.Run("OBSERVE_API_KEY does not qualify", func(t *testing.T) {
		clearObserveEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", vendor)
		t.Setenv("OBSERVE_API_KEY", "obs_key")
		if _, err := New(Options{}); err == nil {
			t.Fatal("the Observe key must not reach an OTEL endpoint")
		}
	})
	t.Run("explicit key does not qualify", func(t *testing.T) {
		clearObserveEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", vendor)
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
		if _, err := New(Options{APIKey: "obs_code"}); err == nil {
			t.Fatal("a key in code must not reach an OTEL endpoint")
		}
	})
	t.Run("OBSERVE_ENDPOINT wins and takes no vendor key", func(t *testing.T) {
		clearObserveEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", vendor)
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
		t.Setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
		var o Options
		applyEnv(&o)
		if o.Endpoint != "https://ingest.example.com" || o.APIKey != "" {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("explicit endpoint takes no vendor key", func(t *testing.T) {
		clearObserveEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
		o := Options{Endpoint: "https://code.example.com"}
		applyEnv(&o)
		if o.APIKey != "" {
			t.Fatalf("%+v", o)
		}
	})
}

func TestSiteIDDefaultsOnlyWithoutAKey(t *testing.T) {
	clearObserveEnv(t)
	keyless, err := New(Options{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer keyless.Close()
	if keyless.opts.SiteID != "default" {
		t.Errorf("keyless SiteID = %q", keyless.opts.SiteID)
	}
	keyed, err := New(Options{Endpoint: "http://127.0.0.1:1", APIKey: "obs_key"})
	if err != nil {
		t.Fatal(err)
	}
	defer keyed.Close()
	if keyed.opts.SiteID != "" {
		t.Errorf("keyed SiteID = %q, want empty so the key's site applies", keyed.opts.SiteID)
	}
	explicit, err := New(Options{Endpoint: "http://127.0.0.1:1", APIKey: "obs_key", SiteID: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.Close()
	if explicit.opts.SiteID != "mine" {
		t.Errorf("explicit SiteID = %q", explicit.opts.SiteID)
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
