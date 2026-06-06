package metrics

import (
	"math"
	"testing"
)

func TestAttrsToMap_AllScalarKinds(t *testing.T) {
	in := []KeyValue{
		{Key: "k_str", Value: AnyValue{StringValue: "hello"}},
		{Key: "k_int", Value: AnyValue{IntValue: "42"}},
		{Key: "k_bool", Value: AnyValue{BoolValue: true}},
		{Key: "k_dbl", Value: AnyValue{DoubleValue: 3.14}},
	}
	got := AttrsToMap(in)
	if got["k_str"] != "hello" || got["k_int"] != "42" || got["k_bool"] != "true" {
		t.Errorf("attrs map = %v", got)
	}
	if got["k_dbl"] == "" {
		t.Errorf("expected double encoded, got empty")
	}
}

func TestMarshalAttrs_DeterministicOrdering(t *testing.T) {
	a := MarshalAttrs(map[string]string{"b": "2", "a": "1", "c": "3"})
	b := MarshalAttrs(map[string]string{"c": "3", "a": "1", "b": "2"})
	if a != b {
		t.Errorf("expected stable encoding, got %q vs %q", a, b)
	}
}

func TestMatchLabels(t *testing.T) {
	have := map[string]string{"region": "us-east-1", "tier": "prod"}
	if !MatchLabels(have, map[string]string{"region": "us-east-1"}) {
		t.Error("subset match should succeed")
	}
	if MatchLabels(have, map[string]string{"region": "eu-west-1"}) {
		t.Error("mismatched value should fail")
	}
	if !MatchLabels(have, map[string]string{}) {
		t.Error("empty filter should match anything")
	}
}

func TestMarshalUnmarshalHistogram(t *testing.T) {
	dp := HistogramDataPoint{
		Count:          "10",
		Sum:            123.4,
		BucketCounts:   []string{"1", "2", "3", "4"},
		ExplicitBounds: []float64{1, 5, 10},
	}
	raw := MarshalHistogram(dp)
	if raw == "" {
		t.Fatal("marshal returned empty")
	}
	got := UnmarshalHistogram(raw)
	if got.Count != 10 || math.Abs(got.Sum-123.4) > 1e-9 {
		t.Errorf("got %+v", got)
	}
	if len(got.Counts) != 4 || got.Counts[2] != 3 {
		t.Errorf("counts = %v", got.Counts)
	}
	if len(got.Bounds) != 3 || got.Bounds[1] != 5 {
		t.Errorf("bounds = %v", got.Bounds)
	}
}

func TestUnmarshalHistogram_Empty(t *testing.T) {
	got := UnmarshalHistogram("")
	if got.Bounds == nil || got.Counts == nil {
		t.Errorf("empty histogram should still yield non-nil slices: %+v", got)
	}
}

func TestReduce_AllAggregations(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50}
	cases := []struct {
		agg  Aggregation
		want float64
	}{
		{AggLast, 50}, // last passed in separately = 50
		{AggSum, 150},
		{AggAvg, 30},
		{AggMin, 10},
		{AggMax, 50},
	}
	for _, tc := range cases {
		t.Run(string(tc.agg), func(t *testing.T) {
			got := reduce(values, 50, tc.agg)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("reduce(%s) = %v, want %v", tc.agg, got, tc.want)
			}
		})
	}
}

func TestReduce_EmptyReturnsZero(t *testing.T) {
	if got := reduce(nil, 0, AggAvg); got != 0 {
		t.Errorf("empty avg = %v, want 0", got)
	}
}

func TestIsValidAggregation(t *testing.T) {
	for _, ok := range []string{"last", "avg", "sum", "min", "max", "rate", "p50", "p95", "p99"} {
		if !IsValidAggregation(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "delta", "histogram_quantile", "p100"} {
		if IsValidAggregation(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}

func TestParseLabelFilters(t *testing.T) {
	q := map[string][]string{
		"site_id":      {"default"},
		"label.region": {"us-east-1"},
		"label.tier":   {"prod"},
		"name":         {"http_requests"},
	}
	got := ParseLabelFilters(q)
	if len(got) != 2 || got["region"] != "us-east-1" || got["tier"] != "prod" {
		t.Errorf("filters = %v", got)
	}
}

func TestExtractServiceName(t *testing.T) {
	attrs := []KeyValue{
		{Key: "service.name", Value: AnyValue{StringValue: "api"}},
		{Key: "host", Value: AnyValue{StringValue: "x"}},
	}
	if ExtractServiceName(attrs) != "api" {
		t.Errorf("expected api")
	}
	if ExtractServiceName(nil) != "" {
		t.Errorf("expected empty on nil")
	}
}

func TestAggregationTemporality(t *testing.T) {
	if AggregationTemporality(1) != "delta" {
		t.Error("1 -> delta")
	}
	if AggregationTemporality(2) != "cumulative" {
		t.Error("2 -> cumulative")
	}
	if AggregationTemporality(0) != "cumulative" {
		t.Error("0 -> cumulative (default)")
	}
}

// ─── Phase 2 ─────────────────────────────────────────────────────────────

func TestParseStep(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 60_000},
		{"15s", 15_000},
		{"30s", 30_000},
		{"60s", 60_000},
		{"1m", 60_000},
		{"5m", 300_000},
		{"1h", 3_600_000},
		{"1d", 86_400_000},
		{"10m", 600_000}, // generic Go duration fallback
	}
	for _, c := range cases {
		got, err := ParseStep(c.in)
		if err != nil {
			t.Errorf("ParseStep(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseStep(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"abc", "0", "-5s", "48h"} {
		if _, err := ParseStep(bad); err == nil {
			t.Errorf("ParseStep(%q) expected error", bad)
		}
	}
}

func TestParseGroupBy(t *testing.T) {
	if got := ParseGroupBy(""); len(got) != 0 {
		t.Errorf("empty = %v, want nil", got)
	}
	got := ParseGroupBy("region, instance ,, service")
	want := []string{"region", "instance", "service"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// rateReduce: counter resets and per-second slope.
func TestRateReduce_BasicAndReset(t *testing.T) {
	// Synthetic cumulative counter: 0 → 10 → 30 (rate=10/s, 20/s) then RESET
	// to 5 → 15 (rate=10/s).
	rows := []pointRow{
		{TsNs: 0, Value: 0, Kind: "sum"},
		{TsNs: 1_000_000_000, Value: 10, Kind: "sum"},  // +10 over 1s
		{TsNs: 2_000_000_000, Value: 30, Kind: "sum"},  // +20 over 1s
		{TsNs: 3_000_000_000, Value: 5, Kind: "sum"},   // RESET, drop slope
		{TsNs: 4_000_000_000, Value: 15, Kind: "sum"},  // +10 over 1s
	}
	// Use 10s step so all valid pairs land in the same bucket → average.
	pts := rateReduce(rows, 10_000)
	if len(pts) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(pts))
	}
	// Slopes 10, 20, (skipped), 10 → mean 13.333…
	if math.Abs(pts[0].Value-(10+20+10)/3.0) > 1e-9 {
		t.Errorf("rate = %v, want 13.333", pts[0].Value)
	}
}

func TestRateReduce_PerSecondAcrossBuckets(t *testing.T) {
	// One slope per 1s bucket so we exercise step boundaries too.
	rows := []pointRow{
		{TsNs: 0, Value: 0, Kind: "sum"},
		{TsNs: 1_000_000_000, Value: 5, Kind: "sum"},
		{TsNs: 2_000_000_000, Value: 15, Kind: "sum"},
	}
	pts := rateReduce(rows, 1_000) // 1s buckets
	if len(pts) != 2 {
		t.Fatalf("expected 2 buckets, got %d (%+v)", len(pts), pts)
	}
	if math.Abs(pts[0].Value-5) > 1e-9 || math.Abs(pts[1].Value-10) > 1e-9 {
		t.Errorf("rates = %v, want [5,10]", []float64{pts[0].Value, pts[1].Value})
	}
}

func TestRateReduce_TooShortReturnsEmpty(t *testing.T) {
	if got := rateReduce([]pointRow{{TsNs: 0, Value: 1}}, 60_000); len(got) != 0 {
		t.Errorf("single point should yield no rate, got %v", got)
	}
	if got := rateReduce(nil, 60_000); len(got) != 0 {
		t.Errorf("empty rows should yield no rate, got %v", got)
	}
}

// histogramQuantile: linear interpolation across the crossing bucket.
func TestHistogramQuantile_Interpolation(t *testing.T) {
	// 4 buckets: <=10, <=50, <=100, +Inf with counts 5,5,5,5 (total=20).
	bounds := []float64{10, 50, 100}
	counts := []float64{5, 5, 5, 5}
	total := 20.0

	// p50 → target=10, falls in bucket [0..10] (cumulative crosses at 10).
	got := histogramQuantile(bounds, counts, total, 0.50)
	// First bucket: cum=0, c=5, target=10 → next=5; not crossed.
	// Second bucket: cum=5, c=5, next=10 — crosses at the upper bound.
	// frac = (10-5)/5 = 1.0 → returns lower + (upper-lower)*1 = upper = 50.
	if math.Abs(got-50) > 1e-9 {
		t.Errorf("p50 = %v, want 50", got)
	}
	// p25 → target=5; first bucket crosses at exactly 5.
	if got := histogramQuantile(bounds, counts, total, 0.25); math.Abs(got-10) > 1e-9 {
		t.Errorf("p25 = %v, want 10", got)
	}
	// p70 → target=14; cum after bucket 1 = 10, bucket 2 = 15 ≥ 14.
	// frac=(14-10)/5=0.8 → 50 + (100-50)*0.8 = 90.
	if got := histogramQuantile(bounds, counts, total, 0.70); math.Abs(got-90) > 1e-9 {
		t.Errorf("p70 = %v, want 90", got)
	}
	// p95 → target=19; falls in +Inf overflow → saturates to last
	// bound (100). Matches Prometheus's histogram_quantile().
	if got := histogramQuantile(bounds, counts, total, 0.95); math.Abs(got-100) > 1e-9 {
		t.Errorf("p95 = %v, want 100 (saturated)", got)
	}
}

func TestHistogramQuantile_Edges(t *testing.T) {
	if got := histogramQuantile(nil, nil, 0, 0.5); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	// All weight in +Inf overflow → saturate to last bound.
	if got := histogramQuantile([]float64{10, 50}, []float64{0, 0, 5}, 5, 0.99); math.Abs(got-50) > 1e-9 {
		t.Errorf("overflow saturate = %v, want 50", got)
	}
	// q clamp.
	if got := histogramQuantile([]float64{10}, []float64{1, 0}, 1, -1); got != 0 {
		// target=0; first bucket: cum=0, c=1, next=1 ≥ 0 → frac=0 → returns lower (0).
		// We assert q is clamped, not the exact value.
		if math.Abs(got) > 1e-9 {
			t.Errorf("q<0 should clamp to 0; got %v", got)
		}
	}
}

func TestQuantileReduce_AggregatesPerBucket(t *testing.T) {
	// Two histogram observations in the same 60s bucket should sum their
	// counts before solving the quantile.
	histA := MarshalHistogram(HistogramDataPoint{
		Count: "10", BucketCounts: []string{"5", "5", "0"}, ExplicitBounds: []float64{10, 50},
	})
	histB := MarshalHistogram(HistogramDataPoint{
		Count: "10", BucketCounts: []string{"0", "0", "10"}, ExplicitBounds: []float64{10, 50},
	})
	rows := []pointRow{
		{TsNs: 1_000_000_000, Histogram: histA, Kind: "histogram"},
		{TsNs: 2_000_000_000, Histogram: histB, Kind: "histogram"},
	}
	pts := quantileReduce(rows, 0.99, 60_000)
	if len(pts) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(pts))
	}
	// Combined counts: 5,5,10 (total=20). p99 target=19.8; cum after 2nd
	// bucket = 10, +10 in overflow → saturates to last bound 50.
	if math.Abs(pts[0].Value-50) > 1e-9 {
		t.Errorf("p99 = %v, want 50 (saturated)", pts[0].Value)
	}
}

// scalarReduce step boundary rounding.
func TestScalarReduce_StepBoundaryRounding(t *testing.T) {
	rows := []pointRow{
		{TsNs: 1_000 * 1_000_000, Value: 1, Kind: "gauge"},   // bucket 0
		{TsNs: 14_999 * 1_000_000, Value: 2, Kind: "gauge"},  // bucket 0
		{TsNs: 15_000 * 1_000_000, Value: 3, Kind: "gauge"},  // bucket 15s
		{TsNs: 29_999 * 1_000_000, Value: 4, Kind: "gauge"},  // bucket 15s
	}
	pts := scalarReduce(rows, AggSum, 15_000)
	if len(pts) != 2 {
		t.Fatalf("expected 2 buckets, got %d (%+v)", len(pts), pts)
	}
	if pts[0].TsMs != 0 || pts[0].Value != 3 {
		t.Errorf("bucket[0] = (%d, %v), want (0, 3)", pts[0].TsMs, pts[0].Value)
	}
	if pts[1].TsMs != 15_000 || pts[1].Value != 7 {
		t.Errorf("bucket[1] = (%d, %v), want (15000, 7)", pts[1].TsMs, pts[1].Value)
	}
}

// groupKey + per-label-set fan-out.
func TestGroupKey_StableAcrossOrder(t *testing.T) {
	a := map[string]string{"region": "us-east-1", "instance": "1"}
	b := map[string]string{"instance": "1", "region": "us-east-1"}
	groupBy := []string{"region", "instance"}
	ka, _ := groupKey(a, groupBy)
	kb, _ := groupKey(b, groupBy)
	if ka != kb {
		t.Errorf("keys should match regardless of map order: %q vs %q", ka, kb)
	}
}

func TestAggregateSeries_RoutesByAgg(t *testing.T) {
	// rate path returns empty for too-short input.
	if got := aggregateSeries([]pointRow{{TsNs: 0, Value: 1, Kind: "sum"}}, AggRate, 60_000); len(got) != 0 {
		t.Errorf("rate path: got %v, want empty", got)
	}
	// p95 path returns empty when no histograms present.
	if got := aggregateSeries([]pointRow{{TsNs: 0, Value: 1, Kind: "gauge"}}, AggP95, 60_000); len(got) != 0 {
		t.Errorf("p95 path: got %v, want empty", got)
	}
	// scalar path runs the bucketed reducer.
	rows := []pointRow{{TsNs: 0, Value: 1, Kind: "gauge"}, {TsNs: 1_000_000, Value: 3, Kind: "gauge"}}
	got := aggregateSeries(rows, AggSum, 60_000)
	if len(got) != 1 || got[0].Value != 4 {
		t.Errorf("scalar sum: got %+v, want one bucket value=4", got)
	}
}

// TestRate_DeltaTemporality verifies delta counters are bucket-summed and
// divided by the bucket length, not consecutive-differenced. Two deltas of 3
// and 9 in a 60s bucket → (3+9)/60 = 0.2/s.
func TestRate_DeltaTemporality(t *testing.T) {
	rows := []pointRow{
		{TsNs: 10_000_000_000, Value: 3, Kind: "sum", Temporality: "delta"},
		{TsNs: 20_000_000_000, Value: 9, Kind: "sum", Temporality: "delta"},
	}
	got := aggregateSeries(rows, AggRate, 60_000)
	if len(got) != 1 {
		t.Fatalf("delta rate: want 1 bucket, got %d (%+v)", len(got), got)
	}
	if want := 12.0 / 60.0; got[0].Value != want {
		t.Fatalf("delta rate: got %v, want %v", got[0].Value, want)
	}
}

// Per-label-set fan-out emulated via two separate row buckets keyed by
// label fingerprint. Smoke-tests the groupKey + Series wiring without
// needing a live DB.
func TestQuerySeries_FanOutByLabels(t *testing.T) {
	// 6 rows: 3 distinct (region) values × 2 timestamps each.
	type fakeRow struct {
		ts   int64
		v    float64
		attr map[string]string
	}
	in := []fakeRow{
		{1_000_000, 1, map[string]string{"region": "us-east-1"}},
		{2_000_000, 2, map[string]string{"region": "us-east-1"}},
		{1_000_000, 10, map[string]string{"region": "eu-west-1"}},
		{2_000_000, 20, map[string]string{"region": "eu-west-1"}},
		{1_000_000, 100, map[string]string{"region": "ap-south-1"}},
		{2_000_000, 200, map[string]string{"region": "ap-south-1"}},
	}
	// Run the fan-out logic manually mirroring QuerySeries — we can't hit
	// the DB from a unit test, but the grouping + aggregation paths are
	// what we care about.
	groups := map[string][]pointRow{}
	labels := map[string]map[string]string{}
	for _, r := range in {
		k, lbls := groupKey(r.attr, []string{"region"})
		groups[k] = append(groups[k], pointRow{TsNs: r.ts, Value: r.v, Kind: "gauge", Attributes: MarshalAttrs(r.attr)})
		labels[k] = lbls
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 series, got %d", len(groups))
	}
	for k, rows := range groups {
		pts := aggregateSeries(rows, AggSum, 60_000)
		if len(pts) != 1 {
			t.Errorf("series %q: expected 1 bucket, got %d", k, len(pts))
		}
	}
}
