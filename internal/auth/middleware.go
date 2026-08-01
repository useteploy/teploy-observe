package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/neutronauth"

	"github.com/useteploy/teploy-observe/internal/ingest"
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
			hasAdmins, err := authSvc.HasAdminUsers(r.Context())
			if err != nil {
				neutron.WriteError(w, r, neutron.ErrInternal("auth check unavailable"))
				return
			}
			// Grace period only when there is no way to authenticate yet — no
			// local admins AND no SSO. With OIDC configured, require auth.
			if !hasAdmins && !authSvc.OIDCEnabled() {
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
			// Grace period: if there is no way to authenticate yet (no admin
			// users AND no SSO), allow unauthenticated access for onboarding.
			// Fail closed on a DB error rather than treating it as grace.
			hasAdmins, err := authSvc.HasAdminUsers(r.Context())
			if err != nil {
				neutron.WriteError(w, r, neutron.ErrInternal("auth check unavailable"))
				return
			}
			if !hasAdmins && !authSvc.OIDCEnabled() {
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

			// OBS-011: reject a token whose embedded version doesn't match the
			// user's current token_version — this is what makes a password
			// change actually revoke previously issued tokens instead of
			// leaving them valid until their 24-hour expiry. OIDC-issued
			// subjects ("oidc:<subject>") have no admin_users row to version
			// (see oidc.go's GenerateToken call, always tv=0) and are skipped;
			// their session freshness comes from re-authenticating with the
			// IdP, not from this check.
			sub, _ := claims["sub"].(string)
			if sub != "" && !strings.HasPrefix(sub, "oidc:") {
				tokenTV, _ := claims["tv"].(float64)
				currentTV, err := authSvc.CurrentTokenVersion(r.Context(), sub)
				if err != nil {
					neutron.WriteError(w, r, neutron.ErrUnauthorized("session invalid"))
					return
				}
				if int64(tokenTV) != currentTV {
					neutron.WriteError(w, r, neutron.ErrUnauthorized("session revoked — please sign in again"))
					return
				}
			}

			// OBS-016: a query-string token is real bearer material and can
			// leak via browser history, proxy/access logs, or the Referer
			// header on any outbound link/subresource the response contains.
			// It's only accepted at all (queryTokenAllowed, above) because
			// EventSource/download contexts can't set a custom header — for
			// exactly those responses, stop this page/response from
			// propagating a Referer that would carry the token onward.
			if token == r.URL.Query().Get("token") {
				w.Header().Set("Referrer-Policy", "no-referrer")
			}

			// Stash role for downstream RequireRole middleware. Missing role
			// claim defaults to RoleViewer so we fail closed on reads.
			role, _ := claims["role"].(string)
			if role == "" {
				role = RoleViewer
			}
			ctx := WithRole(r.Context(), role)
			// Store full claims so handlers can read sub/username/role via
			// neutronauth.ClaimsFromContext (e.g. changePasswordHandler).
			ctx = neutronauth.WithClaims(ctx, claims)
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
				// No key provided — check grace period. Fail closed on a DB
				// error instead of falling into the no-keys grace path.
				hasKeys, err := authSvc.HasAPIKeys(r.Context())
				if err != nil {
					neutron.WriteError(w, r, neutron.ErrInternal("auth check unavailable"))
					return
				}
				if !hasKeys {
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
