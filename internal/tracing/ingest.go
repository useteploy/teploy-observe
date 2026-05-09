package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// bucketSizeMs is the rollup bucket width for service_stats / service_dependencies.
// 60s gives sub-minute resolution for the Services + ServiceMap UX without
// exploding the row count for low-traffic sites.
const bucketSizeMs int64 = 60_000

// IngestService handles OTLP trace ingestion and RED metrics computation.
type IngestService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

func NewIngestService(db *nucleus.Client) *IngestService {
	return &IngestService{db: db, logger: slog.Default()}
}

// WithLogger lets callers thread their own logger (used by main + seed so
// rollup-write failures show up under the same handler context).
func (s *IngestService) WithLogger(logger *slog.Logger) *IngestService {
	if logger == nil {
		return s
	}
	s.logger = logger
	return s
}

// IngestResponse is returned to the OTLP exporter.
type IngestResponse struct {
	OK    bool `json:"ok"`
	Spans int  `json:"spans"`
}

// Ingest processes an OTLP ExportTraceServiceRequest. Spans are written
// synchronously; the rollup tables (service_stats, service_dependencies)
// are written in the background so a slow rollup never stalls ingest.
func (s *IngestService) Ingest(ctx context.Context, siteID string, req ExportTraceRequest) (IngestResponse, error) {
	return s.ingest(ctx, siteID, req, false)
}

// IngestSync is identical to Ingest but blocks until the rollup writes
// finish. Used by the seed path so a fresh dev stack shows data the
// moment the HTTP server is ready.
func (s *IngestService) IngestSync(ctx context.Context, siteID string, req ExportTraceRequest) (IngestResponse, error) {
	return s.ingest(ctx, siteID, req, true)
}

func (s *IngestService) ingest(ctx context.Context, siteID string, req ExportTraceRequest, syncRollup bool) (IngestResponse, error) {
	sql := s.db.SQL()
	total := 0

	// Flatten the OTLP envelope into a single slice — keeps the rollup
	// aggregator pure and unit-testable without an OTLP request struct.
	flat := flattenSpans(req)

	for _, sp := range flat {
		_, err := sql.Exec(ctx,
			`INSERT INTO spans (
				trace_id, span_id, parent_span_id, tenant_id, site_id,
				service_name, operation_name, span_kind,
				start_time, end_time, duration_ms,
				status_code, status_message,
				attributes, resource, events
			) VALUES ($1,$2,$3,'default',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			sp.TraceID, sp.SpanID, sp.ParentSpanID, siteID,
			sp.ServiceName, sp.OperationName, sp.SpanKind,
			sp.StartMs, sp.EndMs, sp.DurationMs,
			sp.StatusCode, sp.StatusMessage,
			sp.AttributesJSON, sp.ResourceJSON, sp.EventsJSON,
		)
		if err != nil {
			return IngestResponse{}, fmt.Errorf("insert span: %w", err)
		}
		total++
	}

	services, deps := aggregateRollups(flat)

	if syncRollup {
		s.writeRollups(ctx, siteID, services, deps)
	} else {
		// Detach from the request context so handler timeouts don't kill
		// a half-finished rollup write. A 30s ceiling keeps a stuck DB
		// from leaking goroutines.
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.writeRollups(bg, siteID, services, deps)
		}()
	}

	return IngestResponse{OK: true, Spans: total}, nil
}

// flatSpan is the per-span view used by both the spans INSERT loop and
// the pure rollup aggregator. ServiceName comes from the resource attrs
// or, for child spans in the seed, the per-span override attribute.
type flatSpan struct {
	TraceID        string
	SpanID         string
	ParentSpanID   string
	ServiceName    string
	OperationName  string
	SpanKind       string
	StartMs        int64
	EndMs          int64
	DurationMs     int64
	StatusCode     string
	StatusMessage  string
	AttributesJSON string
	ResourceJSON   string
	EventsJSON     string
}

func flattenSpans(req ExportTraceRequest) []flatSpan {
	var out []flatSpan
	for _, rs := range req.ResourceSpans {
		resourceService := ExtractServiceName(rs.Resource.Attributes)
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

				// A per-span service.name override (used by the seeder to
				// produce cross-service dependencies inside a single OTLP
				// envelope) takes precedence over the resource's
				// service.name.
				svcName := resourceService
				if override := ExtractServiceName(span.Attributes); override != "unknown" && override != "" {
					svcName = override
				}

				out = append(out, flatSpan{
					TraceID:        span.TraceID,
					SpanID:         span.SpanID,
					ParentSpanID:   span.ParentSpanID,
					ServiceName:    svcName,
					OperationName:  span.Name,
					SpanKind:       SpanKind(span.Kind),
					StartMs:        startMs,
					EndMs:          endMs,
					DurationMs:     durationMs,
					StatusCode:     StatusCode(span.Status.Code),
					StatusMessage:  span.Status.Message,
					AttributesJSON: jsonOrEmpty(AttrsToMap(span.Attributes)),
					ResourceJSON:   resourceJSON,
					EventsJSON:     jsonOrEmpty(span.Events),
				})
			}
		}
	}
	return out
}

// ServiceBucket keys a service_stats row.
type ServiceBucket struct {
	Service   string
	Operation string
	Bucket    int64
}

// ServiceEdge keys a service_dependencies row (parent service -> child service).
type ServiceEdge struct {
	Src    string
	Dst    string
	Bucket int64
}

// Aggregate accumulates RED metrics for a (service, operation, bucket) triple.
type Aggregate struct {
	Count       int64
	Errors      int64
	DurationSum int64
	DurationMin int64
	DurationMax int64
	Durations   []int64
}

// EdgeAgg accumulates per-edge call + error counts and a duration sum
// (used to compute avg_duration on flush).
type EdgeAgg struct {
	CallCount   int64
	ErrorCount  int64
	DurationSum int64
}

// aggregateRollups groups a batch of spans into per-bucket service stats
// and per-edge service dependencies. Pure and side-effect-free so it
// can be unit-tested without a Nucleus connection.
func aggregateRollups(spans []flatSpan) (map[ServiceBucket]*Aggregate, map[ServiceEdge]*EdgeAgg) {
	services := make(map[ServiceBucket]*Aggregate)

	// span_id -> service_name lookup so child spans can resolve their
	// parent's service for the dependency edge. Built only over the
	// current batch — cross-batch parent linkage is intentionally
	// out of scope (would require querying spans on every ingest).
	idToService := make(map[string]string, len(spans))
	for _, sp := range spans {
		if sp.SpanID != "" {
			idToService[sp.SpanID] = sp.ServiceName
		}
	}

	deps := make(map[ServiceEdge]*EdgeAgg)

	for _, sp := range spans {
		bucket := (sp.StartMs / bucketSizeMs) * bucketSizeMs

		key := ServiceBucket{Service: sp.ServiceName, Operation: sp.OperationName, Bucket: bucket}
		agg, ok := services[key]
		if !ok {
			agg = &Aggregate{}
			services[key] = agg
		}
		agg.Count++
		if sp.StatusCode == "error" {
			agg.Errors++
		}
		agg.DurationSum += sp.DurationMs
		agg.Durations = append(agg.Durations, sp.DurationMs)
		if sp.DurationMs < agg.DurationMin || agg.Count == 1 {
			agg.DurationMin = sp.DurationMs
		}
		if sp.DurationMs > agg.DurationMax {
			agg.DurationMax = sp.DurationMs
		}

		// Build the dependency edge: parent service -> this span's service.
		if sp.ParentSpanID == "" {
			continue
		}
		parentSvc, ok := idToService[sp.ParentSpanID]
		if !ok || parentSvc == "" || parentSvc == sp.ServiceName {
			continue
		}
		edgeKey := ServiceEdge{Src: parentSvc, Dst: sp.ServiceName, Bucket: bucket}
		edge, ok := deps[edgeKey]
		if !ok {
			edge = &EdgeAgg{}
			deps[edgeKey] = edge
		}
		edge.CallCount++
		if sp.StatusCode == "error" {
			edge.ErrorCount++
		}
		edge.DurationSum += sp.DurationMs
	}

	return services, deps
}

// writeRollups flushes the aggregated services + dependency edges to the
// rollup tables. Each insert is best-effort — a failure logs and moves
// on so a single bad row never aborts the rollup batch.
func (s *IngestService) writeRollups(ctx context.Context, siteID string, services map[ServiceBucket]*Aggregate, deps map[ServiceEdge]*EdgeAgg) {
	sql := s.db.SQL()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	for key, agg := range services {
		sort.Slice(agg.Durations, func(i, j int) bool { return agg.Durations[i] < agg.Durations[j] })
		p50 := percentile(agg.Durations, 0.50)
		p95 := percentile(agg.Durations, 0.95)
		p99 := percentile(agg.Durations, 0.99)

		_, err := sql.Exec(ctx,
			`INSERT INTO service_stats (
				tenant_id, site_id, service_name, operation_name, ts_bucket,
				request_count, error_count, duration_sum,
				duration_min, duration_max, p50_ms, p95_ms, p99_ms, version
			) VALUES ('default',$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			siteID, key.Service, key.Operation,
			dbutil.IntParam(key.Bucket),
			strconv.FormatInt(agg.Count, 10),
			strconv.FormatInt(agg.Errors, 10),
			strconv.FormatInt(agg.DurationSum, 10),
			strconv.FormatInt(agg.DurationMin, 10),
			strconv.FormatInt(agg.DurationMax, 10),
			strconv.FormatInt(p50, 10),
			strconv.FormatInt(p95, 10),
			strconv.FormatInt(p99, 10),
			now,
		)
		if err != nil {
			s.logger.Warn("rollup: service_stats insert failed",
				"service", key.Service, "operation", key.Operation,
				"bucket", key.Bucket, "err", err)
		}
	}

	for key, edge := range deps {
		avg := int64(0)
		if edge.CallCount > 0 {
			avg = edge.DurationSum / edge.CallCount
		}
		_, err := sql.Exec(ctx,
			`INSERT INTO service_dependencies (
				tenant_id, site_id, src_service, dst_service,
				call_count, error_count, avg_duration, ts_bucket, version
			) VALUES ('default',$1,$2,$3,$4,$5,$6,$7,$8)`,
			siteID, key.Src, key.Dst,
			strconv.FormatInt(edge.CallCount, 10),
			strconv.FormatInt(edge.ErrorCount, 10),
			strconv.FormatInt(avg, 10),
			dbutil.IntParam(key.Bucket),
			now,
		)
		if err != nil {
			s.logger.Warn("rollup: service_dependencies insert failed",
				"src", key.Src, "dst", key.Dst,
				"bucket", key.Bucket, "err", err)
		}
	}
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
