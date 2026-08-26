// Package mcp implements teploy-observe's Model Context Protocol server: a
// bearer-token-authenticated JSON-RPC endpoint exposing a curated set of
// read tools to AI clients (Claude Code, Cursor, any MCP client).
//
// The protocol plumbing — the JSON-RPC handler, the token store, the Tool
// struct and dispatch — is a PORT of teploy-dash's internal/mcp, which was
// already written and tested. Two things change on the port: the token prefix
// is `tpo_`, and the Backend is Observe's own services rather than Dash's CLI
// delegate. A third follows from the product: Dash stores its tokens in a JSON
// file beside auth.json, and Observe has no local data directory — everything
// it owns lives in Nucleus, so the store is a table.
//
// Dash's invariant carries over verbatim: TOOLS NEVER GROW THEIR OWN STATE OR
// BYPASS THE EXISTING SERVICE LAYER. Every tool wraps a method that already
// exists and is already tested. The only state owned here is the token table —
// auth material, not product data.
//
// What is different in kind from Dash is the data boundary. Dash's worst case
// is an env var name; Observe holds personal data, so every query path goes
// through the allowlist in allowlist.go, and every call lands in the audit
// trail. See docs/mcp.md.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Roles an MCP token can hold. They are the canonical Teploy roles (see the
// RBAC contract): read -> viewer, mutation -> editor. `admin` is deliberately
// not mintable — MCP has no configuration or credential surface, and a token
// that cannot be granted admin cannot be tricked into using it.
//
// The role is stored as a role NAME rather than a capability bit precisely so
// it maps 1:1 onto an OIDC `teploy_role` claim if MCP ever federates.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
)

// normalizeRole returns a mintable role, defaulting to viewer — least
// privilege — for anything unrecognized.
func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleEditor:
		return RoleEditor
	default:
		return RoleViewer
	}
}

// Token is one MCP access token. Only the SHA-256 of the secret is stored; the
// plaintext is shown once at creation and is not recoverable afterwards.
type Token struct {
	ID         string `json:"id" db:"token_id"`
	Name       string `json:"name" db:"name"`
	Hash       string `json:"-" db:"hash"` // hex sha256; never serialized to a client
	Role       string `json:"role" db:"role"`
	CreatedAt  int64  `json:"created_at" db:"created_at"`
	LastUsedAt int64  `json:"last_used_at" db:"last_used_at"`
	RevokedAt  int64  `json:"revoked_at" db:"revoked_at"`
}

// ReadOnly reports whether the token may only call read-only tools. Read-only
// is about MUTATION, not sensitivity: no token of any role reaches personal
// data, because that boundary is enforced separately by allowlist.go.
func (t Token) ReadOnly() bool { return t.Role != RoleEditor }

// Revoked reports whether the token has been revoked.
func (t Token) Revoked() bool { return t.RevokedAt > 0 }

// TokenPrefix distinguishes an Observe MCP token from Dash's `tpd_`.
const TokenPrefix = "tpo_"

const tokenColumns = "token_id, tenant_id, name, hash, role, created_at, last_used_at, revoked_at, updated_at"

// lastUsedInterval throttles the last-used write. `mcp_tokens` is an
// append-only MergeTree, so persisting last-used on every verify would add a
// row per tool call; an agent's usage is legible at five-minute resolution and
// the audit trail carries the per-call record anyway.
const lastUsedInterval = 5 * time.Minute

// TokenStore persists MCP tokens in Nucleus.
//
// `mcp_tokens` is a plain MergeTree with no version column, so every write is
// an append and reads collapse to the newest row per token_id with argMax over
// updated_at — the same shape internal/incidents uses, and for the same reason:
// a ReplacingMergeTree's collapse is not something this codebase can rely on.
type TokenStore struct {
	db *nucleus.Client
}

// NewTokenStore wires the token store to the shared Nucleus client.
func NewTokenStore(db *nucleus.Client) *TokenStore { return &TokenStore{db: db} }

// Create mints a new token and returns its plaintext — shown once, never
// again — alongside the stored record.
func (s *TokenStore) Create(ctx context.Context, name, role string) (string, Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, fmt.Errorf("token name is required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, err
	}
	plaintext := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return "", Token{}, err
	}
	now := time.Now().UTC().UnixMilli()
	t := Token{
		ID:        hex.EncodeToString(id),
		Name:      name,
		Hash:      hashToken(plaintext),
		Role:      normalizeRole(role),
		CreatedAt: now,
	}
	if err := s.write(ctx, t, now); err != nil {
		return "", Token{}, err
	}
	return plaintext, t, nil
}

// List returns the token records, oldest first. It never returns the hash (the
// `db` tag loads it, the `json:"-"` tag keeps it off the wire) and there is no
// path that returns the plaintext — RBAC rule 3: secret values never cross a
// read boundary.
func (s *TokenStore) List(ctx context.Context) ([]Token, error) {
	rows, err := nucleus.Query[Token](ctx, s.db.SQL(), latestTokens(""))
	if err != nil {
		return nil, fmt.Errorf("mcp: listing tokens: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt < rows[j].CreatedAt
		}
		return rows[i].ID < rows[j].ID
	})
	for i := range rows {
		rows[i].Hash = ""
	}
	return rows, nil
}

// Revoke marks a token revoked. The row stays: a revoked token that vanished
// would take its audit-trail identity with it, and the trail has to be able to
// name what acted.
func (s *TokenStore) Revoke(ctx context.Context, id string) error {
	rows, err := nucleus.Query[Token](ctx, s.db.SQL(), latestTokens("token_id = $1"), id)
	if err != nil {
		return fmt.Errorf("mcp: revoking token: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("token not found")
	}
	t := rows[0]
	now := time.Now().UTC().UnixMilli()
	t.RevokedAt = now
	return s.write(ctx, t, now)
}

// Verify checks a presented plaintext token and reports the matching record.
//
// It fails closed: a store read error, a revoked row, or no match all return
// (Token{}, false). Brute force is not a practical concern — 256-bit secrets
// compared in constant time — so there is no lockout.
func (s *TokenStore) Verify(ctx context.Context, plaintext string) (Token, bool) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return Token{}, false
	}
	rows, err := nucleus.Query[Token](ctx, s.db.SQL(), latestTokens(""))
	if err != nil {
		return Token{}, false
	}
	tok, ok := match(rows, plaintext)
	if !ok {
		return Token{}, false
	}
	now := time.Now().UTC().UnixMilli()
	if now-tok.LastUsedAt >= lastUsedInterval.Milliseconds() {
		used := tok
		used.LastUsedAt = now
		if err := s.write(ctx, used, now); err == nil {
			tok.LastUsedAt = now
		}
	}
	tok.Hash = ""
	return tok, true
}

// match is the pure selection step of Verify: constant-time hash comparison
// against every stored token, skipping revoked rows. Split out so the refusal
// of a revoked or unknown token is testable without a database.
func match(rows []Token, plaintext string) (Token, bool) {
	want := []byte(hashToken(plaintext))
	for _, t := range rows {
		if subtle.ConstantTimeCompare([]byte(t.Hash), want) != 1 {
			continue
		}
		if t.Revoked() {
			return Token{}, false
		}
		return t, true
	}
	return Token{}, false
}

func (s *TokenStore) write(ctx context.Context, t Token, updatedAt int64) error {
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO mcp_tokens (`+tokenColumns+`)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.Name, t.Hash, t.Role,
		dbutil.IntParam(t.CreatedAt), dbutil.IntParam(t.LastUsedAt),
		dbutil.IntParam(t.RevokedAt), dbutil.IntParam(updatedAt))
	if err != nil {
		return fmt.Errorf("mcp: writing token: %w", err)
	}
	return nil
}

// latestTokens collapses mcp_tokens to one row per token_id, keeping the
// highest-updated_at version of every column. See internal/incidents for the
// same shape and the reasoning: FINAL parses but is silently ignored by
// Nucleus, so an explicit argMax is the only form that collapses.
//
// The ORDER BY a caller wraps this in must name the OUTPUT aliases — Nucleus
// resolves ORDER BY against the select list's output names only — which is why
// List sorts in Go instead.
func latestTokens(whereFrag string) string {
	if strings.TrimSpace(whereFrag) == "" {
		whereFrag = "1 = 1"
	}
	return `SELECT token_id,
	               argMax(name, updated_at)         AS name,
	               argMax(hash, updated_at)         AS hash,
	               argMax(role, updated_at)         AS role,
	               argMax(created_at, updated_at)   AS created_at,
	               argMax(last_used_at, updated_at) AS last_used_at,
	               argMax(revoked_at, updated_at)   AS revoked_at
	        FROM mcp_tokens WHERE ` + whereFrag + `
	        GROUP BY token_id`
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
