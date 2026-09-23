package dashboards

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

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
	// The tombstone filter runs outside the collapse — see replacing.go.
	return nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM `+dashboardsLatest("site_id = $1")+`
		 WHERE name != ''
		 ORDER BY created_at DESC`, siteID)
}

func (s *DashboardService) Get(ctx context.Context, dashboardID string) (*Dashboard, error) {
	rows, err := nucleus.Query[Dashboard](ctx, s.db.SQL(),
		`SELECT dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version
		 FROM `+dashboardsLatest("dashboard_id = $1")+`
		 WHERE name != ''`, dashboardID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *DashboardService) Delete(ctx context.Context, dashboardID string) error {
	// Soft delete by setting name to empty with higher version. Reading through
	// the collapse is what keeps this to one row per call.
	// R19 (round 4): the tombstone version is strictly greater than the
	// latest row's — a tied/regressed version would collapse away and
	// resurrect the dashboard.
	latest, err := nucleus.Query[struct {
		V string `db:"v"`
	}](ctx, s.db.SQL(),
		"SELECT CAST(MAX(version) AS TEXT) AS v FROM "+dashboardsLatest("dashboard_id = $1"),
		dashboardID)
	if err != nil {
		return fmt.Errorf("dashboards: delete lookup: %w", err)
	}
	if len(latest) == 0 {
		return fmt.Errorf("dashboards: dashboard not found")
	}
	prior, _ := strconv.ParseInt(latest[0].V, 10, 64)
	next := nextVersionMS(prior, time.Now().UTC().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO dashboards (dashboard_id, tenant_id, site_id, name, description, created_by, created_at, version)
		 SELECT dashboard_id, tenant_id, site_id, '', description, created_by, created_at, $2
		 FROM `+dashboardsLatest("dashboard_id = $1"),
		dashboardID, next)
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

// ValidatePanel is the exported boundary check (handlers map rejection to
// 400); AddPanel and UpdatePanel apply it as the write gate.
func ValidatePanel(p *Panel) error { return validatePanel(p) }

// validatePanel is the one normalization/validation gate for panel writes
// (R36, round 4), applied identically by AddPanel and UpdatePanel. Create
// used to accept an empty panel_type that ListPanels then filtered as a
// tombstone (a successful create that immediately disappears), arbitrary
// position strings that broke the whole listing's BIGINT cast, and Update
// bypassed every check AddPanel had.
func validatePanel(p *Panel) error {
	if p.PanelType == "" || !IsValidPanelType(p.PanelType) {
		return fmt.Errorf("dashboards: panel_type must be a supported nonempty value")
	}
	if p.QueryType == "" || !isValidQueryType(p.QueryType) {
		return fmt.Errorf("dashboards: query_type must be a supported nonempty value")
	}
	if p.QueryConfig != "" && !json.Valid([]byte(p.QueryConfig)) {
		return fmt.Errorf("dashboards: query_config must be valid JSON")
	}
	fields := []struct {
		name     string
		value    *string
		fallback int
		min, max int
	}{
		{"position_x", &p.PositionX, 0, 0, 23},
		{"position_y", &p.PositionY, 0, 0, 100000},
		{"width", &p.Width, 6, 1, 24},
		{"height", &p.Height, 4, 1, 100},
	}
	for _, f := range fields {
		n := f.fallback
		if *f.value != "" {
			parsed, err := strconv.Atoi(*f.value)
			if err != nil {
				return fmt.Errorf("dashboards: %s must be an integer", f.name)
			}
			n = parsed
		}
		if n < f.min || n > f.max {
			return fmt.Errorf("dashboards: %s out of range [%d, %d]", f.name, f.min, f.max)
		}
		*f.value = strconv.Itoa(n)
	}
	// metric_series stores its query in query_config — ensure it parses and
	// names a metric before the row lands so the dashboard view doesn't
	// crash later.
	if p.PanelType == "metric_series" || p.QueryType == "metric_series" {
		var cfg PanelConfig
		if p.QueryConfig != "" {
			if err := json.Unmarshal([]byte(p.QueryConfig), &cfg); err != nil {
				return fmt.Errorf("dashboards: query_config invalid JSON: %w", err)
			}
		}
		if cfg.Metric == "" {
			return fmt.Errorf("dashboards: metric_series panels require query_config.metric")
		}
		if cfg.Agg != "" && !metrics.IsValidAggregation(cfg.Agg) {
			return fmt.Errorf("dashboards: unsupported agg %q", cfg.Agg)
		}
		if _, err := metrics.ParseStep(cfg.Step); err != nil {
			return err
		}
	}
	return nil
}

func (s *DashboardService) AddPanel(ctx context.Context, dashboardID string, panel Panel) (*Panel, error) {
	panel.PanelID = genID()
	// R19 (round 4): strictly-monotonic replacement version — the layout
	// write must not tie or regress against the latest row's version.
	prior, err := s.latestPanelVersion(ctx, dashboardID, panel.PanelID)
	if err != nil {
		return nil, err
	}
	now := nextVersionMS(prior, time.Now().UTC().UnixMilli())

	if err := validatePanel(&panel); err != nil {
		return nil, err
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11)`,
		panel.PanelID, dashboardID, panel.PanelType, panel.Title, panel.QueryType,
		panel.QueryConfig, panel.PositionX, panel.PositionY, panel.Width, panel.Height, now,
	)
	if err != nil {
		return nil, fmt.Errorf("add panel: %w", err)
	}
	panel.DashboardID = dashboardID
	return &panel, nil
}

// latestPanelVersion reads the max collapsed version for a panel (0 when
// none), so replacement writes can be made strictly newer (R19). Scoped to
// the dashboard so a panel id cannot cross dashboards.
func (s *DashboardService) latestPanelVersion(ctx context.Context, dashboardID, panelID string) (int64, error) {
	rows, err := nucleus.Query[struct {
		V string `db:"v"`
	}](ctx, s.db.SQL(),
		"SELECT CAST(MAX(version) AS TEXT) AS v FROM "+panelsLatestShim(dashboardID, panelID),
		dashboardID, panelID)
	if err != nil {
		return 0, fmt.Errorf("dashboards: read panel version: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	v, _ := strconv.ParseInt(rows[0].V, 10, 64)
	return v, nil
}

// panelsLatestShim is the collapsed per-panel read used by version checks.
func panelsLatestShim(dashboardID, panelID string) string {
	return panelsLatest("dashboard_id = $1 AND panel_id = $2")
}

// nextVersionMS returns a version strictly greater than prior.
func nextVersionMS(prior, nowMS int64) string {
	next := nowMS
	if prior+1 > next {
		next = prior + 1
	}
	return strconv.FormatInt(next, 10)
}

func (s *DashboardService) ListPanels(ctx context.Context, dashboardID string) ([]Panel, error) {
	// dashboard_panels is ReplacingMergeTree (insert-on-update). Collapse to the
	// highest-version row per panel so edits take effect. This replaces a
	// correlated `version = (SELECT MAX(version) FROM dashboard_panels dp2 ...)`
	// — the same collapse, expressed as a per-row subquery, and one more place a
	// stale version could win.
	//
	// Audit F26: the tombstone filter runs AFTER the collapse, mirroring
	// dashboards.List — DeletePanel writes an empty-panel_type row as the new
	// version, and filtering before the collapse would resurrect the
	// superseded live row. Without this filter, deleted panels stayed in the
	// list as empty/broken panels and could be selected for execution.
	rows, err := nucleus.Query[Panel](ctx, s.db.SQL(),
		`SELECT panel_id, tenant_id, dashboard_id, panel_type, title, query_type,
			COALESCE(query_config, '') AS query_config,
			position_x, position_y, width, height, version
		 FROM `+panelsLatest("dashboard_id = $1")+
			` WHERE panel_type != ''
		 ORDER BY CAST(position_y AS BIGINT), CAST(position_x AS BIGINT)`, dashboardID)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []Panel{}
	}
	return rows, nil
}

// UpdatePanel replaces a panel row. R36 (round 4): update passes the SAME
// validation gate as create (it used to bypass every check — an update could
// land an empty panel_type tombstone or nonnumeric coordinates that broke the
// whole listing). R19: the replacement version is strictly greater than the
// latest row's.
func (s *DashboardService) UpdatePanel(ctx context.Context, panel Panel) error {
	if err := validatePanel(&panel); err != nil {
		return err
	}
	prior, err := s.latestPanelVersion(ctx, panel.DashboardID, panel.PanelID)
	if err != nil {
		return err
	}
	now := nextVersionMS(prior, time.Now().UTC().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11)`,
		panel.PanelID, panel.DashboardID, panel.PanelType, panel.Title, panel.QueryType,
		panel.QueryConfig, panel.PositionX, panel.PositionY, panel.Width, panel.Height, now,
	)
	return err
}

func (s *DashboardService) DeletePanel(ctx context.Context, panelID string) error {
	// R19 (round 4): tombstone version strictly greater than the latest
	// row's — a same-millisecond delete after an update could collapse away
	// and resurrect the live panel.
	latest, err := nucleus.Query[struct {
		DashboardID string `db:"dashboard_id"`
		V           string `db:"v"`
	}](ctx, s.db.SQL(),
		"SELECT dashboard_id, CAST(MAX(version) AS TEXT) AS v FROM "+panelsLatest("panel_id = $1"),
		panelID)
	if err != nil {
		return fmt.Errorf("dashboards: delete panel lookup: %w", err)
	}
	if len(latest) == 0 {
		return fmt.Errorf("dashboards: panel not found")
	}
	prior, _ := strconv.ParseInt(latest[0].V, 10, 64)
	next := nextVersionMS(prior, time.Now().UTC().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO dashboard_panels (panel_id, tenant_id, dashboard_id, panel_type, title, query_type, query_config, position_x, position_y, width, height, version)
		 SELECT panel_id, tenant_id, dashboard_id, '', '', '', NULL, '0', '0', '0', '0', $2
		 FROM `+panelsLatest("panel_id = $1"),
		panelID, next)
	return err
}

// ExecutePanel runs the query for a panel and returns JSON-serializable results.
func (s *DashboardService) ExecutePanel(ctx context.Context, siteID string, panel Panel, from, to string) (any, error) {
	var config PanelConfig
	if panel.QueryConfig != "" {
		// R36 (round 4): a config that cannot parse is a descriptive error,
		// not an ignored one that silently queries with zero values.
		if err := json.Unmarshal([]byte(panel.QueryConfig), &config); err != nil {
			return nil, fmt.Errorf("dashboards: query_config invalid JSON: %w", err)
		}
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

	// Audit F27: a failed query is an error, not a healthy zero. The old
	// `if err != nil || len(rows) == 0 { return 0 }` turned a backend outage
	// into "0 errors / 0 pageviews" — indistinguishable from real data.
	// An empty result set remains a legitimate zero.
	switch panel.QueryType {
	case "pageviews":
		type r struct {
			Count string `db:"count"`
		}
		rows, err := nucleus.Query[r](ctx, sql,
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`,
			siteID, from, to)
		if err != nil {
			return nil, fmt.Errorf("query panel (pageviews): %w", err)
		}
		if len(rows) == 0 {
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
		if err != nil {
			return nil, fmt.Errorf("query panel (visitors): %w", err)
		}
		if len(rows) == 0 {
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
		if err != nil {
			return nil, fmt.Errorf("query panel (errors): %w", err)
		}
		if len(rows) == 0 {
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
