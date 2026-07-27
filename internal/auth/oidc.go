package auth

import (
	"context"
	"crypto/rand"
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

	initMu   sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier

	flowMu sync.Mutex
	flows  map[string]*oidcFlow
}

// oidcFlow is one in-progress login, keyed by the OAuth state parameter and
// bound to the initiating browser by the state cookie. It carries the nonce and
// PKCE verifier the callback must present.
type oidcFlow struct {
	nonce    string
	verifier string
	exp      time.Time
}

// NewOIDCAuth reads SSO configuration from the environment. It returns nil (SSO
// disabled) unless at least the issuer and client ID are set.
func NewOIDCAuth(authSvc *AuthService, logger *slog.Logger) *OIDCAuth {
	issuer := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ISSUER"))
	clientID := strings.TrimSpace(os.Getenv("OBSERVE_OIDC_CLIENT_ID"))
	if issuer == "" || clientID == "" {
		return nil
	}
	o := &OIDCAuth{
		authSvc:       authSvc,
		logger:        logger,
		issuer:        issuer,
		clientID:      clientID,
		clientSecret:  strings.TrimSpace(os.Getenv("OBSERVE_OIDC_CLIENT_SECRET")),
		redirectURL:   strings.TrimSpace(os.Getenv("OBSERVE_OIDC_REDIRECT_URL")),
		scopes:        parseOIDCScopes(os.Getenv("OBSERVE_OIDC_SCOPES")),
		label:         orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_LABEL")), "Single sign-on"),
		usernameClaim: orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_USERNAME_CLAIM")), "preferred_username"),
		roleClaim:     orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ROLE_CLAIM")), "teploy_role"),
		groupsClaim:   orDefault(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_GROUPS_CLAIM")), "groups"),
		adminGroup:    strings.TrimSpace(os.Getenv("OBSERVE_OIDC_ADMIN_GROUP")),
		editorGroup:   strings.TrimSpace(os.Getenv("OBSERVE_OIDC_EDITOR_GROUP")),
		viewerGroup:   strings.TrimSpace(os.Getenv("OBSERVE_OIDC_VIEWER_GROUP")),
		defaultRole:   normalizeRole(strings.TrimSpace(os.Getenv("OBSERVE_OIDC_DEFAULT_ROLE"))),
		flows:         make(map[string]*oidcFlow),
	}
	logger.Info("OIDC SSO enabled", "issuer", issuer)
	return o
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
	for k, v := range o.flows {
		if now.After(v.exp) {
			delete(o.flows, k)
		}
	}
	o.flows[state] = f
}

func (o *OIDCAuth) takeFlow(state string) (*oidcFlow, bool) {
	o.flowMu.Lock()
	defer o.flowMu.Unlock()
	f, ok := o.flows[state]
	if !ok {
		return nil, false
	}
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
		o.fail(w, r, "SSO error: "+strings.TrimSpace(e+" "+q.Get("error_description")))
		return
	}

	state := q.Get("state")
	cookie, cookieErr := r.Cookie(oidcStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	if cookieErr != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		o.fail(w, r, "SSO state mismatch — please sign in again")
		return
	}
	flow, ok := o.takeFlow(state)
	if !ok {
		o.fail(w, r, "SSO session expired — please sign in again")
		return
	}

	if err := o.ensure(ctx); err != nil {
		o.fail(w, r, "SSO provider is unavailable — try again shortly")
		return
	}
	cfg := o.oauthConfig(o.effectiveRedirect(r))
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		o.logger.Error("OIDC token exchange failed", "err", err)
		o.fail(w, r, "SSO sign-in failed — please try again")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		o.fail(w, r, "SSO response was missing an ID token")
		return
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		o.logger.Error("OIDC ID-token verification failed", "err", err)
		o.fail(w, r, "SSO sign-in failed — please try again")
		return
	}
	if idToken.Nonce != flow.nonce {
		o.fail(w, r, "SSO sign-in failed — please try again")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		o.fail(w, r, "SSO sign-in failed — please try again")
		return
	}
	username := o.resolveUsername(claims)
	if username == "" {
		o.fail(w, r, "SSO identity has no usable username claim")
		return
	}
	role := o.resolveRole(claims)

	// Mint the same JWT password login issues. The user ID is the IdP subject,
	// namespaced so it never collides with a local admin_users row ID.
	userID := "oidc:" + idToken.Subject
	jwt, err := o.authSvc.GenerateToken(userID, username, role)
	if err != nil {
		o.logger.Error("OIDC token mint failed", "err", err)
		o.fail(w, r, "SSO sign-in failed — please try again")
		return
	}
	o.deliverToken(w, jwt)
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
