package auth

import (
	"net/http"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/teploy/observe/internal/ingest"
)

// JWTAuthMiddleware returns middleware that validates JWT tokens from the
// Authorization: Bearer <token> header. If no admin users exist yet
// (first-run grace period), requests are allowed through unauthenticated.
func JWTAuthMiddleware(authSvc *AuthService) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Grace period: if no admin users exist, allow unauthenticated access
			if !authSvc.HasAdminUsers(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}

			header := r.Header.Get("Authorization")
			if header == "" {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("missing authorization header"))
				return
			}

			if !strings.HasPrefix(header, "Bearer ") {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("invalid authorization scheme"))
				return
			}

			token := strings.TrimPrefix(header, "Bearer ")
			_, err := authSvc.ValidateToken(token)
			if err != nil {
				neutron.WriteError(w, r, neutron.ErrUnauthorized(err.Error()))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// APIKeyAuthMiddleware returns middleware that validates API keys from the
// X-API-Key header. If the key is valid, the associated site_id is stored
// in the request context. If no API keys exist in the system (first-run
// grace period), the request is allowed with the default site ID.
func APIKeyAuthMiddleware(authSvc *AuthService, defaultSiteID string) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")

			if key == "" {
				// No key provided — check grace period
				if !authSvc.HasAPIKeys(r.Context()) {
					// Grace period: no keys in system, use default site
					siteID := r.Header.Get("X-Observe-Site")
					if siteID == "" {
						siteID = defaultSiteID
					}
					ctx := ingest.WithSiteID(r.Context(), siteID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				neutron.WriteError(w, r, neutron.ErrUnauthorized("missing API key"))
				return
			}

			siteID, err := authSvc.ValidateAPIKey(r.Context(), key)
			if err != nil {
				neutron.WriteError(w, r, neutron.ErrUnauthorized(err.Error()))
				return
			}

			ctx := ingest.WithSiteID(r.Context(), siteID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
