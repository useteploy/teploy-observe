package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func refusalOf(t *testing.T, err error) *queryguard.Refusal {
	t.Helper()
	var ref *queryguard.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a *queryguard.Refusal, got %v", err)
	}
	return ref
}

func guarded(rows int64, timeout time.Duration, l *queryguard.Limiter) *Service {
	s := NewService(nil)
	b := queryguard.DefaultBudgets()
	b.MaxScanRows = rows
	b.Timeout = timeout
	return s.WithQueryGuard(l, b)
}

// QuerySeries must refuse, not truncate, once the engine returns more raw
// points than the row budget.
func TestQuerySeriesRowCapRefuses(t *testing.T) {
	s := guarded(3, time.Second, nil)
	var gotSQL string
	s.queryPointRows = func(_ context.Context, q string, _ ...any) ([]pointRow, error) {
		gotSQL = q
		rows := make([]pointRow, 4) // budget+1 rows: over the ceiling
		return rows, nil
	}
	_, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{})
	ref := refusalOf(t, err)
	if ref.Code != queryguard.CodeBudgetRows || ref.Status != 429 {
		t.Fatalf("refusal = %+v", ref)
	}
	if !strings.Contains(gotSQL, "LIMIT 4") {
		t.Fatalf("SQL must bound the read at budget+1 rows:\n%s", gotSQL)
	}
}

func TestQuerySeriesAtCapStillAnswers(t *testing.T) {
	s := guarded(3, time.Second, nil)
	s.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) {
		return []pointRow{{TsNs: 1_000_000, Value: 1, Kind: "gauge"}, {TsNs: 2_000_000, Value: 2, Kind: "gauge"}, {TsNs: 3_000_000, Value: 3, Kind: "gauge"}}, nil
	}
	series, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{Agg: "sum"})
	if err != nil || len(series) != 1 || len(series[0].Points) == 0 {
		t.Fatalf("at-cap query must answer: %v %+v", err, series)
	}
}

func TestQuerySeriesTimeBudgetIs504(t *testing.T) {
	s := guarded(100, 20*time.Millisecond, nil)
	s.queryPointRows = func(ctx context.Context, _ string, _ ...any) ([]pointRow, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{})
	ref := refusalOf(t, err)
	if ref.Code != queryguard.CodeBudgetTime || ref.Status != 504 {
		t.Fatalf("refusal = %+v", ref)
	}
}

// A cancelled caller is not a time-budget refusal.
func TestQuerySeriesCallerCancelPassesThrough(t *testing.T) {
	s := guarded(100, time.Second, nil)
	s.queryPointRows = func(ctx context.Context, _ string, _ ...any) ([]pointRow, error) {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.QuerySeries(ctx, "site", "m", nil, 0, 1000, QueryOptions{})
	var ref *queryguard.Refusal
	if errors.As(err, &ref) {
		t.Fatalf("caller cancellation must not become a refusal: %v", err)
	}
}

func TestQuerySeriesConcurrencyRefusal(t *testing.T) {
	l := queryguard.NewLimiter(1, 1)
	hold, err := l.Acquire(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	s := guarded(100, time.Second, l)
	called := false
	s.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) { called = true; return nil, nil }
	_, err = s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{})
	ref := refusalOf(t, err)
	if ref.Status != 429 || called {
		t.Fatalf("refusal = %+v called=%v", ref, called)
	}
	if _, err := s.ListMetrics(context.Background(), "site"); refusalOf(t, err).Status != 429 {
		t.Fatalf("ListMetrics must share the admission: %v", err)
	}
	// Releasing the slot readmits; the slot is released after each query.
	hold()
	s.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) { return nil, nil }
	if _, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{}); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if _, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{}); err != nil {
		t.Fatalf("slot leaked after a completed query: %v", err)
	}
}

func TestListMetricsRowCapAndTime(t *testing.T) {
	s := guarded(2, 20*time.Millisecond, nil)
	s.queryMetricRows = func(context.Context, string, ...any) ([]metricRow, error) {
		return make([]metricRow, 3), nil
	}
	if ref := refusalOf(t, errOnly(s.ListMetrics(context.Background(), "site"))); ref.Code != queryguard.CodeBudgetRows {
		t.Fatalf("refusal = %+v", ref)
	}
	s.queryMetricRows = func(ctx context.Context, _ string, _ ...any) ([]metricRow, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if ref := refusalOf(t, errOnly(s.ListMetrics(context.Background(), "site"))); ref.Code != queryguard.CodeBudgetTime {
		t.Fatalf("refusal = %+v", ref)
	}
}

func errOnly[T any](_ T, err error) error { return err }

// The zero-guard service (tests, embedded use) keeps default budgets.
func TestUnguardedServiceKeepsDefaultBudgets(t *testing.T) {
	s := NewService(nil)
	var gotSQL string
	s.queryPointRows = func(_ context.Context, q string, _ ...any) ([]pointRow, error) { gotSQL = q; return nil, nil }
	if _, err := s.QuerySeries(context.Background(), "site", "m", nil, 0, 1000, QueryOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotSQL, "LIMIT 1000001") {
		t.Fatalf("default row budget missing from SQL:\n%s", gotSQL)
	}
}
