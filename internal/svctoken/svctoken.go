// Package svctoken implements scoped service tokens: long-lived machine
// credentials an admin mints for another Teploy product so it can provision
// sites and telemetry keys without holding a person's session.
//
// The scope set is closed and deliberately small. Nothing here can grant users,
// SQL, telemetry reads or site deletion, because no scope for those exists and
// the routes that do them authenticate with a JWT, which a service token is
// not. See docs/service-tokens.md.
package svctoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Prefix marks a service token. It is distinct from a telemetry key's "obs_"
// only by what follows, so nothing may test for "obs_" and assume a key.
const Prefix = "obs_svc_"

// The closed scope set.
const (
	ScopeSitesCreate   = "sites:create"
	ScopeKeysMint      = "keys:mint"
	ScopeShareMint     = "share:mint"
	ScopeWebhooksWrite = "webhooks:write"
	ScopeAlertsRead    = "alerts:read"
)

var validScopes = map[string]bool{
	ScopeSitesCreate:   true,
	ScopeKeysMint:      true,
	ScopeShareMint:     true,
	ScopeWebhooksWrite: true,
	ScopeAlertsRead:    true,
}

// NormalizeScopes validates a requested scope list. Unlike telemetry keys there
// is no default: a token that names no capability is a mistake, and an unknown
// scope must not mint a token that does less than its creator believes.
func NormalizeScopes(requested []string) ([]string, error) {
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !validScopes[s] {
			return nil, fmt.Errorf("unknown scope %q", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one scope is required")
	}
	sort.Strings(out)
	return out, nil
}

// Token is one service token record. The hash never leaves this package's
// reads over JSON, and no read path carries the plaintext.
type Token struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ManagedBy  string   `json:"managed_by"`
	Scopes     []string `json:"scopes"`
	CreatedBy  string   `json:"created_by"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt int64    `json:"last_used_at"`
	RevokedAt  int64    `json:"revoked_at"`

	hash      string
	updatedAt int64
}

// Has reports whether the token carries a scope.
func (t Token) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Revoked reports whether the token has been revoked.
func (t Token) Revoked() bool { return t.RevokedAt > 0 }

// ErrInvalid means the presented value is not a live service token.
var ErrInvalid = errors.New("invalid or revoked service token")

// ErrUnavailable means the store could not answer. It is not a verdict on the
// credential: a caller must not retire a good token because the database
// blinked, so the middleware maps this to 503, not 401.
var ErrUnavailable = errors.New("service token store unavailable")

// ErrNotFound means no token has the given id.
var ErrNotFound = errors.New("service token not found")

// lastUsedInterval throttles last-used writes. Every write here is an append.
const lastUsedInterval = 5 * time.Minute

const maxLabelLen = 128

// Store persists service tokens in Nucleus.
type Store struct {
	db *nucleus.Client
	mu sync.Mutex
}

// NewStore wires the store to the shared Nucleus client.
func NewStore(db *nucleus.Client) *Store { return &Store{db: db} }

// cleanLabel validates a human-chosen identifier.
func cleanLabel(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(v) > maxLabelLen {
		return "", fmt.Errorf("%s is longer than %d bytes", field, maxLabelLen)
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s contains control characters", field)
	}
	return v, nil
}

// Create mints a token and returns its plaintext, shown once. managedBy names
// the namespace of sites the token acts on and defaults to name, so minting a
// replacement under the same name inherits the old token's sites. createdBy
// must identify the admin: a token nobody can be held to is refused.
func (s *Store) Create(ctx context.Context, name, managedBy, createdBy string, scopes []string) (string, Token, error) {
	name, err := cleanLabel("name", name)
	if err != nil {
		return "", Token{}, err
	}
	if strings.TrimSpace(managedBy) == "" {
		managedBy = name
	}
	managedBy, err = cleanLabel("managed_by", managedBy)
	if err != nil {
		return "", Token{}, err
	}
	if strings.TrimSpace(createdBy) == "" {
		return "", Token{}, errors.New("a service token must be created by an authenticated admin")
	}
	normalized, err := NormalizeScopes(scopes)
	if err != nil {
		return "", Token{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", Token{}, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return "", Token{}, err
	}
	plaintext := Prefix + base64.RawURLEncoding.EncodeToString(secret)
	now := time.Now().UTC().UnixMilli()
	t := Token{
		ID:        hex.EncodeToString(id),
		Name:      name,
		ManagedBy: managedBy,
		Scopes:    normalized,
		CreatedBy: strings.TrimSpace(createdBy),
		CreatedAt: now,
		hash:      hashToken(plaintext),
	}
	if err := s.write(ctx, t, 0, now); err != nil {
		return "", Token{}, err
	}
	return plaintext, t, nil
}

// List returns every token record, oldest first.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	rows, err := s.read(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("svctoken: list: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt < rows[j].CreatedAt
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

// Revoke retires a token. The row stays so the audit trail can still name what
// acted. A success return means a read of the store sees the revocation; a
// write that is not visible must not be reported as done.
func (s *Store) Revoke(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.read(ctx, "token_id = $1", id)
	if err != nil {
		return fmt.Errorf("svctoken: revoke: %w", err)
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	t := rows[0]
	if t.Revoked() {
		return nil
	}
	now := time.Now().UTC().UnixMilli()
	t.RevokedAt = now
	if err := s.write(ctx, t, t.updatedAt, now); err != nil {
		return fmt.Errorf("svctoken: revoke: %w", err)
	}
	after, err := s.read(ctx, "token_id = $1", id)
	if err != nil {
		return fmt.Errorf("svctoken: confirm revocation: %w", err)
	}
	if len(after) == 0 || !after[0].Revoked() {
		return errors.New("svctoken: revocation could not be confirmed")
	}
	return nil
}

// Verify resolves a presented plaintext to its live token. It returns
// ErrInvalid for anything that is not one and ErrUnavailable when the store
// cannot answer; neither lets a request through.
func (s *Store) Verify(ctx context.Context, plaintext string) (Token, error) {
	if !strings.HasPrefix(plaintext, Prefix) {
		return Token{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.read(ctx, "")
	if err != nil {
		return Token{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	tok, ok := match(rows, plaintext)
	if !ok {
		return Token{}, ErrInvalid
	}
	now := time.Now().UTC().UnixMilli()
	if now-tok.LastUsedAt >= lastUsedInterval.Milliseconds() {
		used := tok
		used.LastUsedAt = now
		if err := s.write(ctx, used, tok.updatedAt, now); err == nil {
			tok.LastUsedAt = now
		}
	}
	return tok, nil
}

// match is the pure selection step of Verify: a constant-time comparison
// against every stored hash, refusing a revoked row.
func match(rows []Token, plaintext string) (Token, bool) {
	want := []byte(hashToken(plaintext))
	for _, t := range rows {
		if subtle.ConstantTimeCompare([]byte(t.hash), want) != 1 {
			continue
		}
		if t.Revoked() {
			return Token{}, false
		}
		return t, true
	}
	return Token{}, false
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// nextUpdatedAt keeps the version strictly monotonic: updated_at is what reads
// collapse by, so a same-millisecond rewrite must not tie with the prior row.
func nextUpdatedAt(prior, now int64) int64 {
	if prior+1 > now {
		return prior + 1
	}
	return now
}

func (s *Store) write(ctx context.Context, t Token, prior, now int64) error {
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO service_tokens (token_id, tenant_id, name, managed_by, hash, scopes, created_by, created_at, last_used_at, revoked_at, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t.ID, t.Name, t.ManagedBy, t.hash, strings.Join(t.Scopes, ","), t.CreatedBy,
		dbutil.IntParam(t.CreatedAt), dbutil.IntParam(t.LastUsedAt),
		dbutil.IntParam(t.RevokedAt), dbutil.IntParam(nextUpdatedAt(prior, now)))
	if err != nil {
		return fmt.Errorf("svctoken: write: %w", err)
	}
	return nil
}

type tokenRow struct {
	ID         string `db:"token_id"`
	Name       string `db:"name"`
	ManagedBy  string `db:"managed_by"`
	Hash       string `db:"hash"`
	Scopes     string `db:"scopes"`
	CreatedBy  string `db:"created_by"`
	CreatedAt  int64  `db:"created_at"`
	LastUsedAt int64  `db:"last_used_at"`
	RevokedAt  int64  `db:"revoked_at"`
	UpdatedAt  int64  `db:"updated_at"`
}

// read returns the collapsed rows. FINAL parses but is ignored by Nucleus, so
// argMax is the only form that collapses (see internal/mcp and internal/incidents).
// Revocation collapses with MAX so a stale last-used append cannot undo it.
func (s *Store) read(ctx context.Context, where string, args ...any) ([]Token, error) {
	if where == "" {
		where = "1 = 1"
	}
	rows, err := nucleus.Query[tokenRow](ctx, s.db.SQL(),
		`SELECT token_id,
		        argMax(name, updated_at)         AS name,
		        argMax(managed_by, updated_at)   AS managed_by,
		        argMax(hash, updated_at)         AS hash,
		        argMax(scopes, updated_at)       AS scopes,
		        argMax(created_by, updated_at)   AS created_by,
		        argMax(created_at, updated_at)   AS created_at,
		        argMax(last_used_at, updated_at) AS last_used_at,
		        MAX(revoked_at)                  AS revoked_at,
		        MAX(updated_at)                  AS updated_at
		 FROM service_tokens WHERE `+where+`
		 GROUP BY token_id`, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Token, len(rows))
	for i, r := range rows {
		out[i] = Token{
			ID: r.ID, Name: r.Name, ManagedBy: r.ManagedBy,
			Scopes: strings.Split(r.Scopes, ","), CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt,
			hash: r.Hash, updatedAt: r.UpdatedAt,
		}
	}
	return out, nil
}
