package platform

// O10: maintenance windows suppress NOTIFICATION DELIVERY only. Evaluation
// state keeps recording and incidents keep their lifecycle; the notifier
// checks these windows at delivery time and marks suppressed deliveries
// visibly (notification_outbox.suppressed_at + reason + the incident
// timeline) instead of silently dropping them.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// MaintenanceWindow is the domain type. SiteID "" covers every site.
type MaintenanceWindow struct {
	WindowID  string `json:"window_id"`
	SiteID    string `json:"site_id"`
	StartsAt  int64  `json:"starts_at"`
	EndsAt    int64  `json:"ends_at"`
	Reason    string `json:"reason"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

type MaintenanceService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

func NewMaintenanceService(db *nucleus.Client, logger *slog.Logger) *MaintenanceService {
	if logger == nil {
		logger = slog.Default()
	}
	return &MaintenanceService{db: db, logger: logger}
}

// maintenanceCollapseSelect collapses maintenance_windows to one row per
// window id (the argMax form; tombstone deletes win by version).
func maintenanceCollapseSelect(where string) string {
	if where == "" {
		where = "1 = 1"
	}
	return `SELECT tenant_id, window_id,
			argMax(site_id, version) AS site_id,
			argMax(starts_at, version) AS starts_at,
			argMax(ends_at, version) AS ends_at,
			argMax(reason, version) AS reason,
			argMax(created_by, version) AS created_by,
			argMax(created_at, version) AS created_at,
			argMax(enabled, version) AS enabled,
			MAX(version) AS version
		FROM maintenance_windows WHERE ` + where + `
		GROUP BY tenant_id, window_id`
}

// Create persists a window. ends must be after starts.
func (s *MaintenanceService) Create(ctx context.Context, siteID string, startsAt, endsAt int64, reason, createdBy string) (*MaintenanceWindow, error) {
	if endsAt <= startsAt {
		return nil, fmt.Errorf("maintenance window must end after it starts")
	}
	id := genID()
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO maintenance_windows (tenant_id, window_id, site_id, starts_at, ends_at, reason, created_by, created_at, enabled, version)
		 VALUES ('default', $1, $2, $3, $4, $5, $6, $7, 'true', $7)`,
		id, siteID, dbutil.IntParam(startsAt), dbutil.IntParam(endsAt), reason, createdBy, dbutil.IntParam(now),
	)
	if err != nil {
		return nil, fmt.Errorf("create maintenance window: %w", err)
	}
	return &MaintenanceWindow{
		WindowID: id, SiteID: siteID, StartsAt: startsAt, EndsAt: endsAt,
		Reason: reason, CreatedBy: createdBy, CreatedAt: now,
	}, nil
}

// List returns all live windows, newest first.
func (s *MaintenanceService) List(ctx context.Context) ([]MaintenanceWindow, error) {
	return nucleus.Query[MaintenanceWindow](ctx, s.db.SQL(),
		`SELECT window_id, site_id, starts_at, ends_at, reason, created_by, created_at
		 FROM (`+maintenanceCollapseSelect("")+`)
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC, window_id ASC`)
}

// Delete tombstones a window (the webhooks delete pattern: strictly-
// monotonic version so a same-ms create+delete cannot tie).
func (s *MaintenanceService) Delete(ctx context.Context, windowID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO maintenance_windows (tenant_id, window_id, site_id, starts_at, ends_at, reason, created_by, created_at, enabled, version)
		 SELECT tenant_id, window_id, site_id, starts_at, ends_at, reason, created_by, created_at, 'false',
		        GREATEST(CAST($2 AS BIGINT), version + 1)
		 FROM (`+maintenanceCollapseSelect("window_id = $1")+")",
		windowID, now,
	)
	return err
}

// Active returns the windows covering at (enabled, started, not ended).
// The interval comparison runs in Go because Nucleus reports BIGINT columns
// as text over the wire, which defeats a BIGINT range comparison in SQL
// (the same reason incidents.InRange filters in Go).
func (s *MaintenanceService) Active(ctx context.Context, at time.Time) ([]MaintenanceWindow, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	ms := at.UTC().UnixMilli()
	out := make([]MaintenanceWindow, 0, len(all))
	for _, w := range all {
		if w.StartsAt <= ms && ms <= w.EndsAt {
			out = append(out, w)
		}
	}
	return out, nil
}
