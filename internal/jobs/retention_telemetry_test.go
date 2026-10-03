package jobs

import (
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// TestDefaultTelemetryPolicies pins the tables that used to grow unbounded and
// the column/unit each is compared on. metric_points MUST be nanoseconds.
func TestDefaultTelemetryPolicies(t *testing.T) {
	p := DefaultTelemetryPolicies(30, 30, 90)
	want := map[string]RetentionPolicy{
		"metric_points":        {Table: "metric_points", Column: "ts_ns", Days: 30, Unit: UnitNanos},
		"host_metrics":         {Table: "host_metrics", Column: "timestamp", Days: 30},
		"uptime_results":       {Table: "uptime_results", Column: "timestamp", Days: 90},
		"performance_issues":   {Table: "performance_issues", Column: "last_seen", Days: 90},
		"service_dependencies": {Table: "service_dependencies", Column: "ts_bucket", Days: 30},
	}
	if len(p) != len(want) {
		t.Fatalf("want %d telemetry policies, got %d", len(want), len(p))
	}
	for _, x := range p {
		if w, ok := want[x.Table]; !ok || x != w {
			t.Errorf("policy %s = %+v, want %+v", x.Table, x, w)
		}
	}
	// Knobs flow through.
	q := DefaultTelemetryPolicies(3, 4, 5)
	if PolicyDays(q, "metric_points") != 3 || PolicyDays(q, "host_metrics") != 4 || PolicyDays(q, "uptime_results") != 5 {
		t.Errorf("configured days not applied: %+v", q)
	}
}

// TestTimeUnitCutoffScaling: a ns column compared against a ms cutoff would
// delete nothing, so the cutoff must be scaled to the column's unit.
func TestTimeUnitCutoffScaling(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	const dayMs = int64(24 * 60 * 60 * 1000)

	ms := UnitMillis.CutoffAt(now, 30)
	if want := now.UnixMilli() - 30*dayMs; ms != want {
		t.Errorf("ms cutoff = %d, want %d", ms, want)
	}
	ns := UnitNanos.CutoffAt(now, 30)
	if want := (now.UnixMilli() - 30*dayMs) * 1_000_000; ns != want {
		t.Errorf("ns cutoff = %d, want %d", ns, want)
	}
	if ns/1_000_000 != ms {
		t.Errorf("ns and ms cutoffs disagree: %d vs %d", ns, ms)
	}
	// A 40-day-old point is below the ns cutoff and a 1-day-old one above it.
	old := now.Add(-40 * 24 * time.Hour).UnixNano()
	recent := now.Add(-24 * time.Hour).UnixNano()
	if !(old < ns && recent > ns) {
		t.Errorf("ns cutoff misclassifies rows: old=%d recent=%d cutoff=%d", old, recent, ns)
	}
	// The unscaled ms cutoff is smaller than every ns row, so it would match
	// nothing: this is the bug the Unit field exists to prevent.
	if old < ms {
		t.Errorf("sanity: unscaled ms cutoff should (wrongly) match nothing for ns rows")
	}
	// Zero value stays milliseconds so legacy policy literals are unchanged.
	if (RetentionPolicy{}).Unit != UnitMillis {
		t.Errorf("zero Unit must be milliseconds")
	}
}

// UNVERIFIED against live Nucleus (skips without a database): metric_points
// uses a nanosecond column, so the ns-scaled cutoff must delete the old point
// and keep the recent one.
func TestRetentionMetricPointsNanosecondColumn(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	site := "retmp_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now()
	ins := func(name string, ts int64) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO metric_points (site_id, metric_name, metric_kind, ts_ns, value) VALUES ($1,$2,'gauge',$3,1)`,
			site, name, ts); err != nil {
			t.Fatalf("insert metric point: %v", err)
		}
	}
	ins("old", now.Add(-40*24*time.Hour).UnixNano())
	ins("recent", now.Add(-24*time.Hour).UnixNano())

	svc := NewRetentionServiceWithPolicies(db, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]RetentionPolicy{{Table: "metric_points", Column: "ts_ns", Days: 30, Unit: UnitNanos}})
	if err := svc.RunCleanup(ctx); err != nil {
		t.Fatalf("RunCleanup: %v", err)
	}
	for name, want := range map[string]int64{"old": 0, "recent": 1} {
		rows, err := nucleus.Query[countRow](ctx, db.SQL(),
			`SELECT COUNT(*) AS n FROM metric_points WHERE site_id = $1 AND metric_name = $2`, site, name)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if rows[0].N != want {
			t.Errorf("%s metric point count = %d, want %d", name, rows[0].N, want)
		}
	}
}
