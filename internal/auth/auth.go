package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/neutron-dev/neutron-go/nucleus"
	"github.com/neutron-dev/neutron-go/neutronauth"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// AuthService handles JWT token management, admin user authentication,
// and API key validation.
type AuthService struct {
	db        *nucleus.Client
	jwtSecret string
	logger    *slog.Logger
}

// adminUserRow maps to the admin_users table.
type adminUserRow struct {
	ID           string `db:"id"`
	Username     string `db:"username"`
	PasswordHash string `db:"password_hash"`
	CreatedAt    string `db:"created_at"`
	Role         string `db:"role"`
}

// Role constants.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// normalizeRole returns a known role or RoleViewer if unrecognized.
func normalizeRole(r string) string {
	switch r {
	case RoleAdmin, RoleEditor, RoleViewer:
		return r
	default:
		return RoleViewer
	}
}

// countRow is used for COUNT queries.
type countRow struct {
	Count int64 `db:"count"`
}

// NewAuthService creates a new AuthService. If jwtSecret is empty, a random
// 32-byte hex secret is generated.
func NewAuthService(db *nucleus.Client, jwtSecret string, logger *slog.Logger) *AuthService {
	if jwtSecret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		jwtSecret = hex.EncodeToString(b)
		logger.Info("generated random JWT secret (set OBSERVE_JWT_SECRET to persist across restarts)")
	}
	return &AuthService{
		db:        db,
		jwtSecret: jwtSecret,
		logger:    logger,
	}
}

// GenerateToken creates a signed JWT with a 24-hour expiry. role is stored
// in the token so middleware can enforce RBAC without hitting the database
// on every request.
func (s *AuthService) GenerateToken(userID, username, role string) (string, error) {
	claims := neutronauth.Claims{
		"sub":      userID,
		"username": username,
		"role":     normalizeRole(role),
	}
	return neutronauth.GenerateToken(claims, s.jwtSecret, 24*time.Hour)
}

// ValidateToken verifies a JWT and returns the claims.
func (s *AuthService) ValidateToken(tokenStr string) (neutronauth.Claims, error) {
	return neutronauth.ParseToken(tokenStr, s.jwtSecret)
}

// EnsureAdmin creates the initial admin user if the admin_users table is empty.
// It returns true if it created one. The caller is responsible for surfacing a
// generated password — EnsureAdmin never logs the password itself.
func (s *AuthService) EnsureAdmin(ctx context.Context, username, password string) (bool, error) {
	sql := s.db.SQL()

	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM admin_users")
	if err != nil {
		return false, fmt.Errorf("auth: check admin users: %w", err)
	}
	if len(rows) > 0 && rows[0].Count > 0 {
		return false, nil
	}

	id := generateID()
	hash, err := hashPassword(password)
	if err != nil {
		// Never insert an empty hash — that would create an admin nobody can
		// log into (and that fails open in any "no real hash" check).
		return false, err
	}
	now := dbutil.IntParam(time.Now().UnixMilli())

	_, err = sql.Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		id, username, hash, now, RoleAdmin,
	)
	if err != nil {
		return false, fmt.Errorf("auth: create default admin: %w", err)
	}

	s.logger.Info("created initial admin user", "username", username)
	return true, nil
}

// Login validates credentials and returns a JWT token.
func (s *AuthService) Login(ctx context.Context, username, password string) (string, error) {
	sql := s.db.SQL()

	user, err := nucleus.QueryOne[adminUserRow](ctx, sql,
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE username = $1",
		username,
	)
	if err != nil {
		// Run a bcrypt comparison against a fixed dummy hash even when the user
		// doesn't exist, so the response time doesn't leak username existence.
		checkPassword(password, dummyBcryptHash)
		return "", fmt.Errorf("auth: invalid credentials")
	}

	if !checkPassword(password, user.PasswordHash) {
		return "", fmt.Errorf("auth: invalid credentials")
	}

	return s.GenerateToken(user.ID, user.Username, user.Role)
}

// HasAdminUsers reports whether at least one admin user exists. The error is
// returned (not swallowed as false) so callers can fail CLOSED on a DB outage —
// treating a query failure as "no admins → grace period" previously let a
// Nucleus outage bypass authentication entirely.
func (s *AuthService) HasAdminUsers(ctx context.Context) (bool, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM admin_users")
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].Count > 0, nil
}

// ChangePassword updates the password for the given user ID.
func (s *AuthService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, err := nucleus.QueryOne[adminUserRow](ctx, s.db.SQL(),
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE id = $1", userID,
	)
	if err != nil {
		return fmt.Errorf("user not found")
	}
	if !checkPassword(currentPassword, user.PasswordHash) {
		return fmt.Errorf("current password is incorrect")
	}
	if len(newPassword) < 8 {
		return fmt.Errorf("new password must be at least 8 characters")
	}
	if len(newPassword) > maxPasswordBytes {
		// bcrypt errors past 72 bytes; the old code stored the resulting empty
		// hash and locked the account out. Reject loudly instead.
		return fmt.Errorf("new password must be at most %d bytes", maxPasswordBytes)
	}
	newHash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.db.SQL().Exec(ctx,
		"UPDATE admin_users SET password_hash = $1 WHERE id = $2",
		newHash, user.ID,
	)
	return err
}

// ForceResetAdminPassword replaces the first admin user's password.
// Used by the OBSERVE_RESET_ADMIN_PASSWORD startup escape hatch.
//
// Nucleus finding #30: UPDATE does not invalidate the server-side query result
// cache, so a plain UPDATE is not visible to subsequent SELECTs on the same SQL
// text. We DELETE + INSERT instead — the INSERT lands as a new physical row and
// bypasses the stale cache entry for the prior row.
func (s *AuthService) ForceResetAdminPassword(ctx context.Context, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	user, err := nucleus.QueryOne[adminUserRow](ctx, s.db.SQL(),
		"SELECT id, username, password_hash, created_at, role FROM admin_users WHERE role = $1",
		RoleAdmin,
	)
	if err != nil {
		return fmt.Errorf("auth: no admin user found to reset: %w", err)
	}

	if _, err = s.db.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", user.ID); err != nil {
		return fmt.Errorf("auth: force reset delete: %w", err)
	}

	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		user.ID, user.Username, hash, now, user.Role,
	)
	if err != nil {
		return fmt.Errorf("auth: force reset insert: %w", err)
	}
	return nil
}

// maxPasswordBytes is bcrypt's hard input ceiling — GenerateFromPassword errors
// beyond it. Callers must reject longer passwords rather than store the empty
// hash the error path used to produce (which silently locked accounts out).
const maxPasswordBytes = 72

// dummyBcryptHash is a valid bcrypt hash (of a random string) used to spend the
// same CPU on a nonexistent-user login as a real one, removing the timing
// side-channel that would otherwise reveal which usernames exist.
const dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye1J7.6FkVqI3rR0pQ1bQ8XfQ9qK0e2C"

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(hash), nil
}

func checkPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomSecret returns a 48-hex-char (24-byte) random string, for generating a
// secret/salt/password that wasn't supplied via config.
func RandomSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
