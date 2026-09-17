package schema

import (
	"context"
	"fmt"
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
