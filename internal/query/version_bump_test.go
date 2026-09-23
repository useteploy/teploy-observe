package query

import (
	"context"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// The tie mechanism, pinned at the engine level with no service in the loop.
//
// Every version-rewriting write in this repo resolves the latest row through
// LatestRows and stamps the new row's version from the clock. When two writes
// to the same key land in the same millisecond, the versions tie and
// argMax(col, version) resolves the tie arbitrarily — the just-written row can
// lose to the one it was supposed to replace. The fleet-wide fix stamps the
// new version as GREATEST(CAST($now AS BIGINT), version + 1), where `version`
// is the latest row's version from the collapse: the millisecond base is kept
// when the clock has advanced, and the bump arm resolves ties (and clock
// regression) when it hasn't.
//
// This test pins that expression against the real engine with fully
// controlled versions — no timing luck. Verified on Nucleus v1.1.1: GREATEST
// over a collapsed derived table parses and computes as intended.
func TestGreatestVersionBumpIsStrictOverACollapsedPrior(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	const columns = `(
		k TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '',
		version BIGINT NOT NULL DEFAULT 0
	)`
	nucleustest.AsPlainMergeTree(t, db, "query_version_bump", columns, "(k)", "version")

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.SQL().Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO query_version_bump (k, payload, version) VALUES ('tie', 'first', $1)", "1700000000000")
	exec("INSERT INTO query_version_bump (k, payload, version) VALUES ('tie', 'second', $1)", "1700000000000")

	// The fix's exact shape over the same collapse the services use (inlined
	// here because the registry only carries production tables). `now` is
	// deliberately EQUAL to the tied prior version — the same-millisecond
	// write — so only the version + 1 arm can produce a strict increase.
	latest := `(SELECT k, argMax(payload, version) AS payload, argMax(version, version) AS version
		FROM query_version_bump WHERE k = 'tie' GROUP BY k)`
	exec(`INSERT INTO query_version_bump (k, payload, version)
		SELECT k, 'third', GREATEST(CAST($1 AS BIGINT), version + 1)
		FROM `+latest+` AS query_version_bump`, "1700000000000")

	type row struct {
		Payload string `db:"payload"`
		Version string `db:"version"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT argMax(payload, version) AS payload, CAST(MAX(version) AS TEXT) AS version
		 FROM query_version_bump WHERE k = 'tie' GROUP BY k`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("collapse returned %d rows, want 1", len(rows))
	}
	if rows[0].Version != "1700000000001" {
		t.Fatalf("version = %s, want 1700000000001 — the tie bump arm did not fire", rows[0].Version)
	}
	if rows[0].Payload != "third" {
		t.Fatalf("payload = %q, want the newly written row to resolve", rows[0].Payload)
	}

	// Clock advanced: the max arm keeps the millisecond base.
	exec(`INSERT INTO query_version_bump (k, payload, version)
		SELECT k, 'fourth', GREATEST(CAST($1 AS BIGINT), version + 1)
		FROM `+latest+` AS query_version_bump`, "1700000005000")
	rows, err = nucleus.Query[row](ctx, db.SQL(),
		`SELECT argMax(payload, version) AS payload, CAST(MAX(version) AS TEXT) AS version
		 FROM query_version_bump WHERE k = 'tie' GROUP BY k`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rows[0].Version != "1700000005000" {
		t.Fatalf("version = %s, want 1700000005000 — the millisecond base was not kept", rows[0].Version)
	}
	if rows[0].Payload != "fourth" {
		t.Fatalf("payload = %q, want fourth", rows[0].Payload)
	}
}
