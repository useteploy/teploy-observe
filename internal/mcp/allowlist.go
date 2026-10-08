package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/useteploy/teploy-observe/internal/explorer"
)

// The data boundary.
//
// Observe holds personal data — raw events keyed to a distinct_id, sessions,
// replays, heatmaps, cohort membership, LLM prompts. Dash's MCP exposes
// deployment metadata, where the worst case is an env var *name*. Observe's
// worst case is an identified user's behaviour handed to a model and out to
// whatever it is plugged into. A raw query tool over an analytics warehouse is
// an exfiltration primitive, so MCP reaches ANALYTICS AGGREGATES ONLY.
//
// That is enforced by an ALLOWLIST, never a denylist, and the direction is the
// whole point: when a migration adds a table, an allowlist refuses it until
// somebody decides otherwise, and a denylist serves it until somebody
// remembers. This file is that decision point. Adding a table here is a
// deliberate act with a reviewer.
//
// Each SELECT resolves source columns against its own relations. Derived tables
// and CTEs expose only validated projections; output aliases grant no input
// privilege. Only explicitly approved pure functions are callable. Unsupported
// syntax fails closed. Wildcards (except count(*)) and schema names are refused.
//
// Read-only is a SEPARATE axis from sensitivity and does not overlap with any
// of this: a read-only token governs mutation, and these rules govern what
// data exists to be read at all. v1 ships no PII-enabled token of any kind.
//
// REVERSAL CONDITION: if a consumer genuinely needs person-level data, it gets
// an explicitly granted SEPARATE scope with its own audit trail — a new token
// capability, a new tool, and its own entry in the audit action namespace.
// Never by widening the default allowlist below, and never by relaxing these
// rules for everyone to serve one caller.

// tableSpec is one allowlisted table: the columns MCP may name, and a line of
// prose the schema card hands the model.
type tableSpec struct {
	columns []string
	note    string
}

// allowedTables is the entire reachable surface of `observe_query`,
// `observe_explain` and any SQL `observe_ask` generates.
//
// Deliberately absent, and why:
//
//	events, events_recent, sessions   raw per-visitor rows (distinct_id, session_id)
//	error_events, logs, spans         payloads carrying request paths, ids, user values
//	llm_traces                        prompts and completions
//	replay_sessions, replay_events    session recordings — out of scope entirely
//	click_heatmaps                    behaviour recordings — out of scope entirely
//	cohorts, groups, group_members    membership is person-level by definition
//	flag_evaluations,                 one row per (user, evaluation)
//	experiment_exposures/conversions
//	survey_responses, feedback        free text a person wrote
//	link_clicks, tracked_links        per-click rows
//	users, admin_users, api_keys,     credentials and account records
//	share_links, sso_configs,
//	instance_settings
//	audit_events                      the audit trail itself; reading it is its own scope
//	boards, dashboards, saved_views,  authored config, no analytics value to an agent
//	scheduled_exports, webhooks, …
var allowedTables = map[string]tableSpec{
	"stats_hourly": {
		columns: []string{"tenant_id", "site_id", "ts_bucket", "pathname", "event_type",
			"pageviews", "visitors", "sessions", "bounces", "total_duration", "version"},
		note: "hourly traffic rollup, ts_bucket is epoch ms at the hour",
	},
	"stats_daily": {
		columns: []string{"tenant_id", "site_id", "ts_bucket", "pathname", "event_type",
			"referrer", "browser", "os", "country", "device",
			"utm_source", "utm_medium", "utm_campaign",
			"pageviews", "visitors", "sessions", "bounces", "total_duration", "version"},
		note: "daily traffic rollup with acquisition and device dimensions",
	},
	"service_stats": {
		columns: []string{"tenant_id", "site_id", "service_name", "operation_name", "ts_bucket",
			"request_count", "error_count", "duration_sum", "duration_min", "duration_max",
			"p50_ms", "p95_ms", "p99_ms", "version"},
		note: "RED metrics per service and operation, hourly buckets",
	},
	"service_dependencies": {
		columns: []string{"tenant_id", "site_id", "src_service", "dst_service",
			"call_count", "error_count", "avg_duration", "ts_bucket", "version"},
		note: "service call graph edges",
	},
	"issues": {
		columns: []string{"issue_id", "tenant_id", "site_id", "group_hash", "title", "culprit",
			"level", "status", "first_seen", "last_seen", "event_count", "user_count",
			"release_tag", "version"},
		note: "grouped error issues; event_count and user_count are counts, not identities",
	},
	"performance_issues": {
		columns: []string{"issue_id", "tenant_id", "site_id", "detector_name", "fingerprint",
			"title", "description", "severity", "count", "first_seen", "last_seen"},
		note: "detected performance problems (trace_id is withheld — it joins to raw spans)",
	},
	"host_metrics": {
		columns: []string{"metric_id", "tenant_id", "site_id", "hostname", "timestamp",
			"cpu_percent", "memory_percent", "memory_used_mb", "memory_total_mb",
			"disk_percent", "disk_used_gb", "disk_total_gb",
			"net_rx_bytes", "net_tx_bytes", "load_1m", "load_5m", "load_15m"},
		note: "infrastructure metrics per host",
	},
	"metric_points": {
		columns: []string{"site_id", "tenant_id", "metric_name", "metric_kind", "service_name",
			"ts_ns", "value", "histogram", "is_monotonic", "aggregation_temporality"},
		note: "OTLP metric points (the free-form `attributes` label bag is withheld)",
	},
	"incidents": {
		columns: []string{"incident_id", "tenant_id", "site_id", "title", "description",
			"severity", "source", "rule_id", "started_at", "ended_at", "created_by", "updated_at"},
		note: "declared incidents; append-only, collapse with argMax(..., updated_at) per incident_id",
	},
	"alert_rules": {
		columns: []string{"rule_id", "tenant_id", "site_id", "name", "metric", "operator",
			"threshold", "window_minutes", "check_interval", "cooldown", "enabled",
			"created_by", "created_at", "version"},
		note: "alerting configuration",
	},
	"uptime_monitors": {
		columns: []string{"monitor_id", "tenant_id", "site_id", "name", "url", "method",
			"interval_secs", "expected_status", "enabled", "created_at", "version"},
		note: "uptime check configuration",
	},
	"uptime_results": {
		columns: []string{"result_id", "tenant_id", "monitor_id", "site_id", "timestamp",
			"status_code", "response_ms", "is_up", "error_message"},
		note: "uptime check results",
	},
	"cron_monitors": {
		columns: []string{"cron_id", "tenant_id", "site_id", "name", "slug", "schedule",
			"grace_period", "enabled", "created_at", "version"},
		note: "cron monitor configuration (ping_token is withheld — it is a credential)",
	},
	"feature_flags": {
		columns: []string{"flag_id", "tenant_id", "site_id", "flag_key", "name", "description",
			"flag_type", "enabled", "rollout_pct", "variants", "created_at", "version"},
		note: "feature flag definitions (`targeting` is withheld — its rules can name individual users)",
	},
	"experiments": {
		columns: []string{"experiment_id", "tenant_id", "site_id", "name", "flag_key",
			"goal_metric", "goal_value", "status", "min_sample", "started_at", "ended_at",
			"created_at", "version"},
		note: "A/B experiment definitions",
	},
	"goals": {
		columns: []string{"goal_id", "tenant_id", "site_id", "name", "goal_type", "goal_value",
			"created_at", "version"},
		note: "conversion goal definitions",
	},
	"sites": {
		columns: []string{"site_id", "tenant_id", "domain", "name", "created_at"},
		note:    "site registry (session_salt is withheld — it is a credential)",
	},
}

// AllowedTables returns the allowlisted table names, sorted.
func AllowedTables() []string {
	out := make([]string, 0, len(allowedTables))
	for t := range allowedTables {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// AllowedColumns returns the allowlisted columns of one table, sorted, or nil
// if the table is not allowlisted.
func AllowedColumns(table string) []string {
	spec, ok := allowedTables[strings.ToLower(table)]
	if !ok {
		return nil
	}
	out := append([]string(nil), spec.columns...)
	sort.Strings(out)
	return out
}

// SchemaCard renders the allowlist as the prose an LLM reasons over. It is
// deliberately built from the ALLOWLIST and not from information_schema: a
// model handed the real schema drafts queries against tables it will then be
// refused, which reads as a broken tool rather than a boundary, and mentioning
// the excluded tables at all tells the model they exist.
func SchemaCard() string {
	var b strings.Builder
	b.WriteString("Tables (these are the only readable tables — anything else is refused):\n")
	for _, name := range AllowedTables() {
		spec := allowedTables[name]
		b.WriteString("- " + name + ": ")
		b.WriteString(strings.Join(spec.columns, ", "))
		if spec.note != "" {
			b.WriteString("\n  (" + spec.note + ")")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nNotes:\n")
	b.WriteString("- Timestamps are BIGINT milliseconds since the Unix epoch (metric_points.ts_ns is nanoseconds).\n")
	b.WriteString("- Nucleus reports BIGINT over the wire as text; CAST(col AS BIGINT) before comparing ranges.\n")
	b.WriteString("- ORDER BY resolves against the select list's OUTPUT names only, and silently ignores a term\n")
	b.WriteString("  it cannot resolve. Name the alias, and give every LIMIT a total order.\n")
	b.WriteString("- The rollup tables carry a `version` column and do NOT collapse on their own. Read them as\n")
	b.WriteString("  SELECT keys..., argMax(col, version) AS col ... GROUP BY keys, or the numbers double-count.\n")
	b.WriteString("- SELECT * is refused. Name the columns. count(*) is fine.\n")
	b.WriteString("- Spell every alias with AS (`count(*) AS hits`); an implicit alias is refused.\n")
	return b.String()
}

// Check enforces the data boundary on one SQL statement. It is the ONLY gate:
// a hand-written query from `observe_query` and a model-generated query from
// `observe_ask` both pass through this same function, so there is no path that
// reaches the database having been checked more loosely.
//
// It layers on top of explorer.ClassifyReadOnlySQL rather than replacing it —
// that classifier already rejects writes, DDL and stacked statements, and is
// the same one the dashboard's explorer uses.
func Check(sql string) error {
	if _, err := explorer.ClassifyReadOnlySQL(sql); err != nil {
		return err
	}
	toks, err := lex(sql)
	if err != nil {
		return err
	}

	_, err = checkScope(toks, nil)
	return err
}

// isWildcard distinguishes the `*` of `SELECT *` from the `*` of `a * b`.
//
// A wildcard follows a keyword (SELECT, DISTINCT), a comma, or the dot of a
// qualified `t.*`. Multiplication follows a value — an identifier, a number or
// a closing paren — and `count(*)`'s star follows the opening paren of a
// function call, which is the one wildcard that is allowed through.
func isWildcard(toks []token, i int) bool {
	p := prev(toks, i-1)
	if p == nil {
		return true
	}
	switch p.kind {
	case tokNumber:
		return false
	case tokIdent:
		return sqlKeywords[p.upper]
	case tokPunct:
		return p.text == "," || p.text == "." || p.text == "("
	}
	return true
}

func next(toks []token, i int) *token {
	if i < 0 || i >= len(toks) {
		return nil
	}
	return &toks[i]
}

func prev(toks []token, i int) *token { return next(toks, i) }

func isWord(toks []token, i int, upper string) bool {
	t := next(toks, i)
	return t != nil && t.kind == tokIdent && t.upper == upper
}

// --- lexer ------------------------------------------------------------------

type tokKind int

const (
	tokIdent tokKind = iota
	tokPunct
	tokNumber
)

type token struct {
	quoted bool
	kind   tokKind
	text   string // identifiers lowercased; punctuation verbatim
	upper  string
}

// lex tokenizes SQL into identifiers, numbers and single-character
// punctuation, discarding comments and string literals. Discarding literals is
// what stops `/* */ FROM persons` and `'... persons ...'` from either hiding a
// table reference or inventing one.
func lex(sql string) ([]token, error) {
	var out []token
	i, n := 0, len(sql)
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 >= n {
				return nil, fmt.Errorf("unterminated block comment")
			}
			i += 2
		case c == '\'':
			i++
			closed := false
			for i < n {
				if sql[i] == '\'' {
					if i+1 < n && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if sql[i] == '\\' {
					return nil, fmt.Errorf("backslash string escapes are not supported over MCP")
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string literal")
			}
		case c == '"':
			// A quoted identifier is still an identifier. The explorer's lexer
			// skips these outright; here they must be tokenized, or
			// `FROM "events"` would name no table at all and pass unchecked.
			i++
			start := i
			for i < n && sql[i] != '"' {
				i++
			}
			word := sql[start:i]
			if i >= n {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			if i < n {
				i++
			}
			out = append(out, token{kind: tokIdent, quoted: true, text: strings.ToLower(word), upper: strings.ToUpper(word)})
		case c >= '0' && c <= '9':
			start := i
			for i < n && ((sql[i] >= '0' && sql[i] <= '9') || sql[i] == '.') {
				i++
			}
			out = append(out, token{kind: tokNumber, text: sql[start:i]})
		case isIdentStart(c):
			start := i
			for i < n && isIdentPart(sql[i]) {
				i++
			}
			word := sql[start:i]
			out = append(out, token{kind: tokIdent, text: strings.ToLower(word), upper: strings.ToUpper(word)})
		default:
			out = append(out, token{kind: tokPunct, text: string(c)})
			i++
		}
	}
	return out, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// sqlKeywords are the words that are never column references. A word missing
// from this list is treated as an identifier and checked against the column
// allowlist, so the failure mode of an incomplete list is a refused query, not
// a leaked one.
var sqlKeywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "BY": true, "ORDER": true,
	"HAVING": true, "LIMIT": true, "OFFSET": true, "AS": true, "ON": true, "USING": true,
	"JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true, "OUTER": true, "FULL": true,
	"CROSS": true, "NATURAL": true, "LATERAL": true, "UNION": true, "INTERSECT": true,
	"EXCEPT": true, "ALL": true, "DISTINCT": true, "AND": true, "OR": true, "NOT": true,
	"IN": true, "IS": true, "NULL": true, "LIKE": true, "ILIKE": true, "SIMILAR": true,
	"BETWEEN": true, "CASE": true, "WHEN": true, "THEN": true, "ELSE": true, "END": true,
	"ASC": true, "DESC": true, "NULLS": true, "FIRST": true, "LAST": true, "WITH": true,
	"RECURSIVE": true, "EXPLAIN": true, "ANALYZE": true, "VERBOSE": true, "SHOW": true,
	"DESCRIBE": true, "DESC_": true, "CAST": true, "INTERVAL": true, "TRUE": true,
	"FALSE": true, "EXISTS": true, "ANY": true, "SOME": true, "OVER": true,
	"PARTITION": true, "WINDOW": true, "FILTER": true, "ROWS": true, "RANGE": true,
	"PRECEDING": true, "FOLLOWING": true, "CURRENT": true, "ROW": true, "UNBOUNDED": true,
	"FOR": true, "ESCAPE": true, "COLLATE": true, "AT": true, "TIME": true, "ZONE": true,
	// Cast targets.
	"TEXT": true, "BIGINT": true, "INTEGER": true, "INT": true, "SMALLINT": true,
	"DOUBLE": true, "PRECISION": true, "REAL": true, "NUMERIC": true, "DECIMAL": true,
	"BOOLEAN": true, "BOOL": true, "TIMESTAMP": true, "TIMESTAMPTZ": true, "DATE": true,
	"VARCHAR": true, "CHAR": true, "JSON": true, "JSONB": true, "UUID": true, "FLOAT": true,
	// EXTRACT / date_trunc field names.
	"EPOCH": true, "YEAR": true, "MONTH": true, "DAY": true, "HOUR": true, "MINUTE": true,
	"SECOND": true, "WEEK": true, "QUARTER": true, "DOW": true, "DOY": true,
	"MILLISECONDS": true, "MICROSECONDS": true, "TIMEZONE": true,
}
