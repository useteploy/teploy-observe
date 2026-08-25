package views

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type ViewService struct {
	db *nucleus.Client
}

func NewViewService(db *nucleus.Client) *ViewService {
	return &ViewService{db: db}
}

type SavedView struct {
	ViewID     string `json:"view_id" db:"view_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	Name       string `json:"name" db:"name"`
	ViewConfig string `json:"view_config" db:"view_config"`
	CreatedBy  string `json:"created_by" db:"created_by"`
	CreatedAt  string `json:"created_at" db:"created_at"`
	Version    string `json:"-" db:"version"`
}

func (s *ViewService) Create(ctx context.Context, siteID, name, viewConfig, createdBy string) (*SavedView, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO saved_views (view_id, tenant_id, site_id, name, view_config, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, NULLIF($4, ''), $5, $6, $7)`,
		id, siteID, name, viewConfig, createdBy, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create view: %w", err)
	}
	return &SavedView{ViewID: id, SiteID: siteID, Name: name, ViewConfig: viewConfig, CreatedBy: createdBy, CreatedAt: now}, nil
}

func (s *ViewService) List(ctx context.Context, siteID string) ([]SavedView, error) {
	// The `name != ''` filter runs OUTSIDE the collapse and covers tombstones
	// written by the old Delete (below); filtering it inside would match a
	// superseded row and resurrect a deleted view.
	return nucleus.Query[SavedView](ctx, s.db.SQL(),
		`SELECT view_id, tenant_id, site_id, name, COALESCE(view_config, '') AS view_config, created_by, created_at, version
		 FROM `+savedViewsLatest("site_id = $1")+`
		 WHERE name != ''
		 ORDER BY created_at DESC`, siteID)
}

// Delete removes a saved view outright.
//
// It used to append a blank tombstone - an empty name, a NULL view_config, a bumped
// version — and List did not filter it, so a deleted view came back as a
// nameless entry in /views and (via the funnel handler's own guard) simply
// vanished from the funnel list while its row stayed on disk forever. The
// tombstone was also the doubling shape: `INSERT INTO saved_views SELECT ...
// FROM saved_views WHERE view_id = $1` writes one row per version present.
//
// A hard DELETE is the right behaviour here and matches the two siblings that
// already settled this question — boards.DeleteBoard and query.DeleteGoal.
// Nothing references a view_id, the view carries no state worth keeping, and
// DELETE is the only form Nucleus honours immediately on a replacing table.
func (s *ViewService) Delete(ctx context.Context, viewID string) error {
	_, err := s.db.SQL().Exec(ctx,
		`DELETE FROM saved_views WHERE view_id = $1`, viewID)
	if err != nil {
		return fmt.Errorf("delete view: %w", err)
	}
	return nil
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
