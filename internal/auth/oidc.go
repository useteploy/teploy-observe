package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC single sign-on (Phase 2 of the Teploy RBAC contract). Observe acts as an
// OpenID Connect relying party: login is delegated to an external identity
// provider (the customer's own Okta/Azure AD/Google/Keycloak — "generic OIDC" —
// or Teploy Platform acting as the IdP for Cloud). The IdP authenticates the
// user; Observe verifies the signed ID token, maps a claim to admin/editor/
// viewer, and mints its own JWT (the same token password login issues), so the
// SPA and every downstream middleware treat an SSO session identically.
//
// The role is re-read from the token on every login, keeping the IdP
// authoritative. Local username/password login remains the break-glass path.
// SSO is enabled only when OBSERVE_OIDC_ISSUER + OBSERVE_OIDC_CLIENT_ID are set.

const (
	oidcStateCookie = "observe_oidc_state"
	oidcFlowTTL     = 10 * time.Minute
	oidcTokenKey    = "obs_token" // localStorage key the SPA reads
)

// OIDCAuth holds SSO configuration and the lazily discovered provider. Discovery
// happens on first use rather than at startup so an IdP that is down at boot
// doesn't permanently disable SSO.
type OIDCAuth struct {
	authSvc *AuthService
	logger  *slog.Logger

	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string // optional; derived per-request from Host when empty
	scopes       []string
	label        string

	usernameClaim string
	roleClaim     string
	groupsClaim   string
	adminGroup    string
	editorGroup   string
	viewerGroup   string
	defaultRole   string

	// Optional identity allowlist. Empty (the default) means every identity
	// the IdP authenticates is allowed — fine for a single-tenant issuer the
	// customer controls. Set either to restrict SSO to specific users, which
	// matters for a multi-tenant issuer (e.g. plain Google) where
	// "authenticated by the IdP" does not imply "should have access here".
	allowedEmails  map[string]bool
	allowedDomains []string

	// audit records SSO sign-in attempts to the compliance trail, mirroring
	// what password login does. A func rather than the audit service itself
	// so this package stays independent of it, and because the audit service
	// is constructed after OIDC in main.
	audit OIDCAuditFunc

	initMu   sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier

	flowMu sync.Mutex
	flows  map[string]*oidcFlow
	// flowOrder is insertion order. Every flow shares the same TTL, so
	// insertion order is also expiry order — storeFlow prunes from the front
	// instead of sweeping the whole map, and caps total size so a
	// distributed attacker (many source IPs, each under the per-IP login
	// rate limit) can't grow this unboundedly.
	flowOrder []string
}

// maxOIDCFlows bounds the in-flight SSO login count. Comfortably above any
// real concurrent-login volume, well below what would trouble memory.
const maxOIDCFlows = 10000

// oidcFlow is one in-progress login, keyed by the OAuth state parameter and
// bound to the initiating browser by the state cookie. It carries the nonce and
// PKCE verifier the callback must present.
type oidcFlow struct {
	nonce    string
	verifier string
	exp      time.Time
}

// NewOIDCAuth reads SSO configuration from the environment. It returns nil
// (SSO disabled) when no OIDC variable is set at all.
//
// AUD-007 (round 2): partial configuration used to return nil — "SSO
// disabled" — instead of an error, so an operator who supplied only the
// issuer (or dropped a variable on redeploy) silently lost the security
// boundary, and on an unclaimed instance the anonymous grace path came
// back with it. OIDC is now all-or-none: any set variable requires both
// issuer and client ID, and the issuer must be an absolute URL without
// credentials/query/fragment. HTTPS is required unless
// OBSERVE_OIDC_ALLOW_HTTP_ISSUER=true (narrow local-development override).
func NewOIDCAuth(authSvc *AuthService, logger *slog.Logger) (*OIDCAuth, error) {
	issuer := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ISSUER"))
	clientID := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_CLIENT_SECRET"))
	redirectURL := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_REDIRECT_URL"))
	configured := issuer != "" || clientID != "" || clientSecret != "" || redirectURL != ""
	if !configured {
		return nil, nil
	}
	if issuer == "" || clientID == "" {
		return nil, fmt.Errorf("OIDC requires both OBSERVE_OIDC_ISSUER and OBSERVE_OIDC_CLIENT_ID when any OIDC variable is set — refusing to start with SSO silently disabled")
	}
	if err := validateOIDCIssuer(issuer); err != nil {
		return nil, err
	}
	o := &OIDCAuth{
		authSvc:        authSvc,
		logger:         logger,
		issuer:         issuer,
		clientID:       clientID,
		clientSecret:   clientSecret,
		redirectURL:    redirectURL,
		scopes:         parseOIDCScopes(os.Getenv("OBSERVE_OIDC_SCOPES")),
		label:          orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_LABEL")), "Single sign-on"),
		usernameClaim:  orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_USERNAME_CLAIM")), "preferred_username"),
		roleClaim:      orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ROLE_CLAIM")), "teploy_role"),
		groupsClaim:    orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_GROUPS_CLAIM")), "groups"),
		adminGroup:     strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ADMIN_GROUP")),
		editorGroup:    strings.TrimSpace(os.Getenv("OBSERVE_OIDC_EDITOR_GROUP")),
		viewerGroup:    strings.TrimSpace(os.Getenv("OBSERVE_OIDC_VIEWER_GROUP")),
		defaultRole:    normalizeRole(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_DEFAULT_ROLE"))),
		allowedEmails:  parseOIDCAllowlist(os.Getenv("OBSERVE_OIDC_ALLOWED_EMAILS")),
		allowedDomains: parseOIDCAllowlistSlice(os.Getenv("OBSERVE_OIDC_ALLOWED_DOMAINS")),
		flows:          make(map[string]*oidcFlow),
	}
	logger.Info("OIDC SSO enabled", "issuer", issuer)
	return o, nil
}

// validateOIDCIssuer enforces an absolute issuer URL without userinfo,
// query, or fragment, over https (or http with the explicit dev override).
func validateOIDCIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("OBSERVE_OIDC_ISSUER is not a valid URL: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && strings.EqualFold(os.Getenv("OBSERVE_OIDC_ALLOW_HTTP_ISSUER"), "true")) {
		return fmt.Errorf("OBSERVE_OIDC_ISSUER must be an absolute HTTPS URL (set OBSERVE_OIDC_ALLOW_HTTP_ISSUER=true only for local development)")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("OBSERVE_OIDC_ISSUER must be an absolute URL without credentials, query, or fragment")
	}
	return nil
}

// Audit result vocabulary, mirroring the audit package's constants. Duplicated
// as plain strings so this package doesn't have to depend on that one.
const (
	auditResultSuccess = "success"
	auditResultFailure = "failure"
	auditResultDenied  = "denied"
)

// OIDCAuditFunc records one SSO sign-in attempt. username is "" when the
// identity could not be resolved; result is one of the auditResult* values.
type OIDCAuditFunc func(r *http.Request, username, result, detail string)

// SetAuditFunc installs the audit sink. Safe to call with nil (no auditing);
// must be called before the server starts serving.
func (o *OIDCAuth) SetAuditFunc(f OIDCAuditFunc) {
	if o != nil {
		o.audit = f
	}
}

// recordAudit is a no-op when SSO is disabled or no sink is installed.
func (o *OIDCAuth) recordAudit(r *http.Request, username, result, detail string) {
	if o == nil || o.audit == nil {
		return
	}
	o.audit(r, username, result, detail)
}

// Enabled reports whether SSO is configured.
func (o *OIDCAuth) Enabled() bool { return o != nil }

// Label is the text shown on the SSO button.
func (o *OIDCAuth) Label() string {
	if o == nil {
		return ""
	}
	return o.label
}

func parseOIDCScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if !seen[oidc.ScopeOpenID] {
		out = append([]string{oidc.ScopeOpenID}, out...)
	}
	return out
}

// parseOIDCAllowlist parses a comma/space-separated list of emails into a
// lowercased set. Empty input yields a nil (empty) map.
func parseOIDCAllowlist(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out[f] = true
		}
	}
	return out
}

// parseOIDCAllowlistSlice parses a comma/space-separated list of domains into
// a lowercased slice.
func parseOIDCAllowlistSlice(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// allowed reports whether the authenticated identity may sign in. With no
// allowlist configured it always returns true.
//
// When an email/domain allowlist IS configured, the email claim must be
// verified by the IdP (audit F04): a signed ID token proves the issuer
// authenticated the subject, not that the subject controls the asserted
// email — and with an IdP that lets users set an unverified email (or a
// multi-tenant one where anyone can register), an unverified match would
// satisfy the allowlist without proving control of the address. Missing,
// false, or non-boolean email_verified therefore fails closed.
func (o *OIDCAuth) allowed(claims map[string]any) bool {
	if len(o.allowedEmails) == 0 && len(o.allowedDomains) == 0 {
		return true
	}
	verified, ok := claims["email_verified"].(bool)
	if !ok || !verified {
		return false
	}
	email := strings.ToLower(strings.TrimSpace(claimString(claims["email"])))
	if email == "" {
		return false
	}
	if o.allowedEmails[email] {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	for _, d := range o.allowedDomains {
		if domain == d {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ensure lazily discovers the provider and builds the ID-token verifier. Only a
// successful result is cached; a failure is retried on the next login.
func (o *OIDCAuth) ensure(ctx context.Context) error {
	o.initMu.Lock()
	defer o.initMu.Unlock()
	if o.provider != nil {
		return nil
	}
	p, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return err
	}
	o.provider = p
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.clientID})
	return nil
}

func (o *OIDCAuth) oauthConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     o.clientID,
		ClientSecret: o.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     o.provider.Endpoint(),
		Scopes:       o.scopes,
	}
}

func (o *OIDCAuth) storeFlow(state string, f *oidcFlow) {
	o.flowMu.Lock()
	defer o.flowMu.Unlock()
	now := time.Now()
	for len(o.flowOrder) > 0 {
		oldest := o.flowOrder[0]
		if of, ok := o.flows[oldest]; !ok || now.After(of.exp) {
			o.flowOrder = o.flowOrder[1:]
			delete(o.flows, oldest)
			continue
		}
		break
	}
	if len(o.flowOrder) >= maxOIDCFlows {
		delete(o.flows, o.flowOrder[0])
		o.flowOrder = o.flowOrder[1:]
	}
	o.flows[state] = f
	o.flowOrder = append(o.flowOrder, state)
}

func (o *OIDCAuth) takeFlow(state string) (*oidcFlow, bool) {
	o.flowMu.Lock()
	defer o.flowMu.Unlock()
	f, ok := o.flows[state]
	if !ok {
		return nil, false
	}
	// flowOrder keeps the now-stale key until it reaches the front of the
	// queue in a future storeFlow call, where the ok-check above prunes it.
	delete(o.flows, state)
	if time.Now().After(f.exp) {
		return nil, false
	}
	return f, true
}

// resolveUsername picks a stable identity from the token claims.
func (o *OIDCAuth) resolveUsername(claims map[string]any) string {
	for _, c := range []string{o.usernameClaim, "preferred_username", "email", "sub"} {
		if c == "" {
			continue
		}
		if s := claimString(claims[c]); s != "" {
			return s
		}
	}
	return ""
}

// resolveRole maps token claims to a role: a recognized role claim wins, then a
// group claim matched against the configured groups (admin > editor > viewer),
// then the configured default (viewer unless overridden).
func (o *OIDCAuth) resolveRole(claims map[string]any) string {
	if o.roleClaim != "" {
		if r, ok := knownRole(claimString(claims[o.roleClaim])); ok {
			return r
		}
	}
	if o.groupsClaim != "" {
		groups := claimStrings(claims[o.groupsClaim])
		for _, g := range groups {
			if o.adminGroup != "" && g == o.adminGroup {
				return RoleAdmin
			}
		}
		for _, g := range groups {
			if o.editorGroup != "" && g == o.editorGroup {
				return RoleEditor
			}
		}
		for _, g := range groups {
			if o.viewerGroup != "" && g == o.viewerGroup {
				return RoleViewer
			}
		}
	}
	return o.defaultRole
}

func knownRole(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RoleAdmin:
		return RoleAdmin, true
	case RoleEditor:
		return RoleEditor, true
	case RoleViewer:
		return RoleViewer, true
	}
	return "", false
}

func claimString(v any) string {
	s, _ := v.(string)
	return s
}

func claimStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// effectiveRedirect returns the OAuth redirect URL, preferring the configured
// value and otherwise deriving it from the request.
func (o *OIDCAuth) effectiveRedirect(r *http.Request) string {
	if o.redirectURL != "" {
		return o.redirectURL
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/v1/auth/oidc/callback"
}

// requestIsHTTPS reports whether the request arrived over TLS, directly or via a
// terminating proxy's X-Forwarded-Proto.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(proto, "https")
}

// HandleLogin starts the authorization-code flow.
func (o *OIDCAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if o == nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	if err := o.ensure(ctx); err != nil {
		o.logger.Error("OIDC discovery failed", "err", err)
		o.fail(w, r, "SSO provider is unavailable — try again shortly")
		return
	}
	state := oidcRandToken()
	nonce := oidcRandToken()
	verifier := oauth2.GenerateVerifier()
	o.storeFlow(state, &oidcFlow{nonce: nonce, verifier: verifier, exp: time.Now().Add(oidcFlowTTL)})
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: state, Path: "/", MaxAge: int(oidcFlowTTL.Seconds()),
		HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	cfg := o.oauthConfig(o.effectiveRedirect(r))
	authURL := cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// HandleCallback completes the flow: verify state, exchange the code, verify the
// ID token (signature, audience, expiry, nonce), map claims to a role, mint an
// Observe JWT, and hand it to the SPA via a token-delivery interstitial.
func (o *OIDCAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if o == nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		o.failAudit(w, r, "", auditResultFailure, "idp_error", "SSO error: "+strings.TrimSpace(e+" "+q.Get("error_description")))
		return
	}

	state := q.Get("state")
	cookie, cookieErr := r.Cookie(oidcStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	if cookieErr != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		o.failAudit(w, r, "", auditResultFailure, "state_mismatch", "SSO state mismatch — please sign in again")
		return
	}
	flow, ok := o.takeFlow(state)
	if !ok {
		o.failAudit(w, r, "", auditResultFailure, "flow_expired", "SSO session expired — please sign in again")
		return
	}

	if err := o.ensure(ctx); err != nil {
		o.failAudit(w, r, "", auditResultFailure, "provider_unavailable", "SSO provider is unavailable — try again shortly")
		return
	}
	cfg := o.oauthConfig(o.effectiveRedirect(r))
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		o.logger.Error("OIDC token exchange failed", "err", err)
		o.failAudit(w, r, "", auditResultFailure, "code_exchange_failed", "SSO sign-in failed — please try again")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		o.failAudit(w, r, "", auditResultFailure, "missing_id_token", "SSO response was missing an ID token")
		return
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		o.logger.Error("OIDC ID-token verification failed", "err", err)
		o.failAudit(w, r, "", auditResultFailure, "id_token_invalid", "SSO sign-in failed — please try again")
		return
	}
	if idToken.Nonce != flow.nonce {
		o.failAudit(w, r, "", auditResultFailure, "nonce_mismatch", "SSO sign-in failed — please try again")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		o.failAudit(w, r, "", auditResultFailure, "claims_unreadable", "SSO sign-in failed — please try again")
		return
	}
	username := o.resolveUsername(claims)
	if username == "" {
		o.failAudit(w, r, "", auditResultFailure, "no_username_claim", "SSO identity has no usable username claim")
		return
	}
	if !o.allowed(claims) {
		o.logger.Warn("OIDC login denied by allowlist", "username", username)
		o.failAudit(w, r, username, auditResultDenied, "not_in_allowlist", "your account is not authorized for SSO on this instance")
		return
	}
	role := o.resolveRole(claims)

	// F05: the principal id is namespaced by issuer — <sub> is only unique
	// within one issuer, so "oidc:<sub>" alone conflated identities across
	// IdPs. The issuer is hashed (sha256, first 16 hex chars) to keep ids
	// bounded and ASCII. The principal row this id keys is what makes an SSO
	// session revocable: UpsertOIDC creates it on first login (or refreshes
	// username/email/role from the IdP's current claims on later ones, the
	// IdP staying authoritative for role) while PRESERVING token_version, so
	// one device signing in does not retire another's still-valid session.
	// The minted JWT embeds the row's current token_version, and
	// JWTAuthMiddleware version-checks every token against the store — an
	// admin can now revoke a live SSO session via the revoke-sessions
	// operation instead of rotating OBSERVE_JWT_SECRET.
	userID := OIDCSubjectID(o.issuer, idToken.Subject)
	email := strings.ToLower(strings.TrimSpace(claimString(claims["email"])))
	tokenVersion, err := o.authSvc.Principals().UpsertOIDC(ctx, userID, username, email, role)
	if err != nil {
		o.logger.Error("OIDC principal upsert failed", "err", err)
		o.failAudit(w, r, username, auditResultFailure, "principal_store_failed", "SSO sign-in failed — please try again")
		return
	}
	jwt, err := o.authSvc.GenerateToken(userID, username, role, tokenVersion)
	if err != nil {
		o.logger.Error("OIDC token mint failed", "err", err)
		o.failAudit(w, r, username, auditResultFailure, "token_mint_failed", "SSO sign-in failed — please try again")
		return
	}
	o.recordAudit(r, username, auditResultSuccess, "role="+role)
	o.deliverToken(w, jwt)
}

// OIDCSubjectID builds the principal id for an SSO identity: issuer-namespaced
// (audit F05), because <sub> is unique only within one issuer. sha256 keeps
// the namespace fixed-width and ASCII regardless of the issuer URL.
func OIDCSubjectID(issuer, sub string) string {
	sum := sha256.Sum256([]byte(issuer))
	return "oidc:" + hex.EncodeToString(sum[:8]) + ":" + sub
}

// failAudit records the outcome to the audit trail, then redirects the user
// back to the login page with a human-readable message. detail is a stable
// machine-readable reason code; msg is the user-facing text.
func (o *OIDCAuth) failAudit(w http.ResponseWriter, r *http.Request, username, result, detail, msg string) {
	o.recordAudit(r, username, result, detail)
	o.fail(w, r, msg)
}

// deliverToken renders a minimal interstitial that stores the JWT in the SPA's
// localStorage slot and navigates to the app. The token is delivered in the
// response body over the authenticated TLS channel — never in the URL (which
// would leak via history/referrer). No-store so it isn't cached.
func (o *OIDCAuth) deliverToken(w http.ResponseWriter, jwt string) {
	enc, err := json.Marshal(jwt)
	if err != nil {
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The JWT is a JSON string literal; JWTs contain only [A-Za-z0-9._-] so no
	// </script> breakout is possible, but json.Marshal keeps it robust.
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Signing in…</title>`+
		`<body style="background:#0b0e14;color:#e6edf3;font-family:system-ui;padding:2rem">Signing you in…`+
		`<script>try{localStorage.setItem(%q,%s)}catch(e){}location.replace("/")</script></body>`,
		oidcTokenKey, string(enc))
}

func (o *OIDCAuth) fail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusFound)
}

func oidcRandToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
