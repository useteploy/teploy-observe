package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/tracing/detectors"
)

// bucketSizeMs is the rollup bucket width for service_stats / service_dependencies.
// 60s gives sub-minute resolution for the Services + ServiceMap UX without
// exploding the row count for low-traffic sites.
const bucketSizeMs int64 = 60_000

// IngestService handles OTLP trace ingestion and RED metrics computation.
type IngestService struct {
	db        *nucleus.Client
	logger    *slog.Logger
	detectors *detectors.Engine
}

func NewIngestService(db *nucleus.Client) *IngestService {
	return &IngestService{db: db, logger: slog.Default(), detectors: detectors.New(db)}
}

// WithLogger lets callers thread their own logger (used by main + seed so
// rollup-write failures show up under the same handler context).
func (s *IngestService) WithLogger(logger *slog.Logger) *IngestService {
	if logger == nil {
		return s
	}
	s.logger = logger
	if s.detectors != nil {
		s.detectors = s.detectors.WithLogger(logger)
	}
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

	// Flatten the OTLP envelope into a single slice — keeps the rollup
	// aggregator pure and unit-testable without an OTLP request struct.
	flat := flattenSpans(req)

	// Batch-insert spans in chunked multi-row statements instead of one
	// autocommit INSERT per span — an unbatched per-span loop was directly
	// contributing to Nucleus memory pressure (each INSERT is a separate
	// write that also invalidates the whole query-result cache) on every
	// trace export, which for a busy exporter can be hundreds of spans per
	// request. Mirrors the chunking approach in internal/ingest/buffer.go's
	// insertBatch (same chunk size, same placeholder-building shape).
	total, err := insertSpans(ctx, sql, siteID, flat)
	if err != nil {
		return IngestResponse{}, fmt.Errorf("insert span: %w", err)
	}

	services, deps := aggregateRollups(flat)
	detSpans := flatToDetectorSpans(req, flat)

	if syncRollup {
		s.writeRollups(ctx, siteID, services, deps)
		if s.detectors != nil {
			s.detectors.Persist(ctx, siteID, detSpans)
		}
	} else {
		// Detach from the request context so handler timeouts don't kill
		// a half-finished rollup write. A 30s ceiling keeps a stuck DB
		// from leaking goroutines.
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.writeRollups(bg, siteID, services, deps)
			if s.detectors != nil {
				s.detectors.Persist(bg, siteID, detSpans)
			}
		}()
	}

	return IngestResponse{OK: true, Spans: total}, nil
}

// flatToDetectorSpans projects the internal flatSpan slice into the
// detectors-package Span type. Attributes come from the original OTLP
// request because flatSpan only stores the marshalled JSON.
func flatToDetectorSpans(req ExportTraceRequest, flat []flatSpan) []detectors.Span {
	// Build a span_id -> attributes map from the OTLP envelope so we can
	// re-attach attributes without re-parsing the JSON we wrote to the DB.
	attrsByID := make(map[string]map[string]string)
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				attrsByID[sp.SpanID] = AttrsToMap(sp.Attributes)
			}
		}
	}

	out := make([]detectors.Span, 0, len(flat))
	for _, sp := range flat {
		out = append(out, detectors.Span{
			TraceID:       sp.TraceID,
			SpanID:        sp.SpanID,
			ParentSpanID:  sp.ParentSpanID,
			ServiceName:   sp.ServiceName,
			OperationName: sp.OperationName,
			SpanKind:      sp.SpanKind,
			StartMs:       sp.StartMs,
			EndMs:         sp.EndMs,
			DurationMs:    sp.DurationMs,
			StatusCode:    sp.StatusCode,
			Attributes:    attrsByID[sp.SpanID],
		})
	}
	return out
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

// spansCols is the number of bound parameters per row in the batch INSERT
// below (tenant_id is bound per-row here, rather than the literal used by
// the old single-row statement, so every row in a VALUES list has the same
// shape).
const spansCols = 16

const spansColList = `trace_id, span_id, parent_span_id, tenant_id, site_id,
	service_name, operation_name, span_kind,
	start_time, end_time, duration_ms,
	status_code, status_message,
	attributes, resource, events`

// buildSpanPlaceholders returns "($1,...,$16),($17,...,$32),..." for rows*spansCols placeholders.
func buildSpanPlaceholders(rows int) string {
	var b strings.Builder
	b.Grow(rows * spansCols * 5)
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < spansCols; c++ {
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

func spanArgs(dst []any, siteID string, sp *flatSpan) []any {
	return append(dst,
		sp.TraceID, sp.SpanID, sp.ParentSpanID, "default", siteID,
		sp.ServiceName, sp.OperationName, sp.SpanKind,
		sp.StartMs, sp.EndMs, sp.DurationMs,
		sp.StatusCode, sp.StatusMessage,
		nullableJSON(sp.AttributesJSON), nullableJSON(sp.ResourceJSON), nullableJSON(sp.EventsJSON),
	)
}

// insertSpans batch-inserts spans in fixed-size chunks (matching
// internal/ingest/buffer.go's insertBatch chunk size) instead of one
// autocommit INSERT per span. Returns the number of spans successfully
// inserted; on error that count reflects only the chunks that committed
// before the failure, matching the old loop's "insert until first failure"
// semantics.
func insertSpans(ctx context.Context, sql *nucleus.SQLModel, siteID string, flat []flatSpan) (int, error) {
	const batchSize = 50
	total := 0
	for start := 0; start < len(flat); start += batchSize {
		end := start + batchSize
		if end > len(flat) {
			end = len(flat)
		}
		chunk := flat[start:end]

		query := "INSERT INTO spans (" + spansColList + ") VALUES " + buildSpanPlaceholders(len(chunk))
		args := make([]any, 0, len(chunk)*spansCols)
		for i := range chunk {
			args = spanArgs(args, siteID, &chunk[i])
		}

		if _, err := sql.Exec(ctx, query, args...); err != nil {
			return total, fmt.Errorf("batch insert spans %d-%d: %w", start+1, end, err)
		}
		total = end
	}
	return total, nil
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

// nullableJSON maps an empty JSON string to a SQL NULL. The JSONB columns
// (attributes, resource, events) are nullable; handing Nucleus an empty
// string '' for a JSONB column makes the whole INSERT silently vanish (the
// row count says 1 but nothing persists, since '' is not valid JSON). Spans
// frequently have no attributes/events, so those must be written as NULL.
func nullableJSON(s string) any {
	if s == "" {
		return nil
	}
	return s
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
