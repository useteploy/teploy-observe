package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestJWTAuthMiddleware_RevokedTokenRejected is the regression for OBS-011: a
// token minted before a password change must stop working immediately, not
// after its 24-hour expiry.
func TestJWTAuthMiddleware_RevokedTokenRejected(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM admin_users"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	db.KV().Delete(ctx, bootstrapClaimKey)
	defer func() {
		db.SQL().Exec(ctx, "DELETE FROM admin_users")
		db.KV().Delete(ctx, bootstrapClaimKey)
	}()

	username := uniqueSite("mwtest")
	if _, err := svc.EnsureAdmin(ctx, username, "correcthorsebatterystaple"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	token, err := svc.Login(ctx, username, "correcthorsebatterystaple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	user, err := nucleus.QueryOne[adminUserRow](ctx, db.SQL(),
		"SELECT id, username, password_hash, created_at, role, token_version FROM admin_users WHERE username = $1", username)
	if err != nil {
		t.Fatalf("fetch seeded user: %v", err)
	}

	mw := JWTAuthMiddleware(svc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// The freshly issued token works.
	req := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh token: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Changing the password bumps token_version — the old token must now be
	// rejected even though it hasn't expired.
	if err := svc.ChangePassword(ctx, user.ID, "correcthorsebatterystaple", "new-password-999"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	req2 := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: expected 401, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// A freshly minted token (post-change) works again.
	newToken, err := svc.Login(ctx, username, "new-password-999")
	if err != nil {
		t.Fatalf("Login with new password: %v", err)
	}
	req3 := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req3.Header.Set("Authorization", "Bearer "+newToken)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("new token after password change: expected 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestJWTAuthMiddleware_QueryTokenOnlyOnAllowlistedPaths is the regression
// for OBS-016 (reinstated, narrowed scope): a dashboard JWT in ?token= must
// be accepted ONLY on the small set of routes that genuinely can't set an
// Authorization header (EventSource/download contexts), and rejected
// everywhere else.
func TestJWTAuthMiddleware_QueryTokenOnlyOnAllowlistedPaths(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM admin_users"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	db.KV().Delete(ctx, bootstrapClaimKey)
	defer func() {
		db.SQL().Exec(ctx, "DELETE FROM admin_users")
		db.KV().Delete(ctx, bootstrapClaimKey)
	}()

	username := uniqueSite("mwtest")
	if _, err := svc.EnsureAdmin(ctx, username, "correcthorsebatterystaple"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	token, err := svc.Login(ctx, username, "correcthorsebatterystaple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	mw := JWTAuthMiddleware(svc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Allowlisted: /api/v1/export accepts ?token=.
	req := httptest.NewRequest("GET", "/api/v1/export?token="+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("allowlisted path with query token: expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("allowlisted path with query token: expected Referrer-Policy: no-referrer, got %q", got)
	}

	// Not allowlisted: an arbitrary API route must reject ?token=.
	req2 := httptest.NewRequest("GET", "/api/v1/issues?token="+token, nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("non-allowlisted path with query token: expected 401, got %d", rec2.Code)
	}

	// The same non-allowlisted route works fine with a real header.
	req3 := httptest.NewRequest("GET", "/api/v1/issues", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("non-allowlisted path with header token: expected 200, got %d", rec3.Code)
	}
}
