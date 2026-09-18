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

// queryTokenAllowedPaths lists URL path prefixes where a query-string
// credential is accepted on GET requests. EventSource / download contexts
// can't set Authorization headers, so they carry a minted stream ticket in
// ?ticket= instead (AUD-008). All other routes must use the Authorization
// header. Keep in sync with StreamTicketRoutes.
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
//
// AUD-008 (round 2): the query string accepts ONLY short-lived stream
// tickets (?ticket=), never a normal access JWT - a 24h bearer credential
// in a URL leaks through history, proxies, and logs. Symmetrically, a
// stream ticket presented as an Authorization header is rejected: it is
// not a general bearer token. Both checks run BEFORE the signature check
// so the rejection is a property of the token's declared purpose, not of
// its validity.
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
			fromQuery := false
			if strings.HasPrefix(header, "Bearer ") {
				token = strings.TrimPrefix(header, "Bearer ")
			} else if q := r.URL.Query().Get("ticket"); q != "" && queryTokenAllowed(r) {
				token = q
				fromQuery = true
			} else if r.URL.Query().Get("token") != "" && queryTokenAllowed(r) {
				// AUD-008: the old ?token= fallback carried full-access JWTs
				// in URLs. Removed; the message tells legitimate consumers
				// (EventSource/downloads) to mint a stream ticket instead.
				neutron.WriteError(w, r, neutron.ErrUnauthorized(
					"query-string tokens are no longer accepted - mint a stream ticket via POST /api/v1/auth/stream-ticket"))
				return
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
			aud, _ := claims["aud"].(string)

			if fromQuery {
				// AUD-008: only single-purpose stream tickets may travel in
				// the query string, and only on the route prefix they were
				// minted for.
				if aud != StreamTicketAudience {
					neutron.WriteError(w, r, neutron.ErrUnauthorized("normal access tokens are not accepted in the query string"))
					return
				}
				boundRoute, _ := claims["route"].(string)
				if boundRoute == "" || !strings.HasPrefix(r.URL.Path, boundRoute) {
					neutron.WriteError(w, r, neutron.ErrUnauthorized("stream ticket is not valid for this route"))
					return
				}
			} else if aud == StreamTicketAudience {
				// A stream ticket is not a general bearer token.
				neutron.WriteError(w, r, neutron.ErrUnauthorized("stream tickets are only accepted as ?ticket= on their bound route"))
				return
			}

			// OBS-011 + F05: reject a token whose embedded version doesn't
			// match the principal's current token_version — this is what
			// makes a password change, role change, or session revocation
			// actually retire previously issued tokens instead of leaving
			// them valid until their 24-hour expiry. Since migration 040
			// every principal has a row — local and issuer-namespaced OIDC
			// alike — so the check is unconditional. A sub with no principal
			// row (a pre-040 "oidc:<sub>" token, or a deleted principal)
			// fails here and must re-authenticate. The same check retires
			// minted stream tickets when the principal's sessions are
			// revoked (AUD-008).
			sub, _ := claims["sub"].(string)
			if sub != "" {
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

			// OBS-016 / AUD-008: even a stream ticket in a URL is credential
			// material and can leak via browser history, proxy/access logs,
			// or the Referer header on any outbound link/subresource the
			// response contains - stop this response from propagating one.
			if fromQuery {
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
// in the request context.
//
// AUD-002 (round 2): the former no-keys grace period trusted
// caller-selected sites (X-Observe-Site / body site_id) on any instance
// with zero API-key rows — a global state-based exception, independent of
// whether a dashboard administrator already existed. Ingestion now
// requires a valid key by default: a fresh install accepts no telemetry
// until an admin provisions one, and a validated key's site is the only
// site its requests can write to.
func APIKeyAuthMiddleware(authSvc *AuthService) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimSpace(r.Header.Get("X-API-Key"))
			if key == "" {
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
