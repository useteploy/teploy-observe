package backup

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/schema"
)

// TestTablesMatchSchema pins the backup table list to the migrations.
//
// Both directions matter and both used to be wrong. A name in Tables that is
// not a table dumps zero rows and dumpTable's "does not exist" handling reports
// it as a successful, empty table — so "monitors", "crons", "share_tokens" and
// a second copy of the report schedules under the name "reports" sat in the
// list for months while uptime monitors, cron monitors and share links were
// never captured. In the other direction a real table that is simply absent
// from the list is never read at all, which is how stats_daily — the one
// rollup with no retention policy — and 26 others went unbacked. Neither
// failure produces any output, so only a test comparing the list against the
// schema can catch them.
func TestTablesMatchSchema(t *testing.T) {
	inSchema := tablesFromMigrations(t)

	listed := make(map[string]bool, len(Tables))
	for _, name := range Tables {
		if listed[name] {
			t.Errorf("backup.Tables lists %q twice", name)
		}
		listed[name] = true
		if excuse, ok := ExcludedTables[name]; ok {
			t.Errorf("backup.Tables lists %q, which ExcludedTables says to skip (%s)", name, excuse)
		}
		if !inSchema[name] {
			t.Errorf("backup.Tables names %q, which no migration creates — it would dump zero rows and report success", name)
		}
	}

	var missing []string
	for name := range inSchema {
		if listed[name] || ExcludedTables[name] != "" || isRenameAsideArtifact(name) {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the migrations create %d table(s) that backup.Tables never reads, so they are silently absent from every archive: %s\n"+
			"add them to Tables, or to ExcludedTables with the reason they are deliberately skipped",
			len(missing), strings.Join(missing, ", "))
	}
}

// TestRestoreAllowlistTracksTables guards the coupling restore.go depends on:
// its allowlist is derived from Tables, so a table absent from Tables cannot be
// restored even from an archive that contains it.
func TestRestoreAllowlistTracksTables(t *testing.T) {
	for _, name := range Tables {
		if !restorableTables[name] {
			t.Errorf("restore refuses %q even though backup writes it", name)
		}
	}
	if len(restorableTables) != len(Tables) {
		t.Errorf("restore allowlist has %d entries, backup.Tables has %d", len(restorableTables), len(Tables))
	}
}

var (
	reCreateTable = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	reRenameTable = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s+RENAME\s+TO\s+([A-Za-z_][A-Za-z0-9_]*)`)
	reDropTable   = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
)

// tablesFromMigrations replays the embedded migrations in file order and
// returns the set of tables they leave behind. Reading the DDL rather than a
// live database is deliberate: this has to fail on a laptop with no Nucleus,
// which is exactly where the wrong names would otherwise survive review.
func tablesFromMigrations(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := schema.FS().ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	tables := map[string]bool{}
	for _, name := range names {
		raw, err := schema.FS().ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, stmt := range strings.Split(stripSQLComments(string(raw)), ";") {
			switch {
			case reRenameTable.MatchString(stmt):
				m := reRenameTable.FindStringSubmatch(stmt)
				delete(tables, m[1])
				tables[m[2]] = true
			case reDropTable.MatchString(stmt):
				delete(tables, reDropTable.FindStringSubmatch(stmt)[1])
			case reCreateTable.MatchString(stmt):
				tables[reCreateTable.FindStringSubmatch(stmt)[1]] = true
			}
		}
	}
	if len(tables) == 0 {
		t.Fatal("parsed no tables out of the migrations — the parser is broken, not the list")
	}
	return tables
}

// stripSQLComments drops `--` line comments so the long explanatory headers on
// 027/028/033/034 — which quote the very DDL and INSERT shapes they replaced —
// are not parsed as statements.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}
