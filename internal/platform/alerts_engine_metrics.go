package platform

// Alert metrics beyond product analytics: traces, logs and uptime. Each is
// computed over the rule's window for the rule's SITE (no per-service filter:
// AlertRule has no field that could carry one, and adding it needs a schema
// change). Every query is window-bounded and parameterised ($1 site, $2/$3
// the window as CAST($n AS BIGINT) - the Nucleus wire quirk dbutil.IntParam
// documents).
//
// Sample semantics (what "no data" means per metric; queryMetric's contract
// is value + the sample count that backs it, and the engine turns
// samples < min_samples into the labeled no_data state, never a pass):
//
//	trace_error_rate  value = 100*error spans/spans;  samples = spans. No spans
//	                  in the window is NO DATA, never "0%".
//	trace_p95_ms      value = p95 duration_ms of ROOT spans (parent_span_id='');
//	                  samples = root spans. No root spans is NO DATA.
//	log_error_count   value = logs at error/fatal level; samples = all logs
//	                  (a busy log stream with zero errors is HEALTHY, a silent
//	                  pipeline is no-data - the error_count posture).
//	uptime_failures   value = failed checks (is_up = 'false') across the
//	                  site's monitors in the window; samples = all checks.
//	                  No check results (monitors disabled/absent) is NO DATA.

import (
	"context"
	"fmt"
	"sort"
)

// alertMetricNames is the single source of truth for rule metrics; the API
// whitelist (cmd/observe) derives from it so the layers cannot drift.
var alertMetricNames = []string{
	"pageviews", "visitors", "error_count", "error_rate",
	"trace_error_rate", "trace_p95_ms", "log_error_count", "uptime_failures",
}

// AlertMetricSet returns the supported metric names as a set.
func AlertMetricSet() map[string]struct{} {
	m := make(map[string]struct{}, len(alertMetricNames))
	for _, n := range alertMetricNames {
		m[n] = struct{}{}
	}
	return m
}

// AlertMetricNames returns the supported metric names, sorted.
func AlertMetricNames() []string {
	out := append([]string(nil), alertMetricNames...)
	sort.Strings(out)
	return out
}

// SQL for the extended metrics. Constants so tests can assert the exact text.
const (
	sqlSpanCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM spans WHERE site_id = $1 AND start_time >= CAST($2 AS BIGINT) AND start_time < CAST($3 AS BIGINT)`

	sqlSpanErrorCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM spans WHERE site_id = $1 AND start_time >= CAST($2 AS BIGINT) AND start_time < CAST($3 AS BIGINT) AND status_code = 'error'`

	sqlRootSpanCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM spans WHERE site_id = $1 AND start_time >= CAST($2 AS BIGINT) AND start_time < CAST($3 AS BIGINT) AND parent_span_id = ''`

	sqlRootSpanP95 = `SELECT CAST(CAST(percentile_cont(duration_ms, 0.95) AS BIGINT) AS TEXT) AS value FROM spans WHERE site_id = $1 AND start_time >= CAST($2 AS BIGINT) AND start_time < CAST($3 AS BIGINT) AND parent_span_id = ''`

	sqlLogCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM logs WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`

	sqlLogErrorCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM logs WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT) AND level IN ('error', 'fatal')`

	sqlUptimeCount = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM uptime_results WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`

	sqlUptimeFailures = `SELECT CAST(COUNT(*) AS TEXT) AS value FROM uptime_results WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT) AND is_up = 'false'`
)

// queryExtendedMetric handles the trace/log/uptime metrics. handled=false
// means the name is not one of them (the caller reports unknown metric).
func (s *AlertService) queryExtendedMetric(ctx context.Context, siteID, metric, fromMs, toMs string) (float64, int64, bool, error) {
	run := func(q string) (float64, error) { return s.scalarMetric(ctx, siteID, fromMs, toMs, q) }
	switch metric {
	case "trace_error_rate":
		total, err := run(sqlSpanCount)
		if err != nil {
			return 0, 0, true, err
		}
		if total == 0 {
			return 0, 0, true, nil // no spans: NO DATA, not 0%
		}
		errs, err := run(sqlSpanErrorCount)
		if err != nil {
			return 0, 0, true, err
		}
		return 100.0 * errs / total, int64(total), true, nil
	case "trace_p95_ms":
		roots, err := run(sqlRootSpanCount)
		if err != nil {
			return 0, 0, true, err
		}
		if roots == 0 {
			return 0, 0, true, nil // percentile of nothing is not 0 ms
		}
		p95, err := run(sqlRootSpanP95)
		if err != nil {
			return 0, 0, true, err
		}
		return p95, int64(roots), true, nil
	case "log_error_count":
		v, err := run(sqlLogErrorCount)
		if err != nil {
			return 0, 0, true, err
		}
		n, err := run(sqlLogCount)
		if err != nil {
			return 0, 0, true, err
		}
		return v, int64(n), true, nil
	case "uptime_failures":
		n, err := run(sqlUptimeCount)
		if err != nil {
			return 0, 0, true, err
		}
		if n == 0 {
			return 0, 0, true, nil
		}
		v, err := run(sqlUptimeFailures)
		if err != nil {
			return 0, 0, true, err
		}
		return v, int64(n), true, nil
	}
	return 0, 0, false, nil
}

// AlertFireEvent is the opening-edge event handed to the optional
// integrations hook (OBSERVE_ALERTS_TO_INTEGRATIONS). Plain strings so
// platform needs no import of internal/integrations.
type AlertFireEvent struct {
	SiteID, Title, Message, Severity   string
	RuleName, Metric, Value, Threshold string
}

// SetIntegrationsHook registers the FIRE-only integrations dispatcher. nil
// (the default) disables it. The engine calls it on its own goroutine, after
// the edge has committed.
func (s *AlertService) SetIntegrationsHook(fn func(ctx context.Context, ev AlertFireEvent)) {
	s.integrationsHook = fn
}

// fireIntegrations dispatches an opening edge to the hook, detached from the
// evaluation tick (a slow or failing integration never delays or fails the
// engine; delivery history is the integration service's own).
func (s *AlertService) fireIntegrations(rule AlertRule, value float64, message string) {
	fn := s.integrationsHook
	if fn == nil {
		return
	}
	ev := AlertFireEvent{
		SiteID: rule.SiteID, Title: rule.Name, Message: message,
		Severity: rule.severityOrDefault(), RuleName: rule.Name, Metric: rule.Metric,
		Value:     fmt.Sprintf("%.2f", value),
		Threshold: fmt.Sprintf("%g", rule.Threshold),
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("alert integrations hook panicked", "rule", rule.RuleID, "panic", r)
			}
		}()
		fn(context.Background(), ev)
	}()
}
