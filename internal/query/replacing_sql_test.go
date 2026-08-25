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
	// One table can be declared in more than one migration (033 recreates the
	// rollups), so collect every declaration and require them all to agree.
	type decl struct {
		file      string
		orderBy   []string
		replacing bool
	}
	decls := map[string][]decl{}
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
			decls[m[1]] = append(decls[m[1]], decl{
				file:      e.Name(),
				orderBy:   cols,
				replacing: strings.Contains(m[2], "replacing_mergetree"),
			})
		}
	}

	for table, keys := range replacingKeys {
		found := decls[table]
		if len(found) == 0 {
			t.Fatalf("%s is registered for latest-version reads but no migration declares it", table)
		}
		for _, d := range found {
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
	create := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)ORDER BY \(`)
	verCol := regexp.MustCompile(`version_column\s*=\s*'([^']+)'`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		src, err := os.ReadFile("../schema/migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			if _, registered := replacingKeys[m[1]]; !registered {
				continue
			}
			v := verCol.FindStringSubmatch(m[2])
			if v == nil || v[1] != "version" {
				t.Fatalf("%s (%s): LatestRows assumes a column named `version`, but the migration declares %v",
					m[1], e.Name(), v)
			}
		}
	}
}
