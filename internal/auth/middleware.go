package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/observe/internal/ingest"
)

type roleCtxKey struct{}

// WithRole stores the authenticated user's role in the context. Used by
// JWTAuthMiddleware and tested by RequireRole.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleCtxKey{}, role)
}

// RoleFromContext returns the role placed by JWTAuthMiddleware, or "" if not
// set (e.g., first-run grace period).
func RoleFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(roleCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// RequireRole wraps a handler so only callers whose JWT carries one of the
// allowed roles may pass. During the first-run grace period (no admin users
// yet), all requests are allowed through — otherwise an unauthenticated user
// would be locked out of the onboarding flow.
func RequireRole(authSvc *AuthService, allowed ...string) neutron.Middleware {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, r := range allowed {
		allowSet[r] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authSvc.HasAdminUsers(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			role := RoleFromContext(r.Context())
			if role == "" {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("missing role claim"))
				return
			}
			if _, ok := allowSet[role]; !ok {
				neutron.WriteError(w, r, neutron.ErrForbidden("insufficient role: "+role))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// queryTokenAllowedPaths lists URL path prefixes where ?token=<jwt> is accepted
// on GET requests. EventSource / download contexts can't set Authorization
// headers, so they carry auth in the query string. All other routes must use
// the Authorization header.
var queryTokenAllowedPaths = []string{
	"/api/v1/export",
	"/api/v1/logs/stream",
	"/api/v1/live",
	"/api/v1/stats/live",
}

func queryTokenAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.Path
	for _, prefix := range queryTokenAllowedPaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

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
			var token string
			if strings.HasPrefix(header, "Bearer ") {
				token = strings.TrimPrefix(header, "Bearer ")
			} else if q := r.URL.Query().Get("token"); q != "" && queryTokenAllowed(r) {
				token = q
			} else if header == "" {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("missing authorization header"))
				return
			} else {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("invalid authorization scheme"))
				return
			}

			claims, err := authSvc.ValidateToken(token)
			if err != nil {
				neutron.WriteError(w, r, neutron.ErrUnauthorized(err.Error()))
				return
			}
			// Stash role for downstream RequireRole middleware. Missing role
			// claim defaults to RoleViewer so we fail closed on reads.
			role, _ := claims["role"].(string)
			if role == "" {
				role = RoleViewer
			}
			ctx := WithRole(r.Context(), role)
			next.ServeHTTP(w, r.WithContext(ctx))
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
