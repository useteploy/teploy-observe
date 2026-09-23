package metrics

// O07 executable binding: the reference-calculation literals from
// o07_reference_test.go asserted END TO END through the production
// Ingest (wire decode → metricPointRow → insertMetricRows) and
// QuerySeries (scan with COALESCE'd temporality, attributes round-trip
// through JSON, full-fingerprint per-series split, merge, labels).
// Nucleus-gated: self-skips without nucleustest.DSN.

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// o07SeedIngest posts one OTLP/JSON export through the production Ingest.
func o07SeedIngest(t *testing.T, svc *Service, site string, req ExportMetricsRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := svc.Ingest(ctx, site, req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !resp.OK || resp.Points == 0 {
		t.Fatalf("ingest resp = %+v", resp)
	}
}

func o07Query(t *testing.T, svc *Service, site, name string, labels map[string]string, fromMs, toMs int64, agg Aggregation, stepMs int64, groupBy []string) []Series {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	series, err := svc.QuerySeries(ctx, site, name, labels, fromMs, toMs, QueryOptions{
		Agg: string(agg), StepMs: stepMs, GroupBy: groupBy,
	})
	if err != nil {
		t.Fatalf("QuerySeries(%s/%s): %v", name, agg, err)
	}
	return series
}

// TestO07_RateCounterResetPerSeriesEndToEnd — cumulative counter with a
// reset on series a, two instances, verified collapsed AND fanned out.
//
// Series a: 0 → 100 (+100/10s), RESET to 40, → 60. Increases 100 + 40
// (restart-at-zero) + 20 = 160 over span [0s,30s] → 5.333333333333333/s,
// estimate (reset assumption invoked).
// Series b (offset timestamps — real instances do not share nanoseconds):
// 0 → 50 → 90: increases 50 + 40 = 90 over [5s,35s] → 3/s exact.
// Collapsed total (no group_by): 25/3 = 8.333333333333333/s, estimate
// (series a's reset lives in the shared bucket).
func TestO07_RateCounterResetPerSeriesEndToEnd(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	site := fmt.Sprintf("o07_rate_%d", time.Now().UnixNano())
	svc := NewService(db)

	const base int64 = 1_700_000_000_000_000_000 // ns
	ts := func(sec int) string { return fmt.Sprintf("%d", base+int64(sec)*1_000_000_000) }
	dp := func(sec int, v float64, instance string) NumberDataPoint {
		return NumberDataPoint{
			TimeUnixNano: ts(sec), AsDouble: v,
			Attributes: []KeyValue{{Key: "instance", Value: AnyValue{StringValue: instance}}},
		}
	}
	o07SeedIngest(t, svc, site, ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: []KeyValue{{Key: "service.name", Value: AnyValue{StringValue: "o07-svc"}}}},
		ScopeMetrics: []ScopeMetrics{{Scope: InstrumentationScope{Name: "o07"}, Metrics: []OTLPMetric{{
			Name: "o07.rt.counter",
			Sum: &Sum{
				IsMonotonic: true, AggregationTemporality: 2,
				DataPoints: []NumberDataPoint{
					dp(0, 0, "a"), dp(10, 100, "a"), dp(20, 40, "a"), dp(30, 60, "a"),
					dp(5, 0, "b"), dp(15, 50, "b"), dp(35, 90, "b"),
				},
			},
		}}},
		}}}})

	fromMs, toMs := base/1_000_000-1_000, base/1_000_000+120_000

	// Collapsed: total rate with the reset assumption labeled.
	series := o07Query(t, svc, site, "o07.rt.counter", nil, fromMs, toMs, AggRate, 60_000, nil)
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("collapsed: got %+v", series)
	}
	p := series[0].Points[0]
	if math.Abs(p.Value-25.0/3.0) > 1e-9 {
		t.Errorf("collapsed rate = %v, want 8.333333333333333", p.Value)
	}
	if p.Estimate == nil || !*p.Estimate || p.Method != "rate/per-series+reset-assumed" {
		t.Errorf("collapsed labels = %+v %q, want estimate=true reset-assumed", p.Estimate, p.Method)
	}

	// Fanned out by instance: per-series reference values.
	series = o07Query(t, svc, site, "o07.rt.counter", nil, fromMs, toMs, AggRate, 60_000, []string{"instance"})
	if len(series) != 2 {
		t.Fatalf("fan-out: got %d series, want 2", len(series))
	}
	byInst := map[string]Series{}
	for _, s := range series {
		byInst[s.Labels["instance"]] = s
	}
	a, b := byInst["a"], byInst["b"]
	if len(a.Points) != 1 || math.Abs(a.Points[0].Value-160.0/30.0) > 1e-9 {
		t.Errorf("series a rate = %+v, want 5.333333333333333", a.Points)
	}
	if a.Points[0].Estimate == nil || !*a.Points[0].Estimate {
		t.Errorf("series a must be estimate (reset), got %+v", a.Points[0].Estimate)
	}
	if len(b.Points) != 1 || math.Abs(b.Points[0].Value-3) > 1e-9 {
		t.Errorf("series b rate = %+v, want 3", b.Points)
	}
	if b.Points[0].Estimate == nil || *b.Points[0].Estimate || b.Points[0].Method != "rate/per-series" {
		t.Errorf("series b must be exact rate/per-series, got %+v %q", b.Points[0].Estimate, b.Points[0].Method)
	}
}

// TestO07_DeltaRateEndToEnd — delta increments are direct and additive:
// 3 + 4 in one 60s bucket = 7/60 = 0.11666666666666667/s.
func TestO07_DeltaRateEndToEnd(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	site := fmt.Sprintf("o07_delta_%d", time.Now().UnixNano())
	svc := NewService(db)

	const base int64 = 1_700_000_000_000_000_000
	ts := func(sec int) string { return fmt.Sprintf("%d", base+int64(sec)*1_000_000_000) }
	dp := func(sec int, v float64, instance string) NumberDataPoint {
		return NumberDataPoint{
			TimeUnixNano: ts(sec), AsDouble: v,
			Attributes: []KeyValue{{Key: "instance", Value: AnyValue{StringValue: instance}}},
		}
	}
	o07SeedIngest(t, svc, site, ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: []KeyValue{{Key: "service.name", Value: AnyValue{StringValue: "o07-svc"}}}},
		ScopeMetrics: []ScopeMetrics{{Scope: InstrumentationScope{Name: "o07"}, Metrics: []OTLPMetric{{
			Name: "o07.rt.delta",
			Sum: &Sum{
				IsMonotonic: true, AggregationTemporality: 1,
				DataPoints: []NumberDataPoint{dp(10, 3, "a"), dp(10, 4, "b")},
			},
		}}},
		}}}})

	fromMs, toMs := base/1_000_000-1_000, base/1_000_000+120_000
	series := o07Query(t, svc, site, "o07.rt.delta", nil, fromMs, toMs, AggRate, 60_000, nil)
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("delta: got %+v", series)
	}
	p := series[0].Points[0]
	if math.Abs(p.Value-7.0/60.0) > 1e-12 {
		t.Errorf("delta rate = %v, want 0.11666666666666667", p.Value)
	}
	if p.Estimate == nil || *p.Estimate || p.Method != "rate/delta-window-sum" {
		t.Errorf("delta labels = %+v %q", p.Estimate, p.Method)
	}
}

// TestO07_CumulativeHistogramQuantileEndToEnd — cumulative snapshots are
// differenced per series then merged: a's window [1,0,0] + b's [0,1,0]
// = [1,1,0] total 2; p50 rank 1 lands exactly on the boundary after
// bucket 0 → 10. Old snapshot-summing behavior answered 27.142857...
func TestO07_CumulativeHistogramQuantileEndToEnd(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	site := fmt.Sprintf("o07_hist_%d", time.Now().UnixNano())
	svc := NewService(db)

	const base int64 = 1_700_000_000_000_000_000
	ts := func(sec int) string { return fmt.Sprintf("%d", base+int64(sec)*1_000_000_000) }
	hdp := func(sec int, counts []string, instance string) HistogramDataPoint {
		bc := make([]jsonInt, len(counts))
		for i, c := range counts {
			bc[i] = jsonInt(c)
		}
		return HistogramDataPoint{
			TimeUnixNano: ts(sec), BucketCounts: bc, ExplicitBounds: []float64{10, 50, 100},
			Attributes: []KeyValue{{Key: "instance", Value: AnyValue{StringValue: instance}}},
		}
	}
	o07SeedIngest(t, svc, site, ExportMetricsRequest{ResourceMetrics: []ResourceMetrics{{
		Resource: Resource{Attributes: []KeyValue{{Key: "service.name", Value: AnyValue{StringValue: "o07-svc"}}}},
		ScopeMetrics: []ScopeMetrics{{Scope: InstrumentationScope{Name: "o07"}, Metrics: []OTLPMetric{{
			Name: "o07.rt.latency",
			Histogram: &Histogram{
				AggregationTemporality: 2,
				DataPoints: []HistogramDataPoint{
					hdp(0, []string{"2", "2", "2"}, "a"), hdp(30, []string{"3", "2", "2"}, "a"),
					hdp(0, []string{"1", "1", "1"}, "b"), hdp(30, []string{"1", "2", "1"}, "b"),
				},
			},
		}}},
		}}}})

	fromMs, toMs := base/1_000_000-1_000, base/1_000_000+120_000
	series := o07Query(t, svc, site, "o07.rt.latency", nil, fromMs, toMs, AggP50, 60_000, nil)
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("histogram p50: got %+v", series)
	}
	p := series[0].Points[0]
	if math.Abs(p.Value-10) > 1e-9 {
		t.Errorf("p50 = %v, want 10 (boundary-exact)", p.Value)
	}
	if p.Estimate == nil || !*p.Estimate || p.Method != "histogram-quantile/linear-interpolation" {
		t.Errorf("p50 labels = %+v %q, want estimate=true", p.Estimate, p.Method)
	}

	// Single-series view: series a's window is [1,0,0] total 1 → p95
	// rank 0.95 is interior to bucket 0 [0..10] → 0 + 10*0.95 = 9.5.
	series = o07Query(t, svc, site, "o07.rt.latency", map[string]string{"instance": "a"}, fromMs, toMs, AggP95, 60_000, nil)
	if len(series) != 1 || len(series[0].Points) != 1 {
		t.Fatalf("histogram p95 a: got %+v", series)
	}
	if math.Abs(series[0].Points[0].Value-9.5) > 1e-9 {
		t.Errorf("series a p95 = %v, want 9.5 (interior interpolation)", series[0].Points[0].Value)
	}
}
