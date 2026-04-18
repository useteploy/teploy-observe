package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// IngestService handles OTLP trace ingestion and RED metrics computation.
type IngestService struct {
	db *nucleus.Client
}

func NewIngestService(db *nucleus.Client) *IngestService {
	return &IngestService{db: db}
}

// IngestResponse is returned to the OTLP exporter.
type IngestResponse struct {
	OK    bool `json:"ok"`
	Spans int  `json:"spans"`
}

// Ingest processes an OTLP ExportTraceServiceRequest.
func (s *IngestService) Ingest(ctx context.Context, siteID string, req ExportTraceRequest) (IngestResponse, error) {
	sql := s.db.SQL()
	total := 0

	// Track RED metrics per (service, operation, hour bucket)
	type redKey struct{ Service, Op string; Bucket int64 }
	redMetrics := make(map[redKey]*redAgg)

	for _, rs := range req.ResourceSpans {
		serviceName := ExtractServiceName(rs.Resource.Attributes)
		resourceJSON := jsonOrEmpty(AttrsToMap(rs.Resource.Attributes))

		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				startNano, _ := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
				endNano, _ := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
				startMs := startNano / 1_000_000
				endMs := endNano / 1_000_000
				durationMs := endMs - startMs
				if durationMs < 0 {
					durationMs = 0
				}

				attrsJSON := jsonOrEmpty(AttrsToMap(span.Attributes))
				eventsJSON := jsonOrEmpty(span.Events)
				kind := SpanKind(span.Kind)
				status := StatusCode(span.Status.Code)

				_, err := sql.Exec(ctx,
					`INSERT INTO spans (
						trace_id, span_id, parent_span_id, tenant_id, site_id,
						service_name, operation_name, span_kind,
						start_time, end_time, duration_ms,
						status_code, status_message,
						attributes, resource, events
					) VALUES ($1,$2,$3,'default',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
					span.TraceID, span.SpanID, span.ParentSpanID, siteID,
					serviceName, span.Name, kind,
					startMs, endMs, durationMs,
					status, span.Status.Message,
					attrsJSON, resourceJSON, eventsJSON,
				)
				if err != nil {
					return IngestResponse{}, fmt.Errorf("insert span: %w", err)
				}
				total++

				// Accumulate RED metrics
				bucket := (startMs / 3600000) * 3600000
				rk := redKey{serviceName, span.Name, bucket}
				agg, ok := redMetrics[rk]
				if !ok {
					agg = &redAgg{}
					redMetrics[rk] = agg
				}
				agg.count++
				if status == "error" {
					agg.errors++
				}
				agg.durationSum += durationMs
				agg.durations = append(agg.durations, durationMs)
				if durationMs < agg.durationMin || agg.count == 1 {
					agg.durationMin = durationMs
				}
				if durationMs > agg.durationMax {
					agg.durationMax = durationMs
				}
			}
		}
	}

	// Write RED metrics to service_stats
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	for rk, agg := range redMetrics {
		sort.Slice(agg.durations, func(i, j int) bool { return agg.durations[i] < agg.durations[j] })
		p50 := percentile(agg.durations, 0.50)
		p95 := percentile(agg.durations, 0.95)
		p99 := percentile(agg.durations, 0.99)

		_, err := sql.Exec(ctx,
			`INSERT INTO service_stats (
				tenant_id, site_id, service_name, operation_name, ts_bucket,
				request_count, error_count, duration_sum,
				duration_min, duration_max, p50_ms, p95_ms, p99_ms, version
			) VALUES ('default',$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			siteID, rk.Service, rk.Op,
			dbutil.IntParam(rk.Bucket),
			strconv.FormatInt(agg.count, 10),
			strconv.FormatInt(agg.errors, 10),
			strconv.FormatInt(agg.durationSum, 10),
			strconv.FormatInt(agg.durationMin, 10),
			strconv.FormatInt(agg.durationMax, 10),
			strconv.FormatInt(p50, 10),
			strconv.FormatInt(p95, 10),
			strconv.FormatInt(p99, 10),
			now,
		)
		if err != nil {
			// Non-fatal — spans stored, RED metrics are best-effort
			_ = err
		}
	}

	return IngestResponse{OK: true, Spans: total}, nil
}

type redAgg struct {
	count       int64
	errors      int64
	durationSum int64
	durationMin int64
	durationMax int64
	durations   []int64
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func jsonOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(raw)
	if s == "null" || s == "[]" || s == "{}" {
		return ""
	}
	return s
}
