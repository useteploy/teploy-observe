package dashboards

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type DashboardService struct {
	db *nucleus.Client
}

func NewDashboardService(db *nucleus.Client) *DashboardService {
	return &DashboardService{db: db}
}

type Dashboard struct {
	DashboardID string `json:"dashboard_id" db:"dashboard_id"`
	TenantID    string `json:"-" db:"tenant_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	Name        string `json:"name" db:"name"`
	Description string `json:"description" db:"description"`
	CreatedBy   string `json:"created_by" db:"created_by"`
	CreatedAt   string `json:"created_at" db:"created_at"`
	Version     string `json:"-" db:"version"`
}

type Panel struct {
	PanelID     string `json:"panel_id" db:"panel_id"`
	TenantID    string `json:"-" db:"tenant_id"`
	DashboardID string `json:"dashboard_id" db:"dashboard_id"`
	PanelType   string `json:"panel_type" db:"panel_type"`     // metric, timeseries, table, bar
	Title       string `json:"title" db:"title"`
	QueryType   string `json:"query_type" db:"query_type"`     // pageviews, visitors, errors, custom_sql
	QueryConfig string `json:"query_config" db:"query_config"` // JSONB
	PositionX   string `json:"position_x" db:"position_x"`
	PositionY   string `json:"position_y" db:"position_y"`
	Width       string `json:"width" db:"width"`
	Height      string `json:"height" db:"height"`
	Version     string `json:"-" db:"version"`
}

// PanelConfig is the structured query configuration for a panel.
type PanelConfig struct {
	Metric   string `json:"metric,omitempty"`   // for metric panels
	GroupBy  string `json:"group_by,omitempty"` // for table/bar panels
	Filters  map[string]string `json:"filters,omitempty"`
	Interval string `json:"interval,omitempty"` // for timeseries
	SQL      string `json:"sql,omitempty"`      // for custom SQL
}

func (s *DashboardService) Create(ctx context.Context, siteID, name, description, createdBy string) (*Dashboard, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO dashboards (dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7)`,
		id, siteID, name, description, createdBy, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create dashboard: %w", err)
	}
	return &Dashboard{DashboardID: id, SiteID: siteID, Name: name, Description: description, CreatedBy: createdBy, CreatedAt: now}, nil
}

func (s *DashboardService) List(ctx context.Context, siteID string) ([]Dashboard, error) {
	return nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM dashboards WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *DashboardService) Get(ctx context.Context, dashboardID string) (*Dashboard, error) {
	rows, err := nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM dashboards WHERE dashboard_id = $1`, dashboardID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *DashboardService) Delete(ctx context.Context, dashboardID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Soft delete by setting name to empty with higher version
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO dashboards (dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version)
		 SELECT dashboard_id, tenant_id, site_id, '', description, created_by, created_at, $2
		 FROM dashboards WHERE dashboard_id = $1`,
		dashboardID, now)
	return err
}

func (s *DashboardService) AddPanel(ctx context.Context, dashboardID string, panel Panel) (*Panel, error) {
	panel.PanelID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	if panel.Width == "" { panel.Width = "6" }
	if panel.Height == "" { panel.Height = "4" }
	if panel.PositionX == "" { panel.PositionX = "0" }
	if panel.PositionY == "" { panel.PositionY = "0" }

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		panel.PanelID, dashboardID, panel.PanelType, panel.Title, panel.QueryType,
		panel.QueryConfig, panel.PositionX, panel.PositionY, panel.Width, panel.Height, now,
	)
	if err != nil {
		return nil, fmt.Errorf("add panel: %w", err)
	}
	panel.DashboardID = dashboardID
	return &panel, nil
}

func (s *DashboardService) ListPanels(ctx context.Context, dashboardID string) ([]Panel, error) {
	return nucleus.Query[Panel](ctx, s.db.SQL(),
		`SELECT panel_id, tenant_id, dashboard_id, panel_type, title, query_type,
			COALESCE(query_config, '') AS query_config,
			position_x, position_y, width, height, version
		 FROM dashboard_panels WHERE dashboard_id = $1
		 ORDER BY position_y, position_x`, dashboardID)
}

func (s *DashboardService) UpdatePanel(ctx context.Context, panel Panel) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		panel.PanelID, panel.DashboardID, panel.PanelType, panel.Title, panel.QueryType,
		panel.QueryConfig, panel.PositionX, panel.PositionY, panel.Width, panel.Height, now,
	)
	return err
}

func (s *DashboardService) DeletePanel(ctx context.Context, panelID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 SELECT panel_id, tenant_id, dashboard_id, '', '', '', '', '0', '0', '0', '0', $2
		 FROM dashboard_panels WHERE panel_id = $1`,
		panelID, now)
	return err
}

// ExecutePanel runs the query for a panel and returns JSON-serializable results.
func (s *DashboardService) ExecutePanel(ctx context.Context, siteID string, panel Panel, from, to string) (any, error) {
	var config PanelConfig
	if panel.QueryConfig != "" {
		json.Unmarshal([]byte(panel.QueryConfig), &config)
	}

	sql := s.db.SQL()

	switch panel.QueryType {
	case "pageviews":
		type r struct{ Count string `db:"count"` }
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`,
			siteID, from, to)
		if err != nil || len(rows) == 0 { return map[string]string{"value": "0"}, nil }
		return map[string]string{"value": rows[0].Count}, nil

	case "visitors":
		type r struct{ Count string `db:"count"` }
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
			siteID, from, to)
		if err != nil || len(rows) == 0 { return map[string]string{"value": "0"}, nil }
		return map[string]string{"value": rows[0].Count}, nil

	case "errors":
		type r struct{ Count string `db:"count"` }
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM error_events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
			siteID, from, to)
		if err != nil || len(rows) == 0 { return map[string]string{"value": "0"}, nil }
		return map[string]string{"value": rows[0].Count}, nil

	default:
		return map[string]string{"value": "0"}, nil
	}
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
