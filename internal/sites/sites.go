package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
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
