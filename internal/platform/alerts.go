package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

type AlertService struct {
	db         *nucleus.Client
	logger     *slog.Logger
	webhookSvc *WebhookService
}

func NewAlertService(db *nucleus.Client, logger *slog.Logger, webhookSvc *WebhookService) *AlertService {
	return &AlertService{db: db, logger: logger, webhookSvc: webhookSvc}
}

// AlertRule is the domain type returned to API callers.
// Numeric and boolean fields are typed; timestamps serialize as RFC3339.
type AlertRule struct {
	RuleID        string    `json:"rule_id"`
	SiteID        string    `json:"site_id"`
	Name          string    `json:"name"`
	Metric        string    `json:"metric"`
	Operator      string    `json:"operator"`
	Threshold     float64   `json:"threshold"`
	WindowMinutes int       `json:"window_minutes"`
	CheckInterval int       `json:"check_interval"`
	Cooldown      int       `json:"cooldown"`
	Enabled       bool      `json:"enabled"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// AlertHistoryEntry is the domain type for triggered alerts.
type AlertHistoryEntry struct {
	AlertID     string    `json:"alert_id"`
	RuleID      string    `json:"rule_id"`
	SiteID      string    `json:"site_id"`
	TriggeredAt time.Time `json:"triggered_at"`
	MetricValue float64   `json:"metric_value"`
	Threshold   float64   `json:"threshold"`
	Status      string    `json:"status"`
}

// alertRuleRow mirrors the DB column shape. Nucleus MergeTree columns come
// back as strings; we keep that shape confined to this package and convert
// to the typed domain at the boundary.
type alertRuleRow struct {
	RuleID        string `db:"rule_id"`
	TenantID      string `db:"tenant_id"`
	SiteID        string `db:"site_id"`
	Name          string `db:"name"`
	Metric        string `db:"metric"`
	Operator      string `db:"operator"`
	Threshold     string `db:"threshold"`
	WindowMinutes string `db:"window_minutes"`
	CheckInterval string `db:"check_interval"`
	Cooldown      string `db:"cooldown"`
	Enabled       string `db:"enabled"`
	CreatedBy     string `db:"created_by"`
	CreatedAt     string `db:"created_at"`
	Version       string `db:"version"`
}

type alertHistoryRow struct {
	AlertID     string `db:"alert_id"`
	RuleID      string `db:"rule_id"`
	SiteID      string `db:"site_id"`
	TriggeredAt string `db:"triggered_at"`
	MetricValue string `db:"metric_value"`
	Threshold   string `db:"threshold"`
	Status      string `db:"status"`
}

func parseBool(s string) bool {
	switch s {
	case "true", "1", "TRUE", "True":
		return true
	}
	return false
}

// parseEpochMillis parses a unix-ms string (common case from CAST in queries)
// or falls back to time.Parse for RFC3339-ish strings.
func parseEpochMillis(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func (r alertRuleRow) toDomain() AlertRule {
	thr, _ := strconv.ParseFloat(r.Threshold, 64)
	win, _ := strconv.Atoi(r.WindowMinutes)
	chk, _ := strconv.Atoi(r.CheckInterval)
	cool, _ := strconv.Atoi(r.Cooldown)
	return AlertRule{
		RuleID: r.RuleID, SiteID: r.SiteID, Name: r.Name,
		Metric: r.Metric, Operator: r.Operator,
		Threshold: thr, WindowMinutes: win, CheckInterval: chk, Cooldown: cool,
		Enabled: parseBool(r.Enabled), CreatedBy: r.CreatedBy,
		CreatedAt: parseEpochMillis(r.CreatedAt),
	}
}

func (r alertHistoryRow) toDomain() AlertHistoryEntry {
	mv, _ := strconv.ParseFloat(r.MetricValue, 64)
	thr, _ := strconv.ParseFloat(r.Threshold, 64)
	return AlertHistoryEntry{
		AlertID: r.AlertID, RuleID: r.RuleID, SiteID: r.SiteID,
		TriggeredAt: parseEpochMillis(r.TriggeredAt),
		MetricValue: mv, Threshold: thr, Status: r.Status,
	}
}

// CreateRule persists a new alert rule. Numeric fields are stored as text
// columns (MergeTree schema is stringly-typed; see alertRuleRow).
func (s *AlertService) CreateRule(ctx context.Context, rule AlertRule) (*AlertRule, error) {
	rule.RuleID = genID()
	now := time.Now().UTC()
	nowMs := strconv.FormatInt(now.UnixMilli(), 10)
	rule.CreatedAt = now

	if rule.Operator == "" {
		rule.Operator = "gt"
	}
	if rule.WindowMinutes <= 0 {
		rule.WindowMinutes = 5
	}
	if rule.Cooldown <= 0 {
		rule.Cooldown = 5
	}
	if rule.CheckInterval <= 0 {
		rule.CheckInterval = 60
	}
	rule.Enabled = true

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO alert_rules (rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rule.RuleID, rule.SiteID, rule.Name, rule.Metric, rule.Operator,
		strconv.FormatFloat(rule.Threshold, 'f', -1, 64),
		strconv.Itoa(rule.WindowMinutes),
		strconv.Itoa(rule.CheckInterval),
		strconv.Itoa(rule.Cooldown),
		"true",
		rule.CreatedBy, nowMs, nowMs,
	)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	return &rule, nil
}

func (s *AlertService) ListRules(ctx context.Context, siteID string) ([]AlertRule, error) {
	rows, err := nucleus.Query[alertRuleRow](ctx, s.db.SQL(),
		`SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version
		 FROM alert_rules WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]AlertRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

func (s *AlertService) DeleteRule(ctx context.Context, ruleID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO alert_rules (rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version)
		 SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, 'false', created_by, created_at, $2
		 FROM alert_rules WHERE rule_id = $1`,
		ruleID, now,
	)
	return err
}

func (s *AlertService) ListHistory(ctx context.Context, siteID string, limit int) ([]AlertHistoryEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := nucleus.Query[alertHistoryRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT alert_id, rule_id, site_id,
			CAST(triggered_at AS TEXT) AS triggered_at,
			metric_value, threshold, status
		 FROM alert_history WHERE site_id = $1
		 ORDER BY triggered_at DESC LIMIT %d`, limit),
		siteID,
	)
	if err != nil {
		return nil, err
	}
	out := make([]AlertHistoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// CheckRules evaluates all enabled rules for a site and triggers alerts.
func (s *AlertService) CheckRules(ctx context.Context) error {
	rows, err := nucleus.Query[alertRuleRow](ctx, s.db.SQL(),
		`SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version
		 FROM alert_rules WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("check rules query: %w", err)
	}

	now := time.Now().UTC()
	for _, row := range rows {
		rule := row.toDomain()
		windowMins := rule.WindowMinutes
		if windowMins <= 0 {
			windowMins = 5
		}
		fromMs := dbutil.IntParam(now.Add(-time.Duration(windowMins) * time.Minute).UnixMilli())
		toMs := dbutil.IntParam(now.UnixMilli())

		value, err := s.queryMetric(ctx, rule.SiteID, rule.Metric, fromMs, toMs)
		if err != nil {
			s.logger.Error("alert metric query failed", "rule", rule.RuleID, "metric", rule.Metric, "err", err)
			continue
		}

		triggered := false
		switch rule.Operator {
		case "gt":
			triggered = value > rule.Threshold
		case "gte":
			triggered = value >= rule.Threshold
		case "lt":
			triggered = value < rule.Threshold
		case "lte":
			triggered = value <= rule.Threshold
		case "eq":
			triggered = value == rule.Threshold
		}

		if !triggered {
			continue
		}

		if rule.Cooldown > 0 {
			cooldownFrom := dbutil.IntParam(now.Add(-time.Duration(rule.Cooldown) * time.Minute).UnixMilli())
			type countRow struct {
				Count string `db:"count"`
			}
			crows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
				`SELECT CAST(COUNT(*) AS TEXT) AS count FROM alert_history
				 WHERE rule_id = $1 AND triggered_at >= $2`,
				rule.RuleID, cooldownFrom)
			if err == nil && len(crows) > 0 {
				c, _ := strconv.ParseInt(crows[0].Count, 10, 64)
				if c > 0 {
					continue
				}
			}
		}

		alertID := genID()
		_, err = s.db.SQL().Exec(ctx,
			`INSERT INTO alert_history (alert_id, tenant_id, rule_id, site_id, triggered_at, metric_value, threshold, status)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'triggered')`,
			alertID, rule.RuleID, rule.SiteID, now.UnixMilli(),
			strconv.FormatFloat(value, 'f', 2, 64),
			strconv.FormatFloat(rule.Threshold, 'f', -1, 64),
		)
		if err != nil {
			s.logger.Error("alert history insert failed", "rule", rule.RuleID, "err", err)
			continue
		}
		s.logger.Info("alert triggered", "rule", rule.Name, "metric", rule.Metric, "value", value, "threshold", rule.Threshold)
		if s.webhookSvc != nil {
			s.webhookSvc.Fire(ctx, rule.SiteID, AlertPayload{
				AlertID:   alertID,
				RuleName:  rule.Name,
				Metric:    rule.Metric,
				Value:     value,
				Threshold: strconv.FormatFloat(rule.Threshold, 'f', -1, 64),
				SiteID:    rule.SiteID,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}
	return nil
}

// queryMetric runs the metric query for a rule window and returns the value.
// Supported metrics: error_count, error_rate, pageviews, visitors.
// Latency percentiles (p95/p99) are intentionally not supported yet — they
// require the APM query layer over the spans table, which is a separate
// build-out (see MASTER_PLAN Phase 3).
func (s *AlertService) queryMetric(ctx context.Context, siteID, metric, fromMs, toMs string) (float64, error) {
	type result struct {
		Value string `db:"value"`
	}

	var q string
	switch metric {
	case "pageviews":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`
	case "visitors":
		q = `SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`
	case "error_count":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`
	case "error_rate":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`
	default:
		return 0, fmt.Errorf("unknown metric: %s", metric)
	}

	rows, err := nucleus.Query[result](ctx, s.db.SQL(), q, siteID, fromMs, toMs)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	v, _ := strconv.ParseFloat(rows[0].Value, 64)
	return v, nil
}
