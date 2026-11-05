package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/config"
)

func TestDiscoveryDocumentShape(t *testing.T) {
	cfg := config.Config{PublicURL: "https://observe.example.com", IngestAddr: ":3001", IngestURL: "https://ingest.example.com"}
	rec := httptest.NewRecorder()
	discoveryHandler(newDiscovery(cfg, "abc123"))(rec, httptest.NewRequest(http.MethodGet, "/.well-known/teploy.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 6 {
		t.Fatalf("fields = %v: the document is a contract, adding one must be deliberate", doc)
	}
	if doc["product"] != "observe" || doc["instance_id"] != "abc123" ||
		doc["dashboard_url"] != "https://observe.example.com" || doc["ingest_url"] != "https://ingest.example.com" {
		t.Fatalf("doc = %v", doc)
	}
	if _, ok := doc["capabilities"].([]any); !ok {
		t.Fatalf("capabilities = %v", doc["capabilities"])
	}
}

func TestDiscoveryIngestURLNeverGuesses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"one listener is the public url", config.Config{PublicURL: "https://o.example.com"}, "https://o.example.com"},
		{"operator named it", config.Config{PublicURL: "https://o.example.com", IngestAddr: ":3001", IngestURL: "https://i.example.com"}, "https://i.example.com"},
		{"separate listener unnamed is unknown", config.Config{PublicURL: "https://o.example.com", IngestAddr: ":3001"}, ""},
		{"nothing configured", config.Config{}, ""},
	} {
		if got := discoveryIngestURL(tc.cfg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDiscoveryCapabilitiesFollowGRPC(t *testing.T) {
	if got := strings.Join(discoveryCapabilities(false), ","); strings.Contains(got, "otlp_grpc") {
		t.Errorf("gRPC off: %s", got)
	}
	if got := strings.Join(discoveryCapabilities(true), ","); !strings.Contains(got, "otlp_grpc") {
		t.Errorf("gRPC on: %s", got)
	}
}

// The ingest listener is the internet-facing one. See the decision on
// discoveryDoc: neither discovery nor the provisioning API belongs there.
func TestDiscoveryAndServiceRoutesAreNotOnIngestListener(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/.well-known/teploy.json"},
		{http.MethodPost, "/api/v1/service/sites"},
		{http.MethodPost, "/api/v1/service/sites/s/keys"},
		{http.MethodPost, "/api/v1/service/sites/s/share"},
		{http.MethodPost, "/api/v1/service/sites/s/webhooks"},
		{http.MethodDelete, "/api/v1/service/sites/s/webhooks/w"},
		{http.MethodGet, "/api/v1/service/sites/s/alerts/rules"},
		{http.MethodGet, "/api/v1/service/sites/s/alerts/history"},
		{http.MethodGet, "/api/v1/service-tokens"},
		{http.MethodPost, "/api/v1/service-tokens"},
		{http.MethodDelete, "/api/v1/service-tokens/t"},
	} {
		if isIngestPath(tc.method, tc.path) {
			t.Errorf("%s %s must 404 on the ingest listener", tc.method, tc.path)
		}
	}
}
