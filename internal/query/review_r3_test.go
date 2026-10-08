package query

import (
	"context"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestR3CumulativeScanAllowance(t *testing.T) {
	s := NewStatsService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2})
	ctx, done, err := s.beginQuery(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err = s.accountScan(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.accountScan(ctx, 1); err == nil {
		t.Fatal("sessions plus events exceeded one allowance")
	}
}
func TestR3RangeAdmission(t *testing.T) {
	s := NewStatsService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxWindow: time.Hour})
	from := time.Unix(0, 0)
	for _, call := range []func() error{func() error { _, e := s.Journeys(context.Background(), "s", from, from.Add(2*time.Hour), 10); return e }, func() error {
		_, e := s.CorrelationAnalysis(context.Background(), "s", "signup", from, from.Add(2*time.Hour))
		return e
	}, func() error {
		_, e := s.TopChannels(context.Background(), "s", from, from.Add(2*time.Hour), 10, nil)
		return e
	}, func() error {
		_, e := s.channelEventIDs(context.Background(), "s", ChannelPaid, from, from.Add(2*time.Hour))
		return e
	}} {
		if call() == nil {
			t.Fatal("range escaped admission")
		}
	}
	if _, e := s.Journeys(context.Background(), "s", from, from.Add(time.Hour), math.MaxInt); e == nil {
		t.Fatal("limit overflow")
	}
}
func TestR3JourneyFramingTiesAndCancellation(t *testing.T) {
	rows := []journeyEvent{{SessionID: "1", Pathname: "a->b"}, {SessionID: "1", Pathname: "c"}, {SessionID: "2", Pathname: "a"}, {SessionID: "2", Pathname: "b->c"}, {SessionID: "3", Pathname: "a > b"}, {SessionID: "3", Pathname: "c"}}
	got, err := computeJourneys(context.Background(), rows, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Transitions) != 3 || got.TotalPaths != 3 {
		t.Fatalf("ambiguous encoding: %+v", got)
	}
	for i := 0; i < 10; i++ {
		again, e := computeJourneys(context.Background(), rows, 1)
		if e != nil || !reflect.DeepEqual(again.TopPaths, got.TopPaths[:1]) {
			t.Fatal("tied top K unstable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = computeJourneys(ctx, rows, 10); err == nil {
		t.Fatal("post-read cancellation ignored")
	}
}
func TestR3FixedGoalRevenueDomain(t *testing.T) {
	for _, v := range []struct {
		unit, n, want int64
		bad           bool
	}{{0, math.MaxInt64, 0, false}, {math.MaxInt64, 1, math.MaxInt64, false}, {math.MaxInt64, 2, 0, true}, {1, -1, 0, true}, {2, math.MaxInt64 / 2, math.MaxInt64 - 1, false}, {2, math.MaxInt64/2 + 1, 0, true}} {
		got, err := checkedGoalTotal(v.unit, v.n)
		if (err != nil) != v.bad || (!v.bad && got != v.want) {
			t.Fatalf("%+v %d %v", v, got, err)
		}
	}
}

func TestR3AgeRoutingFixedClock(t *testing.T) {
	s := NewStatsService(nil)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	from := now.Add(-60 * 24 * time.Hour)
	if got := s.tableForFilters(from, from.Add(time.Hour), nil); got != "stats_daily" {
		t.Fatal(got)
	}
	f := NewFilterBuilder(4)
	f.Add("event_type", "purchase")
	if got := s.tableForFilters(from, from.Add(time.Hour), f); got != "stats_daily" {
		t.Fatal("lost supported rollup predicate", got)
	}
	f.AddIn("distinct_id", []string{"member"})
	if got := s.tableForFilters(from, from.Add(time.Hour), f); got != "events" {
		t.Fatal("unrepresentable cohort predicate routed to rollup", got)
	}
	if _, err := s.PageviewTimeSeries(context.Background(), "s", from, from.Add(time.Hour), "hour", nil); err == nil {
		t.Fatal("daily data invented hourly grain")
	}
	if got := s.sourceFor(from, nil); got != SourceSessions {
		t.Fatal(got)
	}
	if got := s.sourceFor(from, f); got != SourceEvents {
		t.Fatal("raw-only filter lost", got)
	}
}

func TestR3FunnelWindowOverflowAndDimensions(t *testing.T) {
	steps := []FunnelStep{{Type: "event", Value: "start"}, {Type: "event", Value: "buy"}}
	w := newFunnelWalker(steps, 10, nil)
	w.push(funnelEvent{EventType: "start", Timestamp: math.MaxInt64 - 5})
	w.push(funnelEvent{EventType: "buy", Timestamp: math.MaxInt64})
	if !w.done || w.stepIdx != 2 {
		t.Fatal("representable gap lost to addition overflow")
	}
	s := NewStatsService(nil)
	from := time.Unix(0, 0)
	to := from.Add(time.Hour)
	for _, steps := range [][]FunnelStep{make([]FunnelStep, 33)} {
		if _, err := s.FunnelWithOptions(context.Background(), "s", from, to, steps, FunnelOptions{}); err == nil {
			t.Fatal("funnel dimensions accepted")
		}
		if _, err := s.FunnelByBreakdownWithOptions(context.Background(), "s", from, to, steps, "browser", 10, FunnelOptions{}); err == nil {
			t.Fatal("breakdown dimensions accepted")
		}
	}
}
