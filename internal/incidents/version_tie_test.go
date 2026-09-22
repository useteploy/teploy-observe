package incidents

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// incidents has no version column: updated_at IS the version (argMax over
// updated_at, grouped by incident_id). Two rows for one incident whose
// updated_at values tie resolve arbitrarily — the exact shape of the live
// TestInRangeCollapsesVersions flake recorded in AUDIT_OPEN at 70f6eff.

// TestSameMillisecondCreateCloseResolvesClosed is the tight-loop regression
// for that live defect: Create and Close in immediate succession (no sleeps),
// the flake's exact shape. Before the monotonic fix, a same-millisecond pair
// tied on updated_at and the closed row could lose the collapse, leaving a
// closed incident reading as ongoing. After the fix, Close stamps
// GREATEST(now, updated_at + 1) and the closed row always wins.
func TestSameMillisecondCreateCloseResolvesClosed(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	svc := NewService(db)
	const site = "inc-tie-site"
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM incidents WHERE site_id = $1`, site)
	})

	for i := 0; i < 5; i++ {
		inc, err := svc.Create(ctx, CreateInput{SiteID: site, Title: "tie", Severity: "info"}, "tester")
		if err != nil {
			t.Fatalf("iter %d create: %v", i, err)
		}
		if err := svc.Close(ctx, inc.IncidentID); err != nil {
			t.Fatalf("iter %d close: %v", i, err)
		}
		active, err := svc.Active(ctx, site)
		if err != nil {
			t.Fatalf("iter %d active: %v", i, err)
		}
		for _, a := range active {
			if a.IncidentID == inc.IncidentID {
				t.Fatalf("iter %d: a closed incident still reads as ongoing — the close row tied or lost the collapse", i)
			}
		}
		inRange, err := svc.InRange(ctx, site, inc.StartedAt-1, time.Now().Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatalf("iter %d in-range: %v", i, err)
		}
		for _, r := range inRange {
			if r.IncidentID == inc.IncidentID && r.EndedAt == 0 {
				t.Fatalf("iter %d: InRange reports ended_at=0 for a closed incident — the open row won an updated_at tie", i)
			}
		}
	}
}

// TestCloseBumpsPastAFutureUpdatedAt is the deterministic arm: the prior row
// is seeded directly with a FUTURE updated_at (the same protection covers a
// regressed/skewed clock), so only the updated_at + 1 bump arm can make the
// close win. Before the fix, Close stamped updated_at=now below the seed and
// the incident stayed open forever.
func TestCloseBumpsPastAFutureUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	svc := NewService(db)
	const site = "inc-fut-site"
	inc, err := svc.Create(ctx, CreateInput{SiteID: site, Title: "future", Severity: "info"}, "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM incidents WHERE site_id = $1`, site)
	})

	future := strconv.FormatInt(time.Now().UTC().Add(time.Hour).UnixMilli(), 10)
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO incidents (incident_id, tenant_id, site_id, title, description, severity,
		 source, rule_id, started_at, ended_at, created_by, updated_at)
		 VALUES ($1, 'default', $2, 'future-row', '', 'info', 'manual', '', $3, 0, 'seed', $4)`,
		inc.IncidentID, site, future, future); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.Close(ctx, inc.IncidentID); err != nil {
		t.Fatalf("close: %v", err)
	}

	active, err := svc.Active(ctx, site)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	for _, a := range active {
		if a.IncidentID == inc.IncidentID {
			t.Fatal("Close wrote at or below the seeded future row — the incident still reads as ongoing")
		}
	}
}
