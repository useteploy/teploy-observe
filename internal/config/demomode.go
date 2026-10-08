package config

import (
	"net/http"
	"strings"

	"github.com/neutron-build/neutron/go/neutron"
)

// DemoModeMiddleware blocks write operations when demo mode is enabled.
// Reads (GET/HEAD/OPTIONS), auth login, and ingest paths are always allowed.
func DemoModeMiddleware(enabled bool) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled {
				next.ServeHTTP(w, r)
				return
			}
			if !isWrite(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			// Always allow: authentication endpoints, ingest (so the demo still shows live traffic).
			if demoWriteAllowed(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			neutron.WriteError(w, r, neutron.ErrForbidden("demo mode: writes are disabled"))
		})
	}
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// TelemetryWriteAllowed is the method-and-route allowlist shared with the
// ingest listener. Authentication and API-key checks still belong to handlers.
func TelemetryWriteAllowed(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/api/v1/events", "/api/v1/events/batch", "/api/v1/errors",
		"/api/v1/logs", "/api/v1/logs/batch", "/api/v1/replays",
		"/api/v1/feedback", "/api/v1/llm/ingest", "/api/v1/infra/report",
		"/api/v1/experiments/expose", "/api/v1/experiments/convert",
		"/api/v1/experiments/metric", "/api/v1/persons/properties",
		"/api/v1/flags/evaluate", "/api/v1/surveys/expose",
		"/api/v1/surveys/respond", "/api/v1/sourcemaps/upload",
		"/v1/traces", "/v1/metrics", "/v1/logs", "/api/v1/v1/traces":
		return true
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "" && parts[1] == "api" &&
		parts[2] != "" && parts[2] != "v1" &&
		(parts[3] == "envelope" || parts[3] == "store") {
		return true
	}
	return len(parts) == 6 && parts[0] == "" && parts[1] == "api" &&
		parts[2] == "v1" && parts[3] == "checkin" && parts[4] == "token" && parts[5] != ""
}

func demoWriteAllowed(method, path string) bool {
	if TelemetryWriteAllowed(method, path) {
		return true
	}
	if method == http.MethodPost {
		switch path {
		case "/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/stream-ticket":
			return true
		}
	}
	return false
}
