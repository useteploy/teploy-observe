package query

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAuditCorrelationDisjointOracle(t *testing.T) {
	for _, tc := range []struct {
		s, c, n, total int
		want           bool
	}{{50, 25, 100, 35, true}, {50, 10, 100, 35, true}, {100, 25, 100, 25, false}, {50, 0, 100, 0, false}, {50, 50, 100, 100, false}, {20, 10, 100, 18, true}} {
		if got := correlationSignificant(tc.s, tc.c, tc.n, tc.total); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
func TestAuditExtremeParametersRejectBeforeStorage(t *testing.T) {
	svc := NewStatsService(nil)
	now := time.Now()
	steps := make([]FunnelStep, 33)
	if _, err := svc.FunnelWithOptions(context.Background(), "s", now.Add(-time.Hour), now, steps, FunnelOptions{}); err == nil {
		t.Fatal("too many steps accepted")
	}
	if _, err := svc.FunnelByBreakdownWithOptions(context.Background(), "s", now.Add(-time.Hour), now, steps, "browser", 1, FunnelOptions{}); err == nil {
		t.Fatal("too many breakdown steps accepted")
	}
	for _, n := range []int{-1, 187, 1 << 54} {
		if _, err := svc.RetentionWithOptions(context.Background(), "s", now.Add(-time.Hour), now, n, RetentionOptions{}); err == nil {
			t.Fatalf("period %d accepted", n)
		}
	}
	if got := buildRetentionCohorts(nil, now.Add(-time.Hour), now, 0, ""); len(got) != 0 {
		t.Fatal(got)
	}
}
func TestAuditHistoricalSourceAndSessionFilters(t *testing.T) {
	svc := NewStatsService(nil)
	now := time.Now()
	from := now.Add(-60 * 24 * time.Hour)
	if got := svc.tableForFilters(from, from.Add(time.Hour), nil); got != "stats_daily" {
		t.Fatal(got)
	}
	f := NewFilterBuilder(4)
	f.Add("pathname", "/paid")
	f.AddIn("distinct_id", []string{})
	sql, args := sessionEventFilter("s", 1, 2, f)
	if !strings.Contains(sql, "session_id IN (SELECT session_id FROM events") || !strings.Contains(sql, "1 = 0") || !reflect.DeepEqual(args, []any{"s", int64(1), int64(2), "/paid"}) {
		t.Fatalf("%s %#v", sql, args)
	}
	f = StatsInput{EventType: "purchase", Screen: "1920x1080"}.Filters()
	if !strings.Contains(f.SQL(), "screen_width") || !reflect.DeepEqual(f.Params(), []any{"purchase", "1920x1080"}) {
		t.Fatalf("%s %#v", f.SQL(), f.Params())
	}
}

func TestAuditFunnelStateUsesScalarProgress(t *testing.T) {
	steps := []FunnelStep{{Type: "event", Value: "start"}, {Type: "event", Value: "buy"}}
	w := newFunnelWalker(steps, 0, nil)
	w.push(funnelEvent{EventType: "other"})
	if w.stepIdx != 0 {
		t.Fatal(w.stepIdx)
	}
	w.push(funnelEvent{EventType: "start"})
	w.push(funnelEvent{EventType: "buy"})
	w.push(funnelEvent{EventType: "buy"})
	if w.stepIdx != 2 || !w.done {
		t.Fatalf("progress: %+v", w)
	}
}
