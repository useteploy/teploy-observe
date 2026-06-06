// Package jobs/exports implements scheduled SQL exports to S3-compatible
// storage (AWS S3, Cloudflare R2, MinIO, etc.). The admin defines a SELECT
// and a cron spec; the scheduler runs the query on that interval, formats
// the result as NDJSON or CSV, and uploads it as a single object per run.
package jobs

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/explorer"
	"github.com/useteploy/teploy-observe/internal/secretbox"
)

// S3Destination is the encrypted-at-rest config payload for S3/R2 exports.
type S3Destination struct {
	Endpoint        string `json:"endpoint,omitempty"` // blank = AWS, set for R2/MinIO
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"` // MinIO / R2 may need this
}

// ScheduledExport describes one cron-driven export job.
type ScheduledExport struct {
	ExportID        string `json:"export_id" db:"export_id"`
	Name            string `json:"name" db:"name"`
	SQL             string `json:"sql" db:"sql"`
	Format          string `json:"format" db:"format"`
	Cron            string `json:"cron" db:"cron"`
	DestinationType string `json:"destination_type" db:"destination_type"`
	DestinationCfg  string `json:"destination_cfg" db:"destination_cfg"`
	Enabled         string `json:"enabled" db:"enabled"`
	LastRunAt       int64  `json:"last_run_at" db:"last_run_at"`
	LastStatus      string `json:"last_status" db:"last_status"`
	LastError       string `json:"last_error" db:"last_error"`
	LastRows        int64  `json:"last_rows" db:"last_rows"`
	CreatedAt       int64  `json:"created_at" db:"created_at"`
	UpdatedAt       int64  `json:"updated_at" db:"updated_at"`
}

// ExportService manages the scheduled_exports table and runs due jobs.
type ExportService struct {
	db       *nucleus.Client
	explorer *explorer.ExplorerService
	logger   *slog.Logger
}

func NewExportService(db *nucleus.Client, ex *explorer.ExplorerService, logger *slog.Logger) *ExportService {
	return &ExportService{db: db, explorer: ex, logger: logger}
}

func (s *ExportService) List(ctx context.Context) ([]ScheduledExport, error) {
	rows, err := nucleus.Query[ScheduledExport](ctx, s.db.SQL(),
		`SELECT export_id, name, sql, format, cron, destination_type, destination_cfg,
		        enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at
		 FROM scheduled_exports ORDER BY updated_at DESC`)
	if err != nil {
		return rows, err
	}
	// Dedup by export_id, picking latest (first because ORDER BY updated_at DESC).
	// Also drop rows where the latest state is enabled='false' (soft-deleted).
	seen := make(map[string]struct{}, len(rows))
	out := make([]ScheduledExport, 0, len(rows))
	for _, r := range rows {
		if _, ok := seen[r.ExportID]; ok {
			continue
		}
		seen[r.ExportID] = struct{}{}
		if r.Enabled != "true" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

type CreateInput struct {
	Name            string        `json:"name"`
	SQL             string        `json:"sql"`
	Format          string        `json:"format"`
	Cron            string        `json:"cron"`
	DestinationType string        `json:"destination_type"`
	Destination     S3Destination `json:"destination"`
}

func (s *ExportService) Create(ctx context.Context, in CreateInput) (ScheduledExport, error) {
	if in.Name == "" || in.SQL == "" || in.Cron == "" {
		return ScheduledExport{}, fmt.Errorf("name, sql, and cron are required")
	}
	if in.Format == "" {
		in.Format = "ndjson"
	}
	if in.DestinationType == "" {
		in.DestinationType = "s3"
	}
	if _, err := explorer.ClassifyReadOnlySQL(in.SQL); err != nil {
		return ScheduledExport{}, fmt.Errorf("sql rejected: %w", err)
	}
	// Reject cron specs isDue can't interpret, so a standard 5-field expression
	// doesn't silently never run.
	if !isValidCronSpec(in.Cron) {
		return ScheduledExport{}, fmt.Errorf("unsupported cron %q: use @hourly, @daily, @weekly, or */N * * * *", in.Cron)
	}
	// Encrypt the S3/R2 secret key at rest. Fail closed rather than persist it
	// in plaintext (the destination_cfg column is readable by the explorer/DB).
	if in.Destination.SecretAccessKey != "" {
		enc, err := secretbox.Encrypt(in.Destination.SecretAccessKey)
		if err != nil {
			return ScheduledExport{}, fmt.Errorf("cannot store destination secret: %w (set OBSERVE_SECRET_KEY)", err)
		}
		in.Destination.SecretAccessKey = enc
	}
	cfgJSON, err := json.Marshal(in.Destination)
	if err != nil {
		return ScheduledExport{}, err
	}
	id := genID()
	now := time.Now().UnixMilli()
	nowP := dbutil.IntParam(now)
	zeroP := dbutil.IntParam(int64(0))
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_exports
		 (export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		  enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, 'true', $8, '', '', $9, $10, $11)`,
		id, in.Name, in.SQL, in.Format, in.Cron, in.DestinationType, string(cfgJSON),
		zeroP, zeroP, nowP, nowP,
	)
	if err != nil {
		return ScheduledExport{}, err
	}
	return ScheduledExport{ExportID: id, Name: in.Name, SQL: in.SQL, Format: in.Format, Cron: in.Cron,
		DestinationType: in.DestinationType, DestinationCfg: string(cfgJSON), Enabled: "true",
		CreatedAt: now, UpdatedAt: now}, nil
}

func (s *ExportService) Delete(ctx context.Context, id string) error {
	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_exports
		 (export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		  enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at)
		 SELECT export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		        'false', last_run_at, last_status, last_error, last_rows, created_at, $2
		 FROM scheduled_exports WHERE export_id = $1`, id, now)
	return err
}

// RunExport executes one scheduled export immediately, regardless of its
// cron schedule, and records the result. Used both by the scheduler and
// by the admin "Run now" button.
func (s *ExportService) RunExport(ctx context.Context, id string) error {
	exports, err := nucleus.Query[ScheduledExport](ctx, s.db.SQL(),
		`SELECT export_id, name, sql, format, cron, destination_type, destination_cfg,
		        enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at
		 FROM scheduled_exports WHERE export_id = $1 ORDER BY updated_at DESC LIMIT 1`, id)
	if err != nil || len(exports) == 0 {
		return fmt.Errorf("export %s not found", id)
	}
	e := exports[0]
	return s.runOne(ctx, e)
}

// exportTimeout bounds a single scheduled export so a hung query or upload
// can't stall RunDue (and the whole scheduler) indefinitely.
const exportTimeout = 10 * time.Minute

func (s *ExportService) runOne(ctx context.Context, e ScheduledExport) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	rowCount, uploadErr := s.executeAndUpload(ctx, e)
	status := "ok"
	errMsg := ""
	if uploadErr != nil {
		status = "error"
		errMsg = uploadErr.Error()
	}
	if err := s.recordRun(ctx, e.ExportID, start, status, errMsg, rowCount); err != nil {
		s.logger.Warn("scheduled export: record run failed", "id", e.ExportID, "err", err)
	}
	return uploadErr
}

func (s *ExportService) executeAndUpload(ctx context.Context, e ScheduledExport) (int64, error) {
	if _, err := explorer.ClassifyReadOnlySQL(e.SQL); err != nil {
		return 0, fmt.Errorf("sql rejected: %w", err)
	}
	var dest S3Destination
	if err := json.Unmarshal([]byte(e.DestinationCfg), &dest); err != nil {
		return 0, fmt.Errorf("parse destination: %w", err)
	}
	if dest.SecretAccessKey != "" {
		dec, err := secretbox.Decrypt(dest.SecretAccessKey)
		if err != nil {
			return 0, fmt.Errorf("decrypt destination secret: %w", err)
		}
		dest.SecretAccessKey = dec
	}

	rows, err := s.db.Pool().Query(ctx, e.SQL, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	fieldDescs := rows.FieldDescriptions()
	columns := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		columns[i] = string(fd.Name)
	}

	var buf bytes.Buffer
	var n int64
	switch strings.ToLower(e.Format) {
	case "csv":
		cw := csv.NewWriter(&buf)
		_ = cw.Write(columns)
		for rows.Next() {
			vals, _ := rows.Values()
			rec := make([]string, len(columns))
			for i, v := range vals {
				rec[i] = csvSafe(fmt.Sprintf("%v", v))
			}
			_ = cw.Write(rec)
			n++
		}
		cw.Flush()
	default: // ndjson
		for rows.Next() {
			vals, _ := rows.Values()
			row := make(map[string]any, len(columns))
			for i, col := range columns {
				row[col] = vals[i]
			}
			enc, err := json.Marshal(row)
			if err != nil {
				return n, err
			}
			buf.Write(enc)
			buf.WriteByte('\n')
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return n, err
	}

	if e.DestinationType != "s3" {
		return n, fmt.Errorf("unsupported destination: %s", e.DestinationType)
	}

	key := dest.Prefix + time.Now().UTC().Format("2006/01/02/") + e.Name + "-" +
		strconv.FormatInt(time.Now().UnixMilli(), 10)
	switch strings.ToLower(e.Format) {
	case "csv":
		key += ".csv"
	default:
		key += ".ndjson"
	}

	cfg := aws.Config{
		Region:      dest.Region,
		Credentials: credentials.NewStaticCredentialsProvider(dest.AccessKeyID, dest.SecretAccessKey, ""),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if dest.Endpoint != "" {
			o.BaseEndpoint = aws.String(dest.Endpoint)
		}
		o.UsePathStyle = dest.ForcePathStyle
	})
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(dest.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(buf.Bytes()),
		ContentType: aws.String(contentType(e.Format)),
	})
	return n, err
}

func contentType(format string) string {
	switch strings.ToLower(format) {
	case "csv":
		return "text/csv"
	default:
		return "application/x-ndjson"
	}
}

func (s *ExportService) recordRun(ctx context.Context, id string, start time.Time, status, errMsg string, rowCount int64) error {
	now := dbutil.IntParam(time.Now().UnixMilli())
	ran := dbutil.IntParam(start.UnixMilli())
	// Append a new row (ORDER BY updated_at DESC picks it on subsequent reads).
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_exports
		 (export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		  enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at)
		 SELECT export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		        enabled, $2, $3, $4, $5, created_at, $6
		 FROM scheduled_exports WHERE export_id = $1 ORDER BY updated_at DESC LIMIT 1`,
		id, ran, status, errMsg, dbutil.IntParam(rowCount), now)
	return err
}

// RunDue finds any enabled export whose next-due time (start of current
// interval) is past last_run_at, and runs it. Called by the scheduler
// once per minute.
func (s *ExportService) RunDue(ctx context.Context, now time.Time) {
	exports, err := s.List(ctx)
	if err != nil {
		s.logger.Warn("scheduled exports: list failed", "err", err)
		return
	}
	for _, e := range exports {
		if e.Enabled != "true" {
			continue
		}
		if !isDue(e.Cron, e.LastRunAt, now) {
			continue
		}
		if err := s.runOne(ctx, e); err != nil {
			s.logger.Warn("scheduled export run failed", "id", e.ExportID, "err", err)
		}
	}
}

// isDue is a simplistic cron interpreter supporting the common shorthands:
//
//	@hourly, @daily, @weekly, or "*/N * * * *" where N is a minute count.
//
// For the launch we don't need the full cron grammar — users write a
// handful of cadences. A richer parser can slot in later without
// changing callers.
func isDue(spec string, lastRunMs int64, now time.Time) bool {
	s := strings.TrimSpace(spec)
	last := time.UnixMilli(lastRunMs)
	switch s {
	case "@hourly":
		return now.Sub(last) >= time.Hour
	case "@daily":
		return now.Sub(last) >= 24*time.Hour
	case "@weekly":
		return now.Sub(last) >= 7*24*time.Hour
	}
	if strings.HasPrefix(s, "*/") && strings.HasSuffix(s, " * * * *") {
		numStr := strings.TrimSuffix(strings.TrimPrefix(s, "*/"), " * * * *")
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			return now.Sub(last) >= time.Duration(n)*time.Minute
		}
	}
	return false
}

// csvSafe neutralizes spreadsheet formula injection: a cell beginning with one
// of = + - @ (or a control char) is apostrophe-prefixed.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// isValidCronSpec reports whether spec is one isDue can actually interpret.
// Keep this in lockstep with isDue.
func isValidCronSpec(spec string) bool {
	s := strings.TrimSpace(spec)
	switch s {
	case "@hourly", "@daily", "@weekly":
		return true
	}
	if strings.HasPrefix(s, "*/") && strings.HasSuffix(s, " * * * *") {
		numStr := strings.TrimSuffix(strings.TrimPrefix(s, "*/"), " * * * *")
		n, err := strconv.Atoi(numStr)
		return err == nil && n > 0
	}
	return false
}

func genID() string {
	b := make([]byte, 16)
	_, _ = cryptoRead(b)
	out := make([]byte, len(b)*2)
	const hex = "0123456789abcdef"
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}
