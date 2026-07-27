package auth

import (
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
