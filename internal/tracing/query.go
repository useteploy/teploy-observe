package tracing

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// servicesCacheTTL is how long a services rollup stays servable. The RED
// aggregate scans every span in the window, which Nucleus costs at seconds on
// a few hundred thousand rows no matter how narrow the time predicate is — so
// without this, every visit to the Traces page pays the full scan again. A few
// seconds of staleness on a services overview is a fair trade for a page that
// opens instantly; per-trace views are uncached and always live.
const servicesCacheTTL = 15 * time.Second

// servicesEntry is one cached rollup, remembered per site and window.
type servicesEntry struct {
	at     time.Time
	result []ServiceSummary
}

// QueryService provides trace and service query methods for the dashboard.
type QueryService struct {
	db *nucleus.Client

	// Keyed by site AND window bounds. Keying on the window *length* alone was
	// wrong: from/to are arbitrary caller-supplied timestamps, so a 24h
	// historical range primed the entry the rolling 24h live view then read,
	// and the Traces page served last January's rollup for a whole TTL. The
	// bounds are snapped to a TTL-wide bucket before they enter the key, so the
	// live view — whose bounds move every request — still shares a key with the
	// requests around it. Entries the lookup would reject are swept on write,
	// so the map stays bounded by the windows queried within one TTL.
	servicesMu    sync.Mutex
	servicesCache map[string]servicesEntry
}

func NewQueryService(db *nucleus.Client) *QueryService {
	return &QueryService{db: db, servicesCache: map[string]servicesEntry{}}
}

// servicesKey identifies a rollup by site and window bounds, each bound
// truncated to a TTL-wide bucket. Truncation is what keeps the cache useful:
// exact bounds would mint a fresh key on every rolling-window request and never
// hit. A bucket is the TTL wide and no wider, so the bounds a hit is served for
// are off by at most the staleness the TTL already permits, and two genuinely
// different windows can never land on the same key.
func servicesKey(siteID string, from, to time.Time) string {
	return fmt.Sprintf("%s|%d|%d", siteID,
		from.Truncate(servicesCacheTTL).UnixMilli(),
		to.Truncate(servicesCacheTTL).UnixMilli())
}

// cachedServices returns a fresh-enough rollup for the same site and window,
// if one was computed within the TTL.
func (q *QueryService) cachedServices(key string) ([]ServiceSummary, bool) {
	q.servicesMu.Lock()
	defer q.servicesMu.Unlock()
	e, ok := q.servicesCache[key]
	if !ok || time.Since(e.at) > servicesCacheTTL {
		return nil, false
	}
	return e.result, true
}

func (q *QueryService) storeServices(key string, result []ServiceSummary) {
	q.servicesMu.Lock()
	defer q.servicesMu.Unlock()
	for k, e := range q.servicesCache {
		if time.Since(e.at) > servicesCacheTTL {
			delete(q.servicesCache, k)
		}
	}
	q.servicesCache[key] = servicesEntry{at: time.Now(), result: result}
}

// ServiceSummary is a service with its RED metrics.
type ServiceSummary struct {
	ServiceName  string  `json:"service_name"`
	RequestCount int64   `json:"request_count"`
	ErrorCount   int64   `json:"error_count"`
	AvgDuration  int64   `json:"avg_duration_ms"`
	P50          int64   `json:"p50_ms"`
	P95          int64   `json:"p95_ms"`
	P99          int64   `json:"p99_ms"`
	ApdexScore   float64 `json:"apdex_score"`
}

// apdexThresholdMs is the default satisfied-threshold T for Apdex calculation.
// Frustrated > 4T; tolerated > T && <= 4T; satisfied <= T. SigNoz parity.
const apdexThresholdMs int64 = 500

// apdex computes the Apdex score (0..1) for a slice of durations.
//
//	satisfied = duration <= t
//	tolerated = t < duration <= 4t
//	frustrated = duration > 4t
//	score = (satisfied + tolerated/2) / total
//
// Returns 0 for an empty input.
func apdex(durations []int64, t int64) float64 {
	if len(durations) == 0 || t <= 0 {
		return 0
	}
	tol := 4 * t
	var satisfied, tolerated int64
	for _, d := range durations {
		if d <= t {
			satisfied++
		} else if d <= tol {
			tolerated++
		}
	}
	return (float64(satisfied) + float64(tolerated)/2.0) / float64(len(durations))
}

// ListServices returns services with aggregated RED metrics for a time range.
func (q *QueryService) ListServices(ctx context.Context, siteID string, from, to time.Time) ([]ServiceSummary, error) {
	key := servicesKey(siteID, from, to)
	if cached, ok := q.cachedServices(key); ok {
		return cached, nil
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Compute RED metrics from raw spans, NOT the service_stats rollup. The
	// rollup is written once per ingest batch with that batch's counts into a
	// ReplacingMergeTree keyed on (service, operation, bucket); read-time dedup
	// keeps only the newest version per key, so a bucket that spanned multiple
	// ingest batches reported only the LAST batch's counts — RED metrics
	// undercounted badly. Raw spans within retention are the source of truth,
	// and Nucleus computes the counts + percentiles directly.
	// Apdex is folded into this same aggregate rather than run as a second
	// query. It used to pull every root span's duration into Go to bucket them,
	// which meant a second scan that transferred one row per span — on a 436k-row
	// table that was the difference between a ~5s response and a ~20s one, and it
	// is what made the Traces page look frozen. The buckets are just counts, so
	// the database can produce them: satisfied <= T, tolerated in (T, 4T].
	type rawStat struct {
		ServiceName    string `db:"service_name"`
		RequestCount   int64  `db:"request_count"`
		ErrorCount     int64  `db:"error_count"`
		DurationSum    int64  `db:"duration_sum"`
		P50            int64  `db:"p50_ms"`
		P95            int64  `db:"p95_ms"`
		P99            int64  `db:"p99_ms"`
		RootCount      int64  `db:"root_count"`
		SatisfiedCount int64  `db:"satisfied_count"`
		ToleratedCount int64  `db:"tolerated_count"`
	}
	satisfiedMs := dbutil.IntParam(apdexThresholdMs)
	toleratedMs := dbutil.IntParam(4 * apdexThresholdMs)
	rows, err := nucleus.Query[rawStat](ctx, q.db.SQL(),
		`SELECT service_name,
			COUNT(*) AS request_count,
			SUM(CASE WHEN status_code = 'error' THEN 1 ELSE 0 END) AS error_count,
			SUM(duration_ms) AS duration_sum,
			CAST(percentile_cont(duration_ms, 0.50) AS BIGINT) AS p50_ms,
			CAST(percentile_cont(duration_ms, 0.95) AS BIGINT) AS p95_ms,
			CAST(percentile_cont(duration_ms, 0.99) AS BIGINT) AS p99_ms,
			SUM(CASE WHEN parent_span_id = '' THEN 1 ELSE 0 END) AS root_count,
			SUM(CASE WHEN parent_span_id = '' AND duration_ms <= $4 THEN 1 ELSE 0 END) AS satisfied_count,
			SUM(CASE WHEN parent_span_id = '' AND duration_ms > $4 AND duration_ms <= $5 THEN 1 ELSE 0 END) AS tolerated_count
		 FROM spans
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3
		 GROUP BY service_name`,
		siteID, fromMs, toMs, satisfiedMs, toleratedMs,
	)
	if err != nil {
		return nil, err
	}

	result := make([]ServiceSummary, 0, len(rows))
	for _, r := range rows {
		avg := int64(0)
		if r.RequestCount > 0 {
			avg = r.DurationSum / r.RequestCount
		}
		result = append(result, ServiceSummary{
			ServiceName: r.ServiceName, RequestCount: r.RequestCount, ErrorCount: r.ErrorCount,
			AvgDuration: avg, P50: r.P50, P95: r.P95, P99: r.P99,
			ApdexScore: apdexFromCounts(r.SatisfiedCount, r.ToleratedCount, r.RootCount),
		})
	}
	q.storeServices(key, result)
	return result, nil
}

// apdexFromCounts is apdex() over pre-bucketed counts — same formula, same
// zero-for-empty behaviour, for when the database did the bucketing.
func apdexFromCounts(satisfied, tolerated, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return (float64(satisfied) + float64(tolerated)/2.0) / float64(total)
}

// OperationSummary is an operation within a service with RED metrics.
type OperationSummary struct {
	OperationName string `json:"operation_name"`
	RequestCount  int64  `json:"request_count"`
	ErrorCount    int64  `json:"error_count"`
	AvgDuration   int64  `json:"avg_duration_ms"`
	P50           int64  `json:"p50_ms"`
	P95           int64  `json:"p95_ms"`
	P99           int64  `json:"p99_ms"`
}

// ListOperations returns operations for a service with RED metrics.
func (q *QueryService) ListOperations(ctx context.Context, siteID, service string, from, to time.Time) ([]OperationSummary, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// RED per operation from raw spans (see ListServices for why the
	// service_stats rollup is not the source of truth).
	type rawStat struct {
		OpName      string `db:"operation_name"`
		ReqCount    int64  `db:"request_count"`
		ErrCount    int64  `db:"error_count"`
		DurationSum int64  `db:"duration_sum"`
		P50         int64  `db:"p50_ms"`
		P95         int64  `db:"p95_ms"`
		P99         int64  `db:"p99_ms"`
	}
	rows, err := nucleus.Query[rawStat](ctx, q.db.SQL(),
		`SELECT operation_name,
			COUNT(*) AS request_count,
			SUM(CASE WHEN status_code = 'error' THEN 1 ELSE 0 END) AS error_count,
			SUM(duration_ms) AS duration_sum,
			CAST(percentile_cont(duration_ms, 0.50) AS BIGINT) AS p50_ms,
			CAST(percentile_cont(duration_ms, 0.95) AS BIGINT) AS p95_ms,
			CAST(percentile_cont(duration_ms, 0.99) AS BIGINT) AS p99_ms
		 FROM spans
		 WHERE site_id = $1 AND service_name = $2 AND start_time >= $3 AND start_time < $4
		 GROUP BY operation_name`,
		siteID, service, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}

	result := make([]OperationSummary, 0, len(rows))
	for _, r := range rows {
		avg := int64(0)
		if r.ReqCount > 0 {
			avg = r.DurationSum / r.ReqCount
		}
		result = append(result, OperationSummary{
			OperationName: r.OpName, RequestCount: r.ReqCount, ErrorCount: r.ErrCount,
			AvgDuration: avg, P50: r.P50, P95: r.P95, P99: r.P99,
		})
	}
	return result, nil
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// TraceSummary is a trace listed in search results.
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	RootService string    `json:"root_service" db:"root_service"`
	RootOp      string    `json:"root_operation" db:"root_op"`
	StartTime   time.Time `json:"start_time"`
	DurationMs  int64     `json:"duration_ms"`
	SpanCount   int64     `json:"span_count"`
	StatusCode  string    `json:"status_code"`
}

// SearchTraces finds traces matching filters.
func (q *QueryService) SearchTraces(ctx context.Context, siteID string, from, to time.Time, service, operation, status string, minDuration, maxDuration int64, limit, offset int) ([]TraceSummary, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	// start_time is stored as TEXT (digit-string) and Nucleus doesn't coerce it
	// to BIGINT for the comparison — both sides need an explicit cast.
	where := "site_id = $1 AND start_time >= $2 AND start_time < $3"
	params := []any{siteID, fromMs, toMs}
	// Site+time-only filter for the span-count aggregate, so a status/service
	// filter on the root-span search doesn't undercount a trace's total spans.
	baseWhere := where
	baseParams := []any{siteID, fromMs, toMs}
	idx := 4

	if service != "" {
		where += fmt.Sprintf(" AND service_name = $%d", idx)
		params = append(params, service)
		idx++
	}
	if operation != "" {
		where += fmt.Sprintf(" AND operation_name = $%d", idx)
		params = append(params, operation)
		idx++
	}
	if status != "" {
		where += fmt.Sprintf(" AND status_code = $%d", idx)
		params = append(params, status)
		idx++
	}
	if minDuration > 0 {
		where += fmt.Sprintf(" AND CAST(duration_ms AS BIGINT) >= %d", minDuration)
	}
	if maxDuration > 0 {
		where += fmt.Sprintf(" AND CAST(duration_ms AS BIGINT) <= %d", maxDuration)
	}

	q2 := fmt.Sprintf(`SELECT trace_id,
			service_name AS root_service,
			operation_name AS root_op,
			CAST(start_time AS TEXT) AS start_time,
			CAST(duration_ms AS TEXT) AS duration_ms,
			'0' AS span_count,
			status_code
		 FROM spans
		 WHERE %s AND parent_span_id = ''
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, where, limit, offset)

	summaries, err := nucleus.Query[TraceSummary](ctx, q.db.SQL(), q2, params...)
	if err != nil || len(summaries) == 0 {
		return summaries, err
	}

	// span_count was previously hardcoded to 1. Compute the real per-trace span
	// count with a second aggregate and fold it in by trace_id (Nucleus rejects
	// some single-query aggregate shapes, hence the separate pass).
	type spanCountRow struct {
		TraceID string `db:"trace_id"`
		N       int64  `db:"n"`
	}
	counts, cErr := nucleus.Query[spanCountRow](ctx, q.db.SQL(),
		fmt.Sprintf(`SELECT trace_id, COUNT(*) AS n FROM spans WHERE %s GROUP BY trace_id`, baseWhere),
		baseParams...)
	if cErr == nil {
		byTrace := make(map[string]int64, len(counts))
		for _, c := range counts {
			byTrace[c.TraceID] = c.N
		}
		for i := range summaries {
			summaries[i].SpanCount = byTrace[summaries[i].TraceID]
		}
	}
	return summaries, nil
}

// Span is a stored span for the waterfall view.
type Span struct {
	TraceID       string    `json:"trace_id"`
	SpanID        string    `json:"span_id"`
	ParentSpanID  string    `json:"parent_span_id"`
	ServiceName   string    `json:"service_name"`
	OperationName string    `json:"operation_name"`
	SpanKind      string    `json:"span_kind"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	DurationMs    int64     `json:"duration_ms"`
	StatusCode    string    `json:"status_code"`
	StatusMessage string    `json:"status_message"`
	Attributes    string    `json:"attributes"`
	Resource      string    `json:"resource"`
	Events        string    `json:"events"`
}

// GetTrace returns all spans for a trace, ordered for waterfall rendering.
func (q *QueryService) GetTrace(ctx context.Context, traceID, siteID string) ([]Span, error) {
	return nucleus.Query[Span](ctx, q.db.SQL(),
		`SELECT trace_id, span_id, parent_span_id, service_name, operation_name,
			span_kind,
			CAST(start_time AS TEXT) AS start_time,
			CAST(end_time AS TEXT) AS end_time,
			CAST(duration_ms AS TEXT) AS duration_ms,
			status_code, status_message,
			COALESCE(attributes, '') AS attributes,
			COALESCE(resource, '') AS resource,
			COALESCE(events, '') AS events
		 FROM spans
		 WHERE trace_id = $1 AND site_id = $2
		 ORDER BY start_time ASC`,
		traceID, siteID,
	)
}

// Dependency represents a service-to-service call edge.
type Dependency struct {
	SrcService  string `json:"src_service"`
	DstService  string `json:"dst_service"`
	CallCount   int64  `json:"call_count"`
	ErrorCount  int64  `json:"error_count"`
	AvgDuration int64  `json:"avg_duration_ms" db:"avg_duration"`
}

// ServiceDependencies returns the dependency graph for all services.
//
// Aggregation is done in Go (not SQL) because Nucleus rejects nested
// aggregates like `CASE WHEN SUM(x) > 0 THEN SUM(y*z)/SUM(z) END` with
// "aggregate function SUM outside of aggregate context". Pull the
// per-bucket rows and fold them in-process.
func (q *QueryService) ServiceDependencies(ctx context.Context, siteID string, from, to time.Time) ([]Dependency, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Dependency edges from raw spans (parent span's service -> child span's
	// service) via self-join, NOT the service_dependencies rollup — that is
	// written per ingest batch and deduped to the newest version per edge, so
	// call counts undercounted (same root cause as ListServices RED metrics).
	type rawDep struct {
		Src         string `db:"src_service"`
		Dst         string `db:"dst_service"`
		CallCount   int64  `db:"call_count"`
		ErrorCount  int64  `db:"error_count"`
		DurationSum int64  `db:"duration_sum"`
	}
	rows, err := nucleus.Query[rawDep](ctx, q.db.SQL(),
		`SELECT p.service_name AS src_service, c.service_name AS dst_service,
			COUNT(*) AS call_count,
			SUM(CASE WHEN c.status_code = 'error' THEN 1 ELSE 0 END) AS error_count,
			SUM(c.duration_ms) AS duration_sum
		 FROM spans c JOIN spans p ON c.parent_span_id = p.span_id AND c.site_id = p.site_id AND c.trace_id = p.trace_id
		 WHERE c.site_id = $1 AND c.start_time >= $2 AND c.start_time < $3
		   AND p.service_name <> c.service_name AND p.service_name <> ''
		 GROUP BY p.service_name, c.service_name`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}

	out := make([]Dependency, 0, len(rows))
	for _, r := range rows {
		avg := int64(0)
		if r.CallCount > 0 {
			avg = r.DurationSum / r.CallCount
		}
		out = append(out, Dependency{
			SrcService:  r.Src,
			DstService:  r.Dst,
			CallCount:   r.CallCount,
			ErrorCount:  r.ErrorCount,
			AvgDuration: avg,
		})
	}
	// Sort by CallCount desc to keep parity with the previous SQL ORDER BY.
	sort.Slice(out, func(i, j int) bool { return out[i].CallCount > out[j].CallCount })
	return out, nil
}

// TraceErrorHit is an error event correlated with a trace.
type TraceErrorHit struct {
	ErrorID    string    `json:"error_id"`
	ErrorType  string    `json:"error_type"`
	ErrorValue string    `json:"error_value"`
	Timestamp  time.Time `json:"timestamp"`
	IssueID    string    `json:"issue_id"`
}

// PerformanceIssue is a row from performance_issues for the UI.
type PerformanceIssue struct {
	IssueID      string `json:"issue_id" db:"issue_id"`
	TraceID      string `json:"trace_id" db:"trace_id"`
	DetectorName string `json:"detector_name" db:"detector_name"`
	Fingerprint  string `json:"fingerprint" db:"fingerprint"`
	Title        string `json:"title" db:"title"`
	Description  string `json:"description" db:"description"`
	Severity     string `json:"severity" db:"severity"`
	Count        int64  `json:"count" db:"count"`
	FirstSeen    int64  `json:"first_seen" db:"first_seen"`
	LastSeen     int64  `json:"last_seen" db:"last_seen"`
}

// ListPerformanceIssues returns detector-emitted issues that touched the
// given window, grouped by fingerprint (most-recent fire wins thanks to the
// replacing-mergetree on last_seen).
func (q *QueryService) ListPerformanceIssues(ctx context.Context, siteID string, fromMs, toMs int64) ([]PerformanceIssue, error) {
	type row struct {
		IssueID      string `db:"issue_id"`
		TraceID      string `db:"trace_id"`
		DetectorName string `db:"detector_name"`
		Fingerprint  string `db:"fingerprint"`
		Title        string `db:"title"`
		Description  string `db:"description"`
		Severity     string `db:"severity"`
		Count        string `db:"count"`
		FirstSeen    string `db:"first_seen"`
		LastSeen     string `db:"last_seen"`
	}
	rows, err := nucleus.Query[row](ctx, q.db.SQL(),
		`SELECT issue_id, trace_id, detector_name, fingerprint, title, description, severity,
			CAST(count AS TEXT) AS count,
			CAST(first_seen AS TEXT) AS first_seen,
			CAST(last_seen AS TEXT) AS last_seen
		 FROM performance_issues
		 WHERE site_id = $1
		   AND last_seen >= $2
		   AND first_seen <= $3`,
		siteID, dbutil.IntParam(fromMs), dbutil.IntParam(toMs),
	)
	if err != nil {
		return nil, err
	}

	// Collapse to the latest row per fingerprint in Go — the
	// replacing-mergetree dedupes at the storage layer but we still need
	// to fold post-CAST in case multiple bucket fires landed in this
	// window.
	type acc struct {
		row   row
		count int64
		first int64
		last  int64
	}
	by := make(map[string]*acc)
	for _, r := range rows {
		fp := r.Fingerprint
		c := parseInt(r.Count)
		first := parseInt(r.FirstSeen)
		last := parseInt(r.LastSeen)
		a, ok := by[fp]
		if !ok {
			by[fp] = &acc{row: r, count: c, first: first, last: last}
			continue
		}
		a.count += c
		if first < a.first || a.first == 0 {
			a.first = first
		}
		if last > a.last {
			a.last = last
			a.row = r // newest row wins for title/desc/severity
		}
	}

	out := make([]PerformanceIssue, 0, len(by))
	for _, a := range by {
		out = append(out, PerformanceIssue{
			IssueID:      a.row.IssueID,
			TraceID:      a.row.TraceID,
			DetectorName: a.row.DetectorName,
			Fingerprint:  a.row.Fingerprint,
			Title:        a.row.Title,
			Description:  a.row.Description,
			Severity:     a.row.Severity,
			Count:        a.count,
			FirstSeen:    a.first,
			LastSeen:     a.last,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out, nil
}

// TraceErrors returns error events correlated with a trace. Errors that
// carry a trace_id (SDKs capturing inside a traced operation) match exactly;
// errors without trace context (browser errors, older SDKs) fall back to
// the trace's timestamp window. Errors tagged with a DIFFERENT trace are
// excluded from the window fallback, so overlapping traces no longer
// cross-contaminate each other's error lists once SDKs send trace_id.
func (q *QueryService) TraceErrors(ctx context.Context, traceID, siteID string) ([]TraceErrorHit, error) {
	type bounds struct {
		MinT string `db:"min_t"`
		MaxT string `db:"max_t"`
	}
	b, err := nucleus.Query[bounds](ctx, q.db.SQL(),
		`SELECT CAST(MIN(CAST(start_time AS BIGINT)) AS TEXT) AS min_t,
			CAST(MAX(CAST(end_time AS BIGINT)) AS TEXT) AS max_t
		 FROM spans WHERE trace_id = $1 AND site_id = $2`,
		traceID, siteID,
	)
	if err != nil || len(b) == 0 || b[0].MinT == "" || b[0].MinT == "0" {
		return nil, err
	}

	return nucleus.Query[TraceErrorHit](ctx, q.db.SQL(),
		`SELECT error_id, error_type, error_value,
			CAST(timestamp AS TEXT) AS timestamp,
			issue_id
		 FROM error_events
		 WHERE site_id = $1
		   AND (trace_id = $4
		        OR (trace_id = '' AND timestamp >= $2 AND timestamp <= $3))
		 ORDER BY timestamp ASC`,
		siteID, b[0].MinT, b[0].MaxT, traceID,
	)
}
