package experiments

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// experiments as 007_wave2 declares it, plus the `variants` column 025 adds.
const experimentColumns = `(
	experiment_id  TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	flag_key       TEXT NOT NULL,
	goal_metric    TEXT NOT NULL DEFAULT 'pageview',
	goal_value     TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT 'draft',
	min_sample     TEXT NOT NULL DEFAULT '100',
	started_at     TEXT NOT NULL DEFAULT '0',
	ended_at       TEXT NOT NULL DEFAULT '0',
	created_at     TEXT NOT NULL,
	variants       TEXT NOT NULL DEFAULT '',
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestStartStopWriteOneRowAndResolveLatest covers both halves of the
// experiments bug at once.
//
// Start and Stop were `INSERT INTO experiments SELECT ... FROM experiments
// WHERE experiment_id = $1`, which writes one row per row already present — so
// the physical count goes 1, 2, 4, 8 across three transitions — and every read
// took whichever version came back first, so a completed experiment could keep
// reporting as running and List returned one entry per surviving version.
//
// Without the fix this fails at the first List assertion — the collapse is
// missing there, so both versions come back — and, had it got past that, at the
// second transition's row count: buggy reaches 8 rows over three transitions
// where fixed reaches 4.
func TestStartStopWriteOneRowAndResolveLatest(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "experiments", experimentColumns,
		"(tenant_id, site_id, experiment_id)", "version")

	svc := NewExperimentService(db)
	const site = "expsite"

	exp, err := svc.Create(ctx, site, "Checkout copy", "new-checkout", "pageview", "", "", 100)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i, step := range []struct {
		run    func() error
		status string
	}{
		{func() error { return svc.Start(ctx, exp.ExperimentID) }, "running"},
		{func() error { return svc.Stop(ctx, exp.ExperimentID) }, "completed"},
		{func() error { return svc.Start(ctx, exp.ExperimentID) }, "running"},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got, want := physicalRows(ctx, t, db, exp.ExperimentID), int64(i+2); got != want {
			t.Fatalf("after %d transition(s) the table holds %d rows, want %d — the write re-inserted one row per existing version", i+1, got, want)
		}
		list, err := svc.List(ctx, site)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("List returned %d experiments, want 1 — one entry per surviving version", len(list))
		}
		if list[0].Status != step.status {
			t.Fatalf("List reported status %q after the transition to %q — it resolved a superseded version", list[0].Status, step.status)
		}
		res, err := svc.Results(ctx, exp.ExperimentID, site)
		if err != nil {
			t.Fatalf("results: %v", err)
		}
		if res.Experiment.Status != step.status {
			t.Fatalf("Results reported status %q, want %q", res.Experiment.Status, step.status)
		}
	}
}

func physicalRows(ctx context.Context, t *testing.T, db *nucleus.Client, experimentID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM experiments WHERE experiment_id = $1`, experimentID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
