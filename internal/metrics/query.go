package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

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
type Point struct {
	TsMs   int64             `json:"ts_ms"`
	Value  float64           `json:"value"`
	Labels map[string]string `json:"labels,omitempty"`
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
// We pull rows + dedupe in Go because Nucleus rejects DISTINCT-with-multi-
// column patterns we'd otherwise want here.
func (s *Service) ListMetrics(ctx context.Context, siteID string) ([]MetricInfo, error) {
	if siteID == "" {
		return nil, fmt.Errorf("metrics: site_id required")
	}
	rows, err := nucleus.Query[metricRow](ctx, s.db.SQL(),
		`SELECT metric_name, metric_kind FROM metric_points WHERE site_id = $1`,
		siteID,
	)
	if err != nil {
		return nil, err
	}
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
	TsNs       int64   `db:"ts_ns"`
	Value      float64 `db:"value"`
	Histogram  string  `db:"histogram"`
	Kind       string  `db:"metric_kind"`
	Attributes string  `db:"attributes"`
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
		`SELECT ts_ns, value, histogram, metric_kind, attributes
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

// aggregateSeries runs the chosen reducer over a single label-fingerprinted
// row slice. Histograms / counters get specialised paths — everything else
// falls back to the bucketed reduce() helper from Phase 1.
func aggregateSeries(rows []pointRow, agg Aggregation, stepMs int64) []Point {
	switch agg {
	case AggRate:
		return rateReduce(rows, stepMs)
	case AggP50:
		return quantileReduce(rows, 0.50, stepMs)
	case AggP95:
		return quantileReduce(rows, 0.95, stepMs)
	case AggP99:
		return quantileReduce(rows, 0.99, stepMs)
	}
	return scalarReduce(rows, agg, stepMs)
}

// scalarReduce buckets rows by stepMs and applies the gauge / sum reducer.
// For histograms it falls back to mean (sum/count) per Phase-1 behaviour.
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
		out = append(out, Point{TsMs: k, Value: reduce(b.points, b.last, agg)})
	}
	return out
}

// rateReduce converts a cumulative counter into a per-second slope.
//
// Algorithm: walk the time-sorted slice, take consecutive (prev, curr)
// pairs, and emit slope = (curr.value - prev.value) / Δt. Counter resets
// (curr.value < prev.value) skip the negative slope and use curr as the
// new baseline — Prometheus does the same.
//
// Output is bucketed by stepMs: when multiple consecutive pairs land in
// the same bucket we average their slopes. Buckets with no pair-data
// are simply omitted (rather than emitting a zero-rate sentinel that
// would distort downstream alerting).
func rateReduce(rows []pointRow, stepMs int64) []Point {
	if len(rows) < 2 {
		return []Point{}
	}
	type acc struct {
		sum  float64
		n    int
		tsMs int64
	}
	buckets := map[int64]*acc{}
	keys := []int64{}

	prev := rows[0]
	for i := 1; i < len(rows); i++ {
		curr := rows[i]
		dtNs := curr.TsNs - prev.TsNs
		if dtNs <= 0 {
			// duplicate / out-of-order — treat curr as the new baseline.
			prev = curr
			continue
		}
		if curr.Value < prev.Value {
			// counter reset — drop the negative slope, advance baseline.
			prev = curr
			continue
		}
		slope := (curr.Value - prev.Value) / float64(dtNs) * 1_000_000_000
		tsMs := curr.TsNs / 1_000_000
		key := (tsMs / stepMs) * stepMs
		b, ok := buckets[key]
		if !ok {
			b = &acc{tsMs: key}
			buckets[key] = b
			keys = append(keys, key)
		}
		b.sum += slope
		b.n++
		prev = curr
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		if b.n == 0 {
			continue
		}
		out = append(out, Point{TsMs: b.tsMs, Value: b.sum / float64(b.n)})
	}
	return out
}

// quantileReduce computes a quantile estimate per stepMs bucket from
// histogram rows. When a bucket holds multiple histogram observations the
// counts are summed across observations before the quantile is solved —
// matches Prometheus's histogram_quantile() over a sum() group.
//
// Linear interpolation across the bucket the cumulative count first
// crosses (q × total). Returns 0 for empty buckets.
func quantileReduce(rows []pointRow, q float64, stepMs int64) []Point {
	type acc struct {
		bounds []float64
		counts []float64
		total  float64
		tsMs   int64
	}
	buckets := map[int64]*acc{}
	keys := []int64{}

	for _, r := range rows {
		if r.Kind != "histogram" {
			continue
		}
		h := UnmarshalHistogram(r.Histogram)
		if len(h.Counts) == 0 {
			continue
		}
		tsMs := r.TsNs / 1_000_000
		key := (tsMs / stepMs) * stepMs
		b, ok := buckets[key]
		if !ok {
			b = &acc{tsMs: key, bounds: append([]float64(nil), h.Bounds...)}
			buckets[key] = b
			keys = append(keys, key)
		}
		// Initialise / resize the per-bucket counter slice on the first
		// row; later rows in the same bucket must share the same bounds
		// or we ignore the mismatch (histograms with shifting bounds in
		// the same window are user error).
		if len(b.counts) < len(h.Counts) {
			grown := make([]float64, len(h.Counts))
			copy(grown, b.counts)
			b.counts = grown
		}
		for i, c := range h.Counts {
			if i < len(b.counts) {
				b.counts[i] += float64(c)
				b.total += float64(c)
			}
		}
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		out = append(out, Point{TsMs: b.tsMs, Value: histogramQuantile(b.bounds, b.counts, b.total, q)})
	}
	return out
}

// histogramQuantile returns the estimate of the q-quantile of a bucketed
// histogram. bounds are the upper inclusive boundaries; counts is the
// per-bucket population (last bucket is the +Inf overflow). Implements
// linear interpolation inside the crossing bucket — within the lower
// boundary and the bucket's upper bound. The very first bucket is
// interpolated from 0 since we have no lower bound for it.
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
