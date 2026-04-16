package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

// LogService handles log ingestion and querying.
type LogService struct {
	db *nucleus.Client
}

func NewLogService(db *nucleus.Client) *LogService {
	return &LogService{db: db}
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
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("generate log id: %w", err)
	}

	attrsJSON := ""
	if len(input.Attributes) > 0 {
		raw, err := json.Marshal(input.Attributes)
		if err != nil {
			return "", fmt.Errorf("marshal attributes: %w", err)
		}
		attrsJSON = string(raw)
	}

	nowMs := dbutil.IntParam(time.Now().UTC().UnixMilli())

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

	return id, nil
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
		`SELECT level, CAST(COUNT(*) AS TEXT) AS count
		 FROM logs
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 GROUP BY level
		 ORDER BY count DESC`,
		siteID, fromMs, toMs,
	)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
