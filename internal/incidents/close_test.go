package incidents

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// TestCloseWritesExactlyOneRow.
//
// Close is an INSERT...SELECT from the same table, which is the shape that
// doubled `issues` to 16.8M rows — but here the SELECT carries `ORDER BY
// updated_at DESC LIMIT 1`, and Nucleus honours both inside an INSERT...SELECT,
// so it appends exactly one row per call. This test is what keeps that true:
// drop the LIMIT and the count goes 1, 2, 4, 8 instead of 1, 2, 3, 4.
//
// It also pins the read side, which is the subtler half. `incidents` is a plain
// mergetree, so the open and closed rows coexist and the ended_at filter has to
// run after the in-Go dedup — filtering it in SQL drops the closed row and
// resurrects the open one.
func TestCloseWritesExactlyOneRow(t *testing.T) {
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
	const site = "incsite"

	inc, err := svc.Create(ctx, CreateInput{SiteID: site, Title: "Latency spike", Severity: "warning"}, "tyler")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM incidents WHERE incident_id = $1`, inc.IncidentID)
	})

	active, err := svc.Active(ctx, site)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("Active returned %d incidents after one Create, want 1", len(active))
	}

	// CloseByRule can call Close more than once across a flapping rule, so
	// repeated Close has to stay linear.
	for i := 1; i <= 3; i++ {
		if err := svc.Close(ctx, inc.IncidentID); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		if got, want := incidentRows(ctx, t, db, inc.IncidentID), int64(i+1); got != want {
			t.Fatalf("after %d Close call(s) the table holds %d rows, want %d — the SELECT lost its LIMIT and re-inserted one row per existing row", i, got, want)
		}
		active, err := svc.Active(ctx, site)
		if err != nil {
			t.Fatalf("active: %v", err)
		}
		for _, a := range active {
			if a.IncidentID == inc.IncidentID {
				t.Fatal("Active still reports a closed incident — the open filter ran before the dedup")
			}
		}
	}
}

func incidentRows(ctx context.Context, t *testing.T, db *nucleus.Client, incidentID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM incidents WHERE incident_id = $1`, incidentID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
