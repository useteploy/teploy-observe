package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
	"github.com/neutron-dev/neutron-go/neutronauth"

	"github.com/teploy/observe/internal/dbutil"
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
	CreatedAt    int64  `db:"created_at"`
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

// GenerateToken creates a signed JWT with a 24-hour expiry.
func (s *AuthService) GenerateToken(userID, username string) (string, error) {
	claims := neutronauth.Claims{
		"sub":      userID,
		"username": username,
	}
	return neutronauth.GenerateToken(claims, s.jwtSecret, 24*time.Hour)
}

// ValidateToken verifies a JWT and returns the claims.
func (s *AuthService) ValidateToken(tokenStr string) (neutronauth.Claims, error) {
	return neutronauth.ParseToken(tokenStr, s.jwtSecret)
}

// EnsureAdmin creates a default admin user if the admin_users table is empty.
func (s *AuthService) EnsureAdmin(ctx context.Context, username, password string) error {
	sql := s.db.SQL()

	// Ensure table exists (migration may have been applied before this table was added)
	_, _ = sql.Exec(ctx, `CREATE TABLE IF NOT EXISTS admin_users (
		id TEXT NOT NULL, username TEXT NOT NULL, password_hash TEXT NOT NULL, created_at BIGINT NOT NULL)`)

	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM admin_users")
	if err != nil {
		return fmt.Errorf("auth: check admin users: %w", err)
	}
	if len(rows) > 0 && rows[0].Count > 0 {
		return nil
	}

	id := generateID()
	hash := hashPassword(password)
	now := dbutil.IntParam(time.Now().UnixMilli())

	_, err = sql.Exec(ctx,
		"INSERT INTO admin_users (id, username, password_hash, created_at) VALUES ($1, $2, $3, $4)",
		id, username, hash, now,
	)
	if err != nil {
		return fmt.Errorf("auth: create default admin: %w", err)
	}

	s.logger.Info("created default admin user", "username", username, "password", password)
	return nil
}

// Login validates credentials and returns a JWT token.
func (s *AuthService) Login(ctx context.Context, username, password string) (string, error) {
	sql := s.db.SQL()
	hash := hashPassword(password)

	user, err := nucleus.QueryOne[adminUserRow](ctx, sql,
		"SELECT id, username, password_hash, created_at FROM admin_users WHERE username = $1",
		username,
	)
	if err != nil {
		return "", fmt.Errorf("auth: invalid credentials")
	}

	if user.PasswordHash != hash {
		return "", fmt.Errorf("auth: invalid credentials")
	}

	return s.GenerateToken(user.ID, user.Username)
}

// HasAdminUsers returns true if at least one admin user exists.
func (s *AuthService) HasAdminUsers(ctx context.Context) bool {
	sql := s.db.SQL()
	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM admin_users")
	if err != nil {
		return false
	}
	return len(rows) > 0 && rows[0].Count > 0
}

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
