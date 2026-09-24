// Package metrics implements OTLP metrics ingest + query for Observe.
//
// Phase 1 (W3.A) shipped the schema, the HTTP ingest endpoint, a small
// list / query API for the dashboard, and the SDK helpers that emit OTLP
// JSON. Phase 2 layered the per-series fan-out with rate and histogram
// quantile reducers; O07 pinned their reference semantics (counter-reset
// restart-at-zero, per-series rate before aggregation, cumulative-histogram
// differencing, boundary interpolation, estimate labeling) in
// o07_reference_test.go. Observe deliberately does not implement PromQL —
// see README "Metrics query semantics".
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Service writes OTLP metric points and answers list / query requests.
// One Service shared across HTTP handlers — it has no per-request state
// beyond the O12 cardinality guard (bounded, counter-only).
type Service struct {
	db     *nucleus.Client
	logger *slog.Logger
	guard  *seriesGuard
}

func NewService(db *nucleus.Client) *Service {
	return &Service{db: db, logger: slog.Default(), guard: newSeriesGuard(DefaultCardinalityLimits())}
}

// WithLogger threads a custom logger so ingest failures appear under the
// caller's request context (matches tracing.IngestService.WithLogger).
func (s *Service) WithLogger(logger *slog.Logger) *Service {
	if logger == nil {
		return s
	}
	s.logger = logger
	return s
}

// WithCardinalityLimits installs the O12 ingest-side cardinality limits
// (env-tunable at boot; defaults declared in cardinality.go).
func (s *Service) WithCardinalityLimits(l CardinalityLimits) *Service {
	s.guard = newSeriesGuard(l)
	return s
}

// CardinalityStats snapshots the ingest cardinality guard for /healthz.
func (s *Service) CardinalityStats() CardinalityStats {
	return s.guard.Stats()
}

// IngestResponse is returned to the OTLP exporter.
type IngestResponse struct {
	OK     bool `json:"ok"`
	Points int  `json:"points"`
}

// metricPointRow is one metric_points row, built without touching the DB.
// Ingest collects a whole OTLP export into a slice of these before writing,
// so the batch lands in chunked multi-row INSERTs instead of one autocommit
// INSERT per data point (see insertMetricRows). Named distinctly from
// query.go's metricRow (the ListMetrics scan target) to avoid colliding in
// this package.
type metricPointRow struct {
	name, kind, service    string
	attrsJSON              string
	tsNs                   int64
	value                  float64
	histogram              string
	monotonic, temporality string
}

// Ingest processes an OTLP ExportMetricsServiceRequest and writes one
// metric_points row per data point. Writes are synchronous so the SDK's
// retry / dropping policy can rely on the response status.
//
// Failure semantics: the first INSERT error aborts the batch and bubbles
// up. We deliberately do NOT swallow errors here — finding #25 (silent
// span persistence) was caused by exactly that pattern in tracing's
// async path. If the synchronous INSERT returns OK but the row is
// missing, we want to surface that as a Nucleus bug, not absorb it.
//
// Rows are batch-inserted (internal/ingest/buffer.go's chunked multi-row
// pattern) rather than one INSERT per data point — an unbatched per-point
// loop was directly contributing to Nucleus memory pressure (each INSERT is
// a separate write that also invalidates the whole query-result cache), and
// a single histogram-heavy export can carry hundreds of data points.
//
// O12 cardinality guard (cardinality.go): per-point attribute maps are
// truncated with counters, a request above MaxPointsPerRequest is refused
// labeled (ErrTooManyPoints — permanent, the exporter must split), and a
// data point introducing a NEW series past the per-site cap is dropped
// and counted (known series keep flowing; the registry is in-memory and
// re-learns after restart). IngestResponse.Points is the ACCEPTED count.
func (s *Service) Ingest(ctx context.Context, siteID string, req ExportMetricsRequest) (IngestResponse, error) {
	if siteID == "" {
		return IngestResponse{}, fmt.Errorf("metrics: site_id required")
	}
	sql := s.db.SQL()

	var rows []metricPointRow
	var dropped int
	// guardedAttrs applies the cardinality guard to one data point's
	// label map (truncation with counters) and returns the marshaled,
	// deterministic JSON the row persists.
	guardedAttrs := func(attrs []KeyValue) string {
		return MarshalAttrs(s.guard.truncateAttrs(siteID, AttrsToMap(attrs)))
	}
	admit := func(r metricPointRow) bool {
		return s.guard.admit(siteID, seriesKey(r.name, r.service, r.attrsJSON))
	}
	for _, rm := range req.ResourceMetrics {
		serviceName := ExtractServiceName(rm.Resource.Attributes)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch {
				case m.Gauge != nil:
					for _, dp := range m.Gauge.DataPoints {
						r := numberRow(m.Name, "gauge", serviceName, dp, guardedAttrs(dp.Attributes), "false", "cumulative")
						if admit(r) {
							rows = append(rows, r)
						} else {
							dropped++
						}
					}
				case m.Sum != nil:
					monotonic := "false"
					if m.Sum.IsMonotonic {
						monotonic = "true"
					}
					temp := AggregationTemporality(m.Sum.AggregationTemporality)
					for _, dp := range m.Sum.DataPoints {
						r := numberRow(m.Name, "sum", serviceName, dp, guardedAttrs(dp.Attributes), monotonic, temp)
						if admit(r) {
							rows = append(rows, r)
						} else {
							dropped++
						}
					}
				case m.Histogram != nil:
					temp := AggregationTemporality(m.Histogram.AggregationTemporality)
					for _, dp := range m.Histogram.DataPoints {
						r := histogramRow(m.Name, serviceName, dp, guardedAttrs(dp.Attributes), temp)
						if admit(r) {
							rows = append(rows, r)
						} else {
							dropped++
						}
					}
				}
			}
		}
	}
	if err := s.guard.checkBatch(len(rows)); err != nil {
		return IngestResponse{}, err
	}

	count, err := insertMetricRows(ctx, sql, siteID, rows)
	if err != nil {
		return IngestResponse{}, fmt.Errorf("insert metric points: %w", err)
	}
	if dropped > 0 {
		s.logger.Warn("metrics ingest: dropped points past the per-site series cap",
			"site", siteID, "dropped", dropped, "limit", s.guard.limits.MaxSeriesPerSite)
	}

	return IngestResponse{OK: true, Points: count}, nil
}

func numberRow(name, kind, service string, dp NumberDataPoint, attrsJSON, monotonic, temporality string) metricPointRow {
	tsNs, _ := strconv.ParseInt(dp.TimeUnixNano, 10, 64)
	value := dp.AsDouble
	if value == 0 && dp.AsInt != "" {
		if iv, err := strconv.ParseInt(string(dp.AsInt), 10, 64); err == nil {
			value = float64(iv)
		}
	}
	return metricPointRow{
		name: name, kind: kind, service: service,
		attrsJSON:   attrsJSON,
		tsNs:        tsNs,
		value:       value,
		monotonic:   monotonic,
		temporality: temporality,
	}
}

func histogramRow(name, service string, dp HistogramDataPoint, attrsJSON, temporality string) metricPointRow {
	tsNs, _ := strconv.ParseInt(dp.TimeUnixNano, 10, 64)
	return metricPointRow{
		name: name, kind: "histogram", service: service,
		attrsJSON:   attrsJSON,
		tsNs:        tsNs,
		value:       0,
		histogram:   MarshalHistogram(dp),
		monotonic:   "false",
		temporality: temporality,
	}
}

// floatParam mirrors dbutil.IntParam for DOUBLE columns. Kept local to
// avoid touching the dbutil package while a parallel agent (W3.B) is
// working in the surrounding area — single-file change keeps the merge
// surface tiny. If FloatParam ever lands in dbutil, replace this helper
// with the dependency.
func floatParam(v float64) string {
	return strconv.FormatFloat(v, 'g', 17, 64)
}

const metricPointCols = 11

const metricPointColList = `site_id, tenant_id, metric_name, metric_kind, service_name,
	attributes, ts_ns, value, histogram, is_monotonic, aggregation_temporality`

// buildMetricPlaceholders returns "($1,...,$11),($12,...,$22),..." for rows*metricPointCols placeholders.
func buildMetricPlaceholders(rows int) string {
	var b strings.Builder
	b.Grow(rows * metricPointCols * 5)
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < metricPointCols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			n++
		}
		b.WriteByte(')')
	}
	return b.String()
}

func metricArgs(dst []any, siteID string, r *metricPointRow) []any {
	return append(dst,
		siteID, "default", r.name, r.kind, r.service,
		r.attrsJSON, dbutil.IntParam(r.tsNs), floatParam(r.value), r.histogram, r.monotonic, r.temporality,
	)
}

// insertMetricRows batch-inserts metric_points rows in fixed-size chunks,
// mirroring internal/ingest/buffer.go's insertBatch chunking (same chunk
// size, same placeholder-building shape) instead of one autocommit INSERT
// per data point. Returns the number of rows successfully inserted; on
// error that count reflects only the chunks that committed before the
// failure, matching the old loop's "insert until first failure" semantics.
func insertMetricRows(ctx context.Context, sql *nucleus.SQLModel, siteID string, rows []metricPointRow) (int, error) {
	const batchSize = 50
	total := 0
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		query := "INSERT INTO metric_points (" + metricPointColList + ") VALUES " + buildMetricPlaceholders(len(chunk))
		args := make([]any, 0, len(chunk)*metricPointCols)
		for i := range chunk {
			args = metricArgs(args, siteID, &chunk[i])
		}

		if _, err := sql.Exec(ctx, query, args...); err != nil {
			return total, fmt.Errorf("batch insert metric_points %d-%d: %w", start+1, end, err)
		}
		total = end
	}
	return total, nil
}
