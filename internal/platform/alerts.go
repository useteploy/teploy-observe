package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/observe/internal/dbutil"
)

type AlertService struct {
	db         *nucleus.Client
	logger     *slog.Logger
	webhookSvc *WebhookService
	// OnTrigger is an optional callback invoked whenever a rule fires.
	// Wired from main.go to incidents.Service so alerts automatically
	// create a visual marker across charts.
	OnTrigger func(ctx context.Context, rule AlertRule, value float64)
}

func NewAlertService(db *nucleus.Client, logger *slog.Logger, webhookSvc *WebhookService) *AlertService {
	return &AlertService{db: db, logger: logger, webhookSvc: webhookSvc}
}

// AlertRule is the domain type returned to API callers.
// The scanner handles text→typed conversion via json tag fallback.
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

// CreateRule persists a new alert rule.
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
	return nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by,
			CAST(created_at AS TEXT) AS created_at
		 FROM alert_rules WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
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

func (s *AlertService) ListHistory(ctx context.Context, siteID string, limit, offset int) ([]AlertHistoryEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return nucleus.Query[AlertHistoryEntry](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT alert_id, rule_id, site_id,
			CAST(triggered_at AS TEXT) AS triggered_at,
			metric_value, threshold, status
		 FROM alert_history WHERE site_id = $1
		 ORDER BY triggered_at DESC LIMIT %d OFFSET %d`, limit, offset),
		siteID,
	)
}

// Silence mutes a rule for the given duration. duration <= 0 clears the silence.
func (s *AlertService) Silence(ctx context.Context, ruleID string, duration time.Duration) error {
	kv := s.db.KV()
	key := "alert_silence:" + ruleID
	if duration <= 0 {
		return kv.Set(ctx, key, []byte("0"))
	}
	until := time.Now().Add(duration).UnixMilli()
	return kv.Set(ctx, key, []byte(strconv.FormatInt(until, 10)))
}

// isSilenced returns true when the rule is currently muted.
func (s *AlertService) isSilenced(ctx context.Context, ruleID string) bool {
	kv := s.db.KV()
	raw, err := kv.Get(ctx, "alert_silence:"+ruleID)
	if err != nil || raw == nil {
		return false
	}
	until, _ := strconv.ParseInt(string(raw), 10, 64)
	return until > time.Now().UnixMilli()
}

// SilenceStatus returns the UnixMilli silence expiry (0 if not silenced).
func (s *AlertService) SilenceStatus(ctx context.Context, ruleID string) int64 {
	kv := s.db.KV()
	raw, err := kv.Get(ctx, "alert_silence:"+ruleID)
	if err != nil || raw == nil {
		return 0
	}
	until, _ := strconv.ParseInt(string(raw), 10, 64)
	if until <= time.Now().UnixMilli() {
		return 0
	}
	return until
}

// CheckRules evaluates all enabled rules for a site and triggers alerts.
func (s *AlertService) CheckRules(ctx context.Context) error {
	rules, err := nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by,
			CAST(created_at AS TEXT) AS created_at
		 FROM alert_rules WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("check rules query: %w", err)
	}

	now := time.Now().UTC()
	for _, rule := range rules {
		if s.isSilenced(ctx, rule.RuleID) {
			continue
		}
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
				Count int64 `db:"count"`
			}
			crows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
				`SELECT COUNT(*) AS count FROM alert_history
				 WHERE rule_id = $1 AND triggered_at >= CAST($2 AS BIGINT)`,
				rule.RuleID, cooldownFrom)
			if err == nil && len(crows) > 0 && crows[0].Count > 0 {
				continue
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
		if s.OnTrigger != nil {
			s.OnTrigger(ctx, rule, value)
		}
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
func (s *AlertService) queryMetric(ctx context.Context, siteID, metric, fromMs, toMs string) (float64, error) {
	type result struct {
		Value float64 `db:"value"`
	}

	var q string
	switch metric {
	case "pageviews":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT) AND event_type = 'pageview'`
	case "visitors":
		q = `SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`
	case "error_count":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`
	case "error_rate":
		q = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`
	default:
		return 0, fmt.Errorf("unknown metric: %s", metric)
	}

	rows, err := nucleus.Query[result](ctx, s.db.SQL(), q, siteID, fromMs, toMs)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Value, nil
}
