package auth

import (
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestChangePassword_HappyPath is the regression for OBS-010: ChangePassword
// was rewritten from two independent DELETE/INSERT statements to one
// transaction. This confirms the rewrite didn't break the normal path — old
// password rejected after the change, new password accepted.
func TestChangePassword_HappyPath(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	id := generateID()
	hash, err := hashPassword("original-password")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if _, err := db.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		id, uniqueSite("pwtest"), hash, "0", RoleAdmin,
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	defer db.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", id)

	if err := svc.ChangePassword(ctx, id, "original-password", "new-password-123"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	user, err := nucleus.QueryOne[adminUserRow](ctx, db.SQL(),
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE id = $1", id)
	if err != nil {
		t.Fatalf("re-fetch user: %v", err)
	}
	if checkPassword("original-password", user.PasswordHash) {
		t.Error("old password still authenticates after ChangePassword")
	}
	if !checkPassword("new-password-123", user.PasswordHash) {
		t.Error("new password does not authenticate after ChangePassword")
	}
}

// TestChangePassword_WrongCurrentPasswordLeavesRowIntact confirms a rejected
// change (wrong current password) never reaches the delete/insert at all —
// the row and its original hash must be untouched.
func TestChangePassword_WrongCurrentPasswordLeavesRowIntact(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	id := generateID()
	hash, err := hashPassword("original-password")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if _, err := db.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		id, uniqueSite("pwtest"), hash, "0", RoleAdmin,
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	defer db.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", id)

	if err := svc.ChangePassword(ctx, id, "totally-wrong", "new-password-123"); err == nil {
		t.Fatal("expected error for wrong current password")
	}

	user, err := nucleus.QueryOne[adminUserRow](ctx, db.SQL(),
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE id = $1", id)
	if err != nil {
		t.Fatalf("re-fetch user: %v", err)
	}
	if !checkPassword("original-password", user.PasswordHash) {
		t.Error("original password no longer authenticates after a rejected change attempt")
	}
}

// TestForceResetAdminPassword_HappyPath mirrors the ChangePassword coverage
// for the OBSERVE_RESET_ADMIN_PASSWORD escape hatch, also rewritten to a
// transaction in the same fix.
func TestForceResetAdminPassword_HappyPath(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM admin_users"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	id := generateID()
	hash, err := hashPassword("original-password")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if _, err := db.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		id, uniqueSite("forcereset"), hash, "0", RoleAdmin,
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	defer db.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", id)

	if err := svc.ForceResetAdminPassword(ctx, "reset-password-456"); err != nil {
		t.Fatalf("ForceResetAdminPassword: %v", err)
	}

	user, err := nucleus.QueryOne[adminUserRow](ctx, db.SQL(),
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE id = $1", id)
	if err != nil {
		t.Fatalf("re-fetch user: %v", err)
	}
	if !checkPassword("reset-password-456", user.PasswordHash) {
		t.Error("reset password does not authenticate after ForceResetAdminPassword")
	}
}
