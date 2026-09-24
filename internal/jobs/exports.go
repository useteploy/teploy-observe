// Package jobs/exports implements scheduled SQL exports to S3-compatible
// storage (AWS S3, Cloudflare R2, MinIO, etc.). The admin defines a SELECT
// and a cron spec; the scheduler runs the query on that interval, formats
// the result as NDJSON or CSV, and uploads it as a single object per run.
//
// O14: a run is a durable intent (scheduled_export_runs, migration 053) on
// the 046/050 outbox pattern. RunDue freezes one run row per due export
// (sql/format/destination snapshot); the drain executes due runs with
// exponential backoff and an attempt budget whose end is a durable dead
// letter (kept with last_error in the run history). The run_id is the S3
// key component, so every attempt of one run writes the SAME object - an
// at-least-once retry overwrites its own partial upload instead of leaving
// one object per attempt. Every successful run also uploads a
// .manifest.json sidecar (columns, row/byte counts, body sha256) so an
// archive can be verified and restored without parsing the data.
package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/neutron-build/neutron/go/nucleus"

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

// validate scopes the destination a new export may declare (O14): region
// and bucket are required, and an explicit endpoint (R2/MinIO) must be a
// clean http(s) URL with a host and no userinfo. A malformed endpoint used
// to surface only at the first scheduled run, minutes after the admin
// walked away from the form.
func (d S3Destination) validate() error {
	if strings.TrimSpace(d.Region) == "" {
		return fmt.Errorf("destination region is required")
	}
	if strings.TrimSpace(d.Bucket) == "" {
		return fmt.Errorf("destination bucket is required")
	}
	if d.Endpoint != "" {
		u, err := url.Parse(d.Endpoint)
		if err != nil {
			return fmt.Errorf("destination endpoint: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("destination endpoint scheme must be http or https")
		}
		if u.User != nil {
			return fmt.Errorf("destination endpoint must not carry userinfo (use access_key_id/secret_access_key)")
		}
		if u.Hostname() == "" {
			return fmt.Errorf("destination endpoint must have a host")
		}
	}
	return nil
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

// ExportRun is one collapsed (latest-version) run row - the durable job
// history. Status derives from the disposition columns the way the outbox
// tables' do (finished_at > 0 = ok; next_attempt_at = -1 = dead letter).
type ExportRun struct {
	RunID           string `json:"run_id" db:"run_id"`
	ExportID        string `json:"export_id" db:"export_id"`
	Name            string `json:"name" db:"name"`
	SQL             string `json:"sql" db:"sql"`
	Format          string `json:"format" db:"format"`
	DestinationType string `json:"destination_type" db:"destination_type"`
	DestinationCfg  string `json:"-" db:"destination_cfg"`
	RunTrigger      string `json:"trigger" db:"run_trigger"`
	CreatedAt       int64  `json:"created_at" db:"created_at"`
	Attempts        int64  `json:"attempts" db:"attempts"`
	NextAttemptAt   int64  `json:"next_attempt_at" db:"next_attempt_at"`
	FinishedAt      int64  `json:"finished_at" db:"finished_at"`
	Rows            int64  `json:"rows" db:"rows"`
	Bytes           int64  `json:"bytes" db:"bytes"`
	LastError       string `json:"last_error" db:"last_error"`
	Version         int64  `json:"-" db:"version"`
}

// ExportService manages the scheduled_exports table and runs due jobs.
type ExportService struct {
	db       *nucleus.Client
	explorer *explorer.ExplorerService
	logger   *slog.Logger

	// Retry policy (the outbox defaults). Tests tune these directly.
	maxAttempts int
	backoffBase time.Duration
	backoffCap  time.Duration
}

func NewExportService(db *nucleus.Client, ex *explorer.ExplorerService, logger *slog.Logger) *ExportService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ExportService{
		db:          db,
		explorer:    ex,
		logger:      logger,
		maxAttempts: 5,
		backoffBase: time.Minute,
		backoffCap:  30 * time.Minute,
	}
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

// ListRuns returns a run history (newest first). It survives restarts by
// construction: the runs table is the ledger, not a summary column that
// the next overwrite erases.
func (s *ExportService) ListRuns(ctx context.Context, exportID string, limit int) ([]ExportRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return nucleus.Query[ExportRun](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT run_id, export_id, name, sql, format, destination_type, destination_cfg,
		        run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		 FROM (`+runsCollapse("export_id = $1")+`)
		 ORDER BY created_at DESC, run_id DESC LIMIT %d`, limit),
		exportID)
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
	// Scoped destination: refuse a malformed/incomplete target at CREATE time
	// (O14) - it used to surface only at the first scheduled run.
	if err := in.Destination.validate(); err != nil {
		return ScheduledExport{}, err
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

// Delete removes a scheduled export and its accumulated run-history rows.
//
// `scheduled_exports` is a PLAIN mergetree with no version column, so there is
// nothing to collapse by: the legacy recordRun appended one row per run and
// List keeps the newest per export_id. The old soft delete -
//
//	INSERT INTO scheduled_exports (...) SELECT ..., 'false', ... FROM scheduled_exports WHERE export_id = $1
//
// - had no LIMIT, so it re-inserted one row per row already present, doubling
// the physical count of an export that had been running for a while. A hard
// DELETE is both the correct fix and the right semantics: the export is gone,
// so its run history has no reader. It matches boards.DeleteBoard and
// query.DeleteGoal. List still drops any legacy enabled='false' rows left by
// the old soft delete. The durable runs ledger (053) goes with it - a
// deleted export's history is deletable with it, same as before.
func (s *ExportService) Delete(ctx context.Context, id string) error {
	if _, err := s.db.SQL().Exec(ctx,
		`DELETE FROM scheduled_export_runs WHERE export_id = $1`, id); err != nil {
		return fmt.Errorf("delete run history: %w", err)
	}
	_, err := s.db.SQL().Exec(ctx,
		`DELETE FROM scheduled_exports WHERE export_id = $1`, id)
	return err
}

// exportTimeout bounds a single scheduled export so a hung query or upload
// can't stall RunDue (and the whole scheduler) indefinitely.
const exportTimeout = 10 * time.Minute

// Run budgets: a scheduled export materializes its whole result before the
// upload (one S3 object per run), so the work needs a loud ceiling -
// exceeding either budget fails the run (recorded on the row, retried by
// the ledger's policy) instead of growing the buffer until the OOM killer
// takes ingestion down with it. Vars (not consts) so tests can lower them.
var (
	maxExportRows  = int64(1_000_000)
	maxExportBytes = int64(256 << 20) // 256 MiB
)

// RunDue finds any enabled export whose next-due time (start of current
// interval) is past last_run_at, enqueues a frozen run for each, then
// drains it. Called by the scheduler once per minute. The pass first
// drains unfinished runs from previous ticks or a restart (the outbox
// startup-resume contract) - a failed run's retry is the drain's job, not
// the next cron tick's.
func (s *ExportService) RunDue(ctx context.Context, now time.Time) {
	if err := s.DrainDue(ctx); err != nil {
		s.logger.Warn("scheduled exports: drain of unfinished runs failed", "err", err)
	}
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
		runID, err := s.enqueueRun(ctx, e.ExportID, "cron")
		if err != nil {
			s.logger.Warn("scheduled export enqueue failed", "id", e.ExportID, "err", err)
			continue
		}
		if err := s.DrainRun(ctx, runID); err != nil {
			s.logger.Warn("scheduled export run failed", "id", e.ExportID, "run", runID, "err", err)
		}
	}
}

// RunExport executes one scheduled export immediately, regardless of its
// cron schedule, and records the result. Used by the admin "Run now"
// button. The run goes through the same durable ledger: one enqueued run
// (trigger 'manual') drained synchronously, so the button's error report
// and the run history agree.
func (s *ExportService) RunExport(ctx context.Context, id string) error {
	runID, err := s.enqueueRun(ctx, id, "manual")
	if err != nil {
		return err
	}
	return s.DrainRun(ctx, runID)
}

// enqueueRun freezes one run intent from the export's current definition.
// The snapshot (sql/format/destination) is what every attempt of this run
// executes - an edit to the export between retries cannot half-apply, and
// the frozen destination secret stays in its encrypted column form.
func (s *ExportService) enqueueRun(ctx context.Context, exportID, trigger string) (string, error) {
	exports, err := nucleus.Query[ScheduledExport](ctx, s.db.SQL(),
		`SELECT export_id, name, sql, format, cron, destination_type, destination_cfg,
		        enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at
		 FROM scheduled_exports WHERE export_id = $1 ORDER BY updated_at DESC LIMIT 1`, exportID)
	if err != nil || len(exports) == 0 {
		return "", fmt.Errorf("export %s not found", exportID)
	}
	e := exports[0]
	runID := "exp-" + genID()
	now := time.Now().UnixMilli()
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_export_runs (
			run_id, tenant_id, export_id, name, sql, format, destination_type, destination_cfg,
			run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		) VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, 0, 0, 0, 0, 0, '', $9)`,
		runID, e.ExportID, e.Name, e.SQL, e.Format, e.DestinationType, e.DestinationCfg,
		trigger, dbutil.IntParam(now),
	)
	if err != nil {
		return "", fmt.Errorf("enqueue export run: %w", err)
	}
	return runID, nil
}

// DrainRun executes one run and records its disposition (the synchronous
// path RunExport and RunDue share). A non-zero return is the run's
// failure, already recorded on the row (retry-scheduled or dead-lettered).
func (s *ExportService) DrainRun(ctx context.Context, runID string) error {
	row, err := s.runByID(ctx, runID)
	if err != nil {
		return fmt.Errorf("read export run %s: %w", runID, err)
	}
	if row == nil {
		return fmt.Errorf("export run %s not found", runID)
	}
	if row.FinishedAt > 0 || row.NextAttemptAt < 0 {
		return nil // already terminal
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	n, byteCount, sum, runErr := s.executeAndUpload(rctx, *row)
	if runErr == nil {
		if derr := s.markRunFinished(ctx, row, start, n, byteCount, sum); derr != nil {
			s.logger.Error("scheduled export: record run failed", "run", runID, "err", derr)
		}
		return nil
	}
	s.recordRunFailure(ctx, row, runErr)
	if row.Attempts+1 >= int64(s.maxAttempts) {
		return fmt.Errorf("export run %s dead-lettered after %d attempts: %w", runID, row.Attempts+1, runErr)
	}
	return runErr
}

// DrainDue runs one pass over due runs (unfinished, retry time reached,
// under the attempt budget) - the background drain shape of the other
// outbox tables. RunDue currently drains synchronously per tick; this is
// the seam a lifecycle-owned cadence plugs into.
func (s *ExportService) DrainDue(ctx context.Context) error {
	rows, err := nucleus.Query[ExportRun](ctx, s.db.SQL(),
		`SELECT run_id, export_id, name, sql, format, destination_type, destination_cfg,
		        run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		 FROM (`+runsCollapse("")+`)
		 WHERE finished_at = 0 AND next_attempt_at >= 0 AND next_attempt_at <= $1 AND attempts < $2
		 ORDER BY created_at, run_id LIMIT 100`,
		dbutil.IntParam(time.Now().UnixMilli()), s.maxAttempts)
	if err != nil {
		return fmt.Errorf("export runs due scan: %w", err)
	}
	for i := range rows {
		if err := s.DrainRun(ctx, rows[i].RunID); err != nil {
			s.logger.Warn("scheduled export run failed", "run", rows[i].RunID, "err", err)
		}
	}
	return nil
}

// runsCollapse renders the argMax collapse over scheduled_export_runs -
// one logical row per run id, highest version wins (the form verified
// against the live engine; see internal/query/replacing.go).
func runsCollapse(where string) string {
	if strings.TrimSpace(where) == "" {
		where = "1 = 1"
	}
	return `SELECT tenant_id, run_id,
			argMax(export_id, version) AS export_id,
			argMax(name, version) AS name,
			argMax(sql, version) AS sql,
			argMax(format, version) AS format,
			argMax(destination_type, version) AS destination_type,
			argMax(destination_cfg, version) AS destination_cfg,
			argMax(run_trigger, version) AS run_trigger,
			argMax(created_at, version) AS created_at,
			argMax(attempts, version) AS attempts,
			argMax(next_attempt_at, version) AS next_attempt_at,
			argMax(finished_at, version) AS finished_at,
			argMax(rows, version) AS rows,
			argMax(bytes, version) AS bytes,
			argMax(last_error, version) AS last_error,
			MAX(version) AS version
		FROM scheduled_export_runs WHERE ` + where + `
		GROUP BY tenant_id, run_id`
}

func (s *ExportService) runByID(ctx context.Context, runID string) (*ExportRun, error) {
	rows, err := nucleus.Query[ExportRun](ctx, s.db.SQL(),
		`SELECT run_id, export_id, name, sql, format, destination_type, destination_cfg,
		        run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		 FROM (`+runsCollapse("run_id = $1")+`)`, runID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// exportBackoff returns the retry delay after the given failed attempt
// count (doubling from the base, capped).
func (s *ExportService) exportBackoff(attempts int64) time.Duration {
	if s.backoffBase <= 0 || attempts < 1 {
		return 0
	}
	d := s.backoffBase
	for i := int64(1); i < attempts && d < s.backoffCap; i++ {
		d *= 2
	}
	if d > s.backoffCap {
		d = s.backoffCap
	}
	return d
}

// recordRunFailure writes the failure disposition on the run row:
// attempts bumped, backoff scheduled, last_error kept. The attempt
// budget's end is a durable dead letter (next_attempt_at = -1, a sentinel
// no clock reaches) - kept in the history, never auto-pruned, never
// retried even by a later process with a larger budget.
func (s *ExportService) recordRunFailure(ctx context.Context, row *ExportRun, cause error) {
	attempts := row.Attempts + 1
	dead := attempts >= int64(s.maxAttempts)
	next := int64(-1)
	if !dead {
		next = time.Now().UnixMilli() + s.exportBackoff(attempts).Milliseconds()
	}
	lastErr := cause.Error()
	if len(lastErr) > 1024 {
		lastErr = lastErr[:1024]
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_export_runs (
			run_id, tenant_id, export_id, name, sql, format, destination_type, destination_cfg,
			run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		) VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, 0, 0, $12, $13)`,
		row.RunID, row.ExportID, row.Name, row.SQL, row.Format, row.DestinationType, row.DestinationCfg,
		row.RunTrigger, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(attempts), dbutil.IntParam(next),
		lastErr, dbutil.IntParam(runNextVersion(row.Version, now)),
	); err != nil {
		s.logger.Error("scheduled export: record failure failed", "run", row.RunID, "err", err)
		return
	}
	s.mirrorLastRun(ctx, row.ExportID, time.UnixMilli(row.CreatedAt), "error", lastErr, 0)
	if dead {
		s.logger.Error("scheduled export run dead-lettered - upload keeps failing",
			"run", row.RunID, "export", row.ExportID, "attempts", attempts, "err", cause)
	}
}

// markRunFinished writes the ok disposition and mirrors the legacy
// last_* summary columns on scheduled_exports (existing readers list from
// there). sum is the sha256 of the uploaded body - recorded on the run so
// an operator can verify the object in place against the ledger.
func (s *ExportService) markRunFinished(ctx context.Context, row *ExportRun, start time.Time, n, byteCount int64, sum string) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_export_runs (
			run_id, tenant_id, export_id, name, sql, format, destination_type, destination_cfg,
			run_trigger, created_at, attempts, next_attempt_at, finished_at, rows, bytes, last_error, version
		) VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		row.RunID, row.ExportID, row.Name, row.SQL, row.Format, row.DestinationType, row.DestinationCfg,
		row.RunTrigger, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(row.Attempts), dbutil.IntParam(row.NextAttemptAt),
		dbutil.IntParam(now), dbutil.IntParam(n), dbutil.IntParam(byteCount),
		"sha256:"+sum, dbutil.IntParam(runNextVersion(row.Version, now)),
	); err != nil {
		return err
	}
	return s.mirrorLastRun(ctx, row.ExportID, start, "ok", "", n)
}

// mirrorLastRun appends the legacy summary row to scheduled_exports (what
// List and the existing dashboard read).
//
// The `ORDER BY updated_at DESC LIMIT 1` is load-bearing, not decoration:
// it is what keeps this INSERT...SELECT-from-the-same-table to ONE row per
// call. Verified on Nucleus v0.1.8 - the identical statement without the
// LIMIT goes 1, 2, 4, 8 ... on successive calls. Do not remove it.
//
// updated_at is the version this table collapses by, so it gets the
// strictly-monotonic stamp (the 70f6eff version-tie defect).
func (s *ExportService) mirrorLastRun(ctx context.Context, id string, start time.Time, status, errMsg string, rowCount int64) error {
	now := dbutil.IntParam(time.Now().UnixMilli())
	ran := dbutil.IntParam(start.UnixMilli())
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO scheduled_exports
		 (export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		  enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at)
		 SELECT export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		        enabled, $2, $3, $4, $5, created_at,
		        GREATEST(CAST($6 AS BIGINT), updated_at + 1)
		 FROM scheduled_exports WHERE export_id = $1 ORDER BY updated_at DESC LIMIT 1`,
		id, ran, status, errMsg, dbutil.IntParam(rowCount), now)
	if err != nil {
		s.logger.Warn("scheduled export: mirror last-run summary failed", "id", id, "err", err)
	}
	return err
}

// exportManifest is the restoration-friendly sidecar uploaded next to every
// successful run's data object (the O14 "restoration-friendly format
// note"): enough identity + integrity metadata to verify and reload an
// archive without parsing the data itself.
type exportManifest struct {
	RunID       string   `json:"run_id"`
	ExportID    string   `json:"export_id"`
	ExportName  string   `json:"export_name"`
	Format      string   `json:"format"`       // ndjson | csv
	Columns     []string `json:"columns"`      // column order of the result set
	Rows        int64    `json:"rows"`         // data rows (excl. header)
	Bytes       int64    `json:"bytes"`        // exact uploaded body size
	BodySHA256  string   `json:"body_sha256"`  // hex sha256 of the uploaded body
	SQL         string   `json:"sql"`          // the frozen query this archive answers to
	GeneratedAt int64    `json:"generated_at"` // unix millis
	Generator   string   `json:"generator"`    // "teploy-observe scheduled export"
}

// executeAndUpload runs the frozen query, formats the result within the
// row/byte budgets, and uploads data + manifest under keys derived from
// the run id (stable across retries: an at-least-once retry overwrites
// its own partial object). Returns (rows, bytes, body sha256 hex, error).
func (s *ExportService) executeAndUpload(ctx context.Context, e ExportRun) (int64, int64, string, error) {
	if _, err := explorer.ClassifyReadOnlySQL(e.SQL); err != nil {
		return 0, 0, "", fmt.Errorf("sql rejected: %w", err)
	}
	var dest S3Destination
	if err := json.Unmarshal([]byte(e.DestinationCfg), &dest); err != nil {
		return 0, 0, "", fmt.Errorf("parse destination: %w", err)
	}
	if dest.SecretAccessKey != "" {
		dec, err := secretbox.Decrypt(dest.SecretAccessKey)
		if err != nil {
			return 0, 0, "", fmt.Errorf("decrypt destination secret: %w", err)
		}
		dest.SecretAccessKey = dec
	}

	rows, err := s.db.Pool().Query(ctx, e.SQL, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return 0, 0, "", fmt.Errorf("query: %w", err)
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
			if n >= maxExportRows {
				return n, 0, "", fmt.Errorf("export exceeded the row budget (%d)", maxExportRows)
			}
			vals, _ := rows.Values()
			rec := make([]string, len(columns))
			for i, v := range vals {
				rec[i] = csvSafe(fmt.Sprintf("%v", v))
			}
			_ = cw.Write(rec)
			n++
			if int64(buf.Len()) > maxExportBytes {
				return n, 0, "", fmt.Errorf("export exceeded the byte budget (%d bytes)", maxExportBytes)
			}
		}
		cw.Flush()
	default: // ndjson
		for rows.Next() {
			if n >= maxExportRows {
				return n, 0, "", fmt.Errorf("export exceeded the row budget (%d)", maxExportRows)
			}
			vals, _ := rows.Values()
			row := make(map[string]any, len(columns))
			for i, col := range columns {
				row[col] = vals[i]
			}
			enc, err := json.Marshal(row)
			if err != nil {
				return n, 0, "", err
			}
			buf.Write(enc)
			buf.WriteByte('\n')
			n++
			if int64(buf.Len()) > maxExportBytes {
				return n, 0, "", fmt.Errorf("export exceeded the byte budget (%d bytes)", maxExportBytes)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return n, 0, "", err
	}

	if e.DestinationType != "s3" {
		return n, 0, "", fmt.Errorf("unsupported destination: %s", e.DestinationType)
	}

	// Key derived from the RUN id: every attempt of this run writes the same
	// object, so a retry overwrites its own partial upload (idempotent
	// delivery side) instead of accumulating one object per attempt.
	key := dest.Prefix + time.Now().UTC().Format("2006/01/02/") + e.Name + "-" + e.RunID
	switch strings.ToLower(e.Format) {
	case "csv":
		key += ".csv"
	default:
		key += ".ndjson"
	}

	sum := sha256.Sum256(buf.Bytes())
	sumHex := hex.EncodeToString(sum[:])

	cfg := aws.Config{
		Region:      dest.Region,
		Credentials: credentials.NewStaticCredentialsProvider(dest.AccessKeyID, dest.SecretAccessKey, ""),
		// One attempt per drain-level try: the run ledger owns retry policy
		// (visible backoff, budget, dead letters). The SDK's silent internal
		// retries would hide attempts from the operator-facing history.
		RetryMaxAttempts: 1,
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if dest.Endpoint != "" {
			o.BaseEndpoint = aws.String(dest.Endpoint)
		}
		o.UsePathStyle = dest.ForcePathStyle
	})
	if _, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(dest.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(buf.Bytes()),
		ContentType: aws.String(contentType(e.Format)),
		Metadata: map[string]string{
			"observe-export-run":    e.RunID,
			"observe-export-id":     e.ExportID,
			"observe-export-format": strings.ToLower(e.Format),
			"observe-export-rows":   strconv.FormatInt(n, 10),
			"observe-export-sha256": sumHex,
		},
	}); err != nil {
		return n, 0, "", err
	}

	// Restoration-friendly sidecar: verification and reload metadata next to
	// the data, machine-readable, never inside the data format itself.
	manifest := exportManifest{
		RunID: e.RunID, ExportID: e.ExportID, ExportName: e.Name,
		Format: strings.ToLower(e.Format), Columns: columns,
		Rows: n, Bytes: int64(buf.Len()), BodySHA256: sumHex,
		SQL: e.SQL, GeneratedAt: time.Now().UTC().UnixMilli(),
		Generator: "teploy-observe scheduled export",
	}
	manifestJSON, _ := json.Marshal(manifest)
	if _, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(dest.Bucket),
		Key:         aws.String(key + ".manifest.json"),
		Body:        bytes.NewReader(manifestJSON),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return n, 0, "", fmt.Errorf("upload manifest sidecar: %w", err)
	}
	return n, int64(buf.Len()), sumHex, nil
}

// runNextVersion stamps a strictly-greater version than prior (the
// version-tie defect fix, same as the other outbox writers).
func runNextVersion(prior, now int64) int64 {
	if now > prior+1 {
		return now
	}
	return prior + 1
}

func contentType(format string) string {
	switch strings.ToLower(format) {
	case "csv":
		return "text/csv"
	default:
		return "application/x-ndjson"
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
