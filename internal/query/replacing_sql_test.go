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
	entries, err := os.ReadDir("../schema/migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	// One table can be declared in more than one migration. Rename-aside
	// rebuilds (027/028/033-036, and 039 for replay_sessions) re-declare a
	// table with a different engine or ORDER BY on purpose, so earlier
	// declarations are history, not drift. Migrations run in filename
	// order, so the LAST declaration per table is the live schema; the
	// guard below pins the registry against that final shape.
	type decl struct {
		file      string
		orderBy   []string
		replacing bool
	}
	decls := map[string]decl{}
	create := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)ORDER BY \(([^)]*)\)`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		src, err := os.ReadFile("../schema/migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			var cols []string
			for _, c := range strings.Split(m[3], ",") {
				cols = append(cols, strings.TrimSpace(c))
			}
			decls[m[1]] = decl{
				file:      e.Name(),
				orderBy:   cols,
				replacing: strings.Contains(m[2], "replacing_mergetree"),
			}
		}
	}

	for table, keys := range replacingKeys {
		d, found := decls[table]
		if !found {
			t.Fatalf("%s is registered for latest-version reads but no migration declares it", table)
		}
		if strings.Join(d.orderBy, ",") != strings.Join(keys, ",") {
			t.Fatalf("%s (%s): replacingKeys is %v but the migration declares ORDER BY %v",
				table, d.file, keys, d.orderBy)
		}
		// The engine must actually be replacing, or argMax over `version`
		// is selecting on a column that means nothing.
		if !d.replacing {
			t.Fatalf("%s (%s) is registered for latest-version reads but is not a replacing_mergetree",
				table, d.file)
		}
	}
}

// TestReplacingKeys_VersionColumnIsNamedVersion guards the one assumption
// LatestRows bakes in: it writes argMax(col, version) literally. Two replacing
// tables version on something else (cohorts on updated_at, performance_issues
// on last_seen); registering either here would silently order by a column that
// is not the version.
func TestReplacingKeys_VersionColumnIsNamedVersion(t *testing.T) {
	entries, err := os.ReadDir("../schema/migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	// Same last-declaration-wins rule as TestReplacingKeys_MatchTheMigration:
	// a rename-aside rebuild redeclares the table, and only the final
	// declaration is the live schema.
	type decl struct {
		file string
		body string
	}
	decls := map[string]decl{}
	create := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)ORDER BY \(`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		src, err := os.ReadFile("../schema/migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			decls[m[1]] = decl{file: e.Name(), body: m[2]}
		}
	}
	verCol := regexp.MustCompile(`version_column\s*=\s*'([^']+)'`)
	for table, d := range decls {
		if _, registered := replacingKeys[table]; !registered {
			continue
		}
		v := verCol.FindStringSubmatch(d.body)
		if v == nil || v[1] != "version" {
			t.Fatalf("%s (%s): LatestRows assumes a column named `version`, but the migration declares %v",
				table, d.file, v)
		}
	}
}
