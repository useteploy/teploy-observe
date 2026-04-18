package share

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// ShareService manages share link tokens for public dashboard access.
type ShareService struct {
	db *nucleus.Client
}

// NewShareService creates a new ShareService.
func NewShareService(db *nucleus.Client) *ShareService {
	// Ensure table exists (migration may have been applied before this table was added)
	ctx := context.Background()
	db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS share_links (
		token TEXT NOT NULL, site_id TEXT NOT NULL, created_at BIGINT NOT NULL)`)
	return &ShareService{db: db}
}

// ShareLink represents a public share token for a site dashboard.
type ShareLink struct {
	Token     string `json:"token" db:"token"`
	SiteID    string `json:"site_id" db:"site_id"`
	CreatedAt int64  `json:"created_at" db:"created_at"`
}

// Create generates a new share link for the given site.
func (s *ShareService) Create(ctx context.Context, siteID string) (ShareLink, error) {
	sql := s.db.SQL()
	token := generateToken()
	now := time.Now().UnixMilli()
	nowStr := dbutil.IntParam(now)

	_, err := sql.Exec(ctx,
		"INSERT INTO share_links (token, site_id, created_at) VALUES ($1, $2, $3)",
		token, siteID, nowStr,
	)
	if err != nil {
		return ShareLink{}, fmt.Errorf("share: create: %w", err)
	}

	return ShareLink{
		Token:     token,
		SiteID:    siteID,
		CreatedAt: now,
	}, nil
}

// Resolve looks up a share token and returns the associated site_id.
func (s *ShareService) Resolve(ctx context.Context, token string) (string, error) {
	sql := s.db.SQL()
	link, err := nucleus.QueryOne[ShareLink](ctx, sql,
		"SELECT token, site_id, created_at FROM share_links WHERE token = $1",
		token,
	)
	if err != nil {
		return "", fmt.Errorf("share: invalid token")
	}
	return link.SiteID, nil
}

// Revoke deletes a share link by token.
func (s *ShareService) Revoke(ctx context.Context, token string) error {
	sql := s.db.SQL()
	_, err := sql.Exec(ctx,
		"DELETE FROM share_links WHERE token = $1",
		token,
	)
	if err != nil {
		return fmt.Errorf("share: revoke: %w", err)
	}
	return nil
}

// List returns all share links for a site.
func (s *ShareService) List(ctx context.Context, siteID string) ([]ShareLink, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[ShareLink](ctx, sql,
		"SELECT token, site_id, created_at FROM share_links WHERE site_id = $1 ORDER BY created_at DESC",
		siteID,
	)
	if err != nil {
		return nil, fmt.Errorf("share: list: %w", err)
	}
	if rows == nil {
		rows = []ShareLink{}
	}
	return rows, nil
}

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
