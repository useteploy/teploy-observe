package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/neutron-dev/neutron-go/neutronauth"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// AuthService handles JWT token management, admin user authentication,
// and API key validation.
type AuthService struct {
	db          *nucleus.Client
	jwtSecret   string
	logger      *slog.Logger
	oidcEnabled bool
}

// SetOIDCEnabled records whether OIDC SSO is configured. When it is, the
// first-run grace period (open access while no admin_users exist) is disabled —
// SSO provides a way to authenticate, so the surface must not be left open.
func (s *AuthService) SetOIDCEnabled(v bool) { s.oidcEnabled = v }

// OIDCEnabled reports whether OIDC SSO is configured.
func (s *AuthService) OIDCEnabled() bool { return s.oidcEnabled }

// adminUserRow maps to the admin_users table.
type adminUserRow struct {
	ID           string `db:"id"`
	Username     string `db:"username"`
	PasswordHash string `db:"password_hash"`
	CreatedAt    string `db:"created_at"`
	Role         string `db:"role"`
	TokenVersion int64  `db:"token_version"`
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
// on every request. tokenVersion is embedded so JWTAuthMiddleware can detect
// revocation (OBS-011): a password change bumps the admin_users row's
// token_version, and any token minted before that bump is rejected on its
// next use even though it hasn't expired. Pass 0 for identities that have no
// admin_users row to version — OIDC-issued sessions ("oidc:<subject>") are
// the current case; see middleware.go for how that's handled on validation.
func (s *AuthService) GenerateToken(userID, username, role string, tokenVersion int64) (string, error) {
	claims := neutronauth.Claims{
		"sub":      userID,
		"username": username,
		"role":     normalizeRole(role),
		"tv":       tokenVersion,
	}
	return neutronauth.GenerateToken(claims, s.jwtSecret, 24*time.Hour)
}

// CurrentTokenVersion returns the live token_version for a local admin_users
// row. Used by JWTAuthMiddleware to check a token's embedded "tv" claim
// against current state on every request — the actual revocation check.
func (s *AuthService) CurrentTokenVersion(ctx context.Context, userID string) (int64, error) {
	row, err := nucleus.QueryOne[struct {
		TokenVersion int64 `db:"token_version"`
	}](ctx, s.db.SQL(), "SELECT token_version FROM admin_users WHERE id = $1", userID)
	if err != nil {
		return 0, err
	}
	return row.TokenVersion, nil
}

// ValidateToken verifies a JWT and returns the claims.
func (s *AuthService) ValidateToken(tokenStr string) (neutronauth.Claims, error) {
	return neutronauth.ParseToken(tokenStr, s.jwtSecret)
}

// bootstrapClaimKey is the KV key EnsureAdmin claims atomically before
// inserting the first admin row.
const bootstrapClaimKey = "auth:bootstrap_admin_claimed"

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

	// The COUNT-then-INSERT above is a classic check-then-act race: two
	// concurrent first-run requests can both observe zero rows and both
	// insert, creating two initial admins. KV.SetNX is the real atomicity
	// boundary — only the request that wins the claim proceeds. Released on
	// insert failure so a transient error doesn't permanently brick
	// bootstrap; a hard crash between the claim and the insert (a narrow
	// window around one fast INSERT) would leave the claim set with no row
	// created, recoverable by deleting the "auth:bootstrap_admin_claimed" KV
	// key by hand — rare enough for a once-ever bootstrap operation not to
	// warrant a TTL-based auto-release, which SetNX doesn't support anyway.
	claimed, err := s.db.KV().SetNX(ctx, bootstrapClaimKey, []byte("1"))
	if err != nil {
		return false, fmt.Errorf("auth: claim bootstrap: %w", err)
	}
	if !claimed {
		return false, nil
	}

	id := generateID()
	hash, err := hashPassword(password)
	if err != nil {
		// Never insert an empty hash — that would create an admin nobody can
		// log into (and that fails open in any "no real hash" check).
		s.db.KV().Delete(ctx, bootstrapClaimKey)
		return false, err
	}
	now := dbutil.IntParam(time.Now().UnixMilli())

	_, err = sql.Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role) VALUES ($1, $2, $3, $4, $5)",
		id, username, hash, now, RoleAdmin,
	)
	if err != nil {
		s.db.KV().Delete(ctx, bootstrapClaimKey)
		return false, fmt.Errorf("auth: create default admin: %w", err)
	}

	s.logger.Info("created initial admin user", "username", username)
	return true, nil
}

// Login validates credentials and returns a JWT token.
func (s *AuthService) Login(ctx context.Context, username, password string) (string, error) {
	sql := s.db.SQL()

	user, err := nucleus.QueryOne[adminUserRow](ctx, sql,
		"SELECT id, username, password_hash, created_at, role, token_version FROM admin_users WHERE username = $1",
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

	return s.GenerateToken(user.ID, user.Username, user.Role, user.TokenVersion)
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
		"SELECT id, username, password_hash, created_at, role, token_version FROM admin_users WHERE id = $1", userID,
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

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: change password begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	// Nucleus finding #30: UPDATE does not reliably invalidate the server-side
	// query result cache. DELETE + INSERT ensures the next SELECT sees a fresh
	// physical row, bypassing any stale cache entry for the old hash. Both run
	// in one transaction — previously they were two independent statements, so
	// an insert failure (outage, timeout, schema error) after the delete
	// succeeded deleted the account with no way back; for the sole admin that
	// locks out the entire instance. The transaction rolls the delete back too.
	//
	// token_version is incremented (OBS-011) so JWTs issued before this change
	// stop working on their next use, even though they haven't expired yet —
	// otherwise a compromised token, or a session that should have been cut
	// off, remains valid for up to 24 more hours after the password changes.
	if _, err = tx.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", user.ID); err != nil {
		return fmt.Errorf("auth: change password delete: %w", err)
	}
	now := dbutil.IntParam(time.Now().UnixMilli())
	if _, err = tx.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role, token_version) VALUES ($1, $2, $3, $4, $5, $6)",
		user.ID, user.Username, newHash, now, user.Role, user.TokenVersion+1,
	); err != nil {
		return fmt.Errorf("auth: change password insert: %w", err)
	}
	return tx.Commit(ctx)
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
		"SELECT id, username, password_hash, created_at, role, token_version FROM admin_users WHERE role = $1",
		RoleAdmin,
	)
	if err != nil {
		return fmt.Errorf("auth: no admin user found to reset: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: force reset begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	if _, err = tx.SQL().Exec(ctx, "DELETE FROM admin_users WHERE id = $1", user.ID); err != nil {
		return fmt.Errorf("auth: force reset delete: %w", err)
	}

	now := dbutil.IntParam(time.Now().UnixMilli())
	// token_version bumped for the same reason as ChangePassword (OBS-011).
	if _, err = tx.SQL().Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at, role, token_version) VALUES ($1, $2, $3, $4, $5, $6)",
		user.ID, user.Username, hash, now, user.Role, user.TokenVersion+1,
	); err != nil {
		return fmt.Errorf("auth: force reset insert: %w", err)
	}
	return tx.Commit(ctx)
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
