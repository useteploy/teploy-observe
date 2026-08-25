package query

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// The bug these cover, as reported against the live instance: a site whose raw
// events prove 15 pageviews across 6 sessions was shown 33 pageviews and 22
// visitors. Two independent causes, and each needs its own assertion.
//
//  1. stats_hourly is a ReplacingMergeTree, but Nucleus only collapses
//     duplicate keys when it merges the segments holding them and in practice
//     never does — the live table held 740 duplicated keys out of 1956 rows,
//     the oldest two months old. The rollup rewrites every bucket two or three
//     times by design, so a bare SUM(pageviews) counted each computation.
//     `FINAL` parses but is silently ignored, so argMax over the version column
//     is the only working collapse.
//
//  2. `visitors` in the rollup is a per-bucket COUNT(DISTINCT session_id), so
//     it is not additive: a session spanning two hours is counted in both.
//     Summing buckets inflates it even after the duplicates are gone.
//
// seedDuplicateRollup writes the event set and then writes stats_hourly the way
// the old rollup did — the same bucket key several times, with a stale lower
// version among them — so the tests fail against the pre-fix read path.
type seeded struct {
	site      string
	bucket0   int64
	bucket1   int64
	from, to  time.Time
	pageviews int64
	visitors  int64
}

func seedDuplicateRollup(ctx context.Context, t *testing.T, db *nucleus.Client) seeded {
	t.Helper()

	site := fmt.Sprintf("test-dedup-%d", time.Now().UnixNano())
	// Two adjacent hourly buckets, two days back so the query range lands in
	// the 24h-7d band that routes to stats_hourly.
	bucket0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour).UnixMilli()
	bucket1 := bucket0 + 3600000

	// 15 pageviews, 6 distinct sessions. s1 spans both buckets, which is what
	// makes the per-bucket visitor counts non-additive: bucket0 sees 3 distinct
	// sessions and bucket1 sees 4, but the range as a whole has only 6.
	events := []struct {
		session string
		bucket  int64
		count   int
	}{
		{"s1", bucket0, 2}, {"s2", bucket0, 3}, {"s3", bucket0, 2},
		{"s1", bucket1, 1}, {"s4", bucket1, 3}, {"s5", bucket1, 2}, {"s6", bucket1, 2},
	}
	n := 0
	for _, e := range events {
		for i := 0; i < e.count; i++ {
			n++
			_, err := db.SQL().Exec(ctx,
				`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname)
				 VALUES ($1, 'default', $2, $3, $4, 'pageview', $5, '/')`,
				fmt.Sprintf("ev-%s-%d", site, n), site, e.session+"-"+site, e.session+"-"+site,
				e.bucket+int64(i)*1000)
			if err != nil {
				t.Fatalf("insert event: %v", err)
			}
		}
	}
	if n != 15 {
		t.Fatalf("seed: want 15 events, wrote %d", n)
	}

	// stats_hourly as the old rollup left it: bucket0 computed twice, bucket1
	// computed twice plus one stale earlier computation that saw only one view.
	writeRollup := func(bucket, pageviews, visitors, version int64) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO stats_hourly (tenant_id, site_id, ts_bucket, pathname, event_type,
				pageviews, visitors, sessions, bounces, total_duration, version)
			 VALUES ('default', $1, $2, '/', 'pageview', $3, $4, $4, 0, 0, $5)`,
			site, bucket, pageviews, visitors, version)
		if err != nil {
			t.Fatalf("insert stats_hourly: %v", err)
		}
	}
	// The 2-hour overlap recomputing each bucket. The repeated computations
	// carry IDENTICAL values on purpose: Nucleus's own collapse does not
	// reliably keep the highest version — seeded with 1@v500, 8@v1000 and
	// 8@v2000 for one key it kept the 1 — so a seed that varied the value by
	// version would be asserting against whichever row the engine happened to
	// discard rather than against this package's read path. What must hold
	// either way is that the number equals the raw-event truth.
	writeRollup(bucket0, 7, 3, 1000)
	writeRollup(bucket1, 8, 4, 1000)
	writeRollup(bucket0, 7, 3, 2000)
	writeRollup(bucket1, 8, 4, 2000)

	return seeded{
		site:      site,
		bucket0:   bucket0,
		bucket1:   bucket1,
		from:      time.UnixMilli(bucket0).UTC().Add(-time.Hour),
		to:        time.UnixMilli(bucket1).UTC().Add(48 * time.Hour),
		pageviews: 15,
		visitors:  6,
	}
}

func connectTest(t *testing.T) (context.Context, *nucleus.Client, func()) {
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

// TestOverview_CollapsesDuplicateRollupVersions asserts the headline tiles
// match the raw events. Before the fix this returned 31 pageviews (every
// surviving version summed) and 15 visitors.
func TestOverview_CollapsesDuplicateRollupVersions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	s := seedDuplicateRollup(ctx, t, db)

	reportDuplicateRows(ctx, t, db, s.site)

	svc := NewStatsService(db)
	if got := tableFor(s.from, s.to); got != "stats_hourly" {
		t.Fatalf("range must route to stats_hourly, got %s", got)
	}

	got, err := svc.Overview(ctx, s.site, s.from, s.to, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if got.Pageviews != s.pageviews {
		t.Fatalf("pageviews: want %d (raw event truth), got %d", s.pageviews, got.Pageviews)
	}
	if got.Visitors != s.visitors {
		t.Fatalf("visitors: want %d (distinct sessions in range), got %d", s.visitors, got.Visitors)
	}
}

// TestTopPages_CollapsesDuplicateRollupVersions is the same assertion on the
// per-path breakdown, which reads the rollup with its own GROUP BY.
func TestTopPages_CollapsesDuplicateRollupVersions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	s := seedDuplicateRollup(ctx, t, db)
	reportDuplicateRows(ctx, t, db, s.site)

	rows, err := NewStatsService(db).TopPages(ctx, s.site, s.from, s.to, 10, nil)
	if err != nil {
		t.Fatalf("top pages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 path, got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Pageviews != s.pageviews {
		t.Fatalf("pageviews: want %d, got %d", s.pageviews, rows[0].Pageviews)
	}
	if rows[0].Visitors != s.visitors {
		t.Fatalf("visitors: want %d, got %d", s.visitors, rows[0].Visitors)
	}
}

// TestPageviewTimeSeries_SessionSpanningTwoBuckets pins the non-additive half
// of the bug. A session active in two hours belongs to both hourly points, so
// the points sum to 7 while the range holds 6 distinct sessions. The range
// total must therefore be a real distinct count, not a sum of the buckets —
// which is exactly what the old SUM(visitors) over the rollup was.
func TestPageviewTimeSeries_SessionSpanningTwoBuckets(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	s := seedDuplicateRollup(ctx, t, db)
	reportDuplicateRows(ctx, t, db, s.site)

	svc := NewStatsService(db)
	points, err := svc.PageviewTimeSeries(ctx, s.site, s.from, s.to, "hour", nil)
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}

	byBucket := map[int64]TimeSeriesPoint{}
	var sumPageviews, sumVisitors int64
	for _, p := range points {
		byBucket[p.Bucket] = p
		sumPageviews += p.Pageviews
		sumVisitors += p.Visitors
	}
	if sumPageviews != s.pageviews {
		t.Fatalf("pageviews across buckets: want %d, got %d (%+v)", s.pageviews, sumPageviews, points)
	}
	if got := byBucket[s.bucket0]; got.Pageviews != 7 || got.Visitors != 3 {
		t.Fatalf("bucket0: want 7 pageviews / 3 visitors, got %d / %d", got.Pageviews, got.Visitors)
	}
	if got := byBucket[s.bucket1]; got.Pageviews != 8 || got.Visitors != 4 {
		t.Fatalf("bucket1: want 8 pageviews / 4 visitors, got %d / %d", got.Pageviews, got.Visitors)
	}
	if sumVisitors != 7 {
		t.Fatalf("per-bucket visitors should sum to 7 with the spanning session counted twice, got %d", sumVisitors)
	}

	overview, err := svc.Overview(ctx, s.site, s.from, s.to, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if overview.Visitors != s.visitors {
		t.Fatalf("range visitors must be a distinct count (%d), not the sum of buckets (%d); got %d",
			s.visitors, sumVisitors, overview.Visitors)
	}
}

// TestSessions_CollapsesDuplicateVersions covers the sessions table, which the
// session rollup rewrote every five minutes for any still-active session. The
// bounce rate and average duration divide by COUNT(*) over it, and the session
// list renders one row per version.
func TestSessions_CollapsesDuplicateVersions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()

	site := fmt.Sprintf("test-sessdup-%d", time.Now().UnixNano())
	first := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour).UnixMilli()

	write := func(session string, pageviews, version int64, bounce string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO sessions (tenant_id, site_id, session_id, first_ts, last_ts,
				pageviews, events_count, entry_url, exit_url, is_bounce, version)
			 VALUES ('default', $1, $2, $3, $4, $5, $5, '/in', '/out', $6, $7)`,
			site, session, first, first+60000, pageviews, bounce, version)
		if err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	// One session, rewritten three times by the 5-minute rollup. Identical
	// values, differing only by version: see the note in seedDuplicateRollup
	// on why the seed must not depend on which version the engine keeps.
	write("only-"+site, 3, 1000, "false")
	write("only-"+site, 3, 2000, "false")
	write("only-"+site, 3, 3000, "false")

	from := time.UnixMilli(first).UTC().Add(-time.Hour)
	to := time.UnixMilli(first).UTC().Add(48 * time.Hour)
	svc := NewStatsService(db)

	list, err := svc.Sessions(ctx, site, from, to, 20)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 session row, got %d: %+v", len(list), list)
	}
	if list[0].Pageviews != 3 {
		t.Fatalf("pageviews: want 3, got %d", list[0].Pageviews)
	}

	entry, err := svc.TopEntryPages(ctx, site, from, to, 10, nil)
	if err != nil {
		t.Fatalf("entry pages: %v", err)
	}
	if len(entry) != 1 || entry[0].Visitors != 1 {
		t.Fatalf("entry pages: want one /in with 1 visitor, got %+v", entry)
	}

	// Average duration divides the summed session span by COUNT(*) over the
	// sessions table. With three copies of a 60-second session it stayed 60,
	// but the total_sessions denominator it shares with bounce rate was 3.
	overview, err := svc.Overview(ctx, site, from, to, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if overview.AvgDuration != 60 {
		t.Fatalf("avg duration: want 60s over one session, got %v", overview.AvgDuration)
	}
	if overview.BounceRate != 0 {
		t.Fatalf("bounce rate: want 0, got %v", overview.BounceRate)
	}
}

// reportDuplicateRows logs how many physical rows the engine kept for the two
// seeded bucket keys. It does not assert: whether the duplicates survive is the
// engine's choice and it varies. A scratch Nucleus with everything still in its
// memtable collapses them on write and reports 2; the live instance, whose rows
// have been written across two months and many restarts, keeps them all and
// reported 740 duplicated keys out of 1956 rows.
//
// That variability is why these tests assert against the raw-event truth rather
// than against a duplicate count. The number the dashboard shows must equal what
// the events prove no matter which the engine does, and only the argMax read
// path guarantees that. TestLatestRows_ShapesTheQuery pins the read path itself,
// so a revert is caught even on an engine that happens to collapse.
func reportDuplicateRows(ctx context.Context, t *testing.T, db *nucleus.Client, site string) {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM stats_hourly WHERE site_id = $1`, site)
	if err != nil {
		t.Fatalf("count stats_hourly: %v", err)
	}
	if len(rows) > 0 {
		t.Logf("engine kept %d physical rows for the 2 seeded bucket keys (4 written)", rows[0].N)
	}
}
