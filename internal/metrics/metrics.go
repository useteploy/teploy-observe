// Package metrics implements OTLP metrics ingest + query for Observe.
//
// Phase 1 (W3.A) ships the schema, the HTTP ingest endpoint, a small list /
// query API for the dashboard, and the SDK helpers that emit OTLP JSON.
// Phase 2 will layer rich UI (heatmap, dashboard widget) and PromQL-style
// rate() handling on top.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Service writes OTLP metric points and answers list / query requests.
// One Service shared across HTTP handlers — it has no per-request state.
type Service struct {
	db     *nucleus.Client
	logger *slog.Logger
}

func NewService(db *nucleus.Client) *Service {
	return &Service{db: db, logger: slog.Default()}
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

// IngestResponse is returned to the OTLP exporter.
type IngestResponse struct {
	OK     bool `json:"ok"`
	Points int  `json:"points"`
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
func (s *Service) Ingest(ctx context.Context, siteID string, req ExportMetricsRequest) (IngestResponse, error) {
	if siteID == "" {
		return IngestResponse{}, fmt.Errorf("metrics: site_id required")
	}
	sql := s.db.SQL()
	count := 0

	for _, rm := range req.ResourceMetrics {
		serviceName := ExtractServiceName(rm.Resource.Attributes)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch {
				case m.Gauge != nil:
					for _, dp := range m.Gauge.DataPoints {
						if err := s.insertNumber(ctx, sql, siteID, m.Name, "gauge", serviceName, dp, "false", "cumulative"); err != nil {
							return IngestResponse{}, fmt.Errorf("insert gauge %s: %w", m.Name, err)
						}
						count++
					}
				case m.Sum != nil:
					monotonic := "false"
					if m.Sum.IsMonotonic {
						monotonic = "true"
					}
					temp := AggregationTemporality(m.Sum.AggregationTemporality)
					for _, dp := range m.Sum.DataPoints {
						if err := s.insertNumber(ctx, sql, siteID, m.Name, "sum", serviceName, dp, monotonic, temp); err != nil {
							return IngestResponse{}, fmt.Errorf("insert sum %s: %w", m.Name, err)
						}
						count++
					}
				case m.Histogram != nil:
					temp := AggregationTemporality(m.Histogram.AggregationTemporality)
					for _, dp := range m.Histogram.DataPoints {
						if err := s.insertHistogram(ctx, sql, siteID, m.Name, serviceName, dp, temp); err != nil {
							return IngestResponse{}, fmt.Errorf("insert histogram %s: %w", m.Name, err)
						}
						count++
					}
				}
			}
		}
	}

	return IngestResponse{OK: true, Points: count}, nil
}

func (s *Service) insertNumber(ctx context.Context, sql *nucleus.SQLModel, siteID, name, kind, service string, dp NumberDataPoint, monotonic, temporality string) error {
	tsNs, _ := strconv.ParseInt(dp.TimeUnixNano, 10, 64)
	value := dp.AsDouble
	if value == 0 && dp.AsInt != "" {
		if iv, err := strconv.ParseInt(string(dp.AsInt), 10, 64); err == nil {
			value = float64(iv)
		}
	}
	attrsJSON := MarshalAttrs(AttrsToMap(dp.Attributes))

	_, err := sql.Exec(ctx,
		`INSERT INTO metric_points (
			site_id, tenant_id, metric_name, metric_kind, service_name,
			attributes, ts_ns, value, histogram, is_monotonic, aggregation_temporality
		) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,'',$8,$9)`,
		siteID, name, kind, service,
		attrsJSON,
		dbutil.IntParam(tsNs),
		floatParam(value),
		monotonic, temporality,
	)
	return err
}

// floatParam mirrors dbutil.IntParam for DOUBLE columns. Kept local to
// avoid touching the dbutil package while a parallel agent (W3.B) is
// working in the surrounding area — single-file change keeps the merge
// surface tiny. If FloatParam ever lands in dbutil, replace this helper
// with the dependency.
func floatParam(v float64) string {
	return strconv.FormatFloat(v, 'g', 17, 64)
}

func (s *Service) insertHistogram(ctx context.Context, sql *nucleus.SQLModel, siteID, name, service string, dp HistogramDataPoint, temporality string) error {
	tsNs, _ := strconv.ParseInt(dp.TimeUnixNano, 10, 64)
	attrsJSON := MarshalAttrs(AttrsToMap(dp.Attributes))
	histJSON := MarshalHistogram(dp)

	_, err := sql.Exec(ctx,
		`INSERT INTO metric_points (
			site_id, tenant_id, metric_name, metric_kind, service_name,
			attributes, ts_ns, value, histogram, is_monotonic, aggregation_temporality
		) VALUES ($1,'default',$2,'histogram',$3,$4,$5,0,$6,'false',$7)`,
		siteID, name, service,
		attrsJSON,
		dbutil.IntParam(tsNs),
		histJSON,
		temporality,
	)
	return err
}
