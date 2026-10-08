package detectors

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"
)

// stubDetector returns whatever issue it currently holds, ignoring spans.
type stubDetector struct{ iss Issue }

func (s *stubDetector) Name() string            { return "n_plus_one_db" }
func (s *stubDetector) Detect(_ []Span) []Issue { return []Issue{s.iss} }

// TestPersistAccumulates verifies audit #160: performance_issues is a
// replacing_mergetree that Nucleus dedups on read, so re-detections of one
// fingerprint must accumulate count and pin the earliest first_seen on WRITE
// (the merge replaces, it does not sum). Requires a live Nucleus.
func TestPersistAccumulates(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}

	site := "perfsite" + genID()
	fp := "fp-" + genID()
	stub := &stubDetector{}
	eng := NewWithDetectors(db, []Detector{stub})

	// Three detection batches of the same fingerprint, timestamps out of order
	// so the earliest (3000) is neither first nor last written.
	for _, ts := range []int64{5000, 3000, 7000} {
		stub.iss = Issue{
			TraceID: "t", DetectorName: "n_plus_one_db", Fingerprint: fp,
			Title: "N+1", Description: "d", Severity: "warning",
			FirstSeen: ts, LastSeen: ts + 100,
		}
		eng.Persist(ctx, site, []Span{{}})
	}

	// Read through the same dedup-on-read path the UI uses: one surviving row.
	type row struct {
		Count     string `db:"count"`
		FirstSeen string `db:"first_seen"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT CAST(count AS TEXT) AS count, CAST(first_seen AS TEXT) AS first_seen
		 FROM performance_issues WHERE site_id=$1 AND fingerprint=$2`, site, fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 deduped row, got %d", len(rows))
	}
	if rows[0].Count != "3" {
		t.Fatalf("expected count=3 (accumulated), got %s — undercount bug", rows[0].Count)
	}
	if rows[0].FirstSeen != "3000" {
		t.Fatalf("expected first_seen=3000 (earliest), got %s", rows[0].FirstSeen)
	}
}

// OBS26-105/106: real persistence after every descending/equal timestamp and
// healthy re-delivery of a multi-occurrence intent, plus concurrent arrivals.
func TestOBS105106IntentAndLateSnapshots(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng := New(db)
	site, fp := "obs105-"+genID(), genID()
	latest := Issue{Fingerprint: fp, TraceID: "new", DetectorName: "slow_db_query", FirstSeen: 5000, LastSeen: 5100, Severity: "warning"}
	late := Issue{Fingerprint: fp, TraceID: "old", DetectorName: "slow_db_query", FirstSeen: 3000, LastSeen: 3100, Severity: "error"}
	assert := func(count, first, last int64) {
		t.Helper()
		rows, err := nucleus.Query[issueSnapshot](ctx, db.SQL(), `SELECT count,first_seen,last_seen,version,severity FROM performance_issues WHERE site_id=$1 AND fingerprint=$2`, site, fp)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Count != count || rows[0].FirstSeen != first || rows[0].LastSeen != last {
			t.Fatalf("current snapshot missing: %+v", rows)
		}
	}
	if err := eng.WriteIssues(ctx, "intent-new-"+site, site, []Issue{latest}); err != nil {
		t.Fatal(err)
	}
	assert(1, 5000, 5100)
	if err := eng.WriteIssues(ctx, "intent-late-"+site, site, []Issue{late}); err != nil {
		t.Fatal(err)
	}
	assert(2, 3000, 5100)
	if err := eng.WriteIssues(ctx, "intent-batch-"+site, site, []Issue{late, latest}); err != nil {
		t.Fatal(err)
	}
	assert(4, 3000, 5100)
	if err := eng.WriteIssues(ctx, "intent-batch-"+site, site, []Issue{latest, late}); err != nil {
		t.Fatal(err)
	}
	assert(4, 3000, 5100)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(i int) { errs <- eng.WriteIssues(ctx, fmt.Sprintf("intent-%d-%s", i, site), site, []Issue{late}) }(i)
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	assert(8, 3000, 5100)
}
