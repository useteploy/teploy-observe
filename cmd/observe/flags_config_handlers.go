package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-observe/internal/flags"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// GET /api/v1/flags/config -- the remote-config bootstrap for SDKs that
// evaluate flags locally. It returns every flag's TARGETING RULES, which
// POST /api/v1/flags/evaluate deliberately never discloses (that endpoint is
// public and answers one decision at a time). So this route is NOT public:
// it requires a site-scoped API key with the telemetry capability
// (X-API-Key), the same credential the browser SDK already holds. The site
// comes from the key, never from the request; a site_id query parameter, if
// sent, must agree with the key or the request is refused (no cross-site
// reads). The key ships to browsers by design, so treat the config as
// readable by anyone who can load your site: keep secrets out of targeting
// values and payloads.

// flagConfigSource is the slice of FlagService the handler needs.
type flagConfigSource interface {
	Config(ctx context.Context, siteID string) (*flags.ConfigBundle, error)
}

// flagConfigRoute wraps the handler in the ingest chain: API key (resolves
// the site into the context) then the per-site rate limiter.
func flagConfigRoute(apiKeyMW func(http.Handler) http.Handler, rl func(http.Handler) http.Handler, src flagConfigSource) http.Handler {
	inner := apiKeyMW(rl(flagConfigHandler(src)))
	// Browsers read this with X-API-Key and If-None-Match, and need ETag
	// exposed. The wildcard OPTIONS handler allows the preflight headers.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "ETag")
		inner.ServeHTTP(w, r)
	})
}

func flagConfigHandler(src flagConfigSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := ingest.SiteIDFromContext(r.Context())
		if siteID == "" {
			// Unreachable behind the API key middleware; fail closed anyway.
			writeJSONError(w, http.StatusUnauthorized, "api key required")
			return
		}
		if q := r.URL.Query().Get("site_id"); q != "" && q != siteID {
			writeJSONError(w, http.StatusForbidden, "site_id does not match the api key")
			return
		}
		bundle, err := src.Config(r.Context(), siteID)
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "flags config unavailable")
			return
		}
		etag := bundle.ETag()
		w.Header().Set("ETag", etag)
		// Config carries targeting rules: keep it out of shared caches.
		w.Header().Set("Cache-Control", "private, no-cache")
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			for _, c := range strings.Split(inm, ",") {
				if strings.TrimSpace(c) == etag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(bundle)
	}
}
