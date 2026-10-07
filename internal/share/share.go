package share

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// defaultShareLinkTTL bounds how long a share link works before it must be
// recreated. OBS-023: links previously never expired, so a leaked URL (or a
// token copied into logs/tickets/chat) remained a durable, unrevocable
// credential forever.
const defaultShareLinkTTL = 30 * 24 * time.Hour

// ShareService manages share link tokens for public dashboard access.
type ShareService struct {
	db *nucleus.Client
}

// NewShareService creates a new ShareService.
func NewShareService(db *nucleus.Client) *ShareService {
	// Ensure table exists (migration may have been applied before this table was added)
	ctx := context.Background()
	db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS share_links (
		token TEXT NOT NULL, site_id TEXT NOT NULL, created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL DEFAULT 0, revoked_at BIGINT NOT NULL DEFAULT 0,
		last_used_at BIGINT NOT NULL DEFAULT 0)`)
	return &ShareService{db: db}
}

// row is the on-disk shape. Kept separate from the API-facing ShareLink so
// List() can return a masked token under the same field name Create() uses
// for the one-time raw reveal, without either shape lying about its columns.
type row struct {
	Token      string `db:"token"`
	SiteID     string `db:"site_id"`
	CreatedAt  int64  `db:"created_at"`
	ExpiresAt  int64  `db:"expires_at"`
	RevokedAt  int64  `db:"revoked_at"`
	LastUsedAt int64  `db:"last_used_at"`
}

// ShareLink represents a public share token for a site dashboard.
//
// Token's content depends on how this value was produced: Create returns the
// raw bearer token (the one and only time it is ever revealed — copy it now).
// List returns a masked value (e.g. "a1b2c3d4…") so a page that merely lists
// existing links never discloses a reusable credential. ID is a stable,
// non-secret identifier derived from the token (safe to log, display, or use
// to correlate a list row with an action) — it is NOT a database column.
type ShareLink struct {
	ID         string `json:"id"`
	Token      string `json:"token"`
	SiteID     string `json:"site_id"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Status     string `json:"status"` // active | expired | revoked
}

// Create generates a new share link for the given site with the default
// 30-day expiry. See CreateWithTTL for a custom duration.
func (s *ShareService) Create(ctx context.Context, siteID string) (ShareLink, error) {
	return s.CreateWithTTL(ctx, siteID, defaultShareLinkTTL)
}

// CreateWithTTL generates a new share link that expires after ttl. The site
// share creation route uses this when the caller supplies ttl_days.
func (s *ShareService) CreateWithTTL(ctx context.Context, siteID string, ttl time.Duration) (ShareLink, error) {
	sql := s.db.SQL()
	token, err := generateToken()
	if err != nil {
		return ShareLink{}, fmt.Errorf("share: generate token: %w", err)
	}
	now := time.Now().UnixMilli()
	expiresAt := now + ttl.Milliseconds()

	_, err = sql.Exec(ctx,
		"INSERT INTO share_links (token, site_id, created_at, expires_at, revoked_at, last_used_at) VALUES ($1, $2, $3, $4, 0, 0)",
		token, siteID, dbutil.IntParam(now), dbutil.IntParam(expiresAt),
	)
	if err != nil {
		return ShareLink{}, fmt.Errorf("share: create: %w", err)
	}

	return ShareLink{
		ID:        shareLinkID(token),
		Token:     token, // raw — this is the one-time reveal at creation
		SiteID:    siteID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		Status:    "active",
	}, nil
}

// Resolve looks up a share token and returns the associated site_id. Rejects
// an expired or revoked token exactly like an unknown one — the caller
// cannot distinguish "never existed" from "existed but is no longer valid",
// which matters so an attacker probing tokens learns nothing extra.
func (s *ShareService) Resolve(ctx context.Context, token string) (string, error) {
	sql := s.db.SQL()
	r, err := nucleus.QueryOne[row](ctx, sql,
		"SELECT token, site_id, created_at, expires_at, revoked_at, last_used_at FROM share_links WHERE token = $1",
		token,
	)
	if err != nil {
		return "", fmt.Errorf("share: invalid token")
	}
	now := time.Now().UnixMilli()
	if r.RevokedAt != 0 {
		return "", fmt.Errorf("share: invalid token")
	}
	if r.ExpiresAt != 0 && now >= r.ExpiresAt {
		return "", fmt.Errorf("share: invalid token")
	}

	// Best-effort last-used stamp — never block or fail a read over it.
	_, _ = sql.Exec(ctx, "UPDATE share_links SET last_used_at = $1 WHERE token = $2", dbutil.IntParam(now), token)

	return r.SiteID, nil
}

// ErrNotFound means no share link matches the supplied identifier. Unknown
// and masked tokens must never produce a successful revocation response.
var ErrNotFound = errors.New("share: link not found")

// ErrAmbiguousID refuses to choose between colliding legacy short IDs.
var ErrAmbiguousID = errors.New("share: ambiguous link ID")

// ErrRevokeUnconfirmed means storage has not confirmed revocation. Returning
// success here would falsely promise that a disclosed capability is disabled.
var ErrRevokeUnconfirmed = errors.New("share: revocation could not be confirmed")

// Revoke retains the legacy raw-token API for existing clients. Already
// revoked links are idempotent successes; unknown tokens now return ErrNotFound.
// New management clients should use RevokeByID instead of handling a secret.
func (s *ShareService) Revoke(ctx context.Context, token string) error {
	_, err := s.revokeToken(ctx, token)
	return err
}

// RevokeByID revokes by the stable, non-secret ID returned by Create and List.
// The caller must authorize management access; an ID is not a capability.
// Site scoping and collision refusal preserve existing IDs without a migration
// or depending on a database-specific hash function.
func (s *ShareService) RevokeByID(ctx context.Context, siteID, id string) (ShareLink, error) {
	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		"SELECT token, site_id, created_at, expires_at, revoked_at, last_used_at FROM share_links WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return ShareLink{}, fmt.Errorf("share: find link: %w", err)
	}
	token, err := tokenForID(rows, id)
	if err != nil {
		return ShareLink{}, err
	}
	r, err := s.revokeToken(ctx, token)
	if err != nil {
		return ShareLink{}, err
	}
	return listedLink(r, time.Now().UnixMilli()), nil
}

func tokenForID(rows []row, id string) (string, error) {
	var token string
	found := false
	for _, r := range rows {
		if shareLinkID(r.Token) != id {
			continue
		}
		if found {
			return "", ErrAmbiguousID
		}
		token, found = r.Token, true
	}
	if !found {
		return "", ErrNotFound
	}
	return token, nil
}

func (s *ShareService) revokeToken(ctx context.Context, token string) (row, error) {
	sql := s.db.SQL()
	_, err := sql.Exec(ctx,
		"UPDATE share_links SET revoked_at = $1 WHERE token = $2 AND revoked_at = 0",
		dbutil.IntParam(time.Now().UnixMilli()), token,
	)
	if err != nil {
		return row{}, fmt.Errorf("share: revoke: %w", err)
	}
	// A successful Exec is not evidence that any row matched. Read back the
	// same state Resolve uses, including for idempotent retries; do not hide
	// a zero-row UPDATE or a write that is not yet visible behind a 2xx.
	rows, err := nucleus.Query[row](ctx, sql,
		"SELECT token, site_id, created_at, expires_at, revoked_at, last_used_at FROM share_links WHERE token = $1",
		token,
	)
	if err != nil {
		return row{}, fmt.Errorf("share: confirm revocation: %w", err)
	}
	return confirmedRevocation(rows)
}

func confirmedRevocation(rows []row) (row, error) {
	if len(rows) == 0 {
		return row{}, ErrNotFound
	}
	for _, r := range rows {
		if r.RevokedAt == 0 {
			return row{}, ErrRevokeUnconfirmed
		}
	}
	return rows[0], nil
}

// List returns all share links for a site, newest first. Token is masked —
// List is a read surface for humans auditing what links exist, not a way to
// retrieve a working credential a second time.
func (s *ShareService) List(ctx context.Context, siteID string) ([]ShareLink, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[row](ctx, sql,
		"SELECT token, site_id, created_at, expires_at, revoked_at, last_used_at FROM share_links WHERE site_id = $1 ORDER BY created_at DESC",
		siteID,
	)
	if err != nil {
		return nil, fmt.Errorf("share: list: %w", err)
	}
	now := time.Now().UnixMilli()
	out := make([]ShareLink, 0, len(rows))
	for _, r := range rows {
		out = append(out, listedLink(r, now))
	}
	return out, nil
}

func listedLink(r row, now int64) ShareLink {
	status := "active"
	switch {
	case r.RevokedAt != 0:
		status = "revoked"
	case r.ExpiresAt != 0 && now >= r.ExpiresAt:
		status = "expired"
	}
	return ShareLink{
		ID:         shareLinkID(r.Token),
		Token:      maskToken(r.Token),
		SiteID:     r.SiteID,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		RevokedAt:  r.RevokedAt,
		LastUsedAt: r.LastUsedAt,
		Status:     status,
	}
}

// maskToken keeps enough of a raw token visible for a human to visually
// distinguish list rows without exposing anything usable as a credential.
func maskToken(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:8] + "…"
}

// shareLinkID derives a stable, non-secret identifier from a token — safe to
// log, display, or use to correlate UI actions with a specific link. Not
// reversible into the token (SHA-256, truncated), and not itself usable to
// resolve the link. RevokeByID requires separately authorized management access.
// Keep the existing ID stable; tokenForID fails closed on short-ID collisions.
func shareLinkID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
