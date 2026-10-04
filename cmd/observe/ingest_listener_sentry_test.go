package main

import (
	"net/http"
	"testing"
)

// The Sentry wire-protocol routes must be reachable on the public ingest
// listener by exact segments, and must never widen into the dashboard API.
func TestIngestListenerSentryPaths(t *testing.T) {
	allow := []string{
		"/api/1/envelope", "/api/1/store", "/api/default/envelope", "/api/site-a/store",
	}
	for _, p := range allow {
		if !isIngestPath(http.MethodPost, p) {
			t.Errorf("POST %s should be served on the ingest listener", p)
		}
	}
	deny := []struct{ method, path string }{
		{http.MethodGet, "/api/1/envelope"},        // wrong method
		{http.MethodPost, "/api/v1/envelope"},      // project segment v1 is the API tree
		{http.MethodPost, "/api/1/envelope/extra"}, // extra segment
		{http.MethodPost, "/api/1/sites"},          // not a sentry route
		{http.MethodPost, "/api//store"},           // empty project
		{http.MethodPost, "/api/1/2/envelope"},     // too deep
		{http.MethodPost, "/envelope"},             // missing prefix
		{http.MethodPost, "/api/v1/sites"},         // dashboard route
		{http.MethodDelete, "/api/1/store"},        // wrong method
	}
	for _, c := range deny {
		if isIngestPath(c.method, c.path) {
			t.Errorf("%s %s must NOT be served on the ingest listener", c.method, c.path)
		}
	}
	for _, r := range []string{"POST /api/v1/experiments/metric", "POST /api/v1/persons/properties"} {
		if !ingestRoutes[r] {
			t.Errorf("%s should be on the ingest allowlist", r)
		}
	}
}
