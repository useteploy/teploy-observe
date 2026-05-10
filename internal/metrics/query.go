package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

// Aggregation names the supported reducers for Query. last/avg/sum/min/max
// cover the Phase-1 list — rate(), heatmap, and quantile reducers ship
// in Phase 2.
type Aggregation string

const (
	AggLast Aggregation = "last"
	AggAvg  Aggregation = "avg"
	AggSum  Aggregation = "sum"
	AggMin  Aggregation = "min"
	AggMax  Aggregation = "max"
)

// IsValidAggregation reports whether agg is one of the supported reducers.
func IsValidAggregation(agg string) bool {
	switch Aggregation(agg) {
	case AggLast, AggAvg, AggSum, AggMin, AggMax:
		return true
	}
	return false
}

// Query returns aggregated points for a metric in [fromMs, toMs], filtered
// by labels. Phase 1 returns one point per minute bucket; the bucket size
// is fixed (60s) — Phase 2 will expose `step` to callers.
//
// labels: AND-joined exact-match predicate applied in Go after the scan.
// Pushing it into SQL would require JSON extract which Nucleus doesn't
// expose; the row counts at Phase-1 scale make Go-side filtering fine.
func (s *Service) Query(ctx context.Context, siteID, name string, labels map[string]string, fromMs, toMs int64, agg string) ([]Point, error) {
	if siteID == "" || name == "" {
		return nil, fmt.Errorf("metrics: site_id and name required")
	}
	if agg == "" {
		agg = string(AggLast)
	}
	if !IsValidAggregation(agg) {
		return nil, fmt.Errorf("metrics: unsupported aggregation %q", agg)
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
		   AND CAST(ts_ns AS BIGINT) >= CAST($3 AS BIGINT)
		   AND CAST(ts_ns AS BIGINT) < CAST($4 AS BIGINT)
		 ORDER BY ts_ns ASC`,
		siteID, name,
		dbutil.IntParam(fromNs),
		dbutil.IntParam(toNs),
	)
	if err != nil {
		return nil, err
	}

	// Filter by labels in Go (see method comment for rationale).
	type bucket struct {
		points []float64
		last   float64
		ts     int64
	}
	const bucketMs int64 = 60_000
	buckets := make(map[int64]*bucket)
	keys := []int64{}

	for _, r := range rows {
		havem := UnmarshalAttrs(r.Attributes)
		if !MatchLabels(havem, labels) {
			continue
		}

		val := r.Value
		if r.Kind == "histogram" {
			h := UnmarshalHistogram(r.Histogram)
			if h.Count > 0 {
				val = h.Sum / float64(h.Count)
			}
		}

		tsMs := r.TsNs / 1_000_000
		key := (tsMs / bucketMs) * bucketMs
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
			keys = append(keys, key)
		}
		b.points = append(b.points, val)
		b.last = val
		b.ts = tsMs
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		v := reduce(b.points, b.last, Aggregation(agg))
		out = append(out, Point{TsMs: k, Value: v})
	}
	return out, nil
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
