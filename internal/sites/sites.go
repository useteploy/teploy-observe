package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/observe/internal/dbutil"
)

// Site represents a tracked website.
type Site struct {
	SiteID      string `json:"site_id" db:"site_id"`
	TenantID    string `json:"tenant_id" db:"tenant_id"`
	Domain      string `json:"domain" db:"domain"`
	Name        string `json:"name" db:"name"`
	CreatedAt   int64  `json:"created_at" db:"created_at"`
	SessionSalt string `json:"-" db:"session_salt"`
}

// SiteService provides CRUD operations for the sites table.
type SiteService struct {
	db *nucleus.Client
}

// NewSiteService creates a new SiteService.
func NewSiteService(db *nucleus.Client) *SiteService {
	return &SiteService{db: db}
}

// List returns all sites.
func (s *SiteService) List(ctx context.Context) ([]Site, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[Site](ctx, sql,
		"SELECT site_id, tenant_id, domain, name, created_at, session_salt FROM sites",
	)
	if err != nil {
		return nil, fmt.Errorf("sites: list: %w", err)
	}
	if rows == nil {
		rows = []Site{}
	}
	return rows, nil
}

// Create adds a new site with a random site_id and session_salt.
func (s *SiteService) Create(ctx context.Context, domain, name string) (Site, error) {
	sql := s.db.SQL()

	siteID := generateID()
	sessionSalt := generateID()
	now := time.Now().UnixMilli()
	nowStr := dbutil.IntParam(now)

	_, err := sql.Exec(ctx,
		"INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt) VALUES ($1, $2, $3, $4, $5, $6)",
		siteID, "default", domain, name, nowStr, sessionSalt,
	)
	if err != nil {
		return Site{}, fmt.Errorf("sites: create: %w", err)
	}

	return Site{
		SiteID:      siteID,
		TenantID:    "default",
		Domain:      domain,
		Name:        name,
		CreatedAt:   now,
		SessionSalt: sessionSalt,
	}, nil
}

// Get returns a single site by ID.
func (s *SiteService) Get(ctx context.Context, siteID string) (Site, error) {
	sql := s.db.SQL()
	site, err := nucleus.QueryOne[Site](ctx, sql,
		"SELECT site_id, tenant_id, domain, name, created_at, session_salt FROM sites WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return Site{}, fmt.Errorf("sites: get: %w", err)
	}
	return site, nil
}

// EnsureDefault creates a site with site_id="default" if none exists.
// Used on first boot so UI defaults that reference "default" work out of the box.
func (s *SiteService) EnsureDefault(ctx context.Context) error {
	sql := s.db.SQL()

	type countRow struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countRow](ctx, sql,
		"SELECT COUNT(*) AS count FROM sites WHERE site_id = $1",
		"default",
	)
	if err != nil {
		return fmt.Errorf("sites: ensure default: %w", err)
	}
	if len(rows) > 0 && rows[0].Count > 0 {
		return nil
	}

	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err = sql.Exec(ctx,
		"INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt) VALUES ($1, $2, $3, $4, $5, $6)",
		"default", "default", "localhost", "Default Site", now, generateID(),
	)
	if err != nil {
		return fmt.Errorf("sites: insert default: %w", err)
	}
	return nil
}

// ListRatelimits returns {site_id: ratelimit_per_second} for every site.
// Used at boot to hydrate the in-memory rate limiter cache.
func (s *SiteService) ListRatelimits(ctx context.Context) (map[string]int, error) {
	type row struct {
		SiteID string `db:"site_id"`
		Rate   int    `db:"ratelimit_per_second"`
	}
	sql := s.db.SQL()
	rows, err := nucleus.Query[row](ctx, sql,
		"SELECT site_id, ratelimit_per_second FROM sites",
	)
	if err != nil {
		return nil, fmt.Errorf("sites: list ratelimits: %w", err)
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.SiteID] = r.Rate
	}
	return out, nil
}

// SetRatelimit updates the per-site events-per-second cap. 0 means "default".
func (s *SiteService) SetRatelimit(ctx context.Context, siteID string, ratePerSecond int) error {
	_, err := s.db.SQL().Exec(ctx,
		"UPDATE sites SET ratelimit_per_second = $1 WHERE site_id = $2",
		ratePerSecond, siteID,
	)
	if err != nil {
		return fmt.Errorf("sites: set ratelimit: %w", err)
	}
	return nil
}

// Delete removes a site by ID.
func (s *SiteService) Delete(ctx context.Context, siteID string) error {
	sql := s.db.SQL()
	_, err := sql.Exec(ctx,
		"DELETE FROM sites WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return fmt.Errorf("sites: delete: %w", err)
	}
	return nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
