package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// MetricInfo names a metric and reports its kind. Used by the /metrics/list
// endpoint to populate the left-hand picker in the UI.
type MetricInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Point is a single value emitted by Query for a (timestamp, labels)
// combination. Histograms collapse to their sum / count via the Aggregation
// field on Query — Phase 2 will expose the underlying buckets.
//
// Estimate/Method differentiate sampled estimates from exact values
// (programme O07): Estimate is true whenever the value depends on an
// interpolation or a modeling assumption (histogram quantiles; counter
// buckets where a reset invoked the restart-at-zero assumption), false for
// exact aggregations of observed values; Method names how the value was
// produced ("exact", "rate/per-series[+reset-assumed]",
// "rate/delta-window-sum", "histogram-quantile/linear-interpolation[...]").
type Point struct {
	TsMs   int64             `json:"ts_ms"`
	Value  float64           `json:"value"`
	Labels map[string]string `json:"labels,omitempty"`
	// Estimate is a pointer so the explicit false (exact) is serialized,
	// not dropped by omitempty.
	Estimate *bool  `json:"estimate,omitempty"`
	Method   string `json:"method,omitempty"`
}

// Series is one labelled time-series — Phase 2 fans the per-bucket
// aggregation out so callers can render one line per distinct label
// combination. Series with no GroupBy collapse to a single Series with an
// empty Labels map (Phase-1 behaviour preserved).
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// metricRow is the raw scan target for ListMetrics. metric_kind is the
// only label-free distinguisher we keep on a per-name basis.
type metricRow struct {
	Name string `db:"metric_name"`
	Kind string `db:"metric_kind"`
}

// ListMetrics returns the distinct metric names known for a site, paired
// with their kind. The kind for a given name is taken from the most
// recently observed point (consistent enough — kinds shouldn't churn).
//
// Grouped in the database, not in Go. Nucleus rejects DISTINCT over multiple
// columns, but GROUP BY over the same pair works and is what this needs: the
// result is one row per metric name, so the cost scales with how many metrics
// a site has rather than how many points it has recorded. Deduping Go-side
// meant dragging every point row across pgwire to produce a handful of names
// — 644k rows for three metrics on our own instance, seconds warm and tens of
// seconds cold, which is what made the Metrics page feel frozen.
func (s *Service) ListMetrics(ctx context.Context, siteID string) ([]MetricInfo, error) {
	if siteID == "" {
		return nil, fmt.Errorf("metrics: site_id required")
	}
	rows, err := nucleus.Query[metricRow](ctx, s.db.SQL(),
		`SELECT metric_name, metric_kind FROM metric_points WHERE site_id = $1
		 GROUP BY metric_name, metric_kind`,
		siteID,
	)
	if err != nil {
		return nil, err
	}
	// A name can still appear under two kinds if a producer changed kind
	// mid-stream; last wins, matching the previous behaviour.
	seen := make(map[string]string, len(rows))
	for _, r := range rows {
		seen[r.Name] = r.Kind
	}
	out := make([]MetricInfo, 0, len(seen))
	for name, kind := range seen {
		out = append(out, MetricInfo{Name: name, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pointRow is the scan target for raw point pulls before aggregation.
type pointRow struct {
	TsNs        int64   `db:"ts_ns"`
	Value       float64 `db:"value"`
	Histogram   string  `db:"histogram"`
	Kind        string  `db:"metric_kind"`
	Attributes  string  `db:"attributes"`
	Temporality string  `db:"aggregation_temporality"`
}

// Aggregation names the supported reducers for Query.
//
//	last/avg/sum/min/max  — work on every metric kind, return the bucketed reduction.
//	rate                  — counter-only; cumulative→delta + per-second slope.
//	p50/p95/p99           — histogram-only quantile reducers via linear interpolation.
type Aggregation string

const (
	AggLast Aggregation = "last"
	AggAvg  Aggregation = "avg"
	AggSum  Aggregation = "sum"
	AggMin  Aggregation = "min"
	AggMax  Aggregation = "max"
	AggRate Aggregation = "rate"
	AggP50  Aggregation = "p50"
	AggP95  Aggregation = "p95"
	AggP99  Aggregation = "p99"
)

// IsValidAggregation reports whether agg is one of the supported reducers.
func IsValidAggregation(agg string) bool {
	switch Aggregation(agg) {
	case AggLast, AggAvg, AggSum, AggMin, AggMax, AggRate, AggP50, AggP95, AggP99:
		return true
	}
	return false
}

// QueryOptions carries the optional knobs that grew out of Phase 1's
// fixed 60s bucket. Kept as a struct so future reducers can extend
// without touching every call site.
type QueryOptions struct {
	Agg     string
	StepMs  int64    // bucket size in milliseconds; 0 falls back to default 60s
	GroupBy []string // label keys to fan series out by; empty = single collapsed series
}

// ParseStep accepts a small whitelist of step durations and returns the
// bucket size in milliseconds. Empty / zero defaults to 60s. Invalid input
// returns an error so the HTTP layer can 400 it.
func ParseStep(raw string) (int64, error) {
	if raw == "" {
		return 60_000, nil
	}
	switch raw {
	case "15s":
		return 15_000, nil
	case "30s":
		return 30_000, nil
	case "60s", "1m":
		return 60_000, nil
	case "5m":
		return 5 * 60_000, nil
	case "1h":
		return 60 * 60_000, nil
	case "1d":
		return 24 * 60 * 60_000, nil
	}
	// Allow generic Go-style durations as a courtesy (e.g. "10m"). Cap at
	// 1d to keep the bucket count finite for sane query windows.
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, fmt.Errorf("metrics: unsupported step %q", raw)
	}
	return d.Milliseconds(), nil
}

// ParseGroupBy splits a comma-separated label-key list into a stable slice.
// Empty entries are dropped so a stray trailing comma doesn't fan out an
// empty key.
func ParseGroupBy(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Query is the Phase-1 entrypoint preserved for callers that only want a
// single collapsed series. Internally it delegates to QuerySeries and
// flattens the result — no rate() / quantile / group_by support here.
func (s *Service) Query(ctx context.Context, siteID, name string, labels map[string]string, fromMs, toMs int64, agg string) ([]Point, error) {
	series, err := s.QuerySeries(ctx, siteID, name, labels, fromMs, toMs, QueryOptions{Agg: agg})
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return []Point{}, nil
	}
	return series[0].Points, nil
}

// QuerySeries is the Phase-2 query entrypoint. It returns one Series per
// distinct combination of labels in opts.GroupBy. Without a group-by the
// result is a single Series with an empty Labels map so the caller can
// uniformly iterate.
//
// labels: AND-joined exact-match predicate applied in Go after the scan.
// Pushing it into SQL would require JSON extract which Nucleus doesn't
// expose; the row counts at Phase-1/2 scale make Go-side filtering fine.
func (s *Service) QuerySeries(ctx context.Context, siteID, name string, labels map[string]string, fromMs, toMs int64, opts QueryOptions) ([]Series, error) {
	if siteID == "" || name == "" {
		return nil, fmt.Errorf("metrics: site_id and name required")
	}
	agg := opts.Agg
	if agg == "" {
		agg = string(AggLast)
	}
	if !IsValidAggregation(agg) {
		return nil, fmt.Errorf("metrics: unsupported aggregation %q", agg)
	}
	stepMs := opts.StepMs
	if stepMs <= 0 {
		stepMs = 60_000
	}

	fromNs := fromMs * 1_000_000
	toNs := toMs * 1_000_000

	// Both sides of the comparison need an explicit BIGINT cast — Nucleus
	// pgwire advertises ts_ns as TEXT (dogfood finding #6) so a bare
	// `ts_ns >= literal` returns zero rows even when the row is in range.
	// The tracing package solved this the same way for `start_time`.
	rows, err := nucleus.Query[pointRow](ctx, s.db.SQL(),
		`SELECT ts_ns, value, histogram, metric_kind, attributes,
		        COALESCE(aggregation_temporality, 'cumulative') AS aggregation_temporality
		 FROM metric_points
		 WHERE site_id = $1 AND metric_name = $2
		   AND ts_ns >= $3
		   AND ts_ns < $4
		 ORDER BY ts_ns ASC`,
		siteID, name,
		dbutil.IntParam(fromNs),
		dbutil.IntParam(toNs),
	)
	if err != nil {
		return nil, err
	}

	// Group rows by (label-set fingerprint) → list of points. Even when
	// the caller didn't set GroupBy we route through the same map so the
	// rate / quantile reducers can run per-series before re-collapse.
	type seriesAcc struct {
		labels map[string]string
		points []pointRow
	}
	groups := map[string]*seriesAcc{}
	keys := []string{}

	for _, r := range rows {
		havem := UnmarshalAttrs(r.Attributes)
		if !MatchLabels(havem, labels) {
			continue
		}
		key, lbls := groupKey(havem, opts.GroupBy)
		acc, ok := groups[key]
		if !ok {
			acc = &seriesAcc{labels: lbls}
			groups[key] = acc
			keys = append(keys, key)
		}
		acc.points = append(acc.points, r)
	}

	sort.Strings(keys)
	out := make([]Series, 0, len(keys))
	for _, k := range keys {
		acc := groups[k]
		pts := aggregateSeries(acc.points, Aggregation(agg), stepMs)
		out = append(out, Series{Labels: acc.labels, Points: pts})
	}

	// Preserve Phase-1 contract: empty group-by + zero matches = single
	// empty series so the UI can still render an "empty" chart instead
	// of crashing on undefined.
	if len(out) == 0 && len(opts.GroupBy) == 0 {
		out = append(out, Series{Labels: map[string]string{}, Points: []Point{}})
	}
	return out, nil
}

// groupKey builds a stable fingerprint for a row's labels limited to the
// keys in groupBy. Keys are sorted so two rows with the same label values
// hash to the same string regardless of map-iteration order.
func groupKey(have map[string]string, groupBy []string) (string, map[string]string) {
	if len(groupBy) == 0 {
		return "", map[string]string{}
	}
	keys := append([]string(nil), groupBy...)
	sort.Strings(keys)
	var b strings.Builder
	out := make(map[string]string, len(keys))
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x1f') // unit-separator: never appears in label values
		}
		v := have[k]
		out[k] = v
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
	}
	return b.String(), out
}

// aggregateSeries runs the chosen reducer over one output group's rows.
// Rate and histogram quantiles FIRST split the rows into per-series
// slices by full label fingerprint (the O07 ordering rule: rate and
// cumulative-histogram differencing are per-series operations, computed
// BEFORE any cross-series aggregation); scalar reducers keep the
// Phase-1 value-level collapse.
func aggregateSeries(rows []pointRow, agg Aggregation, stepMs int64) []Point {
	switch agg {
	case AggRate:
		return rateGroupReduce(splitSeries(rows), stepMs)
	case AggP50:
		return quantileGroupReduce(splitSeries(rows), 0.50, stepMs)
	case AggP95:
		return quantileGroupReduce(splitSeries(rows), 0.95, stepMs)
	case AggP99:
		return quantileGroupReduce(splitSeries(rows), 0.99, stepMs)
	}
	return scalarReduce(rows, agg, stepMs)
}

// sortedLabelKeys returns m's keys sorted (groupKey requires sorted keys
// for a stable fingerprint).
func sortedLabelKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitSeries partitions one output group's rows into per-series row
// slices keyed by the FULL label fingerprint (every attribute key). Rows
// arrive time-ordered with arbitrary tie order across series, which is
// fine: each slice keeps its own rows in scan order.
func splitSeries(rows []pointRow) [][]pointRow {
	groups := map[string][]pointRow{}
	keys := []string{}
	for _, r := range rows {
		m := UnmarshalAttrs(r.Attributes)
		k, _ := groupKey(m, sortedLabelKeys(m))
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Strings(keys)
	out := make([][]pointRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

// rateGroupReduce computes the rate reducer for one output group built
// from one or more full-fingerprint series. The O07 reference semantics:
//
//   - Cumulative series: consecutive-pair differencing PER SERIES. A
//     reset (value decrease) is a new epoch counted under the
//     restart-at-zero assumption — the reset pair contributes curr, never
//     a negative slope (the Prometheus rate() convention). Each step
//     bucket's per-series value is increase / covered-time-span (the pair
//     is attributed to the bucket containing its END timestamp), which
//     time-weights pairs so mixed step sizes and gaps weigh by duration.
//     Duplicate / out-of-order timestamps advance the baseline and
//     contribute nothing.
//   - Delta series: the point value IS the interval increase; bucket
//     value = sum(deltas) / bucket-seconds (delta points carry no start
//     timestamp in this store, so the bucket length is the only
//     well-defined denominator).
//   - Cross-series collapse = SUM of per-series bucket rates
//     (sum(rate(...)) posture).
//
// A bucket whose computation invoked the restart-at-zero assumption is
// labeled estimate:true with the assumption named in Method; pure
// differencing is exact.
func rateGroupReduce(series [][]pointRow, stepMs int64) []Point {
	bucketSecs := float64(stepMs) / 1000.0
	type acc struct {
		perSec     float64
		resets     int
		deltaCnt   int
		cumCnt     int
		anyContrib bool
	}
	buckets := map[int64]*acc{}
	keys := []int64{}
	add := func(key int64) *acc {
		b, ok := buckets[key]
		if !ok {
			b = &acc{}
			buckets[key] = b
			keys = append(keys, key)
		}
		return b
	}

	for _, rows := range series {
		if len(rows) == 0 {
			continue
		}
		if rows[0].Temporality == "delta" {
			for _, r := range rows {
				key := bucketKeyMs(r.TsNs, stepMs)
				b := add(key)
				b.perSec += r.Value / bucketSecs
				b.deltaCnt++
				b.anyContrib = true
			}
			continue
		}
		// Cumulative: per-series differencing with reset epochs.
		type spanAcc struct {
			firstPrevNs int64
			lastCurrNs  int64
			inc         float64
			reset       bool
		}
		spans := map[int64]*spanAcc{}
		prev := rows[0]
		for i := 1; i < len(rows); i++ {
			curr := rows[i]
			if curr.TsNs <= prev.TsNs {
				// duplicate / out-of-order — baseline advances, no contribution
				prev = curr
				continue
			}
			inc := curr.Value - prev.Value
			reset := false
			if inc < 0 {
				// Counter reset: new epoch, restart-at-zero assumption.
				inc, reset = curr.Value, true
			}
			key := bucketKeyMs(curr.TsNs, stepMs)
			s, ok := spans[key]
			if !ok {
				s = &spanAcc{firstPrevNs: prev.TsNs}
				spans[key] = s
			}
			s.inc += inc
			s.reset = s.reset || reset
			s.lastCurrNs = curr.TsNs
			prev = curr
		}
		for k, s := range spans {
			b := add(k)
			b.perSec += s.inc / (float64(s.lastCurrNs-s.firstPrevNs) / 1_000_000_000)
			b.cumCnt++
			if s.reset {
				b.resets++
			}
			b.anyContrib = true
		}
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		if !b.anyContrib {
			continue
		}
		method := refMethodRatePerSeries
		switch {
		case b.resets > 0:
			method = refMethodRatePerSeriesReset
		case b.deltaCnt > 0 && b.cumCnt == 0:
			method = refMethodRateDelta
		}
		est := b.resets > 0
		out = append(out, Point{TsMs: k, Value: b.perSec, Estimate: &est, Method: method})
	}
	return out
}

// bucketKeyMs maps a nanosecond timestamp to its stepMs bucket start (ms).
func bucketKeyMs(tsNs int64, stepMs int64) int64 {
	tsMs := tsNs / 1_000_000
	return (tsMs / stepMs) * stepMs
}

// quantileGroupReduce computes a histogram quantile for one output group.
// Window histograms are built PER SERIES first (temporality-aware), then
// merged across series by adding bucket counts, then the quantile is
// interpolated once over the merged distribution (quantile-of-sums, not
// sum-of-quantiles — the Prometheus histogram_quantile over sum()
// posture):
//
//   - Delta histograms: each observation's counts land in its bucket as-is.
//   - Cumulative histograms: consecutive snapshots are differenced per
//     series (the first in-range snapshot serves only as the baseline —
//     its pre-range observations are not re-attributed). A decreasing
//     total or a bounds change is a reset: a new epoch whose window is
//     the last snapshot alone.
//   - Observations or series with mismatched bounds in the same bucket
//     are skipped and flagged in Method (+mixed-bounds) — index-wise
//     addition across different boundary sets would silently corrupt the
//     distribution.
//
// Every quantile point is an estimate (bucketed interpolation).
func quantileGroupReduce(series [][]pointRow, q float64, stepMs int64) []Point {
	type histAcc struct {
		bounds      []float64
		counts      []float64
		reset       bool
		mixedBounds bool
	}
	buckets := map[int64]*histAcc{}
	keys := []int64{}
	ensure := func(key int64, bounds []float64) *histAcc {
		b, ok := buckets[key]
		if !ok {
			b = &histAcc{bounds: append([]float64(nil), bounds...)}
			buckets[key] = b
			keys = append(keys, key)
		}
		return b
	}
	contribute := func(key int64, bounds []float64, counts []float64, reset bool) {
		b := ensure(key, bounds)
		if !boundsEqual(b.bounds, bounds) {
			b.mixedBounds = true
			return
		}
		if len(b.counts) < len(counts) {
			grown := make([]float64, len(counts))
			copy(grown, b.counts)
			b.counts = grown
		}
		for i, c := range counts {
			if i < len(b.counts) {
				b.counts[i] += c
			}
		}
		b.reset = b.reset || reset
	}

	for _, rows := range series {
		delta := len(rows) > 0 && rows[0].Temporality == "delta"
		if delta {
			for _, r := range rows {
				if r.Kind != "histogram" {
					continue
				}
				h := UnmarshalHistogram(r.Histogram)
				if len(h.Counts) == 0 {
					continue
				}
				contribute(bucketKeyMs(r.TsNs, stepMs), h.Bounds, intsToFloats(h.Counts), false)
			}
			continue
		}
		// Cumulative: difference consecutive snapshots per series.
		var prev HistogramShape
		havePrev := false
		for _, r := range rows {
			if r.Kind != "histogram" {
				continue
			}
			h := UnmarshalHistogram(r.Histogram)
			if len(h.Counts) == 0 {
				continue
			}
			key := bucketKeyMs(r.TsNs, stepMs)
			if !havePrev {
				// Baseline only: its own window is unobservable without
				// the preceding snapshot.
				prev, havePrev = h, true
				continue
			}
			if !boundsEqual(prev.Bounds, h.Bounds) || histTotal(h) < histTotal(prev) {
				// Reset (or exporter bounds change): new epoch — the last
				// snapshot alone is the window.
				contribute(key, h.Bounds, intsToFloats(h.Counts), true)
				prev, havePrev = h, true
				continue
			}
			inc := make([]float64, len(h.Counts))
			for i, c := range h.Counts {
				d := float64(c) - float64(prev.Counts[i])
				if d < 0 {
					d = 0
				}
				inc[i] = d
			}
			contribute(key, h.Bounds, inc, false)
			prev, havePrev = h, true
		}
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		total := 0.0
		for _, c := range b.counts {
			total += c
		}
		method := refMethodHistQuantile
		if b.reset {
			method = refMethodHistQuantileReset
		}
		if b.mixedBounds {
			method += "+mixed-bounds"
		}
		est := true
		out = append(out, Point{
			TsMs: k, Value: histogramQuantile(b.bounds, b.counts, total, q),
			Estimate: &est, Method: method,
		})
	}
	return out
}

// Method-string constants for the O07 estimate labeling (mirrored as
// literals in the reference test suite).
const (
	refMethodExact              = "exact"
	refMethodRatePerSeries      = "rate/per-series"
	refMethodRatePerSeriesReset = "rate/per-series+reset-assumed"
	refMethodRateDelta          = "rate/delta-window-sum"
	refMethodHistQuantile       = "histogram-quantile/linear-interpolation"
	refMethodHistQuantileReset  = "histogram-quantile/linear-interpolation+reset-assumed"
)

func boundsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intsToFloats(xs []int64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
}

func histTotal(h HistogramShape) float64 {
	t := 0.0
	for _, c := range h.Counts {
		t += float64(c)
	}
	return t
}

// scalarReduce buckets rows by stepMs and applies the gauge / sum reducer.
// For histograms it falls back to mean (sum/count) per Phase-1 behaviour —
// a display reduction of observed values, not a temporality-aware derived
// quantity (use p50/p95/p99 for that). Every scalar point aggregates exact
// observed values, so it is labeled exact.
func scalarReduce(rows []pointRow, agg Aggregation, stepMs int64) []Point {
	type bucket struct {
		points []float64
		last   float64
	}
	buckets := map[int64]*bucket{}
	keys := []int64{}

	for _, r := range rows {
		val := r.Value
		if r.Kind == "histogram" {
			h := UnmarshalHistogram(r.Histogram)
			if h.Count > 0 {
				val = h.Sum / float64(h.Count)
			}
		}
		tsMs := r.TsNs / 1_000_000
		key := (tsMs / stepMs) * stepMs
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
			keys = append(keys, key)
		}
		b.points = append(b.points, val)
		b.last = val
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		exact := false
		out = append(out, Point{TsMs: k, Value: reduce(b.points, b.last, agg), Estimate: &exact, Method: refMethodExact})
	}
	return out
}

// histogramQuantile returns the estimate of the q-quantile of a bucketed
// histogram, using the official cumulative-histogram convention: bounds
// are the upper inclusive boundaries; counts is the per-bucket population
// (last bucket is the +Inf overflow). Linear interpolation runs inside the
// crossing bucket between its boundaries (previous explicit bound as the
// lower, 0 for the first bucket); a rank landing exactly on a cumulative
// boundary returns that boundary; the +Inf bucket saturates to the last
// explicit bound. Pinned against hand-computed reference values in
// o07_reference_test.go.
func histogramQuantile(bounds []float64, counts []float64, total float64, q float64) float64 {
	if total <= 0 || len(counts) == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := q * total
	cum := 0.0
	for i, c := range counts {
		next := cum + c
		if next >= target {
			lower := 0.0
			upper := 0.0
			if i > 0 && i-1 < len(bounds) {
				lower = bounds[i-1]
			}
			if i < len(bounds) {
				upper = bounds[i]
			} else {
				// +Inf overflow — there's no upper bound, so the best
				// estimate is the previous bound (saturate).
				if len(bounds) > 0 {
					return bounds[len(bounds)-1]
				}
				return lower
			}
			if c <= 0 {
				return upper
			}
			frac := (target - cum) / c
			return lower + (upper-lower)*frac
		}
		cum = next
	}
	if len(bounds) > 0 {
		return bounds[len(bounds)-1]
	}
	return 0
}

// reduce applies the aggregation across a per-bucket slice. last is
// passed in separately so we don't have to re-sort just to find the
// final element (Query already inserts in time order).
func reduce(values []float64, last float64, agg Aggregation) float64 {
	if len(values) == 0 {
		return 0
	}
	switch agg {
	case AggLast:
		return last
	case AggSum:
		var s float64
		for _, v := range values {
			s += v
		}
		return s
	case AggAvg:
		var s float64
		for _, v := range values {
			s += v
		}
		return s / float64(len(values))
	case AggMin:
		m := values[0]
		for _, v := range values[1:] {
			if v < m {
				m = v
			}
		}
		return m
	case AggMax:
		m := values[0]
		for _, v := range values[1:] {
			if v > m {
				m = v
			}
		}
		return m
	}
	return last
}

// ParseLabelFilters extracts label.* query parameters into a map. Used
// by the HTTP query handler.
func ParseLabelFilters(query map[string][]string) map[string]string {
	out := map[string]string{}
	for k, vs := range query {
		if !strings.HasPrefix(k, "label.") || len(vs) == 0 {
			continue
		}
		out[strings.TrimPrefix(k, "label.")] = vs[0]
	}
	return out
}
