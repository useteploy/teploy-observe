package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type UserService struct {
	db *nucleus.Client
}

func NewUserService(db *nucleus.Client) *UserService {
	return &UserService{db: db}
}

type User struct {
	UserID       string    `json:"user_id" db:"user_id"`
	TenantID     string    `json:"-" db:"tenant_id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-" db:"password_hash"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	InvitedBy    string    `json:"invited_by" db:"invited_by"`
}

func (s *UserService) Create(ctx context.Context, username, email, password, role, invitedBy string) (*User, error) {
	if role != "admin" && role != "editor" && role != "viewer" {
		role = "viewer"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	userID := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO users (user_id, tenant_id, username, email, password_hash, role, created_at, invited_by)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7)`,
		userID, username, email, string(hash), role, now, invitedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	nowMs, _ := strconv.ParseInt(now, 10, 64)
	return &User{
		UserID:    userID,
		Username:  username,
		Email:     email,
		Role:      role,
		CreatedAt: time.UnixMilli(nowMs).UTC(),
		InvitedBy: invitedBy,
	}, nil
}

func (s *UserService) List(ctx context.Context) ([]User, error) {
	return nucleus.Query[User](ctx, s.db.SQL(),
		`SELECT user_id, tenant_id, username, email, password_hash, role, created_at, invited_by
		 FROM users ORDER BY created_at ASC`)
}

func (s *UserService) Get(ctx context.Context, userID string) (*User, error) {
	rows, err := nucleus.Query[User](ctx, s.db.SQL(),
		`SELECT user_id, tenant_id, username, email, password_hash, role, created_at, invited_by
		 FROM users WHERE user_id = $1`, userID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// UpdateRole changes a user's role.
//
// `users` is a PLAIN mergetree: it has no version column, so there is nothing
// to collapse by and the argMax pattern the replacing tables use does not
// apply. The previous shape —
//
//	INSERT INTO users (...) SELECT ..., $2, $3, ... FROM users WHERE user_id = $1
//
// — appended one row per row already present, so the physical count for a user
// DOUBLED on every role change, and List/Get read the raw table with no dedup
// at all: after one demotion the table holds both an 'admin' and a 'viewer' row
// for the same person and whichever comes back first decides what the UI (and
// any check reading Get) believes. That is an authorization result, not a
// cosmetic one.
//
// The only shape that leaves exactly one row on a table with no version column
// is a real replace: delete the id, then insert the new row — in one
// transaction, so a crash between the two cannot lose the user. This also
// collapses any duplicates an earlier UpdateRole already wrote, and preserves
// created_at, which the old statement overwrote with the edit time.
func (s *UserService) UpdateRole(ctx context.Context, userID, role string) error {
	if role != "admin" && role != "editor" && role != "viewer" {
		return fmt.Errorf("invalid role: %s", role)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// created_at DESC picks the newest of any duplicates the old statement left.
	rows, err := nucleus.Query[User](ctx, tx.SQL(),
		`SELECT user_id, tenant_id, username, email, password_hash, role, created_at, invited_by
		 FROM users WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, userID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("user %s not found", userID)
	}
	u := rows[0]

	if _, err := tx.SQL().Exec(ctx, `DELETE FROM users WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.SQL().Exec(ctx,
		`INSERT INTO users (user_id, tenant_id, username, email, password_hash, role, created_at, invited_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.UserID, u.TenantID, u.Username, u.Email, u.PasswordHash, role,
		strconv.FormatInt(u.CreatedAt.UnixMilli(), 10), u.InvitedBy,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
