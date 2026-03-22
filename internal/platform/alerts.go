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
	db     *nucleus.Client
	logger *slog.Logger
}

func NewAlertService(db *nucleus.Client, logger *slog.Logger) *AlertService {
	return &AlertService{db: db, logger: logger}
}

type AlertRule struct {
	RuleID        string `json:"rule_id" db:"rule_id"`
	TenantID      string `json:"-" db:"tenant_id"`
	SiteID        string `json:"site_id" db:"site_id"`
	Name          string `json:"name" db:"name"`
	Metric        string `json:"metric" db:"metric"`
	Operator      string `json:"operator" db:"operator"`
	Threshold     string `json:"threshold" db:"threshold"`
	WindowMinutes string `json:"window_minutes" db:"window_minutes"`
	CheckInterval string `json:"check_interval" db:"check_interval"`
	Cooldown      string `json:"cooldown" db:"cooldown"`
	Enabled       string `json:"enabled" db:"enabled"`
	CreatedBy     string `json:"created_by" db:"created_by"`
	CreatedAt     string `json:"created_at" db:"created_at"`
	Version       string `json:"-" db:"version"`
}

type AlertHistoryEntry struct {
	AlertID     string `json:"alert_id" db:"alert_id"`
	RuleID      string `json:"rule_id" db:"rule_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	TriggeredAt string `json:"triggered_at" db:"triggered_at"`
	MetricValue string `json:"metric_value" db:"metric_value"`
	Threshold   string `json:"threshold" db:"threshold"`
	Status      string `json:"status" db:"status"`
}

func (s *AlertService) CreateRule(ctx context.Context, rule AlertRule) (*AlertRule, error) {
	rule.RuleID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	rule.CreatedAt = now
	rule.Version = now

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO alert_rules (rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rule.RuleID, rule.SiteID, rule.Name, rule.Metric, rule.Operator, rule.Threshold,
		rule.WindowMinutes, rule.CheckInterval, rule.Cooldown, rule.Enabled,
		rule.CreatedBy, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	return &rule, nil
}

func (s *AlertService) ListRules(ctx context.Context, siteID string) ([]AlertRule, error) {
	return nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version
		 FROM alert_rules WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *AlertService) DeleteRule(ctx context.Context, ruleID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Disable by re-inserting with enabled='false' (ReplacingMergeTree keeps latest version)
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
	return nucleus.Query[AlertHistoryEntry](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT alert_id, rule_id, site_id,
			CAST(triggered_at AS TEXT) AS triggered_at,
			metric_value, threshold, status
		 FROM alert_history WHERE site_id = $1
		 ORDER BY triggered_at DESC LIMIT %d`, limit),
		siteID,
	)
}

// CheckRules evaluates all enabled rules for a site and triggers alerts.
func (s *AlertService) CheckRules(ctx context.Context) error {
	rules, err := nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, enabled, created_by, created_at, version
		 FROM alert_rules WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("check rules query: %w", err)
	}

	now := time.Now().UTC()
	for _, rule := range rules {
		windowMins, _ := strconv.ParseInt(rule.WindowMinutes, 10, 64)
		if windowMins <= 0 {
			windowMins = 5
		}
		fromMs := dbutil.IntParam(now.Add(-time.Duration(windowMins) * time.Minute).UnixMilli())
		toMs := dbutil.IntParam(now.UnixMilli())
		threshold, _ := strconv.ParseFloat(rule.Threshold, 64)

		value, err := s.queryMetric(ctx, rule.SiteID, rule.Metric, fromMs, toMs)
		if err != nil {
			s.logger.Error("alert metric query failed", "rule", rule.RuleID, "metric", rule.Metric, "err", err)
			continue
		}

		triggered := false
		switch rule.Operator {
		case "gt":
			triggered = value > threshold
		case "gte":
			triggered = value >= threshold
		case "lt":
			triggered = value < threshold
		case "lte":
			triggered = value <= threshold
		case "eq":
			triggered = value == threshold
		}

		if triggered {
			// Check cooldown
			cooldown, _ := strconv.ParseInt(rule.Cooldown, 10, 64)
			if cooldown > 0 {
				cooldownFrom := dbutil.IntParam(now.Add(-time.Duration(cooldown) * time.Second).UnixMilli())
				type countRow struct{ Count string `db:"count"` }
				rows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
					`SELECT CAST(COUNT(*) AS TEXT) AS count FROM alert_history
					 WHERE rule_id = $1 AND triggered_at >= $2`,
					rule.RuleID, cooldownFrom)
				if err == nil && len(rows) > 0 {
					c, _ := strconv.ParseInt(rows[0].Count, 10, 64)
					if c > 0 {
						continue // still in cooldown
					}
				}
			}

			alertID := genID()
			_, err := s.db.SQL().Exec(ctx,
				`INSERT INTO alert_history (alert_id, tenant_id, rule_id, site_id, triggered_at, metric_value, threshold, status)
				 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'triggered')`,
				alertID, rule.RuleID, rule.SiteID, now.UnixMilli(),
				strconv.FormatFloat(value, 'f', 2, 64), rule.Threshold,
			)
			if err != nil {
				s.logger.Error("alert history insert failed", "rule", rule.RuleID, "err", err)
			} else {
				s.logger.Info("alert triggered", "rule", rule.Name, "metric", rule.Metric, "value", value, "threshold", threshold)
			}
		}
	}
	return nil
}

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
