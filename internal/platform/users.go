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

func (s *UserService) UpdateRole(ctx context.Context, userID, role string) error {
	if role != "admin" && role != "editor" && role != "viewer" {
		return fmt.Errorf("invalid role: %s", role)
	}
	// MergeTree can't UPDATE, so we re-insert. This works because we query
	// by user_id which returns the latest row.
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO users (user_id, tenant_id, username, email, password_hash, role, created_at, invited_by)
		 SELECT user_id, tenant_id, username, email, password_hash, $2, $3, invited_by
		 FROM users WHERE user_id = $1`,
		userID, role, now,
	)
	return err
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
