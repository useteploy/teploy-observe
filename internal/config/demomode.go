package config

import (
	"net/http"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"
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
			if demoWriteAllowed(r.URL.Path) {
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

func demoWriteAllowed(path string) bool {
	// Always allow auth so visitors can log in with demo creds.
	if strings.HasPrefix(path, "/api/v1/auth/") {
		return true
	}
	// Always allow ingest so the demo keeps receiving live traffic.
	if strings.HasPrefix(path, "/api/v1/events") ||
		strings.HasPrefix(path, "/api/v1/errors") ||
		strings.HasPrefix(path, "/api/v1/logs") ||
		strings.HasPrefix(path, "/api/v1/replays") ||
		strings.HasPrefix(path, "/api/v1/ingest/") ||
		strings.HasPrefix(path, "/api/v1/flags/evaluate") {
		return true
	}
	return false
}
