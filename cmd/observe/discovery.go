package main

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/useteploy/teploy-observe/internal/config"
)

// discoveryDoc is GET /.well-known/teploy.json: what another Teploy product
// needs to recognise this instance before an admin connects it. It carries no
// secrets and grants nothing.
//
// The route exists on the dashboard listener only. The ingest listener keeps
// answering 404, deliberately: it is the one port published to the internet,
// and this document names the private dashboard URL and the running version,
// neither of which a stranger needs. Nothing is lost by that, because every
// action discovery leads to (service tokens, site and key provisioning) lives
// on the dashboard listener, so a client that can only see ingest has nothing
// to do with the answer. Staying default-deny there is pinned by
// TestDiscoveryAndServiceRoutesAreNotOnIngestListener.
type discoveryDoc struct {
	Product      string   `json:"product"`
	Version      string   `json:"version"`
	InstanceID   string   `json:"instance_id"`
	DashboardURL string   `json:"dashboard_url"`
	IngestURL    string   `json:"ingest_url"`
	Capabilities []string `json:"capabilities"`
}

func newDiscovery(cfg config.Config, instanceID string) discoveryDoc {
	return discoveryDoc{
		Product:      "observe",
		Version:      version,
		InstanceID:   instanceID,
		DashboardURL: discoveryDashboardURL(cfg),
		IngestURL:    discoveryIngestURL(cfg),
		Capabilities: discoveryCapabilities(os.Getenv("OBSERVE_OTLP_GRPC_ADDR") != ""),
	}
}

// discoveryDashboardURL never guesses. With one listener the public URL is the
// dashboard. With a separate ingest listener PublicURL may be the ingest host (the
// live overlay does exactly that), so only OBSERVE_DASHBOARD_URL can answer.
func discoveryDashboardURL(cfg config.Config) string {
	if cfg.DashboardURL != "" {
		return cfg.DashboardURL
	}
	if cfg.IngestAddr == "" {
		return cfg.PublicURL
	}
	return ""
}

// discoveryIngestURL never guesses. With one listener, ingest is the same port as
// the dashboard. With a separate ingest listener only the operator knows its public
// name, so an unset OBSERVE_INGEST_URL publishes "" and the client has to ask rather
// than write a wrong endpoint into an app.
func discoveryIngestURL(cfg config.Config) string {
	if cfg.IngestURL != "" {
		return cfg.IngestURL
	}
	if cfg.IngestAddr == "" {
		return discoveryDashboardURL(cfg)
	}
	return ""
}

func discoveryCapabilities(otlpGRPCEnabled bool) []string {
	caps := []string{"service_tokens", "sites_external_ref", "otlp_http"}
	if otlpGRPCEnabled {
		caps = append(caps, "otlp_grpc")
	}
	return caps
}

func discoveryHandler(doc discoveryDoc) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(doc)
	}
}
