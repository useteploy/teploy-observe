package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
	"golang.org/x/crypto/bcrypt"

	"github.com/useteploy/teploy-observe/internal/principals"
)

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// UserService manages user records. Since audit F03 (migration 040) it writes
// the same principals store authentication reads from — before that, users
// created here landed in a separate `users` table no login path consulted, so
// managed users could never sign in and role changes never revoked their
// (hypothetical) tokens.
type UserService struct {
	store *principals.Store
}

// NewUserService returns a UserService over the shared principal store. Pass
// the same *principals.Store the AuthService uses (authSvc.Principals()) so
// credential mutations and profile mutations serialize under one lock.
func NewUserService(store *principals.Store) *UserService {
	return &UserService{store: store}
}

// User is the user-management API DTO. Field shapes are unchanged from the
// pre-040 API; kind is additive (local accounts and SSO identities share the
// list now, and an admin needs to tell them apart to revoke SSO sessions).
type User struct {
	UserID    string    `json:"user_id"`
	Kind      string    `json:"kind"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	InvitedBy string    `json:"invited_by"`
}

func toUser(p *principals.Principal) User {
	return User{
		UserID:    p.ID,
		Kind:      p.Kind,
		Username:  p.Username,
		Email:     p.Email,
		Role:      p.Role,
		CreatedAt: time.UnixMilli(p.CreatedAtMs()).UTC(),
		InvitedBy: p.InvitedBy,
	}
}

// normalizeRole collapses anything unrecognized to viewer, matching the
// pre-040 Create behaviour.
func normalizeRole(role string) string {
	switch role {
	case "admin", "editor", "viewer":
		return role
	default:
		return "viewer"
	}
}

// Create provisions a local user who can log in immediately — the F03 fix.
// The username must be unique among local principals; a duplicate would
// leave one of the two passwords silently dead now that both authenticate.
func (s *UserService) Create(ctx context.Context, username, email, password, role, invitedBy string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	p, err := s.store.CreateLocal(ctx, username, email, string(hash), normalizeRole(role), invitedBy, principals.OriginCreated)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	u := toUser(p)
	return &u, nil
}

// List returns every principal — local accounts and SSO identities — oldest
// first, in a total order.
func (s *UserService) List(ctx context.Context) ([]User, error) {
	rows, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	users := make([]User, 0, len(rows))
	for i := range rows {
		users = append(users, toUser(&rows[i]))
	}
	return users, nil
}

// Get returns one principal by id.
func (s *UserService) Get(ctx context.Context, userID string) (*User, error) {
	p, err := s.store.ByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, neutron.ErrNotFound("user not found")
	}
	u := toUser(p)
	return &u, nil
}

// UpdateRole changes a principal's role. Since F03 this also bumps
// token_version, so every JWT issued under the old role — including one
// minted seconds ago — stops authenticating on its next use; the acceptance
// gate is create -> login -> promote -> old token invalid.
func (s *UserService) UpdateRole(ctx context.Context, userID, role string) error {
	if role != "admin" && role != "editor" && role != "viewer" {
		return fmt.Errorf("invalid role: %s", role)
	}
	_, err := s.store.SetRole(ctx, userID, role)
	if err != nil {
		return fmt.Errorf("user %s not found: %w", userID, err)
	}
	return nil
}
