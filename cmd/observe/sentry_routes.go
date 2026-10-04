package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/useteploy/teploy-observe/internal/auth"
	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/sentrycompat"
)

// registerSentryRoutes mounts the Sentry wire-protocol ingest so stock Sentry
// SDKs can use an Observe DSN: https://<observe_api_key>@<host>/<site_id>.
// Authentication, the BoundSite invariant, rate limiting and body caps are
// all enforced inside the handler (the DSN key can arrive in the query, the
// X-Sentry-Auth header or the envelope header, so the X-API-Key middleware
// does not apply). Patterns are exact paths (no subtree claim). No OPTIONS
// route: "OPTIONS /api/{project_id}/..." conflicts with the existing
// "OPTIONS /api/v1/{path...}" preflight handler at boot, and Sentry SDKs send
// simple requests (text/plain, DSN key in the query string), which never
// preflight.
func registerSentryRoutes(r interface {
	Handle(pattern string, handler http.Handler)
}, authSvc *auth.AuthService, buf *obserrors.ErrorBuffer, limiter *ingest.RateLimiter, logger *slog.Logger) {
	h := &sentrycompat.Handler{
		Keys:       authSvc,
		Sink:       buf,
		Limiter:    limiter,
		Logger:     logger,
		RetryAfter: 2 * time.Second,
	}
	for _, p := range []struct {
		path string
		fn   http.HandlerFunc
	}{
		{"/api/{project_id}/envelope/{$}", h.Envelope},
		{"/api/{project_id}/envelope", h.Envelope},
		{"/api/{project_id}/store/{$}", h.Store},
		{"/api/{project_id}/store", h.Store},
	} {
		r.Handle("POST "+p.path, p.fn)
	}
}
