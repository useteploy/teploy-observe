package metrics

// O07 reference-calculation suite (programme workstream O07, "incorrect
// math P0"). Every expected value in this file was HAND-COMPUTED from the
// pinned reference conventions, independent of the implementation:
//
//   RATE (cumulative counters) — per-series consecutive-pair differencing
//   BEFORE any cross-series aggregation; a reset (value decrease) is a new
//   epoch counted under the restart-at-zero assumption (the Prometheus
//   rate() convention: the reset pair contributes curr, never a negative
//   slope); each step bucket's value is total-increase / covered-time-span
//   (time-weighted, so mixed step sizes and gaps weigh pairs by duration).
//   Cross-series collapse = SUM of per-series rates (sum(rate(...))).
//
//   RATE (delta counters) — the delta value IS the interval increase;
//   bucket rate = sum(deltas) / bucket_seconds. Additive across series.
//
//   HISTOGRAM QUANTILES — the official cumulative-histogram convention:
//   linear interpolation INSIDE the crossing bucket between its lower
//   boundary (previous explicit bound, 0 for the first bucket) and its
//   upper boundary (the explicit bound), fraction (rank - below) / count;
//   a rank landing exactly on a cumulative boundary returns that boundary;
//   the +Inf bucket saturates to the last explicit bound. Cumulative
//   temporality histograms are differenced per series (reset = epoch ->
//   last snapshot alone) before merging; delta histograms sum directly.
//
// NAIVE BEHAVIORS THESE TABLES PROVE WRONG (recorded per the programme):
//   - mean-of-pair-slopes per bucket (equal-weights pairs): R3/R4 below —
//     a 1s-dense pair outweighs a 58s pair by 58x.
//   - drop-the-reset-interval (old behavior): R2 below loses the 5 the
//     counter actually accumulated since restart; reference counts it.
//   - differencing a cross-series merge: R5 below — values from different
//     counters subtract into garbage (this is the programme's explicit
//     rate-before-aggregation ordering rule).
//   - summing raw cumulative-histogram snapshots: Q3 below — two
//     snapshots [1,1,1]+[2,3,4] summed to [3,4,5] answer 40 for p50 where
//     the window's real distribution [1,2,3] answers 50.
//   - interpolating across bucket midpoints/centers without boundaries:
//     Q1's p70 is 90 under boundary interpolation; a centers method gives
//     5+(30-5)*(14-5)/(10-5)*... — any value other than 90/50/10/100/4
//     on that table is a wrong method.

import (
	"math"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func ns(sec float64) int64 { return int64(sec * 1_000_000_000) }

// cumRow builds a cumulative sum point with the given label JSON.
func cumRow(sec float64, v float64, attrs string) pointRow {
	return pointRow{TsNs: ns(sec), Value: v, Kind: "sum", Temporality: "cumulative", Attributes: attrs}
}

func deltaRow(sec float64, v float64, attrs string) pointRow {
	return pointRow{TsNs: ns(sec), Value: v, Kind: "sum", Temporality: "delta", Attributes: attrs}
}

func histRow(sec float64, counts []jsonInt, bounds []float64, temporality, attrs string) pointRow {
	return pointRow{
		TsNs:        ns(sec),
		Kind:        "histogram",
		Temporality: temporality,
		Attributes:  attrs,
		Histogram: MarshalHistogram(HistogramDataPoint{
			Count:          jsonInt(sumJSONInt(counts)),
			BucketCounts:   counts,
			ExplicitBounds: bounds,
		}),
	}
}

func sumJSONInt(cs []jsonInt) string {
	t := 0
	for _, c := range cs {
		n := 0
		for _, r := range string(c) {
			n = n*10 + int(r-'0')
		}
		t += n
	}
	s := ""
	if t == 0 {
		return "0"
	}
	for t > 0 {
		s = string(rune('0'+t%10)) + s
		t /= 10
	}
	return s
}

// approx compares with the tolerance used across the reference tables.
func approx(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

type refPoint struct {
	tsMs     int64
	value    float64
	estimate *bool
	method   string
}

func assertRefPoints(t *testing.T, got []Point, want []refPoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.TsMs != w.tsMs {
			t.Errorf("point[%d].ts_ms = %d, want %d", i, g.TsMs, w.tsMs)
		}
		if !approx(g.Value, w.value) {
			t.Errorf("point[%d] (ts %d) value = %v, want %v", i, g.TsMs, g.Value, w.value)
		}
		if w.estimate == nil {
			t.Fatalf("test bug: want.estimate must be explicit")
		}
		if g.Estimate == nil {
			t.Errorf("point[%d] (ts %d): estimate label missing, want %v (every computed point must be labeled estimate or exact)", i, g.TsMs, *w.estimate)
		} else if *g.Estimate != *w.estimate {
			t.Errorf("point[%d] (ts %d).estimate = %v, want %v", i, g.TsMs, *g.Estimate, *w.estimate)
		}
		if g.Method != w.method {
			t.Errorf("point[%d] (ts %d).method = %q, want %q", i, g.TsMs, g.Method, w.method)
		}
	}
}

// ─── RATE, cumulative ────────────────────────────────────────────────────

// R1 — uniform 1s steps, no reset. Increases +10, +20, +15 over [0s,3s]:
// total increase 45 over covered span 3s = 15/s. Single 10s bucket.
func TestO07_RateR1_UniformNoReset(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 10, ""), cumRow(2, 30, ""), cumRow(3, 45, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 10_000), []refPoint{
		{0, 15, boolPtr(false), "rate/per-series"},
	})
}

// R2 — reset is a new epoch counted under restart-at-zero, not a dropped
// interval and not negative rate. Points 0,10,30 then RESET to 5, then 15.
// Increases: +10, +20, reset-pair contributes curr=5, +10 = 45 total over
// span [0s,4s] = 4s -> 11.25/s. The bucket invoked the restart-at-zero
// assumption, so it is labeled estimate.
//
// Hand reference: 45/4 = 11.25 exactly.
func TestO07_RateR2_ResetRestartAtZero(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 10, ""), cumRow(2, 30, ""), cumRow(3, 5, ""), cumRow(4, 15, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 10_000), []refPoint{
		{0, 11.25, boolPtr(true), "rate/per-series+reset-assumed"},
	})
}

// R3 — gap spanning buckets. Points at 0s(0), 1s(10), 100s(110), 101s(120),
// step 60s. Bucket 0s holds the pair ending 1s: +10 over 1s = 10/s.
// Bucket 60s holds pairs ending 100s and 101s: +100 (the gap pair — the
// increase is time-weighted across the gap) and +10 = 110 over span
// [1s,101s] = 100s -> 1.1/s.
//
// The old mean-of-slopes bucket value was (1.0101...+10)/2 ~= 5.5/s — the
// 1s-dense pair outweighed the 99s gap pair ~5x. Reference: time-weight.
func TestO07_RateR3_GapAcrossBuckets(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 10, ""), cumRow(100, 110, ""), cumRow(101, 120, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 10, boolPtr(false), "rate/per-series"},
		{60_000, 1.1, boolPtr(false), "rate/per-series"},
	})
}

// R4 — mixed step sizes inside one bucket. Points 0s(0), 1s(100), 59s(101),
// step 60s. Increases +100 (1s) and +1 (58s) = 101 over span 59s.
// Hand reference: 101/59 = 1.711864406779661...
// Mean-of-slopes would answer (100 + 1/58)/2 ~= 50.0086/s.
func TestO07_RateR4_MixedStepSizes(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 100, ""), cumRow(59, 101, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 1.711864406779661, boolPtr(false), "rate/per-series"},
	})
}

// R5 — the ordering rule: rate per series BEFORE cross-series aggregation.
// Two counters (labels a=1 and b=2) reporting at OFFSET timestamps
// (real instances do not share nanoseconds), interleaved as the scan
// returns them, NO group_by, one 60s bucket. Series a: 0 -> 100 over
// [0s,10s] = 10/s. Series b: 0 -> 50 over [5s,15s] = 5/s. Collapsed
// total = SUM = 15/s.
//
// Differencing the merged rows instead walks a->b->a->b: the cross-series
// drops read as resets (spurious restart-at-zero contributions) and the
// answer is 10/s with a false reset label — this is the mutation target
// for the ordering rule. (Equal timestamps + zero starts would hide the
// defect behind the duplicate-timestamp skip rule — the offsets make the
// kill deterministic.)
func TestO07_RateR5_PerSeriesBeforeAggregation(t *testing.T) {
	attrsA := MarshalAttrs(map[string]string{"a": "1"})
	attrsB := MarshalAttrs(map[string]string{"b": "2"})
	rows := []pointRow{
		cumRow(0, 0, attrsA), cumRow(5, 0, attrsB),
		cumRow(10, 100, attrsA), cumRow(15, 50, attrsB),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 15, boolPtr(false), "rate/per-series"},
	})
}

// R5b — same rule with group_by keys that COLLIDE: two instances share
// region=us-east-1, so the output group holds two full series.
func TestO07_RateR5b_GroupKeyCollision(t *testing.T) {
	i1 := MarshalAttrs(map[string]string{"region": "us-east-1", "instance": "i-1"})
	i2 := MarshalAttrs(map[string]string{"region": "us-east-1", "instance": "i-2"})
	rows := []pointRow{
		cumRow(0, 0, i1), cumRow(5, 0, i2),
		cumRow(10, 100, i1), cumRow(15, 50, i2),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 15, boolPtr(false), "rate/per-series"},
	})
}

// R6 — reset pair straddling a bucket boundary + second bucket without
// reset. Points 0s(100), 30s(150), 60s(10 — RESET), 90s(40), step 60s.
// Bucket 0s: +50 over [0s,30s] = 5/3 = 1.6666666666666667/s, exact.
// Bucket 60s: reset pair (150 -> 10) contributes 10 (restart-at-zero) +
// (40-10) = 40 total over span [30s,90s] = 60s = 0.6666666666666666/s,
// labeled estimate (assumption invoked).
func TestO07_RateR6_ResetAtBucketBoundary(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 100, ""), cumRow(30, 150, ""), cumRow(60, 10, ""), cumRow(90, 40, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 1.6666666666666667, boolPtr(false), "rate/per-series"},
		{60_000, 0.6666666666666666, boolPtr(true), "rate/per-series+reset-assumed"},
	})
}

// R7 — duplicate timestamp: the zero-duration pair contributes nothing and
// advances the baseline (the later sample wins), like-for-like with the
// counter walk. 0s(0), 1s(10), 1s(10) dup, 2s(30) -> +10 then +20 = 30
// over [0s,2s] = 15/s.
func TestO07_RateR7_DuplicateTimestamp(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 10, ""), cumRow(1, 10, ""), cumRow(2, 30, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 10_000), []refPoint{
		{0, 15, boolPtr(false), "rate/per-series"},
	})
}

// ─── RATE, delta temporality ─────────────────────────────────────────────

// D1 — deltas are direct: rate = sum(deltas)/bucket_seconds.
// Deltas 3 (at 10s) and 9 (at 20s) in one 60s bucket: 12/60 = 0.2/s.
func TestO07_RateD1_DeltaDirect(t *testing.T) {
	rows := []pointRow{deltaRow(10, 3, ""), deltaRow(20, 9, "")}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 0.2, boolPtr(false), "rate/delta-window-sum"},
	})
}

// D2 — delta increments are additive across series: 3 + 4 in one bucket
// = 7/60 = 0.11666666666666667/s (collapse is CORRECT for deltas — no
// differencing involved).
func TestO07_RateD2_DeltaAdditiveAcrossSeries(t *testing.T) {
	attrsA := MarshalAttrs(map[string]string{"a": "1"})
	attrsB := MarshalAttrs(map[string]string{"b": "2"})
	rows := []pointRow{deltaRow(10, 3, attrsA), deltaRow(10, 4, attrsB)}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 0.11666666666666667, boolPtr(false), "rate/delta-window-sum"},
	})
}

// D3 — deltas across buckets: 3 in bucket 0s (3/60), 9 in bucket 60s
// (9/60 = 0.15).
func TestO07_RateD3_DeltaAcrossBuckets(t *testing.T) {
	rows := []pointRow{deltaRow(10, 3, ""), deltaRow(70, 9, "")}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 60_000), []refPoint{
		{0, 0.05, boolPtr(false), "rate/delta-window-sum"},
		{60_000, 0.15, boolPtr(false), "rate/delta-window-sum"},
	})
}

// ─── HISTOGRAM QUANTILES ─────────────────────────────────────────────────

// Q1 — the official interpolation convention, boundary cases included.
// Delta observation, bounds [10,50,100], per-bucket counts [5,5,5,5]
// (total 20). A quantile is an ESTIMATE interpolated from bucketed data.
//
//	p25 rank 5  -> exactly the cumulative boundary after bucket 0 -> 10
//	p10 rank 2  -> bucket 0 interior, frac 2/5 from lower bound 0 -> 4
//	p50 rank 10 -> exactly the boundary after bucket 1 -> 50
//	p70 rank 14 -> bucket 1... cum after b0=5, after b1=10 <14, bucket 2
//	              (bounds 50..100), frac (14-10)/5 = 0.8 -> 50+50*0.8 = 90
//	p95 rank 19 -> +Inf bucket -> saturate to last bound 100
//
// Naive linear-across-buckets (midpoints/centers, no boundaries) cannot
// produce this table: midpoints of [0..10],[10..50],[50..100] are
// 5,30,75 — p70 would interpolate between 30 and 75, not 90; p25 between
// 5 and 30, not 10; boundary-exactness (rank on a cumulative boundary
// returns the bound) is unreachable without boundaries.
func TestO07_QuantileQ1_OfficialInterpolation(t *testing.T) {
	rows := []pointRow{histRow(10, []jsonInt{"5", "5", "5", "5"}, []float64{10, 50, 100}, "delta", "")}
	for _, tc := range []struct {
		q    float64
		want float64
	}{{0.50, 50}, {0.25, 10}, {0.10, 4}, {0.70, 90}, {0.95, 100}} {
		pts := quantileGroupReduce(splitSeries(rows), tc.q, 60_000)
		assertRefPoints(t, pts, []refPoint{
			{0, tc.want, boolPtr(true), "histogram-quantile/linear-interpolation"},
		})
	}
}

// Q2 — multiple delta observations in one bucket sum their counts
// (Prometheus histogram_quantile over sum() posture): [5,5,0] + [0,0,10]
// -> [5,5,10] total 20; p95 rank 19 crosses into +Inf -> saturate 50.
func TestO07_QuantileQ2_DeltaObservationsSum(t *testing.T) {
	rows := []pointRow{
		histRow(10, []jsonInt{"5", "5", "0"}, []float64{10, 50}, "delta", ""),
		histRow(20, []jsonInt{"0", "0", "10"}, []float64{10, 50}, "delta", ""),
	}
	pts := quantileGroupReduce(splitSeries(rows), 0.95, 60_000)
	assertRefPoints(t, pts, []refPoint{
		{0, 50, boolPtr(true), "histogram-quantile/linear-interpolation"},
	})
}

// Q3 — CUMULATIVE histograms are differenced per series before the
// quantile is solved. Snapshots at 0s [1,1,1] and 30s [2,3,4] (bounds
// [10,50,100]): window distribution = [1,2,3], total 6.
// p50 rank 3 -> cum after b0=1 <3, b1 cum=3 on boundary -> 50.
// p95 rank 5.7 -> bucket 2, frac (5.7-3)/3=0.9 -> 50+50*0.9 = 95.
//
// Summing the raw snapshots (old behavior) gives [3,4,5] total 12 and
// answers p50 = 10+40*(6-3)/4 = 40 — wrong; the reference is 50.
func TestO07_QuantileQ3_CumulativeDifferenced(t *testing.T) {
	rows := []pointRow{
		histRow(0, []jsonInt{"1", "1", "1"}, []float64{10, 50, 100}, "cumulative", ""),
		histRow(30, []jsonInt{"2", "3", "4"}, []float64{10, 50, 100}, "cumulative", ""),
	}
	for _, tc := range []struct {
		q    float64
		want float64
	}{{0.50, 50}, {0.95, 95}} {
		pts := quantileGroupReduce(splitSeries(rows), tc.q, 60_000)
		assertRefPoints(t, pts, []refPoint{
			{0, tc.want, boolPtr(true), "histogram-quantile/linear-interpolation"},
		})
	}
}

// Q4 — cumulative histogram RESET: total decreases (15 -> 3), so the pair
// is a new epoch; the window distribution is the last snapshot alone
// [1,1,1], total 3. p50 rank 1.5 -> bucket 1, frac (1.5-1)/1 = 0.5 ->
// 10 + 40*0.5 = 30. Labeled estimate with the reset assumption named.
func TestO07_QuantileQ4_CumulativeResetEpoch(t *testing.T) {
	rows := []pointRow{
		histRow(0, []jsonInt{"5", "5", "5"}, []float64{10, 50, 100}, "cumulative", ""),
		histRow(30, []jsonInt{"1", "1", "1"}, []float64{10, 50, 100}, "cumulative", ""),
	}
	pts := quantileGroupReduce(splitSeries(rows), 0.50, 60_000)
	assertRefPoints(t, pts, []refPoint{
		{0, 30, boolPtr(true), "histogram-quantile/linear-interpolation+reset-assumed"},
	})
}

// Q5 — per-series differencing then merge: series a's snapshots [2,2,2]
// -> [3,2,2] give window increment [1,0,0]; series b's [1,1,1] -> [1,2,1]
// give [0,1,0]; merged [1,1,0] total 2. p50 rank 1 is exactly the
// cumulative boundary after bucket 0 -> 10.
//
// Summing the raw snapshots instead gives [7,7,6] total 20 and answers
// 10+40*(10-7)/7 = 27.142857... — wrong.
func TestO07_QuantileQ5_PerSeriesThenMerge(t *testing.T) {
	attrsA := MarshalAttrs(map[string]string{"a": "1"})
	attrsB := MarshalAttrs(map[string]string{"b": "2"})
	rows := []pointRow{
		histRow(0, []jsonInt{"2", "2", "2"}, []float64{10, 50, 100}, "cumulative", attrsA),
		histRow(30, []jsonInt{"3", "2", "2"}, []float64{10, 50, 100}, "cumulative", attrsA),
		histRow(0, []jsonInt{"1", "1", "1"}, []float64{10, 50, 100}, "cumulative", attrsB),
		histRow(30, []jsonInt{"1", "2", "1"}, []float64{10, 50, 100}, "cumulative", attrsB),
	}
	pts := quantileGroupReduce(splitSeries(rows), 0.50, 60_000)
	assertRefPoints(t, pts, []refPoint{
		{0, 10, boolPtr(true), "histogram-quantile/linear-interpolation"},
	})
}

// ─── EXACT LABELING ──────────────────────────────────────────────────────

// S1 — scalar reducers aggregate exact observed values: labeled exact.
func TestO07_ScalarLabeledExact(t *testing.T) {
	rows := []pointRow{
		{TsNs: ns(0), Value: 1, Kind: "gauge", Temporality: "cumulative", Attributes: ""},
		{TsNs: ns(1), Value: 3, Kind: "gauge", Temporality: "cumulative", Attributes: ""},
	}
	assertRefPoints(t, aggregateSeries(rows, AggSum, 10_000), []refPoint{
		{0, 4, boolPtr(false), "exact"},
	})
}

// S2 — rate and quantile surface through the public Aggregation names with
// the same reference values (dispatch wiring check).
func TestO07_DispatchSurfacesReferenceValues(t *testing.T) {
	rows := []pointRow{
		cumRow(0, 0, ""), cumRow(1, 10, ""), cumRow(2, 30, ""), cumRow(3, 5, ""), cumRow(4, 15, ""),
	}
	assertRefPoints(t, aggregateSeries(rows, AggRate, 10_000), []refPoint{
		{0, 11.25, boolPtr(true), "rate/per-series+reset-assumed"},
	})
	hrows := []pointRow{histRow(10, []jsonInt{"5", "5", "5", "5"}, []float64{10, 50, 100}, "delta", "")}
	assertRefPoints(t, aggregateSeries(hrows, AggP95, 60_000), []refPoint{
		{0, 100, boolPtr(true), "histogram-quantile/linear-interpolation"},
	})
	assertRefPoints(t, aggregateSeries(hrows, AggP50, 60_000), []refPoint{
		{0, 50, boolPtr(true), "histogram-quantile/linear-interpolation"},
	})
}
