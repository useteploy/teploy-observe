package query

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// The bug these cover: the breakdown panels on the dashboard rank by a metric
// that ties constantly — on a small site nearly every browser, country and
// referrer sits at one or two visitors — and nothing decided the order within
// a tie.
//
// Server side, Nucleus emits GROUP BY results in hash order, so `ORDER BY
// visitors DESC` alone leaves tied rows in an order that changes with the
// LIMIT. Verified against v0.1.8 on six paths tied at one view each: LIMIT 3
// returned zebra/pear/fig and LIMIT 10 returned pear/apple/fig/kiwi/zebra/
// mango, so the top three were neither a prefix of the full list nor a
// meaningful "top" at all — and pressing "View all" reshuffled the rows the
// user was already looking at.
//
// Client side, the panel then rendered that array verbatim for the default
// descending direction and only sorted on the ascending click. Array.sort is
// stable, so when every value tied the ascending pass reproduced the order it
// was handed and the sort control appeared to do nothing at all.
//
// This file pins the server half. The client half is sortRows() in
// ui/src/utils/sortRows.ts, which applies the same value-then-label order.

// TestRankChannels_IsDeterministicAndRespectsLimit needs no database.
//
// Channels are tallied into a Go map and then ordered, and ranging over a map
// is randomised, so a sort on the count alone leaves tied channels in a
// different order on every call. The old code also ignored the limit entirely,
// which is why the panel's "View all" affordance never behaved.
func TestRankChannels_IsDeterministicAndRespectsLimit(t *testing.T) {
	counts := map[string]int64{
		ChannelDirect:   3,
		ChannelOrganic:  1,
		ChannelSocial:   1,
		ChannelReferral: 1,
		ChannelEmail:    1,
		ChannelPaid:     1,
	}

	want := []ChannelStat{
		{Channel: ChannelDirect, Visitors: 3},
		// Five channels tied at 1, so the name decides: Email, Organic
		// Search, Paid, Referral, Social.
		{Channel: ChannelEmail, Visitors: 1},
		{Channel: ChannelOrganic, Visitors: 1},
		{Channel: ChannelPaid, Visitors: 1},
		{Channel: ChannelReferral, Visitors: 1},
		{Channel: ChannelSocial, Visitors: 1},
	}

	// Repeated because the failure is a randomised map order: one call can
	// agree with the expected order by chance, a hundred cannot.
	for i := 0; i < 100; i++ {
		got := rankChannels(counts, 0)
		if len(got) != len(want) {
			t.Fatalf("call %d: want %d channels, got %d", i, len(want), len(got))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("call %d: position %d: want %+v, got %+v (full: %+v)",
					i, j, want[j], got[j], got)
			}
		}
	}

	// The limit must cut the ranked list, and the cut must be a prefix of it.
	top3 := rankChannels(counts, 3)
	if len(top3) != 3 {
		t.Fatalf("limit 3: want 3 channels, got %d (%+v)", len(top3), top3)
	}
	for j := range top3 {
		if top3[j] != want[j] {
			t.Fatalf("limit 3 is not a prefix of the full ranking: position %d want %+v, got %+v",
				j, want[j], top3[j])
		}
	}
}

// seedTiedBrowsers writes one session per browser, so every browser ties at a
// single visitor and only the tie-break can decide the order.
func seedTiedBrowsers(ctx context.Context, t *testing.T, db *nucleus.Client) (site string, browsers []string) {
	t.Helper()

	site = fmt.Sprintf("test-order-%d", time.Now().UnixNano())
	// Deliberately not alphabetical, and not insertion-ordered either, so a
	// pass cannot come from the engine happening to return either.
	browsers = []string{"Zebra", "Mango", "Apple", "Pear", "Kiwi", "Fig"}
	now := time.Now().UTC().Add(-time.Hour).UnixMilli()

	for i, b := range browsers {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id,
				event_type, timestamp, pathname, browser)
			 VALUES ($1, 'default', $2, $3, $3, 'pageview', $4, '/', $5)`,
			fmt.Sprintf("ev-%s-%d", site, i), site,
			fmt.Sprintf("sess-%s-%d", site, i), now+int64(i)*1000, b)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	return site, browsers
}

// TestTopBrowsers_TiedValuesRankDeterministically is the server half of the
// sort fix. Before it, the two calls below disagreed: the engine's hash order
// for the tied group is not stable across LIMITs, so the "top 3" was an
// arbitrary three of the six and was not a prefix of the full list.
func TestTopBrowsers_TiedValuesRankDeterministically(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()
	site, browsers := seedTiedBrowsers(ctx, t, db)

	svc := NewStatsService(db)
	from := time.Now().UTC().Add(-2 * time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	all, err := svc.TopBrowsers(ctx, site, from, to, 100, nil)
	if err != nil {
		t.Fatalf("top browsers (limit 100): %v", err)
	}
	if len(all) != len(browsers) {
		t.Fatalf("want %d browsers, got %d: %+v", len(browsers), len(all), all)
	}

	// Every browser has exactly one visitor, so the name is the whole order.
	want := []string{"Apple", "Fig", "Kiwi", "Mango", "Pear", "Zebra"}
	for i, w := range want {
		if all[i].Browser != w {
			t.Fatalf("position %d: want %s, got %s (full: %+v)", i, w, all[i].Browser, all)
		}
		if all[i].Visitors != 1 {
			t.Fatalf("%s: want 1 visitor, got %d", all[i].Browser, all[i].Visitors)
		}
	}

	// The truncated read must be the head of the same ranking, or paging the
	// panel from ten rows to a hundred shows a different set in a different
	// order rather than more of the same one.
	top3, err := svc.TopBrowsers(ctx, site, from, to, 3, nil)
	if err != nil {
		t.Fatalf("top browsers (limit 3): %v", err)
	}
	if len(top3) != 3 {
		t.Fatalf("want 3 browsers, got %d: %+v", len(top3), top3)
	}
	for i := range top3 {
		if top3[i].Browser != all[i].Browser {
			t.Fatalf("limit 3 is not a prefix of the full ranking: position %d has %s there and %s here",
				i, all[i].Browser, top3[i].Browser)
		}
	}
}

// TestTopPages_TiedValuesRankDeterministically covers the one panel that ranks
// on pageviews rather than visitors, and does so through the replacing-table
// collapse rather than off raw events.
func TestTopPages_TiedValuesRankDeterministically(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()

	site := fmt.Sprintf("test-order-pages-%d", time.Now().UnixNano())
	paths := []string{"/zebra", "/mango", "/apple", "/pear", "/kiwi", "/fig"}
	// Two days back so the range routes to stats_hourly and the read goes
	// through LatestRows rather than straight at events.
	bucket := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour).UnixMilli()
	for i, p := range paths {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO stats_hourly (tenant_id, site_id, ts_bucket, pathname, event_type,
				pageviews, visitors, sessions, bounces, total_duration, version)
			 VALUES ('default', $1, $2, $3, 'pageview', 1, 1, 1, 0, 0, 1000)`,
			site, bucket, p)
		if err != nil {
			t.Fatalf("insert stats_hourly: %v", err)
		}
		_ = i
	}

	from := time.UnixMilli(bucket).UTC().Add(-time.Hour)
	to := time.UnixMilli(bucket).UTC().Add(48 * time.Hour)
	if got := tableFor(from, to); got != "stats_hourly" {
		t.Fatalf("range must route to stats_hourly, got %s", got)
	}

	svc := NewStatsService(db)
	all, err := svc.TopPages(ctx, site, from, to, 100, nil)
	if err != nil {
		t.Fatalf("top pages (limit 100): %v", err)
	}
	if len(all) != len(paths) {
		t.Fatalf("want %d paths, got %d: %+v", len(paths), len(all), all)
	}
	want := []string{"/apple", "/fig", "/kiwi", "/mango", "/pear", "/zebra"}
	for i, w := range want {
		if all[i].Pathname != w {
			t.Fatalf("position %d: want %s, got %s (full: %+v)", i, w, all[i].Pathname, all)
		}
	}

	top3, err := svc.TopPages(ctx, site, from, to, 3, nil)
	if err != nil {
		t.Fatalf("top pages (limit 3): %v", err)
	}
	if len(top3) != 3 {
		t.Fatalf("want 3 paths, got %d: %+v", len(top3), top3)
	}
	for i := range top3 {
		if top3[i].Pathname != all[i].Pathname {
			t.Fatalf("limit 3 is not a prefix of the full ranking: position %d has %s there and %s here",
				i, all[i].Pathname, top3[i].Pathname)
		}
	}
}

// TestBreakdownTieBreaksNameTheOutputColumn covers the four reads whose label
// is projected under a different name than the column it comes from — screens
// (a concatenation), UTM (`utm_source AS value`), and entry/exit pages
// (`entry_url AS pathname`).
//
// Nucleus resolves ORDER BY against the select list's output names only. A
// tie-break that names the source column or repeats the expression parses,
// runs, returns rows and is silently ignored — verified on v0.1.8, where
// `ORDER BY visitors DESC, utm_source ASC` left three tied sources in
// insertion order and `ORDER BY visitors DESC, value ASC` sorted them. There
// is no error to notice, so the only guard is an assertion on the order.
func TestBreakdownTieBreaksNameTheOutputColumn(t *testing.T) {
	ctx, db, done := connectTest(t)
	defer done()

	site := fmt.Sprintf("test-order-alias-%d", time.Now().UnixNano())
	now := time.Now().UTC().Add(-time.Hour).UnixMilli()

	// Widths chosen so the numeric order (800 < 1280 < 1920) and the string
	// order ("1280x720" < "1920x1080" < "800x600") disagree: the assertion
	// then pins which one the read actually applies.
	screens := []struct {
		w, h   int
		source string
	}{
		{1920, 1080, "zeta"},
		{1280, 720, "alpha"},
		{800, 600, "mid"},
	}
	for i, sc := range screens {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id,
				event_type, timestamp, pathname, screen_width, screen_height, utm_source)
			 VALUES ($1, 'default', $2, $3, $3, 'pageview', $4, '/', $5, $6, $7)`,
			fmt.Sprintf("ev-%s-%d", site, i), site, fmt.Sprintf("sess-%s-%d", site, i),
			now+int64(i)*1000, sc.w, sc.h, sc.source)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	// Sessions carry the entry/exit breakdowns. Written out of alphabetical
	// order so the engine's own order cannot pass by accident.
	entries := []struct{ entry, exit string }{
		{"/zebra", "/out-zebra"},
		{"/apple", "/out-apple"},
		{"/mango", "/out-mango"},
	}
	for i, e := range entries {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO sessions (tenant_id, site_id, session_id, first_ts, last_ts,
				entry_url, exit_url, version)
			 VALUES ('default', $1, $2, $3, $3, $4, $5, 1000)`,
			site, fmt.Sprintf("sess-%s-%d", site, i), now+int64(i)*1000, e.entry, e.exit)
		if err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}

	svc := NewStatsService(db)
	from := time.Now().UTC().Add(-2 * time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	screenRows, err := svc.TopScreens(ctx, site, from, to, 10, nil)
	if err != nil {
		t.Fatalf("top screens: %v", err)
	}
	wantScreens := []string{"1280x720", "1920x1080", "800x600"}
	for i, w := range wantScreens {
		if i >= len(screenRows) || screenRows[i].Screen != w {
			t.Fatalf("screens position %d: want %s, got %+v", i, w, screenRows)
		}
	}

	utmRows, err := svc.TopUTM(ctx, site, from, to, "source", 10, nil)
	if err != nil {
		t.Fatalf("top utm: %v", err)
	}
	wantUTM := []string{"alpha", "mid", "zeta"}
	for i, w := range wantUTM {
		if i >= len(utmRows) || utmRows[i].Value != w {
			t.Fatalf("utm position %d: want %s, got %+v", i, w, utmRows)
		}
	}

	entryRows, err := svc.TopEntryPages(ctx, site, from, to, 10, nil)
	if err != nil {
		t.Fatalf("top entry pages: %v", err)
	}
	wantEntry := []string{"/apple", "/mango", "/zebra"}
	for i, w := range wantEntry {
		if i >= len(entryRows) || entryRows[i].Pathname != w {
			t.Fatalf("entry position %d: want %s, got %+v", i, w, entryRows)
		}
	}

	exitRows, err := svc.TopExitPages(ctx, site, from, to, 10, nil)
	if err != nil {
		t.Fatalf("top exit pages: %v", err)
	}
	wantExit := []string{"/out-apple", "/out-mango", "/out-zebra"}
	for i, w := range wantExit {
		if i >= len(exitRows) || exitRows[i].Pathname != w {
			t.Fatalf("exit position %d: want %s, got %+v", i, w, exitRows)
		}
	}
}
