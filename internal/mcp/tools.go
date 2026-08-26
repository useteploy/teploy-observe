package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Tool is one MCP tool. InputSchema is a JSON-Schema object; Run receives the
// already-decoded arguments and returns human/agent-readable text.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	ReadOnly    bool
	Destructive bool
	Run         func(ctx context.Context, args map[string]interface{}) (string, error)
}

// Backend is what the tools need from Observe. Implemented in
// cmd/observe over the EXISTING services — the explorer service, the aiquery
// service, the incidents service, the stats service, the flag service — each
// method a thin adapter that formats what the service already returns.
//
// Tools must never grow their own state or bypass the service layer: that
// invariant is ported verbatim from Dash's MCP, and it is what keeps the MCP
// answer and the dashboard answer the same answer.
//
// Note what is NOT here: no replay reader, no heatmap reader, no persons or
// cohort-membership reader. Those are out of scope entirely — there is no
// analytics question worth a session recording crossing this boundary — and
// leaving them off this interface is the cheapest possible enforcement.
type Backend interface {
	// ListTables returns the tables that exist in the database. The tool
	// intersects this with the allowlist, so a table the allowlist names but
	// the database lacks is not advertised, and a table the database has but
	// the allowlist omits is never named.
	ListTables(ctx context.Context) ([]string, error)

	// Query runs an already-allowlist-checked read-only SQL statement.
	Query(ctx context.Context, sql string) (string, error)

	// Explain returns the plan for an already-allowlist-checked statement.
	Explain(ctx context.Context, sql string) (string, error)

	// GenerateSQL turns a natural-language question into SQL using the
	// supplied schema card. The result is NOT trusted: the caller runs it
	// through the same allowlist gate a hand-written query goes through.
	GenerateSQL(ctx context.Context, question, schemaCard string) (string, error)

	// ActiveIncidents / IncidentsInRange wrap incidents.Service.
	ActiveIncidents(ctx context.Context, siteID string) (string, error)
	IncidentsInRange(ctx context.Context, siteID string, from, to int64) (string, error)

	// LiveStats reports how many visitors are active in the last `minutes`.
	LiveStats(ctx context.Context, siteID string, minutes int) (string, error)

	// ListFlags wraps flags.FlagService.List.
	ListFlags(ctx context.Context, siteID string) (string, error)
}

func schema(required []string, props map[string]interface{}) map[string]interface{} {
	if props == nil {
		props = map[string]interface{}{}
	}
	s := map[string]interface{}{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func intProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func strArg(args map[string]interface{}, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("missing required argument: %s", key)
	}
	return v, nil
}

// siteArg defaults to the single-tenant site every Observe install has.
func siteArg(args map[string]interface{}) string {
	if v, ok := args["site_id"].(string); ok && v != "" {
		return v
	}
	return "default"
}

func intArg(args map[string]interface{}, key string, def int) int {
	if n, ok := args[key].(float64); ok && n > 0 {
		return int(n)
	}
	return def
}

var siteProp = map[string]interface{}{
	"site_id": strProp("Site id (default: \"default\")"),
}

// Tools builds the v1 tool set over a backend. Everything here is read-only;
// the mutating set (create/close incident, evaluate flag) is a second pass and
// maps to the editor role.
func Tools(b Backend) []Tool {
	return []Tool{
		{
			Name: "observe_tables",
			Description: "List the tables and columns readable over MCP, with a note on what each holds. " +
				"Call this first: it is the complete readable surface. Observe's MCP server reaches analytics " +
				"AGGREGATES ONLY — an allowlist of rollup and configuration tables. Raw events, sessions, " +
				"session replays, heatmaps, cohort membership and LLM prompts are not reachable through any tool.",
			InputSchema: schema(nil, nil),
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				present, err := b.ListTables(ctx)
				if err != nil {
					return "", err
				}
				exists := make(map[string]bool, len(present))
				for _, t := range present {
					exists[strings.ToLower(t)] = true
				}
				var out strings.Builder
				out.WriteString("Readable tables (the allowlist; anything else is refused):\n\n")
				for _, name := range AllowedTables() {
					if !exists[name] {
						continue
					}
					out.WriteString(name + "\n  " + strings.Join(AllowedColumns(name), ", ") + "\n")
				}
				out.WriteString("\n")
				out.WriteString(SchemaCard())
				return out.String(), nil
			},
		},
		{
			Name: "observe_query",
			Description: "Run a read-only SQL query against Observe's analytics aggregates. Only SELECT / WITH; " +
				"only the tables and columns observe_tables lists; no SELECT *; alias with AS. " +
				"Results are capped at 100 rows. A query naming any other table is refused, not filtered.",
			InputSchema: schema([]string{"sql"}, map[string]interface{}{
				"sql": strProp("A single SELECT or WITH ... SELECT statement"),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sql, err := strArg(args, "sql")
				if err != nil {
					return "", err
				}
				if err := Check(sql); err != nil {
					return "", err
				}
				return b.Query(ctx, sql)
			},
		},
		{
			Name: "observe_explain",
			Description: "Return the query plan for a read-only SQL statement without running it. " +
				"Subject to the same table and column allowlist as observe_query.",
			InputSchema: schema([]string{"sql"}, map[string]interface{}{
				"sql": strProp("A single SELECT or WITH ... SELECT statement"),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sql, err := strArg(args, "sql")
				if err != nil {
					return "", err
				}
				if err := Check(sql); err != nil {
					return "", err
				}
				return b.Explain(ctx, sql)
			},
		},
		{
			Name: "observe_ask",
			Description: "Turn a natural-language question into SQL using Observe's own query assistant, so you " +
				"do not have to know the schema. Returns the SQL — run it with observe_query. The generated " +
				"statement passes through the SAME table and column allowlist a hand-written one does, so a " +
				"question that can only be answered from personal data comes back refused rather than answered.",
			InputSchema: schema([]string{"question"}, map[string]interface{}{
				"question": strProp("The analytics question, in plain language"),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				question, err := strArg(args, "question")
				if err != nil {
					return "", err
				}
				// The model is shown the ALLOWLIST as its schema, not the real
				// one: showing it tables it will then be refused wastes a round
				// trip and tells it those tables exist.
				sql, err := b.GenerateSQL(ctx, question, SchemaCard())
				if err != nil {
					return "", err
				}
				// Belt and braces. The schema card constrains what the model
				// SEES; this gate constrains what it can PRODUCE, and it is the
				// same function observe_query calls. A prompt is not a control.
				if err := Check(sql); err != nil {
					return "", fmt.Errorf("the generated query was refused by the data boundary (%w). "+
						"Ask a question that can be answered from the tables observe_tables lists", err)
				}
				return sql, nil
			},
		},
		{
			Name: "observe_list_incidents",
			Description: "List incidents for a site — open ones by default, or every incident overlapping a time " +
				"range when from/to are given (epoch milliseconds).",
			InputSchema: schema(nil, map[string]interface{}{
				"site_id": strProp("Site id (default: \"default\")"),
				"from":    intProp("Range start, epoch milliseconds. Requires `to`."),
				"to":      intProp("Range end, epoch milliseconds. Requires `from`."),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				site := siteArg(args)
				from, to := int64(intArg(args, "from", 0)), int64(intArg(args, "to", 0))
				if from > 0 && to > 0 {
					return b.IncidentsInRange(ctx, site, from, to)
				}
				return b.ActiveIncidents(ctx, site)
			},
		},
		{
			Name: "observe_live_stats",
			Description: "How many visitors are active on a site right now — a count, not a listing. " +
				"There is no tool that returns who they are.",
			InputSchema: schema(nil, map[string]interface{}{
				"site_id": strProp("Site id (default: \"default\")"),
				"minutes": intProp("Activity window in minutes (default 5, max 60)"),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				minutes := intArg(args, "minutes", 5)
				if minutes > 60 {
					minutes = 60
				}
				return b.LiveStats(ctx, siteArg(args), minutes)
			},
		},
		{
			Name:        "observe_list_flags",
			Description: "List a site's feature flags with their key, type, enabled state and rollout percentage.",
			InputSchema: schema(nil, siteProp),
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return b.ListFlags(ctx, siteArg(args))
			},
		},
	}
}

// ToolNames returns the names of a tool set, sorted — for docs and tests that
// pin the shipped surface.
func ToolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}
