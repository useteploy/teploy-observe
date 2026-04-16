package tracing

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

// QueryService provides trace and service query methods for the dashboard.
type QueryService struct {
	db *nucleus.Client
}

func NewQueryService(db *nucleus.Client) *QueryService {
	return &QueryService{db: db}
}

// ServiceSummary is a service with its RED metrics.
type ServiceSummary struct {
	ServiceName  string `json:"service_name"`
	RequestCount int64  `json:"request_count"`
	ErrorCount   int64  `json:"error_count"`
	AvgDuration  int64  `json:"avg_duration_ms"`
	P50          int64  `json:"p50_ms"`
	P95          int64  `json:"p95_ms"`
	P99          int64  `json:"p99_ms"`
}

// ListServices returns services with aggregated RED metrics for a time range.
func (q *QueryService) ListServices(ctx context.Context, siteID string, from, to time.Time) ([]ServiceSummary, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// service_stats stores per-bucket rows. Aggregate in Go because
	// Nucleus doesn't support CAST inside aggregate functions.
	type rawStat struct {
		ServiceName  string `db:"service_name"`
		RequestCount string `db:"request_count"`
		ErrorCount   string `db:"error_count"`
		DurationSum  string `db:"duration_sum"`
		P50          string `db:"p50_ms"`
		P95          string `db:"p95_ms"`
		P99          string `db:"p99_ms"`
	}
	rows, err := nucleus.Query[rawStat](ctx, q.db.SQL(),
		`SELECT service_name, request_count, error_count, duration_sum, p50_ms, p95_ms, p99_ms
		 FROM service_stats
		 WHERE site_id = $1 AND ts_bucket >= $2 AND ts_bucket < $3`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}

	type agg struct {
		reqs, errs, durSum, p50, p95, p99 int64
	}
	m := make(map[string]*agg)
	for _, r := range rows {
		a, ok := m[r.ServiceName]
		if !ok {
			a = &agg{}
			m[r.ServiceName] = a
		}
		a.reqs += parseInt(r.RequestCount)
		a.errs += parseInt(r.ErrorCount)
		a.durSum += parseInt(r.DurationSum)
		if p := parseInt(r.P50); p > a.p50 {
			a.p50 = p
		}
		if p := parseInt(r.P95); p > a.p95 {
			a.p95 = p
		}
		if p := parseInt(r.P99); p > a.p99 {
			a.p99 = p
		}
	}

	result := make([]ServiceSummary, 0, len(m))
	for name, a := range m {
		avg := int64(0)
		if a.reqs > 0 {
			avg = a.durSum / a.reqs
		}
		result = append(result, ServiceSummary{
			ServiceName: name, RequestCount: a.reqs, ErrorCount: a.errs,
			AvgDuration: avg, P50: a.p50, P95: a.p95, P99: a.p99,
		})
	}
	return result, nil
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

	type rawStat struct {
		OpName      string `db:"operation_name"`
		ReqCount    string `db:"request_count"`
		ErrCount    string `db:"error_count"`
		DurationSum string `db:"duration_sum"`
		P50         string `db:"p50_ms"`
		P95         string `db:"p95_ms"`
		P99         string `db:"p99_ms"`
	}
	rows, err := nucleus.Query[rawStat](ctx, q.db.SQL(),
		`SELECT operation_name, request_count, error_count, duration_sum, p50_ms, p95_ms, p99_ms
		 FROM service_stats
		 WHERE site_id = $1 AND service_name = $2 AND ts_bucket >= $3 AND ts_bucket < $4`,
		siteID, service, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}

	type agg struct {
		reqs, errs, durSum, p50, p95, p99 int64
	}
	m := make(map[string]*agg)
	for _, r := range rows {
		a, ok := m[r.OpName]
		if !ok {
			a = &agg{}
			m[r.OpName] = a
		}
		a.reqs += parseInt(r.ReqCount)
		a.errs += parseInt(r.ErrCount)
		a.durSum += parseInt(r.DurationSum)
		if p := parseInt(r.P50); p > a.p50 {
			a.p50 = p
		}
		if p := parseInt(r.P95); p > a.p95 {
			a.p95 = p
		}
		if p := parseInt(r.P99); p > a.p99 {
			a.p99 = p
		}
	}

	result := make([]OperationSummary, 0, len(m))
	for name, a := range m {
		avg := int64(0)
		if a.reqs > 0 {
			avg = a.durSum / a.reqs
		}
		result = append(result, OperationSummary{
			OperationName: name, RequestCount: a.reqs, ErrorCount: a.errs,
			AvgDuration: avg, P50: a.p50, P95: a.p95, P99: a.p99,
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

	where := "site_id = $1 AND start_time >= $2 AND start_time < $3"
	params := []any{siteID, fromMs, toMs}
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
			'1' AS span_count,
			status_code
		 FROM spans
		 WHERE %s AND parent_span_id = ''
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, where, limit, offset)

	return nucleus.Query[TraceSummary](ctx, q.db.SQL(), q2, params...)
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
func (q *QueryService) ServiceDependencies(ctx context.Context, siteID string, from, to time.Time) ([]Dependency, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[Dependency](ctx, q.db.SQL(),
		`SELECT src_service, dst_service,
			CAST(SUM(CAST(call_count AS BIGINT)) AS TEXT) AS call_count,
			CAST(SUM(CAST(error_count AS BIGINT)) AS TEXT) AS error_count,
			CAST(CASE WHEN SUM(CAST(call_count AS BIGINT)) > 0
				THEN SUM(CAST(avg_duration AS BIGINT) * CAST(call_count AS BIGINT)) / SUM(CAST(call_count AS BIGINT))
				ELSE 0 END AS TEXT) AS avg_duration
		 FROM service_dependencies
		 WHERE site_id = $1 AND ts_bucket >= $2 AND ts_bucket < $3
		 GROUP BY src_service, dst_service
		 ORDER BY call_count DESC`,
		siteID, fromMs, toMs,
	)
}

// TraceErrorHit is an error event correlated with a trace.
type TraceErrorHit struct {
	ErrorID    string    `json:"error_id"`
	ErrorType  string    `json:"error_type"`
	ErrorValue string    `json:"error_value"`
	Timestamp  time.Time `json:"timestamp"`
	IssueID    string    `json:"issue_id"`
}

// TraceErrors returns error events correlated with a trace by timestamp overlap.
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
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp <= $3
		 ORDER BY timestamp ASC`,
		siteID, b[0].MinT, b[0].MaxT,
	)
}
