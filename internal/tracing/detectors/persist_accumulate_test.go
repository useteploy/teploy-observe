package detectors

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// stubDetector returns whatever issue it currently holds, ignoring spans.
type stubDetector struct{ iss Issue }

func (s *stubDetector) Name() string                 { return "n_plus_one_db" }
func (s *stubDetector) Detect(_ []Span) []Issue       { return []Issue{s.iss} }

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
