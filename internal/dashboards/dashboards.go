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

	"github.com/useteploy/teploy-observe/internal/metrics"
)

type DashboardService struct {
	db         *nucleus.Client
	metricsSvc *metrics.Service
}

func NewDashboardService(db *nucleus.Client) *DashboardService {
	return &DashboardService{db: db}
}

// WithMetrics wires the metrics service so panels of type "metric_series"
// can be executed via this dashboard service. Wired from main.go where
// both services are constructed; kept optional so unit tests that don't
// touch metric panels can still build a service without dragging in the
// metrics package.
func (s *DashboardService) WithMetrics(svc *metrics.Service) *DashboardService {
	s.metricsSvc = svc
	return s
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
	PanelType   string `json:"panel_type" db:"panel_type"` // metric, timeseries, table, bar
	Title       string `json:"title" db:"title"`
	QueryType   string `json:"query_type" db:"query_type"`     // pageviews, visitors, errors, metric_series
	QueryConfig string `json:"query_config" db:"query_config"` // JSONB
	PositionX   string `json:"position_x" db:"position_x"`
	PositionY   string `json:"position_y" db:"position_y"`
	Width       string `json:"width" db:"width"`
	Height      string `json:"height" db:"height"`
	Version     string `json:"-" db:"version"`
}

// PanelConfig is the structured query configuration for a panel.
//
// metric_series panels (Phase 2) reuse Metric for the metric name, Filters
// as the AND-joined label map, plus three new fields (Agg, Step, GroupBy)
// that map 1:1 onto metrics.QueryOptions.
type PanelConfig struct {
	Metric   string            `json:"metric,omitempty"`   // for metric / metric_series panels
	GroupBy  string            `json:"group_by,omitempty"` // table/bar panels = single key; metric_series = comma-separated
	Filters  map[string]string `json:"filters,omitempty"`  // metric_series = label filter map; also used as `labels`
	Labels   map[string]string `json:"labels,omitempty"`   // metric_series alias of Filters (Filters wins if both set)
	Interval string            `json:"interval,omitempty"` // timeseries
	SQL      string            `json:"sql,omitempty"`      // custom SQL
	Agg      string            `json:"agg,omitempty"`      // metric_series — last|avg|sum|min|max|rate|p50|p95|p99
	Step     string            `json:"step,omitempty"`     // metric_series — bucket size, e.g. "60s"
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
	rows, err := nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM dashboards WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, err
	}
	// Nucleus ReplacingMergeTree read-time dedup is unreliable, so keep the
	// max-version row per id in Go and drop soft-deleted tombstones (name == '').
	latest := map[string]Dashboard{}
	for _, d := range rows {
		if cur, ok := latest[d.DashboardID]; !ok || versionLess(cur.Version, d.Version) {
			latest[d.DashboardID] = d
		}
	}
	out := make([]Dashboard, 0, len(latest))
	for _, d := range rows { // preserve created_at DESC order, one entry per id
		if l, ok := latest[d.DashboardID]; ok && d.Version == l.Version {
			if l.Name != "" {
				out = append(out, l)
			}
			delete(latest, d.DashboardID)
		}
	}
	return out, nil
}

func (s *DashboardService) Get(ctx context.Context, dashboardID string) (*Dashboard, error) {
	rows, err := nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM dashboards WHERE dashboard_id = $1`, dashboardID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	best := rows[0]
	for _, d := range rows[1:] {
		if versionLess(best.Version, d.Version) {
			best = d
		}
	}
	if best.Name == "" { // soft-deleted tombstone
		return nil, nil
	}
	return &best, nil
}

// versionLess reports whether numeric version a < b (versions are millisecond
// epoch strings).
func versionLess(a, b string) bool {
	ai, _ := strconv.ParseInt(a, 10, 64)
	bi, _ := strconv.ParseInt(b, 10, 64)
	return ai < bi
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

// IsValidPanelType reports whether t is a panel type we know how to render.
// Validated on AddPanel so a typo doesn't quietly land an unrenderable
// panel in the DB.
func IsValidPanelType(t string) bool {
	switch t {
	case "metric", "timeseries", "table", "bar", "metric_series":
		return true
	}
	return false
}

// isValidQueryType reports whether ExecutePanel can run the query type.
func isValidQueryType(t string) bool {
	switch t {
	case "pageviews", "visitors", "errors", "metric_series":
		return true
	}
	return false
}

func (s *DashboardService) AddPanel(ctx context.Context, dashboardID string, panel Panel) (*Panel, error) {
	panel.PanelID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	if panel.PanelType != "" && !IsValidPanelType(panel.PanelType) {
		return nil, fmt.Errorf("dashboards: unsupported panel_type %q", panel.PanelType)
	}
	// metric_series stores its query in query_config — ensure it parses
	// before the row lands so the dashboard view doesn't crash later.
	if panel.PanelType == "metric_series" {
		var cfg PanelConfig
		if panel.QueryConfig != "" {
			if err := json.Unmarshal([]byte(panel.QueryConfig), &cfg); err != nil {
				return nil, fmt.Errorf("dashboards: query_config invalid JSON: %w", err)
			}
		}
		if cfg.Metric == "" {
			return nil, fmt.Errorf("dashboards: metric_series panels require query_config.metric")
		}
		if cfg.Agg != "" && !metrics.IsValidAggregation(cfg.Agg) {
			return nil, fmt.Errorf("dashboards: unsupported agg %q", cfg.Agg)
		}
		if _, err := metrics.ParseStep(cfg.Step); err != nil {
			return nil, err
		}
		if panel.QueryType == "" {
			panel.QueryType = "metric_series"
		}
	}

	// Reject query types ExecutePanel can't run (e.g. the never-implemented
	// custom_sql) so an unrenderable panel never lands in the DB.
	if panel.QueryType != "" && !isValidQueryType(panel.QueryType) {
		return nil, fmt.Errorf("dashboards: unsupported query_type %q", panel.QueryType)
	}

	if panel.Width == "" {
		panel.Width = "6"
	}
	if panel.Height == "" {
		panel.Height = "4"
	}
	if panel.PositionX == "" {
		panel.PositionX = "0"
	}
	if panel.PositionY == "" {
		panel.PositionY = "0"
	}

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
	// dashboard_panels is ReplacingMergeTree (insert-on-update). De-dupe to the
	// highest-version row per panel_id so edits take effect.
	return nucleus.Query[Panel](ctx, s.db.SQL(),
		`SELECT panel_id, tenant_id, dashboard_id, panel_type, title, query_type,
			COALESCE(query_config, '') AS query_config,
			position_x, position_y, width, height, version
		 FROM dashboard_panels
		 WHERE dashboard_id = $1
		   AND CAST(version AS BIGINT) = (
		     SELECT MAX(CAST(version AS BIGINT))
		     FROM dashboard_panels dp2
		     WHERE dp2.panel_id = dashboard_panels.panel_id
		   )
		 ORDER BY CAST(position_y AS BIGINT), CAST(position_x AS BIGINT)`, dashboardID)
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

	// metric_series panels run their query against the metrics service
	// instead of the analytics tables.
	if panel.PanelType == "metric_series" || panel.QueryType == "metric_series" {
		if s.metricsSvc == nil {
			return nil, fmt.Errorf("dashboards: metrics service not wired")
		}
		labels := config.Filters
		if labels == nil {
			labels = config.Labels
		}
		stepMs, err := metrics.ParseStep(config.Step)
		if err != nil {
			return nil, err
		}
		fromMs, _ := strconv.ParseInt(from, 10, 64)
		toMs, _ := strconv.ParseInt(to, 10, 64)
		if toMs == 0 {
			toMs = time.Now().UTC().UnixMilli()
		}
		if fromMs == 0 {
			fromMs = toMs - 60*60*1000
		}
		var groupBy []string
		if config.GroupBy != "" {
			groupBy = metrics.ParseGroupBy(config.GroupBy)
		}
		series, err := s.metricsSvc.QuerySeries(ctx, siteID, config.Metric, labels, fromMs, toMs, metrics.QueryOptions{
			Agg:     config.Agg,
			StepMs:  stepMs,
			GroupBy: groupBy,
		})
		if err != nil {
			return nil, err
		}
		if series == nil {
			series = []metrics.Series{}
		}
		return series, nil
	}

	sql := s.db.SQL()

	switch panel.QueryType {
	case "pageviews":
		type r struct {
			Count string `db:"count"`
		}
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`,
			siteID, from, to)
		if err != nil || len(rows) == 0 {
			return map[string]string{"value": "0"}, nil
		}
		return map[string]string{"value": rows[0].Count}, nil

	case "visitors":
		type r struct {
			Count string `db:"count"`
		}
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
			siteID, from, to)
		if err != nil || len(rows) == 0 {
			return map[string]string{"value": "0"}, nil
		}
		return map[string]string{"value": rows[0].Count}, nil

	case "errors":
		type r struct {
			Count string `db:"count"`
		}
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM error_events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
			siteID, from, to)
		if err != nil || len(rows) == 0 {
			return map[string]string{"value": "0"}, nil
		}
		return map[string]string{"value": rows[0].Count}, nil

	default:
		// Surface an unimplemented/unknown query type as broken rather than
		// returning a fake 0 (custom_sql was never implemented).
		return nil, fmt.Errorf("unsupported query_type %q", panel.QueryType)
	}
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
