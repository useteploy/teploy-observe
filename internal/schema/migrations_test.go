package schema

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// lexerPanicPattern matches a non-ASCII character followed by a number.
//
// Inside a `--` comment that shape panics the Nucleus v0.1.8 SQL lexer and
// drops the connection, surfacing as "unexpected EOF" with no hint that a
// comment caused it. Migration 034 shipped with `1, 2, 4, 8 … 4096` in its
// header and therefore failed on every fresh install and on the live upgrade
// path — a crash loop on the next deploy, from prose. The same character
// followed by a word is harmless, which is why 033's em dashes are fine, so
// the rule is this narrow shape rather than "migrations must be ASCII" (they
// are not, in seventeen files, all of which apply cleanly).
var lexerPanicPattern = regexp.MustCompile(`[^\x00-\x7F][^\S\n]*[0-9]`)

// asciiSinceVersion is the first migration version required to be pure ASCII.
//
// The lexer rule above is per-line and character-adjacent, but the current
// Nucleus working tree added a second multi-byte hazard the line shape cannot
// characterize: the query cache-key normalizer byte-slices backwards from a
// digit (`t[t.len()-kw.len()..]` in executor/query.rs, the LIMIT/OFFSET
// operand check) and panics — connection dropped, "unexpected EOF" — when the
// slice start lands inside a multi-byte character anywhere in the statement
// text. Migration 038 tripped it through em dashes in prose comments with a
// digit further down the file; 001-037's non-ASCII prose happens to apply
// cleanly and is live history, so they stay grandfathered. Until the
// upstream slicing bug is fixed, every migration from 038 on is pure ASCII.
const asciiSinceVersion = 38

func TestMigrationsFrom038ArePureASCII(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version := 0
		if _, err := fmt.Sscanf(e.Name(), "%03d", &version); err != nil {
			t.Fatalf("parse version from %s: %v", e.Name(), err)
		}
		if version < asciiSinceVersion {
			continue
		}
		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.IndexFunc(line, func(r rune) bool { return r > 0x7F }) >= 0 {
				t.Errorf("%s:%d is not pure ASCII - any multi-byte character in migration SQL can panic the Nucleus lexer or the query cache-key normalizer (connection dropped mid-migration, surfacing as \"unexpected EOF\"). Use ASCII (\"...\", \"-\") everywhere in migrations from %03d on.\n  %s",
					e.Name(), i+1, asciiSinceVersion, line)
			}
		}
	}
}

// noAlterAddColumnSince is the first migration version that may not use
// ALTER TABLE ... ADD COLUMN. Open P1 L9 (AUDIT_OPEN.md): on a populated
// upgraded store, migration 054's ADD COLUMN pair failed with "corrupt tuple"
// and left llm_traces unreadable. The failure cannot be seen here - these
// tests run migrations against empty tables - so the rule is enforced by
// shape: new columns arrive by rename-aside + create + copy (see 027, 028),
// never by ALTER. 048-056 predate the rule and stay grandfathered: the
// migration runner checksum-verifies every applied script (pinned by
// TestAppliedMigrationsAreChecksumFrozen), so rewriting applied history is
// refused on every store that already ran it - converting them in place is
// not a code change this repo can make (see AUDIT_OPEN.md L9).
const noAlterAddColumnSince = 57

var alterAddColumnPattern = regexp.MustCompile(`(?i)\balter\s+table\s+\S+\s+add\s+(column\b|if\s+not\s+exists\b)`)

func TestNewMigrationsDoNotAlterAddColumn(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version := 0
		if _, err := fmt.Sscanf(e.Name(), "%03d", &version); err != nil {
			t.Fatalf("parse version from %s: %v", e.Name(), err)
		}
		if version < noAlterAddColumnSince {
			continue
		}
		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if idx := strings.Index(line, "--"); idx >= 0 {
				line = line[:idx] // comments may mention the phrase
			}
			if alterAddColumnPattern.MatchString(line) {
				t.Errorf("%s:%d uses ALTER TABLE ... ADD COLUMN, which corrupts populated tables on the current engine (open P1 L9). Use rename-aside + create + copy (see 027, 028) or a new table.\n  %s",
					e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestAlterAddColumnPatternMatches(t *testing.T) {
	for _, bad := range []string{
		"ALTER TABLE t ADD COLUMN x INT;",
		"alter table t add column if not exists x text;",
		"ALTER TABLE t ADD IF NOT EXISTS x TEXT;",
	} {
		if !alterAddColumnPattern.MatchString(bad) {
			t.Errorf("pattern should flag %q", bad)
		}
	}
	for _, ok := range []string{"CREATE TABLE t (x INT);", "INSERT INTO t SELECT * FROM u;"} {
		if alterAddColumnPattern.MatchString(ok) {
			t.Errorf("pattern should not flag %q", ok)
		}
	}
}

func TestMigrationsAvoidNucleusLexerPanic(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if m := lexerPanicPattern.FindString(line); m != "" {
				t.Errorf("%s:%d has %q — a non-ASCII character followed by a number panics the Nucleus lexer and kills the connection mid-migration. Use ASCII (\"...\", \"-\") there.\n  %s",
					e.Name(), i+1, m, line)
			}
		}
	}
}

// TestMigrationsApplyToFreshDatabase is the guard that would have caught 034
// before it was committed: it runs the real migration chain, in order, through
// the same nucleus.Migrate the binary uses, against whatever scratch engine is
// configured.
//
// It needs a database with no Observe schema on it. On an instance that has
// already been migrated it is a no-op that still proves the ledger and the
// files agree, which is worth having; on a genuinely fresh one it exercises
// every statement. Point OBSERVE_NUCLEUS_URL at a throwaway instance to get the
// strong version.
func TestMigrationsApplyToFreshDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("the migration chain does not apply: %v", err)
	}
	// Idempotent: a second pass must be a no-op, not a re-run.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-applying the migration chain failed: %v", err)
	}
}

// TestApplyAdoptsLegacyHistory pins the M04 transition: a history recorded
// by the pre-v2 runner (format marker missing) is refused by Migrate and
// graduates through the SDK's explicit adoption, after which the chain
// applies again. The format downgrade simulates exactly what the old
// runner's rows look like to the new one — no other history shape changes.
func TestApplyAdoptsLegacyHistory(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	if _, err := db.Pool().Exec(ctx, "UPDATE _neutron_migrations SET format = NULL"); err != nil {
		t.Skipf("no migration history to downgrade (fixture already fresh): %v", err)
	}

	report, err := ApplyWithAdoption(ctx, db)
	if err != nil {
		t.Fatalf("apply over legacy history: %v", err)
	}
	if report == nil {
		t.Fatalf("legacy history was not adopted — report is nil")
	}
	if len(report.Verified) == 0 {
		t.Fatalf("adoption verified nothing: %+v", report)
	}
	// The adopted history must now be protocol v2 end to end.
	var legacy int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM _neutron_migrations WHERE format IS NULL OR format != 'v2'").Scan(&legacy); err != nil {
		t.Fatalf("read back history format: %v", err)
	}
	if legacy != 0 {
		t.Fatalf("%d history rows are still not protocol v2 after adoption", legacy)
	}
	// And the chain still applies (no-op) on the adopted history.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-apply after adoption failed: %v", err)
	}
}

// TestAppliedMigrationsAreChecksumFrozen pins the constraint that rules out
// the literal L9 "conversion" plan. The runner records a checksum of every
// script it applies and re-verifies the whole ledger before each run, so an
// applied migration file can never be rewritten in place - including 010's
// and 048-056's ALTER-ADD statements. Any conversion must ride new
// migrations (or owner-led ledger surgery on a quiesced store); the error
// contract below is the runner's, quoted in AUDIT_OPEN.md L9.
func TestAppliedMigrationsAreChecksumFrozen(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply chain: %v", err)
	}

	migrations, err := nucleus.LoadMigrations(migrationsFS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	modified := make([]nucleus.Migration, len(migrations))
	copy(modified, migrations)
	edited := false
	for i := range modified {
		if modified[i].Version == 48 {
			modified[i].Up += "\n-- post-application edit\n"
			edited = true
		}
	}
	if !edited {
		t.Fatal("migration 048 not found in the embedded plan")
	}

	err = db.Migrate(ctx, modified)
	if err == nil {
		t.Fatal("migrate accepted an edited applied script - the ledger is not checksum-enforced on this engine")
	}
	if !strings.Contains(err.Error(), "has been modified since it was applied") {
		t.Fatalf("unexpected refusal error: %v", err)
	}
	// The refusal must have been pre-mutation: the real chain still applies.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-apply after refused edit: %v", err)
	}
}

// l9AffectedTable describes one table migrations 048-063 mutate by
// ALTER TABLE ... ADD COLUMN: the columns that arrive, the key column marker
// rows are seeded and asserted on, the backfill value each added column
// carries after the upgrade, and the engine clause of its CREATE (engines are
// not in information_schema, so the one fact introspection cannot supply is
// declared here, copied from each table's creating migration).
type l9AffectedTable struct {
	table    string
	keyCol   string
	added    []string
	backfill map[string]string
	engine   string
}

var l9AffectedTables = []l9AffectedTable{
	{"experiments", "experiment_id", []string{"conversion_window_hours"},
		map[string]string{"conversion_window_hours": "72"},
		") WITH (engine = 'replacing_mergetree', version_column = 'version') ORDER BY (tenant_id, site_id, experiment_id)"},
	{"alert_rules", "rule_id", []string{"min_samples", "severity"},
		map[string]string{"min_samples": "1", "severity": "warning"},
		") WITH (engine = 'replacing_mergetree', version_column = 'version') ORDER BY (tenant_id, site_id, rule_id)"},
	{"incidents", "incident_id", []string{"acknowledged_at", "acknowledged_by"},
		map[string]string{"acknowledged_at": "0", "acknowledged_by": ""},
		") WITH (engine = 'mergetree') ORDER BY (incident_id)"},
	{"survey_responses", "response_id", []string{"entity_type", "entity_id", "client_id"},
		map[string]string{"entity_type": "", "entity_id": "", "client_id": ""},
		") WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, survey_id, timestamp)"},
	{"llm_traces", "trace_id", []string{"token_source", "cost_source"},
		map[string]string{"token_source": "", "cost_source": ""},
		") WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, timestamp)"},
	{"webhooks", "webhook_id", []string{"severities"},
		map[string]string{"severities": ""},
		") WITH (engine = 'replacing_mergetree', version_column = 'version') ORDER BY (tenant_id, site_id, webhook_id)"},
}

// l9CreatedTables is every table migrations 048-063 create. The upgrade test
// drops them so the re-run exercises the CREATEs, not just the ALTERs.
var l9CreatedTables = []string{
	"alert_evaluations", "notification_outbox", "maintenance_windows", "incident_events",
	"survey_exposures", "scheduled_export_runs", "llm_model_prices", "audit_checkpoints",
	"span_links", "issue_merges", "issue_assignments", "cohort_members",
	"person_properties", "person_aliases", "person_tombstones", "experiment_settings",
	"experiment_metric_events",
}

// l9Column is one introspected column of a table's current shape.
type l9Column struct {
	name     string
	dataType string
	nullable string
	deflt    *string
}

func l9TableExists(ctx context.Context, db *nucleus.Client, name string) bool {
	var n int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1", name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// TestMigrationsUpgradePopulatedStore replays the upgrade path L9 failed on:
// a store last migrated at 047, with real rows in every table the remaining
// chain ALTERs, runs migrations 048-063. On a fresh database the ALTERs hit
// empty tables, which is why the suite never saw the live corruption; here
// each affected table is rebuilt in its pre-048 shape (current shape minus
// the columns 048-063 add, derived from information_schema so interim
// history stays accurate), populated with the fixture's rows plus marker
// rows, and the upgrade must leave every row readable with the documented
// backfill values in the new columns - including the write-shaped column
// probe that failed on the live store. The engine defect itself (corruption
// from tuples written by an older engine) is upstream and does not
// reproduce on fresh data; this test guards the migration contract against
// populated stores for everything the repo controls.
func TestMigrationsUpgradePopulatedStore(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	// t.Cleanup runs LIFO after the test body returns; registering the
	// close FIRST makes it the LAST cleanup, so the restore below still
	// has an open pool even when the body fails midway.
	t.Cleanup(func() { db.Close() })

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply chain: %v", err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, ta := range l9AffectedTables {
			if !l9TableExists(cctx, db, ta.table) && l9TableExists(cctx, db, ta.table+"_l9pre") {
				if _, err := db.Pool().Exec(cctx, "ALTER TABLE "+ta.table+"_l9pre RENAME TO "+ta.table); err != nil {
					t.Logf("cleanup could not restore %s: %v", ta.table, err)
				}
			}
		}
		if err := Apply(cctx, db); err != nil {
			t.Logf("cleanup re-apply: %v", err)
		}
	})

	// An interrupted earlier run may have left aside copies behind; the
	// rebuild below refuses to rename onto one. Restore the copy when the
	// main table is missing, drop it when both exist (the main table is
	// then the full-shape one and the aside is a stale duplicate). Stale
	// marker rows are cleared for the same reason.
	for _, ta := range l9AffectedTables {
		if l9TableExists(ctx, db, ta.table+"_l9pre") {
			if !l9TableExists(ctx, db, ta.table) {
				if _, err := db.Pool().Exec(ctx, "ALTER TABLE "+ta.table+"_l9pre RENAME TO "+ta.table); err != nil {
					t.Fatalf("restore stale aside copy of %s: %v", ta.table, err)
				}
			} else if _, err := db.Pool().Exec(ctx, "DROP TABLE "+ta.table+"_l9pre"); err != nil {
				t.Fatalf("drop stale aside copy of %s: %v", ta.table, err)
			}
		}
		if _, err := db.Pool().Exec(ctx, "DELETE FROM "+ta.table+" WHERE "+ta.keyCol+" LIKE 'l9up-%'"); err != nil {
			t.Fatalf("clear stale markers in %s: %v", ta.table, err)
		}
	}

	// A store last migrated at 047.
	if _, err := db.Pool().Exec(ctx, "DELETE FROM _neutron_migrations WHERE version >= 48"); err != nil {
		t.Fatalf("rewind ledger past 048: %v", err)
	}
	for _, name := range l9CreatedTables {
		if _, err := db.Pool().Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}

	for _, ta := range l9AffectedTables {
		rows, err := db.Pool().Query(ctx, `
			SELECT column_name AS cn, data_type AS dt, is_nullable AS nl, column_default AS cd,
			       ordinal_position AS op
			FROM information_schema.columns WHERE table_name = $1
			ORDER BY op`, ta.table)
		if err != nil {
			t.Fatalf("introspect %s: %v", ta.table, err)
		}
		added := map[string]bool{}
		for _, c := range ta.added {
			added[c] = true
		}
		var cols []l9Column
		for rows.Next() {
			var c l9Column
			var ord int
			if err := rows.Scan(&c.name, &c.dataType, &c.nullable, &c.deflt, &ord); err != nil {
				t.Fatalf("scan column of %s: %v", ta.table, err)
			}
			if added[c.name] {
				continue
			}
			cols = append(cols, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("introspect %s: %v", ta.table, err)
		}
		if len(cols) == 0 {
			t.Fatalf("%s has no columns to rebuild", ta.table)
		}

		if _, err := db.Pool().Exec(ctx, "ALTER TABLE "+ta.table+" RENAME TO "+ta.table+"_l9pre"); err != nil {
			t.Fatalf("rename %s aside: %v", ta.table, err)
		}

		var ddl strings.Builder
		fmt.Fprintf(&ddl, "CREATE TABLE %s (\n", ta.table)
		for i, c := range cols {
			if i > 0 {
				ddl.WriteString(",\n")
			}
			fmt.Fprintf(&ddl, "    %s %s", c.name, c.dataType)
			if c.nullable == "NO" {
				ddl.WriteString(" NOT NULL")
			}
			if c.deflt != nil && *c.deflt != "" {
				fmt.Fprintf(&ddl, " DEFAULT %s", *c.deflt)
			}
		}
		ddl.WriteString("\n")
		ddl.WriteString(ta.engine)
		if _, err := db.Pool().Exec(ctx, ddl.String()); err != nil {
			t.Fatalf("create pre-048 %s: %v\nDDL: %s", ta.table, err, ddl.String())
		}

		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.name
		}
		list := strings.Join(names, ", ")
		if _, err := db.Pool().Exec(ctx,
			"INSERT INTO "+ta.table+" ("+list+") SELECT "+list+" FROM "+ta.table+"_l9pre"); err != nil {
			t.Fatalf("copy rows into rebuilt %s: %v", ta.table, err)
		}

		// Marker rows in the pre-shape: only NOT NULL columns without a
		// default are listed; everything else takes its declared default.
		for n := 1; n <= 2; n++ {
			key := fmt.Sprintf("'l9up-%s-%d'", ta.table, n)
			var insCols, insVals []string
			for _, c := range cols {
				if c.nullable != "NO" || (c.deflt != nil && *c.deflt != "") {
					continue
				}
				insCols = append(insCols, c.name)
				switch {
				case c.name == ta.keyCol:
					insVals = append(insVals, key)
				case strings.EqualFold(c.dataType, "bigint") || strings.EqualFold(c.dataType, "integer") || strings.EqualFold(c.dataType, "double"):
					insVals = append(insVals, "1760000000000")
				default:
					insVals = append(insVals, "'l9up-"+ta.table+"'")
				}
			}
			if len(insCols) == 0 {
				continue
			}
			if _, err := db.Pool().Exec(ctx,
				"INSERT INTO "+ta.table+" ("+strings.Join(insCols, ", ")+") VALUES ("+strings.Join(insVals, ", ")+")"); err != nil {
				t.Fatalf("seed marker row %d into %s: %v", n, ta.table, err)
			}
		}

		if _, err := db.Pool().Exec(ctx, "DROP TABLE "+ta.table+"_l9pre"); err != nil {
			t.Fatalf("drop aside copy of %s: %v", ta.table, err)
		}
	}

	// The upgrade itself: 048-063 against populated pre-shape tables.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("upgrade 048-063 against populated tables: %v", err)
	}

	for _, ta := range l9AffectedTables {
		for _, col := range ta.added {
			var n int
			if err := db.Pool().QueryRow(ctx,
				"SELECT COUNT(*) AS c FROM information_schema.columns WHERE table_name = $1 AND column_name = $2",
				ta.table, col).Scan(&n); err != nil || n == 0 {
				t.Errorf("%s.%s missing after upgrade (err %v)", ta.table, col, err)
			}
			// The write-shaped probe that failed on the live store.
			if _, err := db.Pool().Exec(ctx,
				"UPDATE "+ta.table+" SET "+col+" = "+col+" WHERE 1=0"); err != nil {
				t.Errorf("write-shaped probe on %s.%s failed: %v", ta.table, col, err)
			}
		}
		sel := strings.Join(append([]string{ta.keyCol}, ta.added...), ", ")
		rows, err := db.Pool().Query(ctx, "SELECT "+sel+" FROM "+ta.table+" WHERE "+ta.keyCol+" LIKE 'l9up-%'")
		if err != nil {
			t.Fatalf("read back markers from %s: %v", ta.table, err)
		}
		defer rows.Close()
		found := 0
		for rows.Next() {
			found++
			vals := make([]string, len(ta.added)+1)
			dest := make([]any, len(vals))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatalf("scan marker from %s: %v", ta.table, err)
			}
			for i, col := range ta.added {
				if vals[i+1] != ta.backfill[col] {
					t.Errorf("%s.%s backfill: got %q want %q", ta.table, col, vals[i+1], ta.backfill[col])
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("markers from %s: %v", ta.table, err)
		}
		if found != 2 {
			t.Errorf("%s: %d marker rows survived, want 2", ta.table, found)
		}

		if _, err := db.Pool().Exec(ctx, "DELETE FROM "+ta.table+" WHERE "+ta.keyCol+" LIKE 'l9up-%'"); err != nil {
			t.Logf("could not remove marker rows from %s: %v", ta.table, err)
		}
	}
}
