package query

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// The bug these cover: after unique counts moved off the non-additive rollup
// column and onto raw events, the pageview panel and every visitor panel on the
// same screen described different windows. Pageviews come from stats_hourly /
// stats_daily (a year, then indefinitely); uniques came from events
// (OBSERVE_RAW_RETENTION_DAYS, 30 by default). The date picker offers Last 90
// days, Last 12 months and All time, so on any of them the visitor figures were
// severely UNDER-reported and nothing said so.
//
// The fix tiers the source by what can answer exactly, and marks the case where
// nothing can. These tests pin the tiering and the marker.

// TestUniqueCoverage_TiersOnRangeStartNotLength is the core table. The boundary
// has to be the range's `from`: a 7-day window sitting a year back is outside
// raw retention just as much as a 12-month one, and tiering on the range's
// LENGTH would route it to raw events and return almost nothing.
func TestUniqueCoverage_TiersOnRangeStartNotLength(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	w := RetentionWindows{RawDays: 30, SessionsDays: 90}
	day := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

	cases := []struct {
		name       string
		from, to   time.Time
		wantSource UniqueSource
		wantExact  bool
	}{
		{"today", day(1), now, SourceEvents, true},
		{"last 7 days", day(7), now, SourceEvents, true},
		{"last 30 days, exactly on the raw boundary", day(30), now, SourceEvents, true},
		{"last 90 days is past raw retention", day(90), now, SourceSessions, true},
		{"just past the raw boundary", day(31), now, SourceSessions, true},
		{"on the sessions boundary", day(90), now, SourceSessions, true},
		{"last 12 months is past both", day(365), now, SourceSessions, false},
		{"all time is past both", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), now, SourceSessions, false},

		// Length says "one week"; the start says "a year ago". The start wins.
		{"a short window a year back", day(372), day(365), SourceSessions, false},
		// And the mirror: a long window that still starts inside raw retention.
		{"a long window starting inside raw retention", day(29), day(1), SourceEvents, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := w.Coverage(tc.from, tc.to, now)
			if got.Source != tc.wantSource {
				t.Fatalf("source: want %s, got %s (%+v)", tc.wantSource, got.Source, got)
			}
			if got.Exact != tc.wantExact {
				t.Fatalf("exact: want %v, got %v (%+v)", tc.wantExact, got.Exact, got)
			}
		})
	}
}

// TestUniqueCoverage_FollowsConfiguredRetention is the reason the windows are
// read from config rather than written as constants: an operator who raises
// OBSERVE_RAW_RETENTION_DAYS must get a wider exact tier without a code change.
func TestUniqueCoverage_FollowsConfiguredRetention(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	from := now.Add(-60 * 24 * time.Hour)

	if got := (RetentionWindows{RawDays: 30, SessionsDays: 90}).Coverage(from, now, now); got.Source != SourceSessions {
		t.Fatalf("default retention: 60 days back must leave raw events, got %s", got.Source)
	}
	// Same range, raw retention raised past it.
	if got := (RetentionWindows{RawDays: 180, SessionsDays: 90}).Coverage(from, now, now); got.Source != SourceEvents || !got.Exact {
		t.Fatalf("raised raw retention must widen the exact tier, got %+v", got)
	}
	// Sessions retention raised: a range past both windows becomes exact.
	deep := now.Add(-300 * 24 * time.Hour)
	if got := (RetentionWindows{RawDays: 30, SessionsDays: 90}).Coverage(deep, now, now); got.Exact {
		t.Fatalf("300 days back cannot be exact at 90-day sessions retention: %+v", got)
	}
	if got := (RetentionWindows{RawDays: 30, SessionsDays: 400}).Coverage(deep, now, now); !got.Exact || got.Source != SourceSessions {
		t.Fatalf("raised sessions retention must make it exact, got %+v", got)
	}
	// A policy of 0 days prunes nothing (RetentionService.RunCleanup skips
	// those), so it must never downgrade anything.
	if got := (RetentionWindows{RawDays: 0, SessionsDays: 90}).Coverage(deep, now, now); got.Source != SourceEvents || !got.Exact {
		t.Fatalf("unbounded raw retention must stay on events and stay exact, got %+v", got)
	}
}

// TestUniqueCoverage_BeyondBothWindowsReportsMarkerNotSilence is the whole
// point of the change. A range no table can answer in full must return the
// figure the data supports PLUS a truthful statement of the window it covers.
// Silently returning a small number is the bug.
func TestUniqueCoverage_BeyondBothWindowsReportsMarkerNotSilence(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	w := RetentionWindows{RawDays: 30, SessionsDays: 90}
	from := now.Add(-365 * 24 * time.Hour)

	got := w.Coverage(from, now, now)
	if got.Exact {
		t.Fatalf("a 12-month range cannot be exact at 90-day sessions retention: %+v", got)
	}
	if got.Note == "" {
		t.Fatal("a non-exact coverage must carry the sentence shown to the user, got an empty note")
	}
	if got.CoveredDays != w.SessionsDays {
		t.Fatalf("covered_days: want %d (the window that answered), got %d", w.SessionsDays, got.CoveredDays)
	}
	if got.RangeFrom != from.UnixMilli() {
		t.Fatalf("range_from: want %d, got %d", from.UnixMilli(), got.RangeFrom)
	}
	wantCovered := now.Add(-90 * 24 * time.Hour).UnixMilli()
	if got.CoveredFrom != wantCovered {
		t.Fatalf("covered_from: want the sessions cutoff %d, got %d", wantCovered, got.CoveredFrom)
	}
	if got.CoveredFrom <= got.RangeFrom {
		t.Fatalf("covered_from must be later than range_from when the answer is partial: %+v", got)
	}
	if !strings.Contains(got.Note, "90 days") {
		t.Fatalf("the note must name the window it actually covers, got %q", got.Note)
	}
}

// TestBreakdownSQL_PicksTableByTier pins the read path itself, without a
// database. A scratch Nucleus seeded moments ago has everything inside raw
// retention, so an integration test alone cannot tell the two branches apart.
func TestBreakdownSQL_PicksTableByTier(t *testing.T) {
	svc := &StatsService{retention: RetentionWindows{RawDays: 30, SessionsDays: 90}}
	now := time.Now().UTC()
	b := breakdown{Expr: "browser", Alias: "browser", Where: "browser != ''", Cols: []string{"browser"}}

	inRaw, _ := svc.breakdownSQL("s", now.Add(-7*24*time.Hour), now, b, 10, nil)
	if !strings.Contains(inRaw, "FROM events") {
		t.Fatalf("inside raw retention the count must come from events:\n%s", inRaw)
	}
	if strings.Contains(inRaw, "FROM sessions") {
		t.Fatalf("inside raw retention nothing should touch sessions:\n%s", inRaw)
	}

	pastRaw, _ := svc.breakdownSQL("s", now.Add(-60*24*time.Hour), now, b, 10, nil)
	if !strings.Contains(pastRaw, "FROM sessions") {
		t.Fatalf("past raw retention the count must come from sessions:\n%s", pastRaw)
	}
	if strings.Contains(pastRaw, "FROM events") {
		t.Fatalf("past raw retention nothing should read the pruned events table:\n%s", pastRaw)
	}
	// The sessions branch has to collapse the replacing table's versions and
	// order on the OUTPUT name — Nucleus resolves ORDER BY against the select
	// list only and silently drops a term it cannot resolve, which would leave
	// the LIMIT slicing rows in hash order.
	if !strings.Contains(pastRaw, "argMax(browser, version) AS browser") {
		t.Fatalf("sessions is a replacing table; the read must select the latest version:\n%s", pastRaw)
	}
	if !strings.Contains(pastRaw, "ORDER BY visitors DESC, browser ASC") {
		t.Fatalf("the ordering must name the output aliases:\n%s", pastRaw)
	}
	if !strings.Contains(pastRaw, "LIMIT 10") {
		t.Fatalf("the limit must survive the sessions branch:\n%s", pastRaw)
	}
}

// TestUniqueCoverage_FilterOnlyEventsCanAnswerStaysOnEventsAndIsMarked covers
// the one case the sessions tier cannot rescue. The sessions table has no
// pathname / event_type / distinct_id column, so a filter naming one has to be
// answered from raw events even past raw retention — and that shorter coverage
// gets the same explicit marker rather than a quietly smaller number.
func TestUniqueCoverage_FilterOnlyEventsCanAnswerStaysOnEventsAndIsMarked(t *testing.T) {
	svc := &StatsService{retention: RetentionWindows{RawDays: 30, SessionsDays: 90}}
	now := time.Now().UTC()
	from := now.Add(-60 * 24 * time.Hour)

	unfiltered := svc.coverage(from, now, nil)
	if unfiltered.Source != SourceSessions || !unfiltered.Exact {
		t.Fatalf("unfiltered, 60 days back must be exact from sessions: %+v", unfiltered)
	}

	fb := NewFilterBuilder(4)
	fb.Add("pathname", "/pricing")
	filtered := svc.coverage(from, now, fb)
	if filtered.Source != SourceEvents {
		t.Fatalf("a pathname filter cannot be applied to sessions; want events, got %s", filtered.Source)
	}
	if filtered.Exact {
		t.Fatalf("events only reach 30 days back here, so this cannot claim to be exact: %+v", filtered)
	}
	if filtered.Note == "" || filtered.CoveredDays != 30 {
		t.Fatalf("want a marker naming the 30-day raw window, got %+v", filtered)
	}

	// A filter the sessions table CAN answer must not downgrade anything.
	fb2 := NewFilterBuilder(4)
	fb2.Add("browser", "Chrome")
	if got := svc.coverage(from, now, fb2); got.Source != SourceSessions || !got.Exact {
		t.Fatalf("a browser filter exists on sessions and must stay there: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Integration: the numbers, against a real Nucleus.
// ---------------------------------------------------------------------------

// seedPastRawRetention writes the state a live instance is in past raw
// retention: session rows survive, the raw events they were built from do not.
// Each session is written twice, the way the rollup rewrites it, so the read
// has to collapse the replacing table's versions as well as pick the table.
func seedPastRawRetention(ctx context.Context, t *testing.T, db *nucleus.Client) (site string, sessions int64) {
	t.Helper()
	site = fmt.Sprintf("test-tier-%d", time.Now().UnixNano())
	browsers := []string{"Chrome", "Chrome", "Firefox", "Safari", "Firefox"}

	// 45 days back: past the 30-day raw window, inside the 90-day session one.
	base := time.Now().UTC().Add(-45 * 24 * time.Hour)
	for i, b := range browsers {
		first := base.Add(time.Duration(i) * time.Hour)
		for _, version := range []int64{1000, 2000} {
			if _, err := db.SQL().Exec(ctx,
				`INSERT INTO sessions (tenant_id, site_id, session_id, first_ts, last_ts,
					pageviews, events_count, entry_url, exit_url, referrer, browser, os,
					device, country, language, screen_width, screen_height,
					utm_source, utm_medium, utm_campaign, is_bounce, version, release_tag)
				 VALUES ('default', $1, $2, $3, $4, 2, 2, '/', '/pricing',
					'https://news.ycombinator.com/', $5, 'macOS', 'desktop', 'US', 'en-US',
					1440, 900, '', '', '', 'false', $6, '')`,
				site, fmt.Sprintf("sess-%s-%d", site, i),
				first.UnixMilli(), first.Add(5*time.Minute).UnixMilli(), b, version,
			); err != nil {
				t.Fatalf("insert session: %v", err)
			}
		}
	}

	// The daily rollup for the same window, so pageviews still answer the full
	// range while the raw events behind them are gone. 5 sessions x 2 views.
	bucket := base.Truncate(24 * time.Hour).UnixMilli()
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO stats_daily (tenant_id, site_id, ts_bucket, pathname, event_type,
			referrer, browser, os, country, device, utm_source, utm_medium, utm_campaign,
			pageviews, visitors, sessions, bounces, total_duration, version)
		 VALUES ('default', $1, $2, '/', 'pageview', '', '', '', '', '', '', '', '',
			10, 5, 5, 0, 0, 1000)`,
		site, bucket); err != nil {
		t.Fatalf("insert stats_daily: %v", err)
	}

	return site, int64(len(browsers))
}

// TestOverview_PastRawRetentionCountsFromSessions is the regression. The raw
// events for this window are gone, so before the fix the Visitors tile read 0
// beside a Pageviews tile reading 10 — a silent under-report, not an error.
func TestOverview_PastRawRetentionCountsFromSessions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	site, want := seedPastRawRetention(ctx, t, db)

	svc := NewStatsService(db)
	from := time.Now().UTC().Add(-60 * 24 * time.Hour)
	to := time.Now().UTC()

	if src := svc.coverage(from, to, nil).Source; src != SourceSessions {
		t.Fatalf("60 days back must be answered from sessions, got %s", src)
	}

	got, err := svc.Overview(ctx, site, from, to, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if got.Visitors != want {
		t.Fatalf("visitors: want %d (the sessions on disk), got %d — a range past raw retention must not read 0", want, got.Visitors)
	}
	if got.Pageviews != 10 {
		t.Fatalf("pageviews: want 10 from the rollup, got %d", got.Pageviews)
	}
}

// TestTopBrowsers_PastRawRetentionCountsFromSessions is the same regression on
// a breakdown panel. Before the fix it returned no rows at all.
func TestTopBrowsers_PastRawRetentionCountsFromSessions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	site, _ := seedPastRawRetention(ctx, t, db)

	rows, err := NewStatsService(db).TopBrowsers(ctx, site,
		time.Now().UTC().Add(-60*24*time.Hour), time.Now().UTC(), 10, nil)
	if err != nil {
		t.Fatalf("top browsers: %v", err)
	}

	got := map[string]int64{}
	for _, r := range rows {
		got[r.Browser] = r.Visitors
	}
	want := map[string]int64{"Chrome": 2, "Firefox": 2, "Safari": 1}
	if len(got) != len(want) {
		t.Fatalf("want %d browsers, got %d: %+v", len(want), len(got), rows)
	}
	for b, n := range want {
		if got[b] != n {
			t.Fatalf("browser %s: want %d visitors, got %d (full: %+v)", b, n, got[b], rows)
		}
	}
	// Rank order has to be total, not hash order — Chrome and Firefox tie.
	if rows[0].Browser != "Chrome" || rows[1].Browser != "Firefox" || rows[2].Browser != "Safari" {
		t.Fatalf("tied rows must be ordered by name after the count: %+v", rows)
	}
}

// TestPageviewTimeSeries_PastRawRetentionCountsFromSessions checks the chart
// agrees with the tiles: the visitor series must be populated over a window
// whose raw events are gone, instead of a flat zero line under a real pageview
// line.
func TestPageviewTimeSeries_PastRawRetentionCountsFromSessions(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	site, want := seedPastRawRetention(ctx, t, db)

	points, err := NewStatsService(db).PageviewTimeSeries(ctx, site,
		time.Now().UTC().Add(-60*24*time.Hour), time.Now().UTC(), "day", nil)
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	var visitors int64
	for _, p := range points {
		visitors += p.Visitors
	}
	if visitors != want {
		t.Fatalf("visitors across the series: want %d, got %d (points: %+v)", want, visitors, points)
	}
}

// TestOverview_BeyondBothWindowsReportsWhatSurvivesAndSaysSo is tier 3 end to
// end: the count is the real one the sessions table still holds, and the
// coverage the dashboard labels the panels with says it covers 90 days, not the
// 200 asked for.
func TestOverview_BeyondBothWindowsReportsWhatSurvivesAndSaysSo(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	site, want := seedPastRawRetention(ctx, t, db)

	svc := NewStatsService(db)
	from := time.Now().UTC().Add(-200 * 24 * time.Hour)
	to := time.Now().UTC()

	cov := svc.UniqueCoverageFor(from, to, nil)
	if cov.Exact {
		t.Fatalf("200 days is past both retention windows and cannot be exact: %+v", cov)
	}
	if cov.Note == "" {
		t.Fatal("tier 3 must carry the marker the UI shows, got an empty note")
	}

	got, err := svc.Overview(ctx, site, from, to, nil)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if got.Visitors != want {
		t.Fatalf("visitors: want %d (what the data supports), got %d", want, got.Visitors)
	}
}
