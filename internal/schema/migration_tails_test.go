package schema

import (
	"regexp"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"
)

// These offline guards verify the embedded scripts' storage contract. Populated
// relational copies and crash boundaries are exercised by migration_tails_test.py;
// the parent's serial Nucleus suite still owns native engine acceptance.
func TestTailMigrationsContract(t *testing.T) {
	cases := []struct {
		old, next, table, added, engine, version string
	}{
		{"022_metrics.up.sql", "064_metrics_stream_identity.up.sql", "metric_points", "stream_identity         TEXT NOT NULL DEFAULT ''", "mergetree", ""},
		{"015_performance_issues.up.sql", "065_performance_issue_version.up.sql", "performance_issues", "version        BIGINT NOT NULL", "replacing_mergetree", "version"},
	}
	withoutComments := regexp.MustCompile(`(?m)--[^\n]*`)
	for _, tc := range cases {
		t.Run(tc.next, func(t *testing.T) {
			raw, err := migrationsFS.ReadFile("migrations/" + tc.next)
			if err != nil {
				t.Fatal(err)
			}
			sql := withoutComments.ReplaceAllString(string(raw), "")
			parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(sql), ";"), ";")
			if len(parts) != 3 {
				t.Fatalf("expected exactly rename, create, copy; got %d statements", len(parts))
			}
			aside := tc.table + "_pre" + tc.next[:3]
			if strings.TrimSpace(parts[0]) != "ALTER TABLE "+tc.table+" RENAME TO "+aside {
				t.Fatal("rename must run first and refuse an existing recovery artifact")
			}
			create := strings.TrimSpace(parts[1])
			if !strings.HasPrefix(create, "CREATE TABLE "+tc.table+" (") {
				t.Fatal("create must refuse unexpected existing destination")
			}
			if !strings.Contains(create, tc.added) || !strings.Contains(create, "engine = '"+tc.engine+"'") {
				t.Fatal("new column or storage engine contract missing")
			}
			if tc.version != "" && !strings.Contains(create, "version_column = '"+tc.version+"'") {
				t.Fatal("snapshot replacement must use independent version")
			}
			old, err := migrationsFS.ReadFile("migrations/" + tc.old)
			if err != nil {
				t.Fatal(err)
			}
			oldSQL := withoutComments.ReplaceAllString(string(old), "")
			start, end := strings.Index(oldSQL, "(\n"), strings.Index(oldSQL, ") WITH")
			var columns []string
			for _, line := range strings.Split(oldSQL[start+2:end], "\n") {
				if fields := strings.Fields(line); len(fields) > 0 {
					columns = append(columns, fields[0])
				}
			}
			projection := strings.Join(columns, ", ")
			backfill, added := "''", "stream_identity"
			if tc.version != "" {
				backfill, added = "last_seen", "version"
			}
			wantCopy := "INSERT INTO " + tc.table + " (" + projection + ", " + added + ")\nSELECT " + projection + ", " + backfill + "\nFROM " + aside
			if strings.TrimSpace(parts[2]) != wantCopy {
				t.Fatal("copy must preserve every original column/physical row and append only the explicit backfill")
			}
		})
	}
}

func TestTailMigrationsEmbeddedPlan(t *testing.T) {
	plan, err := nucleus.LoadMigrations(migrationsFS)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{64, 65} {
		count := 0
		for _, migration := range plan {
			if migration.Version == version {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("migration %d occurs %d times in embedded plan", version, count)
		}
	}
}
