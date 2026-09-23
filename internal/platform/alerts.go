package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/incidents"
)

type AlertService struct {
	db         *nucleus.Client
	logger     *slog.Logger
	webhookSvc *WebhookService
	// incidents receives the alert-driven lifecycle (O10): the evaluation
	// engine opens and closes incidents on state edges and appends
	// timeline events.
	incidents *incidents.Service
	// notifier is the durable notification outbox (O10): threshold
	// crossings enqueue delivery intents in the edge transaction.
	notifier *Notifier

	// evalMu serializes evaluations (single-process posture; see
	// alerts_engine.go). now is the clock - tests freeze it to make
	// cooldowns and windows deterministic.
	evalMu sync.Mutex
	now    func() time.Time
}

func NewAlertService(db *nucleus.Client, logger *slog.Logger, webhookSvc *WebhookService, incidentSvc *incidents.Service, notifier *Notifier) *AlertService {
	if logger == nil {
		logger = slog.Default()
	}
	return &AlertService{
		db:         db,
		logger:     logger,
		webhookSvc: webhookSvc,
		incidents:  incidentSvc,
		notifier:   notifier,
		now:        time.Now,
	}
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
	MinSamples    int       `json:"min_samples"`
	Severity      string    `json:"severity"`
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
	if rule.MinSamples <= 0 {
		rule.MinSamples = 1
	}
	if rule.Severity == "" {
		rule.Severity = "warning"
	}
	rule.Enabled = true

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO alert_rules (rule_id, tenant_id, site_id, name, metric, operator, threshold,
		 window_minutes, check_interval, cooldown, min_samples, severity, enabled, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
		rule.RuleID, rule.SiteID, rule.Name, rule.Metric, rule.Operator,
		strconv.FormatFloat(rule.Threshold, 'f', -1, 64),
		strconv.Itoa(rule.WindowMinutes),
		strconv.Itoa(rule.CheckInterval),
		strconv.Itoa(rule.Cooldown),
		strconv.Itoa(rule.MinSamples),
		rule.Severity,
		"true",
		rule.CreatedBy, nowMs,
	)
	if err != nil {
		return nil, fmt.Errorf("create alert rule: %w", err)
	}
	return &rule, nil
}

func (s *AlertService) ListRules(ctx context.Context, siteID string) ([]AlertRule, error) {
	return nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, site_id, name, metric, operator, threshold,
		 window_minutes, check_interval, cooldown, min_samples, severity, enabled, created_by,
		 CAST(created_at AS TEXT) AS created_at
		 FROM `+alertRulesLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
}

func (s *AlertService) DeleteRule(ctx context.Context, ruleID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Strictly-monotonic version (the 70f6eff version-tie defect): a
	// same-millisecond create+delete must not tie, or the tombstone loses
	// the collapse and the deleted rule keeps evaluating.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO alert_rules (rule_id, tenant_id, site_id, name, metric, operator, threshold,
		 window_minutes, check_interval, cooldown, min_samples, severity, enabled, created_by, created_at, version)
		 SELECT rule_id, tenant_id, site_id, name, metric, operator, threshold,
			window_minutes, check_interval, cooldown, min_samples, severity, 'false', created_by, created_at,
		        GREATEST(CAST($2 AS BIGINT), version + 1)
		 FROM `+alertRulesLatest("rule_id = $1"),
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
