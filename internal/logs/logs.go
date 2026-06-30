package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// LogService handles log ingestion and querying.
type LogService struct {
	db        *nucleus.Client
	Bx        *Broadcaster
	pipelines *PipelineService
}

func NewLogService(db *nucleus.Client) *LogService {
	return &LogService{db: db, Bx: NewBroadcaster()}
}

// SetPipelines wires the pipeline processor into the ingest path so drop/mask/
// sample/parse rules are actually applied on every ingested log. Without this
// the configured rules are inert (sensitive data the operator asked to mask
// would be stored in cleartext).
func (s *LogService) SetPipelines(p *PipelineService) {
	s.pipelines = p
}

// Log represents a stored log entry. Timestamps serialize as RFC3339 strings.
type Log struct {
	LogID       string    `json:"log_id"`
	SiteID      string    `json:"site_id"`
	Timestamp   time.Time `json:"timestamp"`
	Level       string    `json:"level"`
	Message     string    `json:"message"`
	ServiceName string    `json:"service_name"`
	TraceID     string    `json:"trace_id"`
	SpanID      string    `json:"span_id"`
	Attributes  string    `json:"attributes"`
}

// LogInput is the payload accepted for log ingestion.
type LogInput struct {
	SiteID      string         `json:"site_id"`
	Level       string         `json:"level"`
	Message     string         `json:"message"`
	ServiceName string         `json:"service_name"`
	TraceID     string         `json:"trace_id"`
	SpanID      string         `json:"span_id"`
	Attributes  map[string]any `json:"attributes"`
}

// IngestLog inserts a single log entry into the logs table.
func (s *LogService) IngestLog(ctx context.Context, input LogInput) (string, error) {
	// Apply configured pipelines (drop/mask/sample/parse) before persisting.
	// A drop/sample rule that returns keep=false skips the insert silently
	// (no error to the client). SiteID must already be resolved by the caller.
	if s.pipelines != nil {
		msg, attrs, keep := s.pipelines.ProcessLog(ctx, input.SiteID, input.Message, input.Attributes)
		if !keep {
			return "", nil
		}
		input.Message = msg
		input.Attributes = attrs
	}

	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("generate log id: %w", err)
	}

	attrsJSON := "null"
	if len(input.Attributes) > 0 {
		raw, err := json.Marshal(input.Attributes)
		if err != nil {
			return "", fmt.Errorf("marshal attributes: %w", err)
		}
		attrsJSON = string(raw)
	}

	now := time.Now().UTC()
	nowMs := dbutil.IntParam(now.UnixMilli())

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO logs (
			log_id, tenant_id, site_id, timestamp, level, message,
			service_name, trace_id, span_id, attributes
		) VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, input.SiteID, nowMs, input.Level, input.Message,
		input.ServiceName, input.TraceID, input.SpanID, attrsJSON,
	)
	if err != nil {
		return "", fmt.Errorf("insert log: %w", err)
	}

	if s.Bx != nil {
		s.Bx.Publish(logToPublish(id, input, now))
	}

	return id, nil
}

// maxLogBatchSize caps a single /logs/batch request, mirroring the events
// batch cap — bounds request size and worst-case per-request insert count.
const maxLogBatchSize = 200

// LogBatchResult reports how many of a batch's entries were stored.
type LogBatchResult struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// IngestLogs ingests a batch of log entries in one call. A structured
// transport (e.g. a batching Pino destination) accumulates lines client-side
// and flushes them together — this is the server side of that contract, so a
// high-throughput logger doesn't burn one request (and one rate-limit token)
// per line. Per-entry failures are counted and skipped rather than aborting
// the batch: a single malformed line must not drop the rest, and the client
// has no way to retry only the bad entries anyway.
func (s *LogService) IngestLogs(ctx context.Context, inputs []LogInput) (LogBatchResult, error) {
	if len(inputs) > maxLogBatchSize {
		return LogBatchResult{}, fmt.Errorf("batch too large: %d entries (max %d)", len(inputs), maxLogBatchSize)
	}
	result := LogBatchResult{}
	for _, input := range inputs {
		if _, err := s.IngestLog(ctx, input); err != nil {
			result.Rejected++
			continue
		}
		result.Accepted++
	}
	return result, nil
}

// SearchLogs queries logs with optional filters.
func (s *LogService) SearchLogs(ctx context.Context, siteID string, from, to time.Time, level, service, search string, limit, offset int) ([]Log, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := "site_id = $1 AND timestamp >= $2 AND timestamp < $3"
	params := []any{siteID, fromMs, toMs}
	idx := 4

	if level != "" {
		where += fmt.Sprintf(" AND level = $%d", idx)
		params = append(params, level)
		idx++
	}
	if service != "" {
		where += fmt.Sprintf(" AND service_name = $%d", idx)
		params = append(params, service)
		idx++
	}
	if search != "" {
		where += fmt.Sprintf(" AND message ILIKE '%%' || $%d || '%%'", idx)
		params = append(params, search)
		idx++
	}

	query := fmt.Sprintf(
		`SELECT log_id, tenant_id, site_id,
			CAST(timestamp AS TEXT) AS timestamp,
			level, message, service_name,
			COALESCE(trace_id, '') AS trace_id,
			COALESCE(span_id, '') AS span_id,
			COALESCE(attributes, '') AS attributes
		 FROM logs
		 WHERE %s
		 ORDER BY timestamp DESC
		 LIMIT %d OFFSET %d`, where, limit, offset)

	return nucleus.Query[Log](ctx, s.db.SQL(), query, params...)
}

// LevelCount holds the count of logs for a single level.
type LevelCount struct {
	Level string `json:"level"`
	Count int64  `json:"count"`
}

// LogStats returns the count of logs per level for a time range.
func (s *LogService) LogStats(ctx context.Context, siteID string, from, to time.Time) ([]LevelCount, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[LevelCount](ctx, s.db.SQL(),
		`SELECT level, COUNT(*) AS count
		 FROM logs
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 GROUP BY level
		 ORDER BY count DESC`,
		siteID, fromMs, toMs,
	)
}

// HistogramBucket represents log counts per level for a single time bucket.
type HistogramBucket struct {
	Bucket int64  `json:"bucket" db:"bucket"`
	Level  string `json:"level" db:"level"`
	Count  int64  `json:"count" db:"count"`
}

// Histogram returns per-level log counts bucketed by `bucketMs` milliseconds
// across the given time range.
func (s *LogService) Histogram(ctx context.Context, siteID string, from, to time.Time, bucketMs int64) ([]HistogramBucket, error) {
	if bucketMs <= 0 {
		bucketMs = 5 * 60 * 1000 // 5 minutes
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	bucketStr := dbutil.IntParam(bucketMs)

	return nucleus.Query[HistogramBucket](ctx, s.db.SQL(),
		`SELECT (CAST(timestamp AS BIGINT) / CAST($4 AS BIGINT)) * CAST($4 AS BIGINT) AS bucket,
		        level,
		        COUNT(*) AS count
		 FROM logs
		 WHERE site_id = $1
		   AND timestamp >= $2
		   AND timestamp < $3
		 GROUP BY (CAST(timestamp AS BIGINT) / CAST($4 AS BIGINT)) * CAST($4 AS BIGINT), level
		 ORDER BY bucket ASC`,
		siteID, fromMs, toMs, bucketStr,
	)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
