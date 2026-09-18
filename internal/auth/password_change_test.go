package auth

import (
	"context"
	"testing"

	"github.com/useteploy/teploy-observe/internal/principals"
)

// seedLocalPrincipal inserts a local principal row directly and returns its
// id, for tests that need a known account without going through EnsureAdmin.
func seedLocalPrincipal(t *testing.T, svc *AuthService, username, password, role, origin string) string {
	t.Helper()
	ctx := context.Background()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	p, err := svc.Principals().CreateLocal(ctx, username, "", hash, role, "", origin)
	if err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	return p.ID
}

// TestChangePassword_HappyPath is the regression for OBS-010: ChangePassword
// was rewritten from two independent DELETE/INSERT statements to one
// transaction. This confirms the rewrite didn't break the normal path — old
// password rejected after the change, new password accepted.
func TestChangePassword_HappyPath(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	id := seedLocalPrincipal(t, svc, uniqueSite("pwtest"), "original-password", RoleAdmin, principals.OriginAdmin)
	defer db.SQL().Exec(ctx, "DELETE FROM principals WHERE id = $1", id)

	if err := svc.ChangePassword(ctx, id, "original-password", "new-password-123"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	user, err := svc.Principals().ByID(ctx, id)
	if err != nil {
		t.Fatalf("re-fetch principal: %v", err)
	}
	if checkPassword("original-password", user.PasswordHash) {
		t.Error("old password still authenticates after ChangePassword")
	}
	if !checkPassword("new-password-123", user.PasswordHash) {
		t.Error("new password does not authenticate after ChangePassword")
	}
}

// TestChangePassword_WrongCurrentPasswordLeavesRowIntact confirms a rejected
// change (wrong current password) never reaches the replace at all — the row
// and its original hash must be untouched.
func TestChangePassword_WrongCurrentPasswordLeavesRowIntact(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	id := seedLocalPrincipal(t, svc, uniqueSite("pwtest"), "original-password", RoleAdmin, principals.OriginAdmin)
	defer db.SQL().Exec(ctx, "DELETE FROM principals WHERE id = $1", id)

	if err := svc.ChangePassword(ctx, id, "totally-wrong", "new-password-123"); err == nil {
		t.Fatal("expected error for wrong current password")
	}

	user, err := svc.Principals().ByID(ctx, id)
	if err != nil {
		t.Fatalf("re-fetch principal: %v", err)
	}
	if !checkPassword("original-password", user.PasswordHash) {
		t.Error("original password no longer authenticates after a rejected change attempt")
	}
}

// TestForceResetAdminPassword_HappyPath mirrors the ChangePassword coverage
// for the OBSERVE_RESET_ADMIN_PASSWORD escape hatch.
func TestForceResetAdminPassword_HappyPath(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM principals"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	id := seedLocalPrincipal(t, svc, uniqueSite("forcereset"), "original-password", RoleAdmin, principals.OriginAdmin)
	defer db.SQL().Exec(ctx, "DELETE FROM principals WHERE id = $1", id)

	if err := svc.ForceResetAdminPassword(ctx, "reset-password-456"); err != nil {
		t.Fatalf("ForceResetAdminPassword: %v", err)
	}

	user, err := svc.Principals().ByID(ctx, id)
	if err != nil {
		t.Fatalf("re-fetch principal: %v", err)
	}
	if !checkPassword("reset-password-456", user.PasswordHash) {
		t.Error("reset password does not authenticate after ForceResetAdminPassword")
	}
}
