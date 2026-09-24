package platform

// O10: the alert evaluation state machine. Replaces the stateless
// CheckRules loop (one re-fire per cooldown, recovery closed nothing, a
// data-less window silently read as "not breached").
//
// Every evaluation of every rule appends ONE transition-edge row to
// alert_evaluations (migration 049): from_state -> to_state with the value,
// the sample count and the incident the edge belongs to. The current state
// of a rule is the to_state of its latest row, so state survives restart
// with no in-memory cache. States:
//
//	healthy      a decision: samples >= minimum AND value in bounds
//	firing       a decision: samples >= minimum AND threshold breached
//	no_data      NOT a decision: the window holds fewer underlying data
//	             points than the rule's minimum (zero included). Distinct
//	             from healthy on purpose - never silently treated as
//	             passing, and it can neither open nor recover an incident.
//	unavailable  NOT a decision: the metric query failed (detail keeps the
//	             error). Same posture as flags' 'unavailable': fail open on
//	             nothing, record loudly, decide on the next tick.
//
// Notification-bearing edges (delivery lives in notify_outbox.go):
//	  -> firing with no open incident       opening notification + incident
//	firing -> healthy (or any healthy decision while an incident for the
//	          rule is still open - the recovery sweep self-heals missed
//	          edges and failed closes)      recovery notification + close
//	firing -> firing past the cooldown    repeat notification
//
// Hysteresis comes from the edges, not a timer: the firing transition is
// re-armed only after a recovery decision, and the opening notification is
// additionally gated on the incident actually being CREATED (EnsureOpen's
// dedupe) - a firing -> no_data -> firing flap with the incident still open
// does not re-notify.
//
// Atomicity: the evaluation row, the alert_history row and the notification
// intents commit in ONE transaction (the 046 outbox boundary applied to
// alerting - an intent can never orphan ahead of its edge, and an edge
// never lands without the notification it owes). The incident-service
// writes (EnsureOpen/Close/RecordEvent) run OUTSIDE that transaction,
// serialized instead by evalMu (single-process posture, AUD-018): a failure
// between the two halves is visible in logs and self-heals - a committed
// edge whose incident write failed re-runs the sweep next tick, and a
// committed incident whose intents failed re-notifies on the first repeat
// tick past the cooldown.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/incidents"
)

// Alert evaluation states (migration 049).
const (
	StateHealthy     = "healthy"
	StateFiring      = "firing"
	StateNoData      = "no_data"
	StateUnavailable = "unavailable"
)

// CheckRules evaluates all enabled rules and drives the state machine. It
// is the background worker's entry point (the "alert-check" job).
func (s *AlertService) CheckRules(ctx context.Context) error {
	// Single-flight: the incident writes outside the edge transaction rely
	// on evaluations being serialized in-process (see the file comment).
	s.evalMu.Lock()
	defer s.evalMu.Unlock()

	rules, err := nucleus.Query[AlertRule](ctx, s.db.SQL(),
		`SELECT rule_id, site_id, name, metric, operator, threshold,
		 window_minutes, check_interval, cooldown, enabled, created_by,
		 CAST(created_at AS TEXT) AS created_at, min_samples, severity
		 FROM `+alertRulesLatest("")+`
		 WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("check rules query: %w", err)
	}

	for _, rule := range rules {
		if s.isSilenced(ctx, rule.RuleID) {
			continue
		}
		if err := s.evaluateRule(ctx, rule); err != nil {
			s.logger.Error("alert evaluation failed", "rule", rule.RuleID, "metric", rule.Metric, "err", err)
		}
	}
	return nil
}

// ruleState is the persisted state of a rule (latest evaluation row).
type ruleState struct {
	State      string
	IncidentID string
}

// latestEvaluation reads the rule's current state from the ledger. An
// unknown rule (no rows yet) returns a zero State, which reads as
// "no baseline" - the first evaluation may open an incident but never
// recovers one. ORDER BY names only output columns (the Nucleus rule
// incidents.latestSelect documents); evaluated_at + eval_id make the order
// total.
func (s *AlertService) latestEvaluation(ctx context.Context, ruleID string) (ruleState, error) {
	rows, err := nucleus.Query[struct {
		ToState    string `db:"to_state"`
		IncidentID string `db:"incident_id"`
	}](ctx, s.db.SQL(),
		`SELECT evaluated_at, eval_id, to_state, incident_id
		 FROM alert_evaluations WHERE rule_id = $1
		 ORDER BY evaluated_at DESC, eval_id DESC LIMIT 1`, ruleID)
	if err != nil {
		return ruleState{}, err
	}
	if len(rows) == 0 {
		return ruleState{}, nil
	}
	return ruleState{State: rows[0].ToState, IncidentID: rows[0].IncidentID}, nil
}

// evaluateRule runs one tick for one rule: query, classify, record the
// edge, and act on the notification-bearing transitions.
func (s *AlertService) evaluateRule(ctx context.Context, rule AlertRule) error {
	now := s.now().UTC()

	prev, err := s.latestEvaluation(ctx, rule.RuleID)
	if err != nil {
		// Without the prior state a transition cannot be classified.
		// Suppress the tick (fail-safe: no decision) and log; the ledger
		// read failing is independently visible.
		return fmt.Errorf("read rule state: %w", err)
	}

	windowMins := rule.WindowMinutes
	if windowMins <= 0 {
		windowMins = 5
	}
	fromMs := dbutil.IntParam(now.Add(-time.Duration(windowMins) * time.Minute).UnixMilli())
	toMs := dbutil.IntParam(now.UnixMilli())

	value, samples, err := s.queryMetric(ctx, rule.SiteID, rule.Metric, fromMs, toMs)
	if err != nil {
		return s.commitEdge(ctx, edgeInput{
			rule: rule, from: prev.State, to: StateUnavailable,
			value: 0, samples: 0, now: now, detail: err.Error(),
		})
	}

	minSamples := rule.MinSamples
	if minSamples <= 0 {
		minSamples = 1
	}
	if samples < int64(minSamples) {
		// NO DATA is a distinct, labeled state - never a pass. It makes no
		// decision: an open incident stays open (data going silent is not
		// evidence of recovery) and nothing fires.
		return s.commitEdge(ctx, edgeInput{
			rule: rule, from: prev.State, to: StateNoData,
			value: value, samples: samples, now: now,
			detail: fmt.Sprintf("window holds %d of %d minimum data points", samples, minSamples),
		})
	}

	if thresholdBreached(rule, value) {
		return s.evaluateFiring(ctx, rule, prev, value, samples, now)
	}
	return s.evaluateHealthy(ctx, rule, prev, value, samples, now)
}

// evaluateFiring handles a firing decision.
func (s *AlertService) evaluateFiring(ctx context.Context, rule AlertRule, prev ruleState, value float64, samples int64, now time.Time) error {
	active, err := s.incidents.ActiveByRule(ctx, rule.RuleID)
	if err != nil {
		// EnsureOpen's contract: a lookup failure must not be read as
		// "nothing open" (that duplicated incidents every tick). Skip the
		// tick entirely - the fail-safe direction.
		return fmt.Errorf("active incident lookup: %w", err)
	}

	if len(active) == 0 {
		// Opening path: ensure the incident first (its id rides the edge
		// and the intents), then commit edge + history + intents in one
		// transaction. The opening notification is gated on created:
		// re-firing with the incident still open must not re-notify.
		inc, created, err := s.incidents.EnsureOpen(ctx, incidents.CreateInput{
			SiteID:      rule.SiteID,
			Title:       rule.Name,
			Description: fmt.Sprintf("alert rule fired: %s=%.2f (threshold %s %.2f)", rule.Metric, value, rule.Operator, rule.Threshold),
			Severity:    rule.severityOrDefault(),
			Source:      incidents.SourceAlert,
			RuleID:      rule.RuleID,
		}, "alert")
		if err != nil {
			return fmt.Errorf("ensure incident: %w", err)
		}
		kind := ""
		if created {
			kind = NotifyIncidentOpened
		}
		hooks := s.listHooks(ctx, rule.SiteID, rule.severityOrDefault())
		if err := s.commitEdge(ctx, edgeInput{
			rule: rule, from: prev.State, to: StateFiring,
			value: value, samples: samples, now: now,
			incidentID: inc.IncidentID, notifyKind: kind, hooks: hooks,
		}); err != nil {
			return err
		}
		if created {
			s.recordIncidentEvent(ctx, inc.IncidentID, incidents.EventOpened, "alert",
				fmt.Sprintf("rule %s fired: %s=%.2f samples=%d", rule.RuleID, rule.Metric, value, samples))
		}
		return nil
	}

	// Still firing with the incident open: no opening notification. A
	// repeat goes out only past the rule's cooldown since the LAST
	// notification (opening included) - the configured repeat policy.
	// Cooldown is minutes, matching the rule CRUD's existing semantics.
	inc := active[0]
	if rule.Cooldown > 0 {
		last, err := s.notifier.LastIntentAt(ctx, rule.RuleID)
		if err != nil {
			// Cooldown cannot be confirmed. Skip the repeat (suppress, not
			// spam) - the same failure mode the old cooldown count had.
			s.logger.Error("alert repeat cooldown check failed; suppressing repeat", "rule", rule.RuleID, "err", err)
			return s.commitEdge(ctx, edgeInput{
				rule: rule, from: prev.State, to: StateFiring,
				value: value, samples: samples, now: now, incidentID: inc.IncidentID,
			})
		}
		if now.UnixMilli()-last >= int64(rule.Cooldown)*60*1000 {
			return s.commitEdge(ctx, edgeInput{
				rule: rule, from: prev.State, to: StateFiring,
				value: value, samples: samples, now: now, incidentID: inc.IncidentID,
				notifyKind: NotifyIncidentRepeat, hooks: s.listHooks(ctx, rule.SiteID, rule.severityOrDefault()),
			})
		}
	}
	return s.commitEdge(ctx, edgeInput{
		rule: rule, from: prev.State, to: StateFiring,
		value: value, samples: samples, now: now, incidentID: inc.IncidentID,
	})
}

// evaluateHealthy handles a healthy decision. The recovery sweep is keyed
// on "a healthy decision while an incident for the rule is still open",
// not just the firing -> healthy edge: that covers the plain edge AND
// self-heals a missed one (an unavailable streak between firing and
// healthy, or a close that failed after the intents committed - the next
// healthy tick retries the close; a duplicate recovery notification in
// that rare window is bounded by the tick rate and logged).
func (s *AlertService) evaluateHealthy(ctx context.Context, rule AlertRule, prev ruleState, value float64, samples int64, now time.Time) error {
	active, err := s.incidents.ActiveByRule(ctx, rule.RuleID)
	if err != nil {
		return fmt.Errorf("active incident lookup: %w", err)
	}
	if len(active) == 0 {
		return s.commitEdge(ctx, edgeInput{
			rule: rule, from: prev.State, to: StateHealthy,
			value: value, samples: samples, now: now,
		})
	}

	hooks := s.listHooks(ctx, rule.SiteID, rule.severityOrDefault())
	if err := s.commitEdge(ctx, edgeInput{
		rule: rule, from: prev.State, to: StateHealthy,
		value: value, samples: samples, now: now,
		notifyKind: NotifyIncidentRecovered, hooks: hooks,
	}); err != nil {
		return err
	}
	for _, inc := range active {
		if err := s.incidents.Close(ctx, inc.IncidentID); err != nil {
			s.logger.Error("incident close after recovery failed; the sweep retries next tick",
				"incident", inc.IncidentID, "rule", rule.RuleID, "err", err)
			continue
		}
		s.recordIncidentEvent(ctx, inc.IncidentID, incidents.EventRecovered, "alert",
			fmt.Sprintf("rule %s recovered: %s=%.2f samples=%d", rule.RuleID, rule.Metric, value, samples))
	}
	return nil
}

// edgeInput is one transition to persist, with its optional notification.
type edgeInput struct {
	rule       AlertRule
	from, to   string
	value      float64
	samples    int64
	now        time.Time
	detail     string
	incidentID string
	notifyKind string // "" records the edge only
	hooks      []Webhook
}

// commitEdge appends the evaluation row, and for a notification-bearing
// edge the alert_history row plus one frozen intent per webhook target, in
// ONE transaction.
func (s *AlertService) commitEdge(ctx context.Context, in edgeInput) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin edge tx: %w", err)
	}
	defer tx.Rollback(ctx)
	sqlc := tx.SQL()

	evalID := genID()
	evalAt := in.now.UnixMilli()
	if _, err := sqlc.Exec(ctx,
		`INSERT INTO alert_evaluations (eval_id, tenant_id, rule_id, site_id, evaluated_at,
		 from_state, to_state, value, threshold, samples, detail, incident_id)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		evalID, in.rule.RuleID, in.rule.SiteID, dbutil.IntParam(evalAt),
		in.from, in.to,
		strconv.FormatFloat(in.value, 'f', -1, 64),
		strconv.FormatFloat(in.rule.Threshold, 'f', -1, 64),
		dbutil.IntParam(in.samples), in.detail, in.incidentID,
	); err != nil {
		return fmt.Errorf("record evaluation: %w", err)
	}

	if in.notifyKind != "" && len(in.hooks) > 0 {
		if _, err := sqlc.Exec(ctx,
			`INSERT INTO alert_history (alert_id, tenant_id, rule_id, site_id, triggered_at, metric_value, threshold, status)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'triggered')`,
			genID(), in.rule.RuleID, in.rule.SiteID, dbutil.IntParam(evalAt),
			strconv.FormatFloat(in.value, 'f', 2, 64),
			strconv.FormatFloat(in.rule.Threshold, 'f', -1, 64),
		); err != nil {
			return fmt.Errorf("record alert history: %w", err)
		}
		payload := BuildNotificationPayload(in.notifyKind, in.rule, in.value, in.samples, in.from, in.to, in.incidentID, in.now)
		for _, hook := range in.hooks {
			if _, err := s.notifier.enqueueTx(ctx, sqlc, NotificationIntent{
				Kind: in.notifyKind, RuleID: in.rule.RuleID, IncidentID: in.incidentID,
				SiteID: in.rule.SiteID, WebhookID: hook.WebhookID,
				TargetType: hook.WebhookType, TargetURL: hook.URL, Secret: hook.Secret,
				Payload: payload,
			}); err != nil {
				return fmt.Errorf("enqueue notification: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit edge tx: %w", err)
	}
	return nil
}

// listHooks returns the site's enabled webhook targets that ROUTE the
// given severity (O10: a webhook's severities filter, empty = all),
// frozen per notification at commit time. A listing failure logs and
// returns nil: the edge still records, and the first repeat tick past the
// cooldown re-notifies (LastIntentAt reads 0 with no intents).
func (s *AlertService) listHooks(ctx context.Context, siteID, severity string) []Webhook {
	if s.webhookSvc == nil {
		return nil
	}
	hooks, err := s.webhookSvc.List(ctx, siteID)
	if err != nil {
		s.logger.Error("webhook listing failed; notification deferred to the repeat tick", "site", siteID, "err", err)
		return nil
	}
	out := hooks[:0]
	for _, h := range hooks {
		if MatchesSeverity(h.Severities, severity) {
			out = append(out, h)
		}
	}
	return out
}

// recordIncidentEvent is a nil-safe timeline append.
func (s *AlertService) recordIncidentEvent(ctx context.Context, incidentID, kind, actor, detail string) {
	if s.incidents == nil || incidentID == "" {
		return
	}
	if err := s.incidents.RecordEvent(ctx, incidentID, kind, actor, detail); err != nil {
		s.logger.Warn("incident timeline write failed", "incident", incidentID, "kind", kind, "err", err)
	}
}

// thresholdBreached evaluates the rule's operator against the value.
func thresholdBreached(rule AlertRule, value float64) bool {
	switch rule.Operator {
	case "gt":
		return value > rule.Threshold
	case "gte":
		return value >= rule.Threshold
	case "lt":
		return value < rule.Threshold
	case "lte":
		return value <= rule.Threshold
	case "eq":
		return value == rule.Threshold
	}
	return false
}

func (r AlertRule) severityOrDefault() string {
	if r.Severity == "" {
		return "warning"
	}
	return r.Severity
}

// queryMetric runs the metric query for a rule window and returns the value
// plus the SAMPLE COUNT backing it - the number of underlying data points
// in the window (the events table's rows, the ingestion signal itself).
// Samples drive the no-data and minimum-samples gates:
//
//	pageviews  value = pageview events;  samples = all events
//	visitors   value = distinct sessions; samples = all events
//	error_count value = error events;    samples = all events (a busy site
//	                                  with zero errors is HEALTHY, not
//	                                  no-data; a silent site is no-data)
//	error_rate value = 100*errors/events; samples = events - an empty
//	                                  window is NO DATA, never "0%"
func (s *AlertService) queryMetric(ctx context.Context, siteID, metric, fromMs, toMs string) (float64, int64, error) {
	switch metric {
	case "pageviews":
		v, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`)
		if err != nil {
			return 0, 0, err
		}
		n, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		return v, int64(n), nil
	case "visitors":
		v, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		n, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		return v, int64(n), nil
	case "error_count":
		v, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		n, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		return v, int64(n), nil
	case "error_rate":
		errs, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		events, err := s.scalarMetric(ctx, siteID, fromMs, toMs,
			`SELECT CAST(COUNT(*) AS TEXT) AS value FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`)
		if err != nil {
			return 0, 0, err
		}
		// An empty window is NO DATA (samples 0), not a 0% rate - the old
		// silent-pass this gate exists to remove.
		if events == 0 {
			return 0, 0, nil
		}
		return 100.0 * errs / events, int64(events), nil
	default:
		return 0, 0, fmt.Errorf("unknown metric: %s", metric)
	}
}

// scalarMetric runs a single-value metric query (site_id, from, to) and
// returns the scalar, or 0 if there are no rows.
func (s *AlertService) scalarMetric(ctx context.Context, siteID, fromMs, toMs, q string) (float64, error) {
	type result struct {
		Value float64 `db:"value"`
	}
	rows, err := nucleus.Query[result](ctx, s.db.SQL(), q, siteID, fromMs, toMs)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Value, nil
}
