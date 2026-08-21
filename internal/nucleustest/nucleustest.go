// Package nucleustest resolves the engine the integration tests run against.
package nucleustest

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// DefaultDSN is where a scratch Nucleus is expected when OBSERVE_NUCLEUS_URL is
// unset. Deliberately not 5432: that is PostgreSQL's port, and on a machine
// running Postgres the old default connected to it happily.
//
//	docker run -d --platform linux/amd64 --name nucleus-test -p 55432:5432 \
//	  -e NUCLEUS_ALLOW_NO_AUTH=1 ghcr.io/neutron-build/nucleus:v0.1.5 \
//	  start --host 0.0.0.0 --port 5432 --cluster-port 5433 --data /data --max-memory 512
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
