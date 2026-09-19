package auth

import (
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestOIDC() *OIDCAuth {
	return &OIDCAuth{
		usernameClaim: "preferred_username",
		roleClaim:     "teploy_role",
		groupsClaim:   "groups",
		adminGroup:    "observe-admins",
		editorGroup:   "observe-editors",
		viewerGroup:   "observe-viewers",
		defaultRole:   RoleViewer,
		flows:         make(map[string]*oidcFlow),
	}
}

func TestResolveRoleDirectClaimWins(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{
		"teploy_role": "editor",
		"groups":      []any{"observe-admins"},
	})
	if got != RoleEditor {
		t.Fatalf("direct claim: got %q, want editor", got)
	}
}

func TestResolveRoleUnknownClaimFallsThroughToGroups(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{
		"teploy_role": "superuser",
		"groups":      []any{"observe-editors"},
	})
	if got != RoleEditor {
		t.Fatalf("fallthrough to groups: got %q, want editor", got)
	}
}

func TestResolveRoleGroupPrecedence(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{
		"groups": []any{"observe-viewers", "observe-editors", "observe-admins"},
	})
	if got != RoleAdmin {
		t.Fatalf("group precedence: got %q, want admin", got)
	}
}

func TestResolveRoleDefaultWhenNothingMatches(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{"groups": []any{"unrelated"}})
	if got != RoleViewer {
		t.Fatalf("default role: got %q, want viewer", got)
	}
}

func TestResolveRoleEmptyConfiguredGroupNeverMatches(t *testing.T) {
	o := newTestOIDC()
	o.adminGroup = ""
	got := o.resolveRole(map[string]any{"groups": []any{""}})
	if got != RoleViewer {
		t.Fatalf("empty group must not escalate: got %q, want viewer", got)
	}
}

func TestResolveUsernamePriority(t *testing.T) {
	o := newTestOIDC()
	if got := o.resolveUsername(map[string]any{"preferred_username": "jane", "email": "j@x", "sub": "abc"}); got != "jane" {
		t.Fatalf("preferred_username: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{"email": "j@x", "sub": "abc"}); got != "j@x" {
		t.Fatalf("email fallback: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{"sub": "abc"}); got != "abc" {
		t.Fatalf("sub fallback: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{}); got != "" {
		t.Fatalf("no claim: got %q, want empty", got)
	}
}

func TestClaimStrings(t *testing.T) {
	if got := claimStrings([]any{"a", "b", 3, "c"}); len(got) != 3 {
		t.Fatalf("[]any mixed: got %v", got)
	}
	if got := claimStrings("solo"); len(got) != 1 || got[0] != "solo" {
		t.Fatalf("string: got %v", got)
	}
	if got := claimStrings([]string{"x", "y"}); len(got) != 2 {
		t.Fatalf("[]string: got %v", got)
	}
	if got := claimStrings(nil); got != nil {
		t.Fatalf("nil: got %v", got)
	}
}

func TestKnownRole(t *testing.T) {
	for _, in := range []string{"admin", "ADMIN", " editor ", "viewer"} {
		if _, ok := knownRole(in); !ok {
			t.Fatalf("expected %q to be a known role", in)
		}
	}
	if _, ok := knownRole("root"); ok {
		t.Fatal("root must not be a known role")
	}
}

func TestParseOIDCScopes(t *testing.T) {
	if got := parseOIDCScopes(""); len(got) != 3 || got[0] != "openid" {
		t.Fatalf("default scopes: got %v", got)
	}
	got := parseOIDCScopes("email, groups profile")
	if got[0] != "openid" {
		t.Fatalf("openid must lead: got %v", got)
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("scope %q appeared %d times: %v", s, n, got)
		}
	}
}

func TestFlowStoreOneTimeUse(t *testing.T) {
	o := newTestOIDC()
	o.storeFlow("s1", &oidcFlow{nonce: "n", verifier: "v", exp: time.Now().Add(time.Minute)})
	if _, ok := o.takeFlow("s1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := o.takeFlow("s1"); ok {
		t.Fatal("second take must fail (one-time use)")
	}
}

func TestFlowStoreRejectsExpired(t *testing.T) {
	o := newTestOIDC()
	o.storeFlow("stale", &oidcFlow{nonce: "n", exp: time.Now().Add(-time.Second)})
	if _, ok := o.takeFlow("stale"); ok {
		t.Fatal("expired flow must not be usable")
	}
}

func TestOIDCEnabledNilSafe(t *testing.T) {
	var o *OIDCAuth
	if o.Enabled() {
		t.Fatal("nil OIDCAuth must report disabled")
	}
	if o.Label() != "" {
		t.Fatal("nil OIDCAuth label must be empty")
	}
}

// TestAllowedRequiresVerifiedEmail is the audit F04 regression: with an
// email/domain allowlist configured, only a boolean-true email_verified plus
// a matching identity passes. A signed ID token alone does not prove the
// subject controls the asserted email.
func TestAllowedRequiresVerifiedEmail(t *testing.T) {
	o := newTestOIDC()
	o.allowedEmails = map[string]bool{"alice@example.com": true}
	o.allowedDomains = []string{"example.com"}

	cases := []struct {
		name   string
		claims map[string]any
		want   bool
	}{
		{"verified matching email", map[string]any{"email": "alice@example.com", "email_verified": true}, true},
		{"verified matching domain", map[string]any{"email": "bob@Example.com", "email_verified": true}, true},
		{"unverified matching email", map[string]any{"email": "alice@example.com", "email_verified": false}, false},
		{"missing email_verified", map[string]any{"email": "alice@example.com"}, false},
		{"string email_verified", map[string]any{"email": "alice@example.com", "email_verified": "true"}, false},
		{"verified non-matching", map[string]any{"email": "mallory@evil.test", "email_verified": true}, false},
		{"domain suffix confusion", map[string]any{"email": "a@notexample.com", "email_verified": true}, false},
		{"whitespace email", map[string]any{"email": " alice@example.com ", "email_verified": true}, true},
	}
	for _, tc := range cases {
		if got := o.allowed(tc.claims); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}

	// No allowlist configured: admission is the issuer's problem, and the
	// verified-email requirement must not activate.
	bare := newTestOIDC()
	if !bare.allowed(map[string]any{"email": "anyone@anywhere.test"}) {
		t.Fatal("no allowlist configured must admit any authenticated identity")
	}
}

// AUD-007 (round 2): partial OIDC configuration must be a startup error,
// never a silent "SSO disabled". The grace period reopens on an unclaimed
// instance when SSO silently turns off, so every one-field configuration
// is refused.
func TestNewOIDCAuth_PartialConfigIsError(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"issuer only", map[string]string{"OBSERVE_OIDC_ISSUER": "https://sso.example.com"}},
		{"client id only", map[string]string{"OBSERVE_OIDC_CLIENT_ID": "observe"}},
		{"secret only", map[string]string{"OBSERVE_OIDC_CLIENT_SECRET": "s3cr3t"}},
		{"redirect only", map[string]string{"OBSERVE_OIDC_REDIRECT_URL": "https://observe.example.com/api/v1/auth/oidc/callback"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			o, err := NewOIDCAuth(nil, slog.Default())
			if err == nil || o != nil {
				t.Fatalf("partial OIDC config must error, got (%v, %v)", o, err)
			}
		})
	}
}

func TestNewOIDCAuth_FullConfigOK(t *testing.T) {
	t.Setenv("OBSERVE_OIDC_ISSUER", "https://sso.example.com")
	t.Setenv("OBSERVE_OIDC_CLIENT_ID", "observe")
	o, err := NewOIDCAuth(nil, slog.Default())
	if err != nil || o == nil {
		t.Fatalf("full config must succeed, got (%v, %v)", o, err)
	}
}

func TestNewOIDCAuth_UnsetMeansDisabled(t *testing.T) {
	o, err := NewOIDCAuth(nil, slog.Default())
	if o != nil || err != nil {
		t.Fatalf("no OIDC env must mean disabled with no error, got (%v, %v)", o, err)
	}
}

func TestNewOIDCAuth_IssuerURLValidation(t *testing.T) {
	t.Setenv("OBSERVE_OIDC_CLIENT_ID", "observe")
	cases := []struct {
		name    string
		issuer  string
		wantErr bool
	}{
		{"https absolute", "https://sso.example.com", false},
		{"https with path", "https://sso.example.com/realms/main", false},
		{"http rejected", "http://localhost:8080", true},
		{"userinfo rejected", "https://user:pw@sso.example.com", true},
		{"query rejected", "https://sso.example.com?a=b", true},
		{"fragment rejected", "https://sso.example.com#frag", true},
		{"relative rejected", "sso.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OBSERVE_OIDC_ISSUER", tc.issuer)
			_, err := NewOIDCAuth(nil, slog.Default())
			if (err != nil) != tc.wantErr {
				t.Fatalf("issuer %q: err = %v, wantErr %v", tc.issuer, err, tc.wantErr)
			}
		})
	}
	// Narrow dev override.
	t.Setenv("OBSERVE_OIDC_ISSUER", "http://localhost:8080")
	t.Setenv("OBSERVE_OIDC_ALLOW_HTTP_ISSUER", "true")
	if _, err := NewOIDCAuth(nil, slog.Default()); err != nil {
		t.Fatalf("http issuer with explicit dev override must pass, got %v", err)
	}
}

// TO-053: a policy-only OIDC configuration (role claim, allowlist, groups)
// must fail closed — the old four-field check read it as "OIDC disabled".
func TestNewOIDCAuth_PolicyOnlyConfigIsError(t *testing.T) {
	cases := map[string]map[string]string{
		"default role only":    {"OBSERVE_OIDC_DEFAULT_ROLE": "viewer"},
		"allowed domains only": {"OBSERVE_OIDC_ALLOWED_DOMAINS": "example.com"},
		"admin group only":     {"OBSERVE_OIDC_ADMIN_GROUP": "observe-admins"},
		"scopes only":          {"OBSERVE_OIDC_SCOPES": "openid email"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			o, err := NewOIDCAuth(nil, slog.Default())
			if err == nil || o != nil {
				t.Fatalf("policy-only OIDC config must error, got (%v, %v)", o, err)
			}
		})
	}
}

// TO-052: the configured redirect is validated at startup and pins the
// Secure cookie decision off request headers.
func TestValidateOIDCRedirect(t *testing.T) {
	const cb = "https://observe.example.com/api/v1/auth/oidc/callback"
	cases := []struct {
		name         string
		raw          string
		allowLocal   bool
		wantErr      bool
	}{
		{"valid https callback", cb, false, false},
		{"wrong path", "https://observe.example.com/callback", false, true},
		{"query on redirect", cb + "?x=1", false, true},
		{"userinfo on redirect", "https://u:p@observe.example.com/api/v1/auth/oidc/callback", false, true},
		{"http production rejected", "http://sso.internal.example/api/v1/auth/oidc/callback", false, true},
		{"http localhost allowed under flag", "http://localhost:3000/api/v1/auth/oidc/callback", true, false},
		{"http localhost rejected without flag", "http://localhost:3000/api/v1/auth/oidc/callback", false, true},
		{"relative rejected", "/api/v1/auth/oidc/callback", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOIDCRedirect(tc.raw, tc.allowLocal)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOIDCRedirect(%q) err=%v wantErr=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// TO-052: with a validated https redirect configured, the state cookie is
// Secure even when the login request arrives over plain HTTP (a proxy that
// drops X-Forwarded-Proto must not downgrade the cookie).
func TestStateCookieSecure_PinnedByConfiguredRedirect(t *testing.T) {
	o := &OIDCAuth{redirectURL: "https://observe.example.com/api/v1/auth/oidc/callback"}
	r := httptest.NewRequest("GET", "http://observe.example.com/api/v1/auth/oidc/login", nil)
	if !o.stateCookieSecure(r) {
		t.Fatal("configured https redirect must pin Secure regardless of the request transport")
	}
	o2 := &OIDCAuth{redirectURL: "http://localhost:3000/api/v1/auth/oidc/callback"}
	if o2.stateCookieSecure(r) {
		t.Fatal("an http redirect (local dev) must not pin Secure")
	}
	o3 := &OIDCAuth{}
	if o3.stateCookieSecure(r) {
		t.Fatal("unconfigured redirect falls back to the request transport")
	}
	r2 := httptest.NewRequest("GET", "http://observe.example.com/api/v1/auth/oidc/login", nil)
	r2.Header.Set("X-Forwarded-Proto", "https")
	if !o3.stateCookieSecure(r2) {
		t.Fatal("forwarded https still counts in the unconfigured local-dev path")
	}
}
