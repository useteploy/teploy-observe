package mcp

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/explorer"
)

// The guard is load-bearing, and this test is the proof.
//
// Observe already had a read-only SQL classifier — the one the dashboard's
// explorer uses — and it ACCEPTS every one of these statements, because
// rejecting writes is all it was ever asked to do. If Check delegated the data
// boundary to it, or if Check were removed, MCP would serve raw person-level
// rows. Everything else in this file only matters because of this.
func TestReadOnlyClassifierAloneDoesNotStopPersonLevelReads(t *testing.T) {
	personLevel := []string{
		"SELECT distinct_id, url FROM events",
		"SELECT session_id, entry_url FROM sessions",
		"SELECT session_id FROM replay_sessions",
		"SELECT prompt, completion FROM llm_traces",
		"SELECT password_hash FROM users",
		"SELECT session_salt FROM sites",
	}
	for _, sql := range personLevel {
		if _, err := explorer.ClassifyReadOnlySQL(sql); err != nil {
			t.Fatalf("premise broken — the read-only classifier rejected %q: %v", sql, err)
		}
		if err := Check(sql); err == nil {
			t.Errorf("Check accepted person-level SQL: %q", sql)
		}
	}
}

func TestCheckAllowsAggregateQueries(t *testing.T) {
	ok := []string{
		"SELECT ts_bucket, sum(pageviews) AS views FROM stats_daily GROUP BY ts_bucket ORDER BY ts_bucket",
		"SELECT country, sum(visitors) AS v FROM stats_daily WHERE site_id = 'default' GROUP BY country ORDER BY v DESC LIMIT 10",
		"SELECT count(*) AS n FROM issues WHERE status = 'open'",
		`SELECT service_name, argMax(p95_ms, version) AS p95 FROM service_stats GROUP BY service_name, ts_bucket ORDER BY p95 DESC`,
		"WITH daily AS (SELECT ts_bucket AS b, sum(pageviews) AS views FROM stats_daily GROUP BY ts_bucket) SELECT b, views FROM daily ORDER BY b",
		"SELECT d.country AS country, d.visitors AS visitors FROM stats_daily d WHERE CAST(d.ts_bucket AS BIGINT) > 0",
		"SELECT s.domain AS domain, i.title AS title FROM incidents i JOIN sites s ON s.site_id = i.site_id",
		"SELECT sum(total_duration) / sum(sessions) * 100 AS pct FROM stats_hourly",
		"EXPLAIN SELECT ts_bucket, pageviews FROM stats_hourly",
		"SELECT hostname, max(cpu_percent) AS cpu FROM host_metrics GROUP BY hostname",
	}
	for _, sql := range ok {
		if err := Check(sql); err != nil {
			t.Errorf("Check refused a legitimate aggregate query:\n  %s\n  %v", sql, err)
		}
	}
}

// The allowlist is checked at every nesting depth and through every syntax that
// can name a table, because a check that only looked at the first FROM would be
// trivially bypassed by putting the real read one level down.
func TestCheckRefusesDisallowedTablesAtAnyDepth(t *testing.T) {
	cases := map[string]string{
		"top-level":        "SELECT distinct_id FROM events",
		"subquery in FROM": "SELECT n FROM (SELECT count(*) AS n FROM events) AS x",
		"subquery in IN":   "SELECT ts_bucket FROM stats_daily WHERE site_id IN (SELECT site_id FROM api_keys)",
		"scalar subquery":  "SELECT (SELECT count(*) AS n FROM sessions) AS n",
		"EXISTS":           "SELECT ts_bucket FROM stats_daily WHERE EXISTS (SELECT 1 FROM replay_events)",
		"CTE body":         "WITH t AS (SELECT distinct_id AS d FROM events) SELECT d FROM t",
		"second CTE":       "WITH a AS (SELECT ts_bucket AS b FROM stats_daily), c AS (SELECT session_id AS s FROM sessions) SELECT b FROM a",
		"UNION arm":        "SELECT ts_bucket AS t FROM stats_daily UNION ALL SELECT timestamp AS t FROM error_events",
		"JOIN":             "SELECT d.country AS c FROM stats_daily d JOIN cohorts x ON x.site_id = d.site_id",
		"LEFT JOIN":        "SELECT d.country AS c FROM stats_daily d LEFT JOIN llm_traces l ON l.site_id = d.site_id",
		"comma join":       "SELECT d.country AS c FROM stats_daily d, group_members g",
		"quoted ident":     `SELECT distinct_id FROM "events"`,
		// An alias must not satisfy a table reference. If it did, this would
		// name the real events table and walk past the allowlist entirely.
		"alias shadowing a table": "SELECT 1 AS events FROM events",
		"alias shadow via join":   "SELECT 1 AS sessions, d.country AS c FROM stats_daily d, sessions",
		"mixed case":              "SELECT distinct_id FROM EvEnTs",
		"line comment":            "SELECT distinct_id\n-- harmless\nFROM events",
		"block comment":           "SELECT /* nothing to see */ distinct_id FROM events",
	}
	for name, sql := range cases {
		if err := Check(sql); err == nil {
			t.Errorf("%s: not refused: %q", name, sql)
		}
	}
}

// A schema-qualified reference is refused by shape, not by whether its base
// name happens to be absent from the allowlist — which is what keeps
// information_schema and pg_catalog unreachable even though the allowlist never
// mentions them.
func TestCheckRefusesQualifiedTableReferences(t *testing.T) {
	for _, sql := range []string{
		"SELECT table_name FROM information_schema.tables",
		"SELECT column_name FROM information_schema.columns",
		"SELECT relname FROM pg_catalog.pg_class",
		"SELECT ts_bucket FROM public.stats_daily",
	} {
		err := Check(sql)
		if err == nil {
			t.Errorf("qualified reference not refused: %q", sql)
			continue
		}
		if strings.Contains(sql, "information_schema") && !strings.Contains(err.Error(), "qualified") {
			t.Errorf("%q refused for the wrong reason: %v", sql, err)
		}
	}
}

// Withheld columns are unreachable even though their table is allowed. This is
// the second half of the boundary: a table-only allowlist would hand out
// sites.session_salt the moment `sites` was listed.
func TestCheckRefusesWithheldColumns(t *testing.T) {
	cases := map[string]string{
		"site session salt":  "SELECT session_salt FROM sites",
		"cron ping token":    "SELECT ping_token FROM cron_monitors",
		"flag targeting":     "SELECT targeting FROM feature_flags",
		"metric attributes":  "SELECT attributes FROM metric_points",
		"perf issue traceid": "SELECT trace_id FROM performance_issues",
		"aliased away":       "SELECT session_salt AS d FROM sites",
		"in a WHERE":         "SELECT domain FROM sites WHERE session_salt = 'x'",
		"in a GROUP BY":      "SELECT domain FROM sites GROUP BY domain, session_salt",
		"in an ORDER BY":     "SELECT domain FROM sites ORDER BY session_salt",
		"inside a function":  "SELECT count(session_salt) AS n FROM sites",
	}
	for name, sql := range cases {
		if err := Check(sql); err == nil {
			t.Errorf("%s: withheld column not refused: %q", name, sql)
		}
	}
	if err := Check("SELECT domain, name FROM sites"); err != nil {
		t.Fatalf("allowlisted columns of the same table were refused: %v", err)
	}
}

// A wildcard would defeat the column allowlist the moment a migration adds a
// column, so it is refused everywhere except as count(*).
func TestCheckRefusesWildcard(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM stats_daily",
		"SELECT d.* FROM stats_daily d",
		"SELECT ts_bucket, * FROM stats_daily",
	} {
		if err := Check(sql); err == nil {
			t.Errorf("wildcard not refused: %q", sql)
		}
	}
	for _, sql := range []string{
		"SELECT count(*) AS n FROM stats_daily",
		"SELECT sum(pageviews) / count(*) AS avg FROM stats_daily",
		"SELECT pageviews * 2 AS doubled FROM stats_daily",
		"SELECT (pageviews + visitors) * 3 AS x FROM stats_daily",
	} {
		if err := Check(sql); err != nil {
			t.Errorf("legitimate * refused: %q -> %v", sql, err)
		}
	}
}

// Writes and stacked statements are still refused — Check layers on the
// existing classifier rather than replacing it.
func TestCheckStillRefusesWritesAndStackedStatements(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO stats_daily (site_id) VALUES ('x')",
		"UPDATE stats_daily SET pageviews = 0",
		"DROP TABLE stats_daily",
		"SELECT ts_bucket FROM stats_daily; DROP TABLE stats_daily",
		"WITH t AS (SELECT 1) INSERT INTO stats_daily (site_id) SELECT 'x'",
	} {
		if err := Check(sql); err == nil {
			t.Errorf("not refused: %q", sql)
		}
	}
}

// EXTRACT(field FROM value) spells FROM without naming a table; treating it as
// a table clause would refuse a perfectly ordinary query.
func TestCheckHandlesExtractFrom(t *testing.T) {
	if err := Check("SELECT extract(epoch FROM ts_bucket) AS e FROM stats_daily"); err != nil {
		t.Fatalf("EXTRACT ... FROM was misread as a table clause: %v", err)
	}
}

func TestSchemaCardIsBuiltFromTheAllowlist(t *testing.T) {
	card := SchemaCard()
	for _, table := range AllowedTables() {
		if !strings.Contains(card, table) {
			t.Errorf("schema card omits allowlisted table %q", table)
		}
	}
	// Naming an excluded table in the card would both invite refused drafts and
	// tell the model the data exists.
	for _, table := range []string{"events", "sessions", "replay_sessions", "cohorts", "llm_traces", "users"} {
		for _, line := range strings.Split(card, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "- "+table+":") {
				t.Errorf("schema card names excluded table %q", table)
			}
		}
	}
}

// Person-level and recording tables are absent from the allowlist by name.
// Redundant with the behavioural tests on purpose: this one fails if somebody
// adds a table to the map without reading why the map exists.
func TestAllowlistExcludesPersonLevelTables(t *testing.T) {
	for _, table := range []string{
		"events", "events_recent", "sessions", "error_events", "logs", "spans",
		"llm_traces", "replay_sessions", "replay_events", "click_heatmaps",
		"cohorts", "groups", "group_members", "flag_evaluations",
		"experiment_exposures", "experiment_conversions", "survey_responses",
		"feedback", "link_clicks", "users", "admin_users", "api_keys",
		"share_links", "sso_configs", "instance_settings", "audit_events",
	} {
		if _, ok := allowedTables[table]; ok {
			t.Errorf("%q is allowlisted — MCP reaches analytics aggregates only", table)
		}
	}
}
