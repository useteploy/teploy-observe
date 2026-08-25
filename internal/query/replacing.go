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
// replacingKeys holds the ORDER BY key of each replacing table this package
// reads, exactly as declared in internal/schema/migrations/001_analytics.up.sql.
var replacingKeys = map[string][]string{
	"stats_hourly": {"tenant_id", "site_id", "ts_bucket", "pathname", "event_type"},
	"stats_daily": {
		"tenant_id", "site_id", "ts_bucket", "pathname", "event_type",
		"referrer", "browser", "os", "country", "device",
		"utm_source", "utm_medium", "utm_campaign",
	},
	"sessions": {"tenant_id", "site_id", "session_id"},
}

// LatestRows renders a derived table that collapses table to one row per
// ORDER BY key, keeping the highest-version value of each column in cols.
// The result is a parenthesised sub-select intended to replace a bare table
// name in a FROM clause; give it an alias at the call site.
//
// where is applied inside the derived table, before the collapse. Every
// column it names must exist on table, but it need not be part of the key:
// the non-key columns of a replacing row are stable across versions in
// practice (a session's browser does not change between rollup passes), so
// filtering before the collapse and after it agree.
//
// cols must not include a key column — those are selected verbatim.
func LatestRows(table string, cols []string, where string) string {
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
	return fmt.Sprintf("(SELECT %s FROM %s WHERE %s GROUP BY %s)",
		strings.Join(sel, ", "), table, where, strings.Join(keys, ", "))
}
