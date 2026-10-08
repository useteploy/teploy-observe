package query

import (
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"reflect"
	"testing"
	"time"
)

func TestAuditEventSeriesScreenChannelAndSessionSegment(t *testing.T) {
	seed, ctx := o04Connect(t)
	svc := NewStatsService(seed.db)
	from := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	to := from.Add(24 * time.Hour)
	for i, r := range []struct {
		session, kind, path, source, medium string
		w, h                                int
	}{{"paid", "pageview", "/paid", "google", "cpc", 1920, 1080}, {"paid", "pageview", "/paid", "bing", "cpc", 1920, 1080}, {"other", "pageview", "/other", "", "", 800, 600}, {"paid", "purchase", "/paid", "google", "cpc", 1920, 1080}} {
		_, err := seed.db.SQL().Exec(ctx, `INSERT INTO events (event_id,tenant_id,site_id,session_id,visit_id,event_type,pathname,timestamp,utm_source,utm_medium,screen_width,screen_height,distinct_id) VALUES ($1,'default',$2,$3,'v',$4,$5,$6,$7,$8,$9,$10,$3)`, r.session+r.kind+string(rune('a'+i)), seed.site, r.session, r.kind, r.path, from.Add(time.Duration(i+1)*time.Hour).UnixMilli(), r.source, r.medium, r.w, r.h)
		if err != nil {
			t.Fatal(err)
		}
	}
	f := StatsInput{EventType: "purchase"}.Filters()
	series, err := svc.PageviewTimeSeries(ctx, seed.site, from, to, "day", f)
	if err != nil || len(series) != 1 || series[0].Pageviews != 1 || series[0].Visitors != 1 {
		t.Fatalf("event series: %+v %v", series, err)
	}
	channels, err := svc.TopChannels(ctx, seed.site, from, to, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		if c.Channel == ChannelPaid && c.Visitors != 1 {
			t.Fatal(channels)
		}
	}
	screen := StatsInput{Screen: "1920x1080"}.Filters()
	// Seed sessions before asking for session-backed fields.
	for _, r := range []struct {
		id, path string
		bounce   string
		duration int64
	}{{"paid", "/paid", "false", 20000}, {"other", "/other", "true", 0}} {
		_, err := seed.db.SQL().Exec(ctx, `INSERT INTO sessions (tenant_id,site_id,session_id,first_ts,last_ts,entry_url,exit_url,is_bounce,version) VALUES ('default',$1,$2,$3,$4,$5,$5,$6,1)`, seed.site, r.id, from.Add(time.Hour).UnixMilli(), from.Add(time.Hour).UnixMilli()+r.duration, r.path, r.bounce)
		if err != nil {
			t.Fatal(err)
		}
	}
	overview, err := svc.Overview(ctx, seed.site, from, to, screen)
	if err != nil || overview.Pageviews != 2 || overview.BounceRate != 0 || overview.AvgDuration != 20 {
		t.Fatalf("screen segment: %+v %v", overview, err)
	}
	f = NewFilterBuilder(4)
	f.Add("pathname", "/paid")
	entries, err := svc.TopEntryPages(ctx, seed.site, from, to, 10, f)
	if err != nil || len(entries) != 1 || entries[0].Pathname != "/paid" || entries[0].Visitors != 1 {
		t.Fatalf("entry segment: %+v %v", entries, err)
	}
	f = NewFilterBuilder(4)
	f.AddIn("distinct_id", []string{})
	overview, err = svc.Overview(ctx, seed.site, from, to, f)
	if err != nil || overview.Pageviews != 0 || overview.AvgDuration != 0 || overview.BounceRate != 0 {
		t.Fatalf("empty cohort: %+v %v", overview, err)
	}
	ids, err := svc.channelEventIDs(ctx, seed.site, ChannelPaid, from, to)
	if err != nil || len(ids) != 3 {
		t.Fatalf("channel IDs: %v %v", ids, err)
	}
	guarded := NewStatsService(seed.db).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	if _, err := guarded.EventPropertyKeys(ctx, seed.site, "pageview", from, to); err == nil {
		t.Fatal("property keys escaped tiny row budget")
	}
	attr := NewAttributionService(seed.db).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	if _, err := attr.AttributionByModel(ctx, seed.site, AttributionFirstTouch, from.UnixMilli(), to.UnixMilli()); err == nil {
		t.Fatal("attribution escaped tiny row budget")
	}

	a, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{Entity: EntityVisitorEstimate})
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("retention alias: %+v %+v %v", a, b, err)
	}
}
func TestAuditOldShortRangeAndRollupIntervals(t *testing.T) {
	seed, ctx := o04Connect(t)
	svc := NewStatsService(seed.db)
	from := time.Now().UTC().Truncate(24 * time.Hour).Add(-60 * 24 * time.Hour)
	to := from.Add(24 * time.Hour)
	_, err := seed.db.SQL().Exec(ctx, `INSERT INTO stats_daily (tenant_id,site_id,ts_bucket,pathname,event_type,pageviews,version) VALUES ('default',$1,$2,'/old','pageview',10,1)`, seed.site, from.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	seed.sessionRow(t, ctx, "old-user", from.Add(time.Hour))
	overview, err := svc.Overview(ctx, seed.site, from, to, nil)
	if err != nil || overview.Pageviews != 10 || overview.Visitors != 1 {
		t.Fatalf("old short range: %+v %v", overview, err)
	}
	pages, err := svc.TopPages(ctx, seed.site, from, to, 10, nil)
	if err != nil || len(pages) != 1 || pages[0].VisitorsExact || pages[0].VisitorsNote == "" {
		t.Fatalf("page unique honesty: %+v %v", pages, err)
	}
	for _, interval := range []string{"day", "week", "month"} {
		rows, err := svc.PageviewTimeSeries(ctx, seed.site, from, to, interval, nil)
		if err != nil || len(rows) != 1 || rows[0].Pageviews != 10 || rows[0].Visitors != 1 {
			t.Fatalf("%s: %+v %v", interval, rows, err)
		}
	}
	if _, err := svc.PageviewTimeSeries(ctx, seed.site, from, to, "hour", nil); err == nil {
		t.Fatal("expired hourly grain accepted")
	}
}

// Sessions and events share one allowance; raw expiry cannot remove the seed
// ceiling. Requires actual Nucleus and is not executed under the source hold.
func TestR3RetentionSessionsSeedBudget(t *testing.T) {
	seed, ctx := o04Connect(t)
	from := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	to := from.Add(24 * time.Hour)
	for _, id := range []string{"a", "b", "c"} {
		seed.sessionRow(t, ctx, id, from.Add(time.Hour))
	}
	svc := NewStatsService(seed.db).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	for _, entity := range []string{"", EntityVisitorEstimate} {
		if _, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{Entity: entity}); err == nil {
			t.Fatalf("%q: oversized retained-only seed accepted", entity)
		}
	}
	if _, err := seed.db.SQL().Exec(ctx, `DELETE FROM sessions WHERE site_id=$1 AND session_id=$2`, seed.site, "c"); err != nil {
		t.Fatal(err)
	}
	svc.WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	a, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{Entity: EntityVisitorEstimate})
	if err != nil || !reflect.DeepEqual(a, b) || len(a) != 1 || a[0].CohortSize != 2 {
		t.Fatalf("accepted alias parity: %+v %+v %v", a, b, err)
	}
}

func TestR3RetentionCumulativeEventsAndSessionDetail(t *testing.T) {
	seed, ctx := o04Connect(t)
	from := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	to := from.Add(24 * time.Hour)
	seed.sessionRow(t, ctx, "a", from.Add(time.Hour))
	seed.sessionRow(t, ctx, "b", from.Add(time.Hour))
	for _, id := range []string{"e1", "e2", "e3"} {
		if _, err := seed.db.SQL().Exec(ctx, `INSERT INTO events(event_id,tenant_id,site_id,session_id,visit_id,event_type,pathname,timestamp) VALUES($1,'default',$2,'a','v','pageview','/p',$3)`, id, seed.site, from.Add(2*time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewStatsService(seed.db).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 4})
	if _, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{}); err == nil {
		t.Fatal("2 retained seeds + 3 raw events exceeded cumulative allowance")
	}
	svc.WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 5})
	if _, err := svc.RetentionWithOptions(ctx, seed.site, from, to, 1, RetentionOptions{}); err != nil {
		t.Fatal(err)
	}
	svc.WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	if _, err := svc.SessionDetail(ctx, "a", seed.site); err == nil {
		t.Fatal("detail escaped allowance")
	}
	svc.WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 3})
	rows, err := svc.SessionDetail(ctx, "a", seed.site)
	if err != nil || len(rows) != 3 {
		t.Fatalf("detail %v %v", rows, err)
	}
}

func TestR3JourneyEqualTimestampEngineOrder(t *testing.T) {
	seed, ctx := o04Connect(t)
	from := time.Now().UTC().Add(-time.Hour)
	to := from.Add(time.Hour)
	for _, id := range []string{"b", "a"} {
		if _, err := seed.db.SQL().Exec(ctx, `INSERT INTO events(event_id,tenant_id,site_id,session_id,visit_id,event_type,pathname,timestamp) VALUES($1,'default',$2,'same','v','pageview',$3,$4)`, id, seed.site, "/"+id, from.Add(time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewStatsService(seed.db).Journeys(ctx, seed.site, from, to, 10)
	if err != nil || len(got.TopPaths) != 1 || !reflect.DeepEqual(got.TopPaths[0].Path, []string{"/a", "/b"}) {
		t.Fatalf("selected event-id total order: %+v %v", got, err)
	}
}
