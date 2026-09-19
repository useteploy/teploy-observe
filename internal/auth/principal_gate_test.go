package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/useteploy/teploy-observe/internal/platform"
)

// authedHandler wraps JWTAuthMiddleware around a 200 handler, the shape every
// gate test below asserts through.
func authedHandler(svc *AuthService) http.Handler {
	return JWTAuthMiddleware(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func bearer(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestF03Gate_CreateLoginPromoteOldTokenInvalid is the F03 acceptance gate
// from the audit register, run through the real wiring: the platform
// UserService writes the same principal store Login reads (migration 040),
// and UpdateRole bumps token_version so the pre-promotion JWT dies on its
// next use instead of carrying "viewer" for up to 24 more hours.
func TestF03Gate_CreateLoginPromoteOldTokenInvalid(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)
	userSvc := platform.NewUserService(svc.Principals())

	username := uniqueSite("f03gate")
	u, err := userSvc.Create(ctx, username, username+"@example.com", "initial-password-123", "viewer", "")
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", u.UserID)
	})

	// The created account can log in — the core F03 defect was that it
	// could not (Login read admin_users, Create wrote users).
	token, err := svc.Login(ctx, username, "initial-password-123")
	if err != nil {
		t.Fatalf("login as managed user: %v", err)
	}
	claims, err := svc.ValidateToken(token)
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if role, _ := claims["role"].(string); role != "viewer" {
		t.Fatalf("fresh token role: got %q, want viewer", role)
	}

	h := authedHandler(svc)
	if code := bearer(t, h, token); code != http.StatusOK {
		t.Fatalf("token before promote: expected 200, got %d", code)
	}

	// Promote. The old token must stop authenticating immediately.
	if err := userSvc.UpdateRole(ctx, u.UserID, "admin"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if code := bearer(t, h, token); code != http.StatusUnauthorized {
		t.Fatalf("token after promote: expected 401, got %d", code)
	}

	// A fresh login works and carries the new role.
	newToken, err := svc.Login(ctx, username, "initial-password-123")
	if err != nil {
		t.Fatalf("login after promote: %v", err)
	}
	claims, err = svc.ValidateToken(newToken)
	if err != nil {
		t.Fatalf("validate new token: %v", err)
	}
	if role, _ := claims["role"].(string); role != "admin" {
		t.Fatalf("post-promote token role: got %q, want admin", role)
	}
	if code := bearer(t, h, newToken); code != http.StatusOK {
		t.Fatalf("fresh token after promote: expected 200, got %d", code)
	}
}

// TestF03ManagedUsernameCollisionIsRefused: with every local principal
// login-capable, two accounts sharing a username would leave one password
// silently dead — CreateLocal must refuse the second one.
func TestF03ManagedUsernameCollisionIsRefused(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	username := uniqueSite("f03dup")
	id := seedLocalPrincipal(t, svc, username, "first-password-1", RoleAdmin, "admin")
	defer db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", id)

	if _, err := svc.Principals().CreateLocal(ctx, username, "", "second-password-2", RoleViewer, "", "created"); err == nil {
		t.Fatal("CreateLocal accepted a duplicate username")
	}
}

// TestF05SubjectIDIssuerNamespaced is the pure half of F05: the principal id
// namespaces <sub> by issuer, because <sub> is only unique within one issuer.
func TestF05SubjectIDIssuerNamespaced(t *testing.T) {
	a1 := OIDCSubjectID("https://accounts.a.example", "user-123")
	a2 := OIDCSubjectID("https://accounts.a.example", "user-123")
	b := OIDCSubjectID("https://accounts.b.example", "user-123")

	if a1 != a2 {
		t.Fatalf("same issuer+sub must be stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("different issuers, same sub must differ: %q", a1)
	}
	if len(a1) < len("oidc:0123456789abcdef:") || a1[:5] != "oidc:" {
		t.Fatalf("expected oidc:<16 hex>:<sub> shape, got %q", a1)
	}
	namespace := a1[5 : len(a1)-len("user-123")-1]
	for _, c := range namespace {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("namespace must be lowercase hex, got %q in %q", string(c), a1)
		}
	}
	if got := a1[len(a1)-len("user-123"):]; got != "user-123" {
		t.Fatalf("sub must be preserved verbatim, got %q", got)
	}
}

// TestF05OIDCSessionsRevocableAndIssuerScoped is the store half of F05: two
// issuers reusing a subject are two principals, each session embeds its row's
// token_version, revocation kills only the targeted identity, and a later
// IdP re-sign-in preserves the bumped version instead of resurrecting the
// revoked session.
func TestF05OIDCSessionsRevocableAndIssuerScoped(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	// A local principal keeps the middleware out of the first-run grace
	// period, which would otherwise wave every token through.
	admin := seedLocalPrincipal(t, svc, uniqueSite("f05gate"), "admin-password-1", RoleAdmin, "admin")
	store := svc.Principals()
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", admin)
	})

	idA := OIDCSubjectID("https://accounts.a.example", "shared-sub")
	idB := OIDCSubjectID("https://accounts.b.example", "shared-sub")
	if idA == idB {
		t.Fatal("issuer namespacing collapsed two issuers into one principal id")
	}

	tvA, err := store.UpsertOIDC(ctx, idA, "alice", "alice@a.example", RoleViewer)
	if err != nil {
		t.Fatalf("upsert oidc A: %v", err)
	}
	tvB, err := store.UpsertOIDC(ctx, idB, "bob", "bob@b.example", RoleViewer)
	if err != nil {
		t.Fatalf("upsert oidc B: %v", err)
	}
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1 OR id = $2", idA, idB)
	})
	if tvA != 0 || tvB != 0 {
		t.Fatalf("first sign-in token_version: got %d/%d, want 0/0", tvA, tvB)
	}

	tokenA, err := svc.GenerateToken(idA, "alice", RoleViewer, tvA)
	if err != nil {
		t.Fatalf("mint A: %v", err)
	}
	tokenB, err := svc.GenerateToken(idB, "bob", RoleViewer, tvB)
	if err != nil {
		t.Fatalf("mint B: %v", err)
	}

	h := authedHandler(svc)
	if code := bearer(t, h, tokenA); code != http.StatusOK {
		t.Fatalf("SSO token A: expected 200, got %d", code)
	}
	if code := bearer(t, h, tokenB); code != http.StatusOK {
		t.Fatalf("SSO token B: expected 200, got %d", code)
	}

	// The F05 admin operation: revoke every session for identity A only.
	if err := svc.RevokeSessions(ctx, idA); err != nil {
		t.Fatalf("revoke A: %v", err)
	}
	if code := bearer(t, h, tokenA); code != http.StatusUnauthorized {
		t.Fatalf("revoked SSO token A: expected 401, got %d", code)
	}
	if code := bearer(t, h, tokenB); code != http.StatusOK {
		t.Fatalf("SSO token B after revoking A: expected 200, got %d", code)
	}

	// A later sign-in on A refreshes the profile but must not reset the
	// version — the revoked session stays dead.
	tvA2, err := store.UpsertOIDC(ctx, idA, "alice", "alice@a.example", RoleEditor)
	if err != nil {
		t.Fatalf("re-sign-in A: %v", err)
	}
	if tvA2 != tvA+1 {
		t.Fatalf("re-sign-in token_version: got %d, want %d (IdP refresh must preserve the revocation bump)", tvA2, tvA+1)
	}
	stale, err := svc.GenerateToken(idA, "alice", RoleEditor, tvA)
	if err != nil {
		t.Fatalf("mint stale A: %v", err)
	}
	if code := bearer(t, h, stale); code != http.StatusUnauthorized {
		t.Fatalf("stale-version SSO token after re-sign-in: expected 401, got %d", code)
	}
	fresh, err := svc.GenerateToken(idA, "alice", RoleEditor, tvA2)
	if err != nil {
		t.Fatalf("mint fresh A: %v", err)
	}
	if code := bearer(t, h, fresh); code != http.StatusOK {
		t.Fatalf("current-version SSO token after re-sign-in: expected 200, got %d", code)
	}
}

// TestF05Pre040OIDCTokenShapeRejected: JWTs minted before migration 040 carry
// sub "oidc:<sub>" with no principal row behind them. The unconditional
// version check retires them — no row means no valid session.
func TestF05Pre040OIDCTokenShapeRejected(t *testing.T) {
	_, db, done := connect(t)
	defer done()
	svc := testService(db)

	admin := seedLocalPrincipal(t, svc, uniqueSite("f05legacy"), "admin-password-1", RoleAdmin, "admin")
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", admin)
	})

	legacy, err := svc.GenerateToken("oidc:legacy-subject", "someone", RoleAdmin, 0)
	if err != nil {
		t.Fatalf("mint legacy token: %v", err)
	}
	if code := bearer(t, authedHandler(svc), legacy); code != http.StatusUnauthorized {
		t.Fatalf("pre-040 oidc:<sub> token: expected 401, got %d", code)
	}
}

// TestTO003_OIDCRoleDowngradeRetiresOldJWT is the TO-003 gate: an admin JWT
// minted before the IdP downgraded the identity to viewer must die at its
// next use, while an unchanged-role re-sign-in preserves the version.
func TestTO003_OIDCRoleDowngradeRetiresOldJWT(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)
	admin := seedLocalPrincipal(t, svc, uniqueSite("to003"), "admin-password-1", RoleAdmin, "admin")
	store := svc.Principals()
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", admin)
	})

	id := OIDCSubjectID("https://sso.example.com", "to003-sub")
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM principals WHERE id = $1", id)
	})

	tv1, err := store.UpsertOIDC(ctx, id, "carol", "carol@example.com", RoleAdmin)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	oldToken, err := svc.GenerateToken(id, "carol", RoleAdmin, tv1)
	if err != nil {
		t.Fatalf("mint admin token: %v", err)
	}
	h := authedHandler(svc)
	if code := bearer(t, h, oldToken); code != http.StatusOK {
		t.Fatalf("pre-downgrade admin token must work, got %d", code)
	}

	// Unchanged role: version preserved (a profile refresh must not log
	// everybody out).
	tv2, err := store.UpsertOIDC(ctx, id, "carol", "carol@example.com", RoleAdmin)
	if err != nil {
		t.Fatalf("refresh sign-in: %v", err)
	}
	if tv2 != tv1 {
		t.Fatalf("unchanged role must preserve token_version: %d -> %d", tv1, tv2)
	}
	if code := bearer(t, h, oldToken); code != http.StatusOK {
		t.Fatalf("unchanged-role re-sign-in must not retire the session, got %d", code)
	}

	// IdP downgrades to viewer: version bumps, the old admin JWT dies.
	tv3, err := store.UpsertOIDC(ctx, id, "carol", "carol@example.com", RoleViewer)
	if err != nil {
		t.Fatalf("downgrade sign-in: %v", err)
	}
	if tv3 != tv2+1 {
		t.Fatalf("role change must bump token_version: %d -> %d", tv2, tv3)
	}
	if code := bearer(t, h, oldToken); code != http.StatusUnauthorized {
		t.Fatalf("pre-downgrade admin token must be rejected after the role change, got %d", code)
	}
	newToken, err := svc.GenerateToken(id, "carol", RoleViewer, tv3)
	if err != nil {
		t.Fatalf("mint viewer token: %v", err)
	}
	if code := bearer(t, h, newToken); code != http.StatusOK {
		t.Fatalf("post-downgrade token must work, got %d", code)
	}
}
