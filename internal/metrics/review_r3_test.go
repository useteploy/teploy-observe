package metrics

import (
	"context"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"testing"
	"time"
)

func TestR3MetricBoundsAndDirectAdmission(t *testing.T) {
	s := NewService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxWindow: time.Hour})
	called := false
	s.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) { called = true; return nil, nil }
	for _, b := range [][2]int64{{9223372036855, 9223372036856}, {-9223372036856, 1}, {2, 1}, {1, 1}, {0, 7200000}} {
		if _, e := s.QuerySeries(context.Background(), "s", "m", nil, b[0], b[1], QueryOptions{}); e == nil {
			t.Fatalf("accepted %v", b)
		}
	}
	if called {
		t.Fatal("invalid predicate reached store")
	}
	if f, to, e := ValidateTimeBounds(0, 1); e != nil || f != 0 || to != 1000000 {
		t.Fatal("explicit zero altered")
	}
	for _, step := range []string{"1ns", "0.5ms", "1.5ms", "0s"} {
		if _, e := ParseStep(step); e == nil {
			t.Fatalf("lossy step %s", step)
		}
	}
	if _, e := s.QuerySeries(context.Background(), "s", "m", nil, 0, 1, QueryOptions{StepMs: -1}); e == nil {
		t.Fatal("negative direct step defaulted")
	}
}
func TestR3SignedHistogramRefused(t *testing.T) {
	if validHistogramShape(HistogramShape{Bounds: []float64{-10, 0}, Counts: []int64{10, 0, 0}, Count: 10}) {
		t.Fatal("unsupported signed first tail accepted")
	}
	for _, h := range []HistogramShape{{Bounds: []float64{0, 10}, Counts: []int64{0, 10, 0}, Count: 10}, {Bounds: []float64{10}, Counts: []int64{10, 0}, Count: 10}, {Bounds: []float64{10}, Counts: []int64{0, 10}, Count: 10}} {
		if !validHistogramShape(h) {
			t.Fatal("supported nonnegative histogram rejected")
		}
	}
}

// The observations used as oracles are independent of the reducer: first-tail
// samples -100,-20,5 have median -20 <= 10; overflow samples 11,40,100 have
// median 40 >= 10. Neither open tail identifies a numeric median.
func TestR3HistogramTailAvailabilityAndBounds(t *testing.T) {
	for _, tc := range []struct {
		bounds []float64
		counts []jsonInt
		value  float64
		suffix string
	}{
		{[]float64{10}, []jsonInt{"3", "0"}, 10, "+underflow-upper-bound"},
		{[]float64{0}, []jsonInt{"3", "0"}, 0, "+underflow-upper-bound"},
		{[]float64{10}, []jsonInt{"0", "3"}, 10, "+overflow-lower-bound"},
		{[]float64{0, 10}, []jsonInt{"0", "3", "0"}, 5, ""},
	} {
		points := quantileGroupReduce(splitSeries([]pointRow{histRow(10, tc.counts, tc.bounds, "delta", "")}), 0.5, 60000)
		if len(points) != 1 || points[0].Value != tc.value || points[0].Method != "histogram-quantile/linear-interpolation+nonnegative-bounds"+tc.suffix {
			t.Fatalf("tail contract: %+v", points)
		}
	}
	svc := NewService(nil)
	written := 0
	svc.writePointRows = func(_ context.Context, _ string, rows []metricPointRow) (int, error) {
		written += len(rows)
		return len(rows), nil
	}
	dp := HistogramDataPoint{Count: "3", BucketCounts: []jsonInt{"3", "0", "0"}, ExplicitBounds: []float64{-10, 0}, Sum: -60}
	metric := OTLPMetric{Name: "signed", Histogram: &Histogram{DataPoints: []HistogramDataPoint{dp}}}
	scope := ScopeMetrics{Metrics: []OTLPMetric{metric}}
	req := ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{ScopeMetrics: []ScopeMetrics{scope}}}}
	result, err := svc.Ingest(context.Background(), "s", req)
	if err != nil || result.Points != 0 || result.Rejected != 1 || written != 0 {
		t.Fatalf("rejection/write disagreement: %+v %v written=%d", result, err, written)
	}
	svc.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) {
		return []pointRow{{Kind: "histogram", Histogram: `{"bounds":[-10,0],"counts":[3,0,0],"count":3,"sum":-60}`}}, nil
	}
	if _, err := svc.QuerySeries(context.Background(), "s", "signed", nil, 0, 1000, QueryOptions{Agg: string(AggP50)}); err == nil {
		t.Fatal("legacy signed quantile presented as available")
	}
}

func TestR3MixedHistogramBoundsUnavailable(t *testing.T) {
	svc := NewService(nil)
	svc.queryPointRows = func(context.Context, string, ...any) ([]pointRow, error) {
		return []pointRow{histRow(0, []jsonInt{"1", "0"}, []float64{10}, "delta", ""), histRow(0, []jsonInt{"1", "0"}, []float64{20}, "delta", "")}, nil
	}
	if _, err := svc.QuerySeries(context.Background(), "s", "mixed", nil, 0, 1000, QueryOptions{Agg: string(AggP50)}); err == nil {
		t.Fatal("partial distribution presented as complete")
	}
}

func TestR3EmptyHistogramHasNoMedian(t *testing.T) {
	if points := quantileGroupReduce(splitSeries([]pointRow{histRow(0, []jsonInt{"0", "0"}, []float64{10}, "delta", "")}), 0.5, 1000); len(points) != 0 {
		t.Fatal("empty observations fabricated median", points)
	}
}
