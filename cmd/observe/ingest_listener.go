package main

import (
	"net/http"
	"path"
	"strings"
	"time"
)

// ─── Ingest listener ────────────────────────────────────────────────────────
//
// Observe is one binary serving three surfaces off one mux: telemetry ingest,
// the read/admin API, and the dashboard SPA. Publishing that mux publishes all
// three — which is how a self-hosted instance ends up with its login page on
// the public internet.
//
// Hosted products in this space don't do that; they give ingest its own
// hostname, which has no UI behind it at all:
//
//	sentry.io            vs  o123.ingest.sentry.io
//	us.posthog.com       vs  us.i.posthog.com
//	app.datadoghq.com    vs  browser-intake-datadoghq.com
//
// OBSERVE_INGEST_ADDR brings that topology to a single self-hosted binary: a
// second listener serving only the telemetry-write endpoints. Publish that one
// (ingest.example.com) and keep OBSERVE_ADDR — dashboard, read API, admin — on
// localhost or a tailnet. The dashboard isn't merely unrouted on the public
// port, it isn't listening there.
//
// The allowlist is default-deny: anything not named below 404s on the ingest
// listener, so adding a dashboard route can never expose it by omission.

// ingestRoutes are the exact "METHOD /path" pairs the ingest listener serves.
var ingestRoutes = map[string]bool{
	// Browser + SDK telemetry writes.
	"POST /api/v1/events":       true,
	"POST /api/v1/events/batch": true,
	"POST /api/v1/errors":       true,
	"POST /api/v1/logs":         true,
	"POST /api/v1/logs/batch":   true,
	"POST /api/v1/replays":      true,
	"POST /api/v1/feedback":     true,
	"POST /api/v1/llm/ingest":   true,
	"POST /api/v1/infra/report": true,

	// Client-SDK support. These read, but only what an SDK needs to function
	// (which flag/survey is live) — never stored telemetry — and they are
	// rate-limited. Same trust level as PostHog's /decide.
	"POST /api/v1/experiments/expose":  true,
	"POST /api/v1/experiments/convert": true,
	"POST /api/v1/flags/evaluate":      true,
	"GET /api/v1/surveys/active":       true,
	"POST /api/v1/surveys/respond":     true,

	// CI uploads source maps so stack traces symbolicate; the handler accepts
	// the site-scoped ingest key (apiKeyOrEditorJWT in main.go).
	"POST /api/v1/sourcemaps/upload": true,

	// Liveness for the proxy/tunnel terminating in front of this listener.
	"GET /healthz": true,
}

// isIngestPath reports whether method+p may be served on the ingest listener.
// p must already be cleaned — see ingestOnly.
func isIngestPath(method, p string) bool {
	if ingestRoutes[method+" "+p] {
		return true
	}
	switch {
	// OTLP. /v1/traces today; /v1/metrics and /v1/logs when they land. The
	// whole /v1/ namespace is OTLP writes (the SPA catch-all already excludes
	// it), so a prefix keeps new OTLP signals working without editing this
	// list — and it stays writes-only because it is POST-scoped.
	case method == http.MethodPost && strings.HasPrefix(p, "/v1/"):
		return true
	// The ingest group is mounted at /api/v1, so its OTLP route lands at the
	// doubled path /api/v1/v1/traces.
	case method == http.MethodPost && strings.HasPrefix(p, "/api/v1/v1/"):
		return true
	// Tracker scripts and the no-JS pixel, fetched by the browser.
	case (method == http.MethodGet || method == http.MethodHead) && strings.HasPrefix(p, "/t/"):
		return true
	// Cron check-ins pinged by external jobs (token and site/slug forms).
	case (method == http.MethodPost || method == http.MethodGet) && strings.HasPrefix(p, "/api/v1/checkin/"):
		return true
	// CORS preflight for browser ingest. The handler only echoes CORS headers
	// and returns 204, so allowing it across /api/v1 discloses nothing.
	case method == http.MethodOptions && strings.HasPrefix(p, "/api/v1/"):
		return true
	}
	return false
}

// ingestOnly wraps the full app handler so only ingest routes are reachable.
func ingestOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match on the *cleaned* path. http.ServeMux cleans before routing, so
		// checking the raw path would let "/t/../api/v1/sites" satisfy the
		// "/t/" prefix here and then be served as /api/v1/sites by the mux.
		p := r.URL.Path
		if p == "" {
			p = "/"
		}
		p = path.Clean(p)
		if !isIngestPath(r.Method, p) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// newIngestServer builds the ingest-only server bound to addr.
func newIngestServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           ingestOnly(h),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
}
