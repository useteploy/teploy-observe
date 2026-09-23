// Package nucleustest resolves the engine the integration tests run against.
package nucleustest

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"
)

// DefaultDSN is where a scratch Nucleus is expected when OBSERVE_NUCLEUS_URL is
// unset. Deliberately not 5432: that is PostgreSQL's port, and on a machine
// running Postgres the old default connected to it happily.
//
// Two working shapes (2026-09-18, later session): the published image —
//
//	docker run -d --platform linux/amd64 --name nucleus-test -p 55432:5432 \
//	  -e NUCLEUS_ALLOW_NO_AUTH=1 -e NUCLEUS_ALLOW_INSECURE_CLUSTER=1 \
//	  -e NUCLEUS_ALLOW_INSECURE_REPLICATION=1 \
//	  ghcr.io/neutron-build/nucleus:v0.1.8 \
//	  start --host 0.0.0.0 --port 5432 --cluster-port 5433 --data /data --max-memory 512
//
// — and a repo-built binary from the Neutron tree
// (`cargo build --bin nucleus` in Neutron/nucleus, then `start` with the
// same flags on a free port). Engines built from the tree FAILED the
// migration ladder at 027 until the 2026-09-18 upstream fix (`6286531a`,
// same-transaction rename visibility); verified fixed from the
// backup/SDK close session — a repo-built engine now applies the full
// ladder and is the shape that exercises the newest engine surfaces
// (e.g. ACQUIRE SNAPSHOT LEASE, which v0.1.8 predates).
const DefaultDSN = "postgres://nucleus@127.0.0.1:55432/observe?sslmode=disable"

// DSN returns the DSN for the test engine, skipping the test unless something
// is listening there AND that something is actually Nucleus.
//
// The identity check is the point. The guard every call site used to carry
// asked only whether a connection SUCCEEDED, and defaulted to
// postgres://postgres@localhost:5432 — PostgreSQL's own port. On a machine with
// Postgres installed the integration suite therefore ran against the wrong
// engine and failed with errors that said nothing about this code: "syntax
// error at or near ORDER" for Nucleus's MergeTree DDL, and columns reported
// missing that Nucleus resolves to NULL. The failures look like regressions and
// are not, which is worse than skipping.
//
// This repo already recorded the general form of the mistake — verify against
// the real dependency, not something shaped like it. The guard is where that
// has to be enforced, because "it connected" is not "it is the right engine".
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = DefaultDSN
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		// A green suite must never mask the store going away mid-run
		// (found live by the 2026-09-22 verification audit: 88 tests
		// silently skipped when the fixture blinked). CI integration jobs
		// set OBSERVE_REQUIRE_NUCLEUS=1 so an unreachable fixture FAILS.
		if os.Getenv("OBSERVE_REQUIRE_NUCLEUS") == "1" {
			t.Fatalf("OBSERVE_REQUIRE_NUCLEUS=1: nucleus not reachable at %s — failing instead of skipping (%v)", dsn, err)
		}
		t.Skipf("nucleus not reachable at %s — skipping integration test (%v)", dsn, err)
	}
	defer db.Close()

	// The client already does this detection at connect time for its own feature
	// gating; the guard just has to consult it instead of assuming.
	if !db.IsNucleus() {
		t.Skipf("the engine at %s is not Nucleus — skipping rather than reporting its errors as failures of this code", dsn)
	}
	return dsn
}
