package query

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestLatestRows_ShapesTheQuery pins the read path itself. The integration
// tests assert the numbers, but a scratch Nucleus with everything still in its
// memtable collapses duplicate keys on write, so on that engine they pass even
// against the old bare-SUM code. This test does not depend on the engine: it
// fails the moment a rollup read stops selecting the latest version.
func TestLatestRows_ShapesTheQuery(t *testing.T) {
	got := LatestRows("stats_hourly", []string{"pageviews"}, "site_id = $1")
	want := "(SELECT tenant_id, site_id, ts_bucket, pathname, event_type, " +
		"argMax(pageviews, version) AS pageviews FROM stats_hourly " +
		"WHERE site_id = $1 GROUP BY tenant_id, site_id, ts_bucket, pathname, event_type)"
	if got != want {
		t.Fatalf("latest-version sub-select changed shape\n got: %s\nwant: %s", got, want)
	}

	// An empty filter must still produce valid SQL rather than a dangling WHERE.
	if s := LatestRows("sessions", []string{"first_ts"}, ""); !strings.Contains(s, "WHERE 1 = 1") {
		t.Fatalf("empty where produced invalid SQL: %s", s)
	}
}

// TestReplacingKeys_MatchTheMigration is the guard that stops the two halves
// drifting. argMax collapses to one row per GROUP BY key, so the key here must
// be exactly the table's declared ORDER BY: too few columns silently merges
// distinct rows together, too many leaves duplicates in place. Both are silent.
func TestReplacingKeys_MatchTheMigration(t *testing.T) {
	src, err := os.ReadFile("../schema/migrations/001_analytics.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(src)

	for table, keys := range replacingKeys {
		// The ORDER BY that follows this table's CREATE TABLE.
		idx := strings.Index(sql, "CREATE TABLE IF NOT EXISTS "+table+" (")
		if idx < 0 {
			t.Fatalf("%s is not declared in 001_analytics.up.sql", table)
		}
		rest := sql[idx:]
		m := regexp.MustCompile(`(?s)ORDER BY \(([^)]*)\)`).FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("%s: no ORDER BY found", table)
		}
		var declared []string
		for _, c := range strings.Split(m[1], ",") {
			declared = append(declared, strings.TrimSpace(c))
		}
		if strings.Join(declared, ",") != strings.Join(keys, ",") {
			t.Fatalf("%s: replacingKeys is %v but the migration declares ORDER BY %v",
				table, keys, declared)
		}
		// The engine must actually be replacing, or argMax over `version` is
		// selecting on a column that means nothing.
		head := rest[:strings.Index(rest, "ORDER BY")]
		if !strings.Contains(head, "replacing_mergetree") {
			t.Fatalf("%s is registered for latest-version reads but is not a replacing_mergetree", table)
		}
	}
}
