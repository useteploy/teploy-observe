package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// The rollups recompute an overlapping window on every tick — two hours for
// the hourly job, forty-eight for the daily one, and the full history of any
// touched session for the session job. That was safe only if the engine
// collapsed the repeated writes, and Nucleus does not do so reliably: the live
// stats_hourly was carrying 740 duplicated bucket keys out of 1956 rows, and
// stats_daily has no retention policy at all, so its copies accumulate
// forever. Each job now clears the window it is about to rewrite.
//
// These assert the physical row count, which is the property the read path can
// no longer be relied on to paper over.

func rollupTestDB(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return ctx, db, func() { db.Close(); cancel() }
}

func countRows(ctx context.Context, t *testing.T, db *nucleus.Client, table, site string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		fmt.Sprintf(`SELECT COUNT(*) AS n FROM %s WHERE site_id = $1`, table), site)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}

// TestRunHourlyRollup_IsIdempotent runs the job three times, the way the
// overlapping window does in production, and requires one row per bucket key.
func TestRunHourlyRollup_IsIdempotent(t *testing.T) {
	ctx, db, done := rollupTestDB(t)
	defer done()

	site := fmt.Sprintf("test-hourly-%d", time.Now().UnixNano())
	// Inside the job's window: [now-2h, now+1h).
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	for i := 0; i < 4; i++ {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname)
			 VALUES ($1, 'default', $2, $3, $3, 'pageview', $4, '/')`,
			fmt.Sprintf("ev-%s-%d", site, i), site, "sess-"+site, base+int64(i)*1000)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	// A row inside the window that no event justifies — what a previous pass
	// leaves behind when its events are later erased, and the deterministic
	// marker for whether the job clears the window it rewrites.
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO stats_hourly (tenant_id, site_id, ts_bucket, pathname, event_type,
			pageviews, visitors, sessions, bounces, total_duration, version)
		 VALUES ('default', $1, $2, '/ghost', 'pageview', 99, 99, 99, 0, 0, 1)`,
		site, base); err != nil {
		t.Fatalf("insert stale row: %v", err)
	}

	r := NewRollupService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	for i := 0; i < 3; i++ {
		if err := r.RunHourlyRollup(ctx); err != nil {
			t.Fatalf("hourly rollup pass %d: %v", i, err)
		}
	}

	if n := countRows(ctx, t, db, "stats_hourly", site); n != 1 {
		t.Fatalf("stats_hourly: want 1 row after 3 identical passes plus a stale row, got %d", n)
	}

	type statRow struct {
		Pageviews int64 `db:"pageviews"`
	}
	rows, err := nucleus.Query[statRow](ctx, db.SQL(),
		`SELECT pageviews FROM stats_hourly WHERE site_id = $1`, site)
	if err != nil {
		t.Fatalf("read stats_hourly: %v", err)
	}
	if len(rows) != 1 || rows[0].Pageviews != 4 {
		t.Fatalf("want a single bucket with 4 pageviews, got %+v", rows)
	}
}

// TestRunDailyRollup_IsIdempotent is the same for stats_daily, which is never
// pruned and so had unbounded duplicate growth.
func TestRunDailyRollup_IsIdempotent(t *testing.T) {
	ctx, db, done := rollupTestDB(t)
	defer done()

	site := fmt.Sprintf("test-daily-%d", time.Now().UnixNano())
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := 0; i < 3; i++ {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname)
			 VALUES ($1, 'default', $2, $3, $3, 'pageview', $4, '/')`,
			fmt.Sprintf("ev-%s-%d", site, i), site, "sess-"+site, base+int64(i)*1000)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO stats_daily (tenant_id, site_id, ts_bucket, pathname, event_type,
			referrer, browser, os, country, device, utm_source, utm_medium, utm_campaign,
			pageviews, visitors, sessions, bounces, total_duration, version)
		 VALUES ('default', $1, $2, '/ghost', 'pageview', '', '', '', '', '', '', '', '',
			99, 99, 99, 0, 0, 1)`,
		site, base); err != nil {
		t.Fatalf("insert stale row: %v", err)
	}

	r := NewRollupService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	for i := 0; i < 3; i++ {
		if err := r.RunDailyRollup(ctx); err != nil {
			t.Fatalf("daily rollup pass %d: %v", i, err)
		}
	}

	if n := countRows(ctx, t, db, "stats_daily", site); n != 1 {
		t.Fatalf("stats_daily: want 1 row after 3 identical passes plus a stale row, got %d", n)
	}
}

// TestRunSessionRollup_IsIdempotent covers the highest-rate producer: the
// session job rewrote every still-active session every five minutes, so a
// long-lived session accumulated a row per pass and every COUNT(*) over the
// sessions table — bounce rate, average duration, the session list, retention
// cohorts, release health — counted them all.
func TestRunSessionRollup_IsIdempotent(t *testing.T) {
	ctx, db, done := rollupTestDB(t)
	defer done()

	site := fmt.Sprintf("test-sessroll-%d", time.Now().UnixNano())
	session := "sess-" + site
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname)
			 VALUES ($1, 'default', $2, $3, $3, 'pageview', $4, '/')`,
			fmt.Sprintf("ev-%s-%d", site, i), site, session,
			now.Add(-time.Duration(i)*time.Minute).UnixMilli())
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	// A stale earlier computation of the same session, as the 5-minute job
	// left behind on every pass.
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO sessions (tenant_id, site_id, session_id, first_ts, last_ts,
			pageviews, events_count, entry_url, exit_url, is_bounce, version)
		 VALUES ('default', $1, $2, 1, 2, 99, 99, '/ghost', '/ghost', 'true', 1)`,
		site, session); err != nil {
		t.Fatalf("insert stale session: %v", err)
	}

	r := NewRollupService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	for i := 0; i < 3; i++ {
		if err := r.RunSessionRollup(ctx); err != nil {
			t.Fatalf("session rollup pass %d: %v", i, err)
		}
	}

	if n := countRows(ctx, t, db, "sessions", site); n != 1 {
		t.Fatalf("sessions: want 1 row after 3 passes over the same session, got %d", n)
	}
}
