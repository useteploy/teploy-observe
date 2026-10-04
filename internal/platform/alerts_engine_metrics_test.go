package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeScalar answers scalarMetric by matching a distinctive fragment of the
// SQL text, and records every query + args it saw.
type fakeScalar struct {
	mu      sync.Mutex
	answers map[string]float64 // SQL constant -> value
	err     error
	seen    []string
	args    [][]any
}

func (f *fakeScalar) hook(_ context.Context, q string, args ...any) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, q)
	f.args = append(f.args, args)
	if f.err != nil {
		return 0, f.err
	}
	return f.answers[q], nil
}

func fakeService(f *fakeScalar) *AlertService {
	return &AlertService{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		scalarHook: f.hook,
		now:        time.Now,
	}
}

func TestQueryMetric_ExtendedMetrics(t *testing.T) {
	cases := []struct {
		name        string
		metric      string
		answers     map[string]float64
		wantValue   float64
		wantSamples int64
		wantQueries int
	}{
		{"trace_error_rate computes percent over spans", "trace_error_rate",
			map[string]float64{sqlSpanCount: 200, sqlSpanErrorCount: 10}, 5, 200, 2},
		{"trace_error_rate no spans is no-data not 0%", "trace_error_rate",
			map[string]float64{sqlSpanCount: 0, sqlSpanErrorCount: 0}, 0, 0, 1},
		{"trace_error_rate spans with zero errors is healthy 0%", "trace_error_rate",
			map[string]float64{sqlSpanCount: 50}, 0, 50, 2},
		{"trace_p95_ms reads p95 of root spans", "trace_p95_ms",
			map[string]float64{sqlRootSpanCount: 40, sqlRootSpanP95: 1234}, 1234, 40, 2},
		{"trace_p95_ms no root spans is no-data not 0ms", "trace_p95_ms",
			map[string]float64{sqlRootSpanCount: 0, sqlRootSpanP95: 999}, 0, 0, 1},
		{"log_error_count counts error+fatal, samples are all logs", "log_error_count",
			map[string]float64{sqlLogErrorCount: 7, sqlLogCount: 900}, 7, 900, 2},
		{"log_error_count silent pipeline is no-data", "log_error_count",
			map[string]float64{}, 0, 0, 2},
		{"uptime_failures counts failed checks", "uptime_failures",
			map[string]float64{sqlUptimeCount: 30, sqlUptimeFailures: 4}, 4, 30, 2},
		{"uptime_failures no results is no-data", "uptime_failures",
			map[string]float64{sqlUptimeCount: 0, sqlUptimeFailures: 3}, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScalar{answers: tc.answers}
			v, n, err := fakeService(f).queryMetric(context.Background(), "site_a", tc.metric, "1000", "2000")
			if err != nil {
				t.Fatal(err)
			}
			if v != tc.wantValue || n != tc.wantSamples {
				t.Fatalf("got value=%v samples=%d, want value=%v samples=%d", v, n, tc.wantValue, tc.wantSamples)
			}
			if len(f.seen) != tc.wantQueries {
				t.Fatalf("ran %d queries, want %d", len(f.seen), tc.wantQueries)
			}
			for i, a := range f.args {
				if len(a) != 3 || a[0] != "site_a" || a[1] != "1000" || a[2] != "2000" {
					t.Fatalf("query %d args = %v, want [site_a 1000 2000]", i, a)
				}
			}
		})
	}
}

// The no-data path must reach the engine's min-samples gate: samples below
// the rule minimum is not a decision. This pins the contract the engine
// relies on (samples < max(min_samples,1) => no_data).
func TestQueryMetric_NoDataSamplesBelowMinimum(t *testing.T) {
	for _, m := range []string{"trace_error_rate", "trace_p95_ms", "log_error_count", "uptime_failures"} {
		f := &fakeScalar{answers: map[string]float64{}}
		_, n, err := fakeService(f).queryMetric(context.Background(), "s", m, "1", "2")
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if n >= 1 {
			t.Fatalf("%s: empty window reported %d samples, want 0 (no-data)", m, n)
		}
	}
	// min_samples: 10 spans with min 20 is still not enough.
	f := &fakeScalar{answers: map[string]float64{sqlSpanCount: 10, sqlSpanErrorCount: 10}}
	v, n, _ := fakeService(f).queryMetric(context.Background(), "s", "trace_error_rate", "1", "2")
	if v != 100 || n != 10 || n >= 20 {
		t.Fatalf("value=%v samples=%d", v, n)
	}
}

func TestQueryMetric_QueryErrorPropagates(t *testing.T) {
	for _, m := range []string{"trace_error_rate", "trace_p95_ms", "log_error_count", "uptime_failures"} {
		f := &fakeScalar{err: errors.New("boom")}
		if _, _, err := fakeService(f).queryMetric(context.Background(), "s", m, "1", "2"); err == nil {
			t.Fatalf("%s: query error swallowed (would read as healthy)", m)
		}
	}
}

func TestQueryMetric_UnknownMetricRejected(t *testing.T) {
	f := &fakeScalar{}
	_, _, err := fakeService(f).queryMetric(context.Background(), "s", "bogus; DROP TABLE spans", "1", "2")
	if err == nil || !strings.Contains(err.Error(), "unknown metric") {
		t.Fatalf("err = %v, want unknown metric", err)
	}
	if len(f.seen) != 0 {
		t.Fatal("unknown metric reached the query layer")
	}
}

func TestAlertMetricWhitelist(t *testing.T) {
	set := AlertMetricSet()
	for _, ok := range []string{"pageviews", "visitors", "error_count", "error_rate",
		"trace_error_rate", "trace_p95_ms", "log_error_count", "uptime_failures"} {
		if _, in := set[ok]; !in {
			t.Errorf("%s missing from whitelist", ok)
		}
	}
	for _, bad := range []string{"", "metric_value", "TRACE_ERROR_RATE", "trace_error_rate ", "1=1", "cpu"} {
		if _, in := set[bad]; in {
			t.Errorf("%q must not be whitelisted", bad)
		}
	}
	// Every whitelisted metric must be computable by the engine (no layer drift).
	for name := range set {
		f := &fakeScalar{answers: map[string]float64{}}
		if _, _, err := fakeService(f).queryMetric(context.Background(), "s", name, "1", "2"); err != nil {
			t.Errorf("whitelisted metric %s not handled by engine: %v", name, err)
		}
	}
}

// SQL text: parameterised, site-scoped, window-bounded, BIGINT-cast; no
// interpolation point for user input.
func TestExtendedMetricSQLText(t *testing.T) {
	sqls := map[string]string{
		"span": sqlSpanCount, "spanErr": sqlSpanErrorCount, "root": sqlRootSpanCount,
		"p95": sqlRootSpanP95, "log": sqlLogCount, "logErr": sqlLogErrorCount,
		"up": sqlUptimeCount, "upFail": sqlUptimeFailures,
	}
	for name, q := range sqls {
		for _, frag := range []string{"site_id = $1", ">= CAST($2 AS BIGINT)", "< CAST($3 AS BIGINT)", "AS value"} {
			if !strings.Contains(q, frag) {
				t.Errorf("%s: missing %q in %s", name, frag, q)
			}
		}
	}
	if !strings.Contains(sqlRootSpanP95, "percentile_cont(duration_ms, 0.95)") || !strings.Contains(sqlRootSpanP95, "parent_span_id = ''") {
		t.Error("p95 must be percentile_cont over root spans")
	}
	if !strings.Contains(sqlSpanErrorCount, "status_code = 'error'") {
		t.Error("span error predicate")
	}
	for _, v := range []string{"error", "ERROR", "Error", "fatal", "FATAL", "Fatal"} {
		if !strings.Contains(sqlLogErrorCount, "'"+v+"'") {
			t.Errorf("log error predicate misses level variant %q", v)
		}
	}
	if !strings.Contains(sqlUptimeFailures, "is_up = 'false'") {
		t.Error("uptime failure predicate")
	}
}

func TestFireIntegrations_HookNonBlockingAndFireOnly(t *testing.T) {
	f := &fakeScalar{}
	s := fakeService(f)
	rule := AlertRule{RuleID: "r1", SiteID: "site_a", Name: "High p95", Metric: "trace_p95_ms", Operator: "gt", Threshold: 500}

	s.fireIntegrations(rule, 900, "m") // no hook: no-op, no panic

	got := make(chan AlertFireEvent, 1)
	release := make(chan struct{})
	s.SetIntegrationsHook(func(_ context.Context, ev AlertFireEvent) {
		<-release // a stuck integration must not block the caller
		got <- ev
	})
	done := make(chan struct{})
	go func() { s.fireIntegrations(rule, 900, "m"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fireIntegrations blocked on a slow hook")
	}
	close(release)
	select {
	case ev := <-got:
		if ev.SiteID != "site_a" || ev.Metric != "trace_p95_ms" || ev.Value != "900.00" || ev.Threshold != "500" || ev.Severity != "warning" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("hook never ran")
	}

	// A panicking hook is contained.
	s.SetIntegrationsHook(func(context.Context, AlertFireEvent) { panic("x") })
	s.fireIntegrations(rule, 1, "m")
	time.Sleep(50 * time.Millisecond)
}
