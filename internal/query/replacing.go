package query

import (
	"fmt"
	"strings"
)

// Latest-version reads for Nucleus ReplacingMergeTree tables.
//
// A replacing table collapses rows that share its ORDER BY key only when the
// engine merges the segments they live in. Nucleus does that opportunistically
// and, for the analytics rollups, effectively never: on the live instance the
// oldest un-collapsed duplicate bucket was two months old, and `stats_hourly`
// held 740 duplicated keys out of 1956 rows. `FINAL` parses but is silently
// ignored — it returns exactly the same numbers as a bare read — so it cannot
// be used to force the collapse.
//
// The only form that works is argMax over the version column, grouped by the
// declared ORDER BY key. Verified against the live engine: a bare
// SUM(pageviews) reported 158 for a window whose raw events prove 72, and the
// argMax form reported exactly 72.
//
// replacingKeys holds the ORDER BY key of each replacing table read through
// this helper, exactly as declared in internal/schema/migrations/.
//
// Every entry here versions on a column literally named `version`; a table that
// versions on something else (cohorts on updated_at, performance_issues on
// last_seen) must not be added without teaching LatestRows which column to use.
var replacingKeys = map[string][]string{
	"stats_hourly": {"tenant_id", "site_id", "ts_bucket", "pathname", "event_type"},
	"stats_daily": {
		"tenant_id", "site_id", "ts_bucket", "pathname", "event_type",
		"referrer", "browser", "os", "country", "device",
		"utm_source", "utm_medium", "utm_campaign",
	},
	"sessions": {"tenant_id", "site_id", "session_id"},

	// 002_errors
	"issues": {"tenant_id", "site_id", "issue_id"},
	// 003_tracing
	"service_stats": {"tenant_id", "site_id", "service_name", "operation_name", "ts_bucket"},
	// 004_platform
	"alert_rules": {"tenant_id", "site_id", "rule_id"},
	"webhooks":    {"tenant_id", "site_id", "webhook_id"},
	// 005_features
	"goals":            {"tenant_id", "site_id", "goal_id"},
	"uptime_monitors":  {"tenant_id", "site_id", "monitor_id"},
	"cron_monitors":    {"tenant_id", "site_id", "cron_id"},
	"dashboards":       {"tenant_id", "site_id", "dashboard_id"},
	"dashboard_panels": {"tenant_id", "dashboard_id", "panel_id"},
	// 006_wave1
	"integrations":     {"tenant_id", "site_id", "integration_id"},
	"saved_views":      {"tenant_id", "site_id", "view_id"},
	"report_schedules": {"tenant_id", "site_id", "schedule_id"},
	// 007_wave2
	"feature_flags": {"tenant_id", "site_id", "flag_id"},
	"experiments":   {"tenant_id", "site_id", "experiment_id"},
	"surveys":       {"tenant_id", "site_id", "survey_id"},
	// 008_wave4 — sso_configs is instance-wide, so its key carries no site_id.
	"sso_configs": {"tenant_id", "sso_id"},
	// 009_llm_infra
	"log_pipelines": {"tenant_id", "site_id", "pipeline_id"},
}

// Keys returns the registered ORDER BY key of a replacing table, or nil.
func Keys(table string) []string {
	return replacingKeys[table]
}

// LatestRows renders a derived table that collapses table to one row per
// ORDER BY key, keeping the highest-version value of each column in cols.
// The result is a parenthesised sub-select intended to replace a bare table
// name in a FROM clause; give it an alias at the call site.
//
// where is applied inside the derived table, BEFORE the collapse, so it may
// only name columns that are stable across versions — the key itself, or a
// value no update path rewrites (a session's browser does not change between
// rollup passes; an issue's group_hash does not change between bumps).
//
// A column an update path DOES rewrite — enabled, status, last_seen — must be
// filtered by the caller OUTSIDE the derived table, against the alias. Filtering
// such a column inside is the exact bug this helper exists to fix: a soft-delete
// writes enabled='false' as a new version, and `WHERE enabled = 'true'` applied
// before the collapse still matches the superseded row and resurrects it.
//
// cols must not include a key column — those are selected verbatim.
func LatestRows(table string, cols []string, where string) string {
	return "(" + LatestSelect(table, cols, where) + ")"
}

// LatestSelect is LatestRows without the enclosing parentheses, for callers
// that run the collapse as a statement in its own right rather than splicing it
// into a FROM clause.
func LatestSelect(table string, cols []string, where string) string {
	keys, ok := replacingKeys[table]
	if !ok {
		panic("query: no ReplacingMergeTree key registered for table " + table)
	}
	sel := make([]string, 0, len(keys)+len(cols))
	sel = append(sel, keys...)
	for _, c := range cols {
		sel = append(sel, fmt.Sprintf("argMax(%s, version) AS %s", c, c))
	}
	if strings.TrimSpace(where) == "" {
		where = "1 = 1"
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s GROUP BY %s",
		strings.Join(sel, ", "), table, where, strings.Join(keys, ", "))
}
