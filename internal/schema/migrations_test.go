package schema

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// lexerPanicPattern matches a non-ASCII character followed by a number.
//
// Inside a `--` comment that shape panics the Nucleus v0.1.8 SQL lexer and
// drops the connection, surfacing as "unexpected EOF" with no hint that a
// comment caused it. Migration 034 shipped with `1, 2, 4, 8 … 4096` in its
// header and therefore failed on every fresh install and on the live upgrade
// path — a crash loop on the next deploy, from prose. The same character
// followed by a word is harmless, which is why 033's em dashes are fine, so the
// rule is this narrow shape rather than "migrations must be ASCII" (they are
// not, in seventeen files, all of which apply cleanly).
var lexerPanicPattern = regexp.MustCompile(`[^\x00-\x7F][^\S\n]*[0-9]`)

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
