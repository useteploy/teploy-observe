package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// o12Connect mirrors o04Connect: DSN, 60s budget, unique per-run site id.
func o12Connect(t *testing.T) (*nucleus.Client, context.Context, string) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, ctx, fmt.Sprintf("test_o12_%d", time.Now().UnixNano())
}

// The load-bearing engine assumption of the O12 streaming scans: ORDER
// BY timestamp ASC, event_id ASC over the plain OLTP events table
// delivers the walk's pinned total order. A silently-ignored ORDER BY
// term (the CLAUDE.md Nucleus gotcha) would not error — it would corrupt
// every funnel/retention result — so it is pinned here with rows
// deliberately inserted OUT of order and sharing timestamps so the
// event_id tie-break must fire.
func TestO12_StreamOrderPinnedAtEngine(t *testing.T) {
	db, ctx, site := o12Connect(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedRows := []struct {
		id string
		ms int64
	}{
		{"e-9", 500}, {"e-1", 100}, {"e-5", 100}, {"e-3", 300}, {"e-7", 300},
		{"e-2", 100}, {"e-8", 400}, {"e-4", 300}, {"e-6", 300}, {"e-0", 0},
	}
	for _, r := range seedRows {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, distinct_id, properties)
			 VALUES ($1, 'default', $2, 'sess', 'visit', 'pageview', $3, '', 'null')`,
			r.id, site, base.Add(time.Duration(r.ms)*time.Millisecond).UnixMilli(),
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	svc := NewStatsService(db)
	var got []string
	var lastTS int64 = -1
	err := svc.streamEvents(ctx, site, base.UnixMilli()-1, base.Add(time.Second).UnixMilli(), "", func(row pgx.Row) error {
		e, err := scanFunnelEvent(row)
		if err != nil {
			return err
		}
		if e.Timestamp < lastTS {
			return fmt.Errorf("timestamp went backwards at %s", e.EventID)
		}
		if e.Timestamp == lastTS && len(got) > 0 && e.EventID <= got[len(got)-1] {
			return fmt.Errorf("event_id tie-break violated at %s after %s", e.EventID, got[len(got)-1])
		}
		lastTS = e.Timestamp
		got = append(got, e.EventID)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	want := []string{"e-0", "e-1", "e-2", "e-5", "e-3", "e-4", "e-6", "e-7", "e-8", "e-9"}
	if len(got) != len(want) {
		t.Fatalf("streamed %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// The row budget refuses labeled instead of scanning to completion, for
// each of the three streaming paths (funnel shown here; retention below)
// and the LIMIT-bounded paths (journeys).
func TestO12_RowBudgetRefusalLabeled(t *testing.T) {
	db, ctx, site := o12Connect(t)
	base := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	// 40 events across 4 sessions; budget 10 must refuse all three
	// heavy paths with the labeled code.
	batch := 20
	for start := 0; start < 40; start += batch {
		var q strings.Builder
		q.WriteString(`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname, distinct_id, properties)
			VALUES `)
		args := []any{site, "sess"} // $1, $2
		for i := 0; i < batch; i++ {
			if i > 0 {
				q.WriteString(",")
			}
			n := len(args)
			q.WriteString(fmt.Sprintf("($%d, 'default', $1, $2, $2, 'pageview', $%d, '/', '', 'null')", n+1, n+2))
			args = append(args,
				fmt.Sprintf("ev-%d", start+i),
				base.Add(time.Duration(start+i)*time.Minute).UnixMilli())
		}
		if _, err := db.SQL().Exec(ctx, q.String(), args...); err != nil {
			t.Fatalf("seed batch %d: %v", start, err)
		}
	}

	svc := NewStatsService(db).WithQueryGuard(nil, queryguard.Budgets{
		Timeout:     30 * time.Second,
		MaxScanRows: 10,
		MaxWindow:   186 * 24 * time.Hour,
	})
	from, to := base.Add(-time.Minute), base.Add(40*time.Minute)

	assertRefusal := func(name string, err error) {
		t.Helper()
		var r *queryguard.Refusal
		if !errors.As(err, &r) {
			t.Fatalf("%s: want *queryguard.Refusal, got %v", name, err)
		}
		if r.Code != queryguard.CodeBudgetRows {
			t.Fatalf("%s: code = %q, want %q (remedy: %q)", name, r.Code, queryguard.CodeBudgetRows, r.Remedy)
		}
	}

	_, err := svc.FunnelWithOptions(ctx, site, from, to, []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
	assertRefusal("funnel", err)

	_, err = svc.RetentionWithOptions(ctx, site, from, to, 1, RetentionOptions{})
	assertRefusal("retention", err)

	_, err = svc.Journeys(ctx, site, from, to, 10)
	assertRefusal("journeys", err)

	_, err = svc.CorrelationAnalysis(ctx, site, "signup", from, to)
	assertRefusal("correlation", err)

	// A budget ABOVE the row count leaves every path answering normally.
	svcOK := NewStatsService(db).WithQueryGuard(nil, queryguard.Budgets{
		Timeout:     30 * time.Second,
		MaxScanRows: 1000,
		MaxWindow:   186 * 24 * time.Hour,
	})
	res, err := svcOK.FunnelWithOptions(ctx, site, from, to, []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
	if err != nil || len(res) != 1 || res[0].Visitors != 1 {
		t.Fatalf("in-budget funnel = (%v, %v), want 1 visitor on one session", res, err)
	}
}

// Concurrency admission refuses labeled at the site bound while a slot is
// held — the O12 v1 posture (refused-with-remedy, no queue wait).
func TestO12_ConcurrencyAdmissionRefusal(t *testing.T) {
	db, ctx, site := o12Connect(t)
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, distinct_id, properties)
		 VALUES ('c-1', 'default', $1, 's', 'v', 'pageview', $2, '', 'null')`, site, base.UnixMilli(),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	limiter := queryguard.NewLimiter(8, 1)
	svc := NewStatsService(db).WithQueryGuard(limiter, queryguard.Budgets{
		Timeout:     10 * time.Second,
		MaxScanRows: 1000,
		MaxWindow:   186 * 24 * time.Hour,
	})
	release, err := limiter.Acquire(ctx, site)
	if err != nil {
		t.Fatalf("hold slot: %v", err)
	}
	_, err = svc.FunnelWithOptions(ctx, site, base.Add(-time.Minute), base.Add(time.Minute), []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
	release()
	var r *queryguard.Refusal
	if !errors.As(err, &r) || r.Code != queryguard.CodeConcurrencySite {
		t.Fatalf("funnel under held slot: want site-concurrency refusal, got %v", err)
	}
	// After release the same query answers.
	if _, err := svc.FunnelWithOptions(ctx, site, base.Add(-time.Minute), base.Add(time.Minute), []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{}); err != nil {
		t.Fatalf("funnel after release: %v", err)
	}
	if snap := limiter.Snapshot(queryguard.DefaultBudgets()); snap.Refused[queryguard.CodeConcurrencySite] != 1 {
		t.Fatalf("refusal counter = %v", snap.Refused)
	}
}

// The window clamp: with MaxWindow set, events older than to-MaxWindow
// are outside the scan even when the requested from reaches further
// back. Mirrors retention's pinned clamp, now declared and tunable.
func TestO12_WindowClampBoundsTheScan(t *testing.T) {
	db, ctx, site := o12Connect(t)
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	seed := func(id string, at time.Time) {
		t.Helper()
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, distinct_id, properties)
			 VALUES ($1, 'default', $2, 's', 'v', 'pageview', $3, '', 'null')`, id, site, at.UnixMilli()); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("in-1", base.Add(-23*time.Hour))
	seed("out-1", base.Add(-48*time.Hour))

	svc := NewStatsService(db).WithQueryGuard(nil, queryguard.Budgets{
		Timeout:     10 * time.Second,
		MaxScanRows: 1000,
		MaxWindow:   24 * time.Hour,
	})
	// Ask for 3 days; the clamp keeps 24h. Both events are pageviews on
	// one session, so the funnel's visitor count distinguishes inclusion.
	res, err := svc.FunnelWithOptions(ctx, site, base.Add(-72*time.Hour), base, []FunnelStep{{Type: "page", Value: "/x"}}, FunnelOptions{})
	if err != nil {
		t.Fatalf("funnel: %v", err)
	}
	// No /x pages were seeded; use the event step instead to count.
	res, err = svc.FunnelWithOptions(ctx, site, base.Add(-72*time.Hour), base, []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
	if err != nil {
		t.Fatalf("funnel: %v", err)
	}
	if res[0].Visitors != 1 {
		t.Fatalf("clamped funnel visitors = %d, want 1 (only the -23h event is inside the 24h clamp)", res[0].Visitors)
	}
}
