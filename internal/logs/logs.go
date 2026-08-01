package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// LogService handles log ingestion and querying.
type LogService struct {
	db        *nucleus.Client
	Bx        *Broadcaster
	pipelines *PipelineService
	logger    *slog.Logger
}

func NewLogService(db *nucleus.Client) *LogService {
	return &LogService{db: db, Bx: NewBroadcaster(), logger: slog.Default()}
}

// log returns the service logger, falling back to the default so a
// zero-value LogService (as some tests build) can still report errors.
func (s *LogService) log() *slog.Logger {
	if s.logger == nil {
		return slog.Default()
	}
	return s.logger
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

// preparedLog is a log entry after pipeline processing (drop/mask/sample/
// parse) and ID/JSON generation, ready to bind into an INSERT. Splitting
// this out of IngestLog lets IngestLogs run pipelines + validation per entry
// but issue one batched multi-row INSERT for the whole request, instead of
// looping a single-row INSERT per line.
type preparedLog struct {
	id        string
	input     LogInput
	attrsJSON string
	tsMs      string
	now       time.Time
}

// prepareLog applies pipelines and builds the row to insert, without
// touching the DB. Returns (nil, nil) if a pipeline rule dropped the entry —
// that mirrors the pre-existing IngestLog contract where a drop returns
// ("", nil), not an error.
func (s *LogService) prepareLog(ctx context.Context, input LogInput) (*preparedLog, error) {
	// Apply configured pipelines (drop/mask/sample/parse) before persisting.
	// A drop/sample rule that returns keep=false skips the insert silently
	// (no error to the client). SiteID must already be resolved by the caller.
	if s.pipelines != nil {
		msg, attrs, keep := s.pipelines.ProcessLog(ctx, input.SiteID, input.Message, input.Attributes)
		if !keep {
			return nil, nil
		}
		input.Message = msg
		input.Attributes = attrs
	}

	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate log id: %w", err)
	}

	attrsJSON := "null"
	if len(input.Attributes) > 0 {
		raw, err := json.Marshal(input.Attributes)
		if err != nil {
			return nil, fmt.Errorf("marshal attributes: %w", err)
		}
		attrsJSON = string(raw)
	}

	now := time.Now().UTC()
	return &preparedLog{
		id:        id,
		input:     input,
		attrsJSON: attrsJSON,
		tsMs:      dbutil.IntParam(now.UnixMilli()),
		now:       now,
	}, nil
}

// IngestLog inserts a single log entry into the logs table.
func (s *LogService) IngestLog(ctx context.Context, input LogInput) (string, error) {
	p, err := s.prepareLog(ctx, input)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", nil // dropped by a pipeline rule
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO logs (
			log_id, tenant_id, site_id, timestamp, level, message,
			service_name, trace_id, span_id, attributes
		) VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9)`,
		p.id, p.input.SiteID, p.tsMs, p.input.Level, p.input.Message,
		p.input.ServiceName, p.input.TraceID, p.input.SpanID, p.attrsJSON,
	)
	if err != nil {
		return "", fmt.Errorf("insert log: %w", err)
	}

	if s.Bx != nil {
		s.Bx.Publish(logToPublish(p.id, p.input, p.now))
	}

	return p.id, nil
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
	if len(inputs) == 0 {
		return result, nil
	}

	// Run pipelines/validation per entry (unchanged), but collect the
	// surviving rows and issue one batched multi-row INSERT for the whole
	// request instead of one autocommit INSERT per line — that per-line loop
	// was a major source of Nucleus memory pressure under load. Mirrors the
	// chunked-INSERT pattern in internal/ingest/buffer.go's insertBatch.
	toInsert := make([]*preparedLog, 0, len(inputs))
	for _, input := range inputs {
		p, err := s.prepareLog(ctx, input)
		if err != nil {
			result.Rejected++
			continue
		}
		if p == nil {
			// Dropped by a pipeline rule — matches the pre-batching
			// IngestLog contract, where a drop counted as accepted (err
			// was nil) rather than rejected.
			result.Accepted++
			continue
		}
		toInsert = append(toInsert, p)
	}

	if len(toInsert) == 0 {
		return result, nil
	}

	// A chunk failure isn't surfaced to the caller as a batch-level error —
	// matches the old per-line loop, which never returned one either. The
	// committed/toInsert count difference already flows into Rejected, and
	// insertLogsBatch has already logged the cause of any shortfall.
	committed, _ := s.insertLogsBatch(ctx, toInsert)
	result.Accepted += len(committed)
	result.Rejected += len(toInsert) - len(committed)

	if s.Bx != nil {
		for _, p := range committed {
			s.Bx.Publish(logToPublish(p.id, p.input, p.now))
		}
	}

	return result, nil
}

// logsCols is the number of bound parameters per row in the batch INSERT
// below (tenant_id is bound per-row, rather than the literal used by the
// single-row IngestLog statement, so every row in the VALUES list has the
// same shape).
const logsCols = 10

const logsColList = `log_id, tenant_id, site_id, timestamp, level, message,
	service_name, trace_id, span_id, attributes`

// buildLogPlaceholders returns "($1,...,$10),($11,...,$20),..." for rows*logsCols placeholders.
func buildLogPlaceholders(rows int) string {
	var b strings.Builder
	b.Grow(rows * logsCols * 5)
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < logsCols; c++ {
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

func logArgs(dst []any, p *preparedLog) []any {
	return append(dst,
		p.id, "default", p.input.SiteID, p.tsMs, p.input.Level, p.input.Message,
		p.input.ServiceName, p.input.TraceID, p.input.SpanID, p.attrsJSON,
	)
}

// insertLogsBatch batch-inserts prepared log rows in fixed-size chunks,
// mirroring internal/ingest/buffer.go's insertBatch chunking (same chunk
// size, same placeholder-building shape) instead of one autocommit INSERT
// per line.
//
// A failing chunk is logged and skipped, not returned on: batching already
// costs blast radius (one bad row takes its 50 rowmates with it, where the
// old per-line loop lost exactly one), and bailing out would compound that by
// discarding every later chunk unattempted — the opposite of IngestLogs'
// documented "a single malformed line must not drop the rest" contract.
//
// Returns the rows actually committed rather than a count, because the
// committed set is no longer a prefix once a middle chunk can fail: the
// caller publishes exactly these rows to subscribers.
func (s *LogService) insertLogsBatch(ctx context.Context, batch []*preparedLog) ([]*preparedLog, error) {
	const batchSize = 50
	sql := s.db.SQL()
	committed := make([]*preparedLog, 0, len(batch))
	var firstErr error
	for start := 0; start < len(batch); start += batchSize {
		end := start + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]

		query := "INSERT INTO logs (" + logsColList + ") VALUES " + buildLogPlaceholders(len(chunk))
		args := make([]any, 0, len(chunk)*logsCols)
		for _, p := range chunk {
			args = logArgs(args, p)
		}

		if _, err := sql.Exec(ctx, query, args...); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("batch insert logs %d-%d: %w", start+1, end, err)
			}
			// Not via s.logger directly: tests construct a zero-value
			// LogService, and a chunk failure there would nil-panic.
			s.log().Error("log batch chunk failed", "from", start+1, "to", end, "dropped", len(chunk), "error", err)
			continue
		}
		committed = append(committed, chunk...)
	}
	return committed, firstErr
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
