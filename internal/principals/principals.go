// Package principals owns the principal store that backs every identity that
// can authenticate to Observe: local accounts (the bootstrap admin and managed
// users, previously split unsynchronized across admin_users and users) and
// issuer-namespaced OIDC identities (previously minted as "oidc:<sub>" with no
// row at all). Audit F03/F05; schema in migration 040.
//
// principals is a ReplacingMergeTree keyed on (id). Nucleus does not reliably
// collapse superseded versions and a table absent from engines.json reads as a
// plain MergeTree after a restart, so EVERY read goes through the argMax
// collapse in internal/query (the derived table exposes the collapsed version
// column as well, so writers can stamp monotonically), and every write is
// DELETE + INSERT in one transaction with version = max(now_ms, prior+1) -
// the 039 stamp, so a version never regresses and a stale duplicate can never
// win the collapse.
//
// Mutations are serialized by a process-wide mutex, the same
// single-instance-per-database discipline AuthService.credentialMu documents
// (AUD-005): each writer derives the next version from the prior row, so two
// concurrent writers could otherwise both stamp the same version and the
// collapse would keep an arbitrary one.
package principals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/query"
)

// Principal kinds.
const (
	KindLocal = "local"
	KindOIDC  = "oidc"
)

// Principal origins - provenance for the one-time 040 merge. Only rows with
// origin 'admin' (from admin_users) were login-capable before unification, so
// LocalByUsername tie-breaks on it: if both legacy tables held the same
// username, the account with a working password must win or the owner locks
// out. Everything created after the migration is 'created' (local) or 'oidc'.
const (
	OriginAdmin   = "admin"
	OriginManaged = "managed"
	OriginCreated = "created"
	OriginOIDC    = "oidc"
)

// Principal is one row of the principals table, collapsed to its latest
// version. JSON tags shape the user-management API DTO.
type Principal struct {
	ID            string `db:"id" json:"user_id"`
	Kind          string `db:"kind" json:"kind"`
	Username      string `db:"username" json:"username"`
	Email         string `db:"email" json:"email"`
	PasswordHash  string `db:"password_hash" json:"-"`
	Role          string `db:"role" json:"role"`
	Origin        string `db:"origin" json:"-"`
	CreatedAt     string `db:"created_at" json:"-"`
	InvitedBy     string `db:"invited_by" json:"-"`
	TokenVersion  int64  `db:"token_version" json:"-"`
	LatestVersion int64  `db:"latest_version" json:"-"`
}

// CreatedAtMs returns created_at as epoch milliseconds (0 when unparsable).
func (p *Principal) CreatedAtMs() int64 {
	ms, _ := strconv.ParseInt(p.CreatedAt, 10, 64)
	return ms
}

// principalCols are the collapsed columns every read selects. "version" rides
// along (argMax(version, version)) so writers can stamp the next one; it maps
// to LatestVersion, not a domain field.
var principalCols = []string{
	"kind", "username", "email", "password_hash", "role", "origin",
	"created_at", "invited_by", "token_version", "version",
}

// principalSelect is the standard read: the collapsed table under the alias
// `principals`, with the collapsed version exposed as latest_version.
// Filters that name rewritten columns belong outside the derived table, on
// the alias in the caller's WHERE.
const principalSelect = "SELECT id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version AS latest_version FROM "

// principalsLatest renders the collapsed principals table.
func principalsLatest(where string) string {
	return query.LatestRows("principals", principalCols, where) + " AS principals"
}

// Store is the principal store. Safe for concurrent use.
type Store struct {
	db *nucleus.Client
	mu sync.Mutex
}

// NewStore returns a Store backed by db.
func NewStore(db *nucleus.Client) *Store {
	return &Store{db: db}
}

// ByID returns the latest version of one principal, or (nil, nil) when no
// such principal exists (TO-004: absence and store failure are different
// answers — callers used to read both as "missing").
func (s *Store) ByID(ctx context.Context, id string) (*Principal, error) {
	return byID(ctx, s.db, id)
}

func byID(ctx context.Context, db *nucleus.Client, id string) (*Principal, error) {
	rows, err := nucleus.Query[Principal](ctx, db.SQL(),
		principalSelect+principalsLatest("id = $1")+" LIMIT 1", id)
	if err != nil {
		return nil, fmt.Errorf("principals: by-id lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// TokenVersionByID returns the live token_version for any principal. This is
// the middleware revocation check: a token whose embedded "tv" differs is
// rejected. A missing row is an error, which callers treat as "session
// invalid" - that is what retires pre-040 "oidc:<sub>" tokens (no row exists
// under that id) and any token for a principal that no longer exists.
func (s *Store) TokenVersionByID(ctx context.Context, id string) (int64, error) {
	row, err := nucleus.QueryOne[struct {
		TokenVersion int64 `db:"token_version"`
	}](ctx, s.db.SQL(), "SELECT token_version FROM "+principalsLatest("id = $1"), id)
	if err != nil {
		return 0, err
	}
	return row.TokenVersion, nil
}

// LocalByUsername resolves a username to the one local principal password
// login may authenticate. Deterministic where the 040 backfill merged the
// same username from both legacy tables: the 'admin'-origin row wins (it held
// a working password pre-merge), then oldest created_at, then lowest id.
// (nil, nil) means the username is genuinely free; an error is a store
// failure the caller must not treat as absence (TO-004).
func (s *Store) LocalByUsername(ctx context.Context, username string) (*Principal, error) {
	return s.localByUsernameLocked(ctx, username)
}

// CountLocal returns how many local principals exist. "Is this install
// claimed" - the first-run grace gate reads it.
func (s *Store) CountLocal(ctx context.Context) (int64, error) {
	rows, err := nucleus.Query[struct {
		Count int64 `db:"count"`
	}](ctx, s.db.SQL(), "SELECT COUNT(*) AS count FROM "+principalsLatest("")+" WHERE kind = 'local'")
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Count, nil
}

// List returns every principal, local and OIDC, oldest first, in a total
// order (id tiebreak) so a later LIMIT cannot reshuffle ties.
func (s *Store) List(ctx context.Context) ([]Principal, error) {
	return nucleus.Query[Principal](ctx, s.db.SQL(),
		principalSelect+principalsLatest("")+" ORDER BY created_at ASC, id ASC")
}

// FirstLocalAdmin returns the first local admin by created_at, id - the
// account OBSERVE_RESET_ADMIN_PASSWORD resets. (nil, nil) when no local
// admin exists.
func (s *Store) FirstLocalAdmin(ctx context.Context) (*Principal, error) {
	rows, err := nucleus.Query[Principal](ctx, s.db.SQL(), principalSelect+principalsLatest("")+
		" WHERE kind = 'local' AND role = 'admin' ORDER BY created_at ASC, id ASC LIMIT 1")
	if err != nil {
		return nil, fmt.Errorf("principals: first-admin lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ErrUsernameTaken is returned by CreateLocal when a local principal already
// holds the username. With every local account login-capable since 040, a
// duplicate username would leave one of the two passwords silently dead.
var ErrUsernameTaken = fmt.Errorf("username already exists")

// CreateLocal inserts a new local principal (token_version 0). It refuses a
// username already held by any local principal. origin should be OriginAdmin
// (bootstrap) or OriginCreated (user management).
func (s *Store) CreateLocal(ctx context.Context, username, email, passwordHash, role, invitedBy, origin string) (*Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TO-004: a failed lookup is NOT a free username — proceeding would
	// let a transient SELECT error green-light a duplicate of an existing
	// account (one of the two passwords silently dead since 040).
	existing, err := s.localByUsernameLocked(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("principals: username lookup: %w", err)
	}
	if existing != nil {
		return nil, ErrUsernameTaken
	}
	nowMs := time.Now().UnixMilli()
	p := &Principal{
		ID:            newID(),
		Kind:          KindLocal,
		Username:      username,
		Email:         email,
		PasswordHash:  passwordHash,
		Role:          role,
		Origin:        origin,
		CreatedAt:     dbutil.IntParam(nowMs),
		InvitedBy:     invitedBy,
		LatestVersion: nowMs,
	}
	if _, err := s.db.SQL().Exec(ctx, `INSERT INTO principals
		(id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, $10)`,
		p.ID, p.Kind, p.Username, p.Email, p.PasswordHash, p.Role, p.Origin, p.CreatedAt, p.InvitedBy, nowMs); err != nil {
		return nil, fmt.Errorf("principals: create local: %w", err)
	}
	return p, nil
}

// SetRole changes a principal's role and bumps token_version, so every JWT
// issued under the old role stops authenticating on its next use (audit F03:
// role changes must revoke tokens). R08 (round 4): demoting the LAST local
// administrator is refused inside the serialized mutation — without a local
// admin, setup stays closed (local principals exist) while the ordinary
// password-reset recovery path has no account to reset. OIDC-managed roles
// (UpsertOIDC) are deliberately not guarded here: the IdP is authoritative
// for them, and SSO-only installs document break-glass via
// OBSERVE_ADMIN_USER provisioning instead. Returns the updated principal.
func (s *Store) SetRole(ctx context.Context, id, role string) (*Principal, error) {
	return s.replace(ctx, id, func(p *Principal) error {
		if p.Kind == KindLocal && p.Role == "admin" && role != "admin" {
			rows, err := nucleus.Query[struct {
				N int64 `db:"n"`
			}](ctx, s.db.SQL(),
				"SELECT COUNT(*) AS n FROM "+principalsLatest("")+
					" WHERE kind = 'local' AND role = 'admin' AND id != $1", id)
			if err != nil {
				// Fail closed: an unreadable principal store must not permit
				// removing the last break-glass admin.
				return fmt.Errorf("principals: check remaining local admins: %w", err)
			}
			if len(rows) != 1 || rows[0].N == 0 {
				return fmt.Errorf("principals: cannot demote the last local administrator")
			}
		}
		p.Role = role
		p.TokenVersion++
		return nil
	})
}

// ReplacePassword swaps the password hash and bumps token_version (OBS-011
// revocation, now on the unified store). expectHash is the row hash the
// caller verified the current password against; if the row moved underneath
// (a concurrent change won the race) the write is refused rather than
// silently discarding the other change. Pass "" to skip the check (the
// administrative reset path, which verifies nothing).
func (s *Store) ReplacePassword(ctx context.Context, id, expectHash, newHash string) (*Principal, error) {
	return s.replace(ctx, id, func(p *Principal) error {
		if expectHash != "" && p.PasswordHash != expectHash {
			return fmt.Errorf("principals: password changed concurrently - retry")
		}
		p.PasswordHash = newHash
		p.TokenVersion++
		return nil
	})
}

// RevokeSessions bumps token_version without touching any credential, killing
// every outstanding JWT for the principal (audit F05's admin operation - the
// only way to proactively retire a still-valid SSO session before expiry).
func (s *Store) RevokeSessions(ctx context.Context, id string) error {
	_, err := s.replace(ctx, id, func(p *Principal) error {
		p.TokenVersion++
		return nil
	})
	return err
}

// UpsertOIDC records an SSO sign-in: creates the issuer-namespaced principal
// on first login, refreshes username/email/role from the IdP on later ones
// (the IdP stays authoritative for role), and PRESERVES token_version on an
// unchanged role so a later sign-in does not retire other still-valid
// sessions for the same identity. A role CHANGE bumps token_version
// (TO-003): an admin JWT minted before the IdP downgraded the identity to
// viewer must die at its next use, exactly like SetRole. Returns the
// token_version a JWT minted now must embed.
func (s *Store) UpsertOIDC(ctx context.Context, id, username, email, role string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nowMs := time.Now().UnixMilli()
	existing, err := byID(ctx, s.db, id)
	if err != nil {
		// TO-004: a lookup failure must not fall through to the
		// create-with-token-version-zero branch — that silently reset
		// revocation state (a suspended/downgraded identity re-logged-in
		// by a transient SELECT error).
		return 0, fmt.Errorf("principals: OIDC lookup: %w", err)
	}
	if existing != nil {
		nextTokenVersion := existing.TokenVersion
		if existing.Role != role {
			nextTokenVersion++
		}
		next := nextVersion(nowMs, existing.LatestVersion)
		if _, err := s.db.SQL().Exec(ctx, `INSERT INTO principals
			(id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			id, KindOIDC, username, email, existing.PasswordHash, role, OriginOIDC,
			existing.CreatedAt, existing.InvitedBy, nextTokenVersion, next); err != nil {
			return 0, fmt.Errorf("principals: upsert oidc: %w", err)
		}
		return nextTokenVersion, nil
	}
	if _, err := s.db.SQL().Exec(ctx, `INSERT INTO principals
		(id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version)
		VALUES ($1, $2, $3, $4, '', $5, $6, $7, '', 0, $8)`,
		id, KindOIDC, username, email, role, OriginOIDC, dbutil.IntParam(nowMs), nowMs); err != nil {
		return 0, fmt.Errorf("principals: create oidc: %w", err)
	}
	return 0, nil
}

// replace is the shared read-modify-write: read the latest row under the
// lock, apply mutate, DELETE by id (an ORDER BY key, so the delete actually
// lands - a Nucleus DML filtered outside the key silently no-ops), INSERT the
// new version in one transaction, version = max(now_ms, prior+1).
func (s *Store) replace(ctx context.Context, id string, mutate func(*Principal) error) (*Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := byID(ctx, s.db, id)
	if err != nil {
		return nil, fmt.Errorf("principals: load %s: %w", id, err)
	}
	if p == nil {
		return nil, fmt.Errorf("principals: %s not found", id)
	}
	if err := mutate(p); err != nil {
		return nil, err
	}
	next := nextVersion(time.Now().UnixMilli(), p.LatestVersion)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("principals: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.SQL().Exec(ctx, "DELETE FROM principals WHERE id = $1", id); err != nil {
		return nil, fmt.Errorf("principals: delete: %w", err)
	}
	if _, err := tx.SQL().Exec(ctx, `INSERT INTO principals
		(id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		p.ID, p.Kind, p.Username, p.Email, p.PasswordHash, p.Role, p.Origin,
		p.CreatedAt, p.InvitedBy, p.TokenVersion, next); err != nil {
		return nil, fmt.Errorf("principals: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("principals: commit: %w", err)
	}
	p.LatestVersion = next
	return p, nil
}

func (s *Store) localByUsernameLocked(ctx context.Context, username string) (*Principal, error) {
	rows, err := nucleus.Query[Principal](ctx, s.db.SQL(), principalSelect+principalsLatest("")+
		` WHERE username = $1 AND kind = 'local'
		  ORDER BY CASE origin WHEN 'admin' THEN 0 ELSE 1 END ASC, created_at ASC, id ASC
		  LIMIT 1`, username)
	if err != nil {
		return nil, fmt.Errorf("principals: username lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// nextVersion is the 039 stamp: strictly increasing even when the wall clock
// went backwards or two writes landed in the same millisecond.
func nextVersion(nowMs, prior int64) int64 {
	if prior+1 > nowMs {
		return prior + 1
	}
	return nowMs
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
