// Package backup implements streaming backup and restore of all Observe tables.
//
// Backup produces a tar archive (written to io.Writer) containing one JSONL
// file per table. Each line is a single row as JSON — no compression here, so
// the caller can pipe through gzip/zstd for on-disk storage.
//
//	observe backup > out.tar           # raw tar
//	observe backup | zstd > out.tar.zst # compressed
//	zstdcat out.tar.zst | observe restore
//
// Set OBSERVE_BACKUP_ENCRYPTION_KEY (see crypto.go) to encrypt backups at
// rest — the archive contains password hashes, API keys, and other secrets in
// plaintext otherwise.
package backup

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/query"
)

// Tables is the ordered list of tables included in a full backup.
// Order matters for restore (dependencies first isn't strictly required because
// there are no FKs, but we sort for reproducible snapshots).
//
// It must name every table the migrations create, minus ExcludedTables.
// TestTablesMatchSchema enforces both directions against
// internal/schema/migrations, because getting either wrong is silent: a name
// that is not a table dumps zero rows and dumpTable's missing-table handling
// reports success, and a table that is simply absent from this list is never
// looked at at all. Four names here were fictional until 2026-08-25 —
// "monitors", "crons", "share_tokens" and a duplicate "reports" — so uptime
// monitors, cron monitors and share links had never been backed up, and 27
// real tables (stats_daily, saved_views, sso_configs, scheduled_exports,
// incidents, boards, cohorts, metric_points, … ) were missing outright.
var Tables = []string{
	"sites",
	"admin_users",
	"principals",
	"api_keys",
	"users",
	"groups",
	"group_members",
	"sso_configs",
	"instance_settings",
	"audit_events",
	"events",
	"events_recent",
	"sessions",
	"stats_hourly",
	"stats_daily",
	"error_events",
	"issues",
	"performance_issues",
	"logs",
	"log_pipelines",
	"spans",
	"service_stats",
	"service_dependencies",
	"llm_traces",
	"metric_points",
	"host_metrics",
	"replay_sessions",
	"replay_batches",
	"replay_events",
	"click_heatmaps",
	"feature_flags",
	"flag_evaluations",
	"experiments",
	"experiment_exposures",
	"experiment_conversions",
	"surveys",
	"survey_responses",
	"cohorts",
	"feedback",
	"alert_rules",
	"alert_history",
	"incidents",
	"webhooks",
	"integrations",
	"integration_deliveries",
	"dashboards",
	"dashboard_panels",
	"boards",
	"saved_views",
	"uptime_monitors",
	"uptime_results",
	"cron_monitors",
	"cron_checkins",
	"goals",
	"tracked_links",
	"link_clicks",
	"share_links",
	"report_schedules",
	"scheduled_exports",
	"mcp_tokens",
}

// ExcludedTables are tables the migrations create that a backup deliberately
// skips. Everything else must be in Tables — see TestTablesMatchSchema.
var ExcludedTables = map[string]string{
	"_neutron_migrations": "migration bookkeeping — the migrations rebuild it, " +
		"and restoring a stale copy would make the schema and the ledger disagree",
}

// isRenameAsideArtifact reports whether table is one of the `<name>_preNNN`
// recovery copies a rename-aside migration leaves behind (027, 028, 033, 034).
// They hold superseded data that the live table already supersedes, they are
// meant to be dropped by hand once the copy is confirmed, and they are absent
// from a fresh install that ran the migrations against an empty database — so
// they are neither backed up nor required to appear in Tables.
func isRenameAsideArtifact(table string) bool {
	i := strings.LastIndex(table, "_pre")
	if i < 0 || len(table) != i+len("_pre")+3 {
		return false
	}
	for _, c := range table[i+len("_pre"):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Manifest is the first entry in a backup archive — a small JSON file that
// describes the archive so restore can validate compatibility.
type Manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Tables    []string  `json:"tables"`
	// KVSections names the KV namespaces the archive additionally carries
	// (F45); absent in pre-KV archives, which restore accepts as simply
	// having no KV section.
	KVSections []string `json:"kv_sections,omitempty"`
	// Lease records whether the dump ran inside a snapshot lease (one
	// point-in-time moment across every table; the KV srcmap section
	// carries its own convergence proof — see kvsrcmap.go). Nil on
	// archives from engines predating the lease, whose dumps are
	// independent per-table reads — the downgrade is stated, not hidden.
	Lease *LeaseInfo `json:"lease,omitempty"`
}

// TableResult records the outcome of dumping one table, written to a trailing
// results entry so a partial backup is distinguishable from a complete one.
type TableResult struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

const resultsName = "observe-backup-results.json"

const manifestName = "observe-backup.json"
const manifestVersion = 1

// Dump streams a tar archive of every configured table to w.
// Caller is responsible for closing w. Per-table errors are written to errLog
// (pass io.Discard to silence them).
func Dump(ctx context.Context, db *nucleus.Client, w io.Writer) error {
	return DumpWithLog(ctx, db, w, io.Discard)
}

// DumpWithLog is Dump plus a stream to record per-table errors for
// diagnostics (backup-to-stdout keeps stdout for tar data).
func DumpWithLog(ctx context.Context, db *nucleus.Client, w io.Writer, errLog io.Writer) error {
	return DumpWithKey(ctx, db, w, errLog, nil)
}

// DumpWithKey is DumpWithLog plus optional at-rest encryption. When key is
// nil the archive is plaintext, identical to DumpWithLog. When key is set
// (see LoadBackupEncryptionKey), the tar stream is wrapped in AES-256-GCM
// chunked encryption — see crypto.go.
func DumpWithKey(ctx context.Context, db *nucleus.Client, w io.Writer, errLog io.Writer, key []byte) error {
	if key != nil {
		ew, err := newEncryptWriter(w, key)
		if err != nil {
			return fmt.Errorf("setting up backup encryption: %w", err)
		}
		if err := dumpTar(ctx, db, ew, errLog); err != nil {
			_ = ew.Close()
			return err
		}
		return ew.Close()
	}
	return dumpTar(ctx, db, w, errLog)
}

func dumpTar(ctx context.Context, db *nucleus.Client, w io.Writer, errLog io.Writer) (retErr error) {
	tw := tar.NewWriter(w)
	// Audit F43: a tar writer buffers the final padding blocks for Close.
	// The deferred Close's error was dropped, so a broken pipe or full
	// destination could end the archive mid-air with a successful exit —
	// defeating the trailing results record whose entire job is making a
	// partial dump detectable.
	defer func() { retErr = errors.Join(retErr, tw.Close()) }()

	// F45: the whole dump reads through one lease-holding transaction, so
	// every table is captured at ONE moment and related tables cannot
	// diverge mid-dump (other sessions' SQL writes wait at the engine's
	// gate until the lease releases at this tx's rollback). The KV srcmap
	// section rides the same runner but cannot inherit the moment — the
	// engine's lease gate does not cover KV scalar writes — so it carries
	// its own convergence proof (kvsrcmap.go). Engines predating the lease
	// fall back to the historical pool reads with the downgrade recorded
	// in the manifest and warned here.
	runner := pgxRunner(db.Pool())
	var lease *leaseState
	leaseInfo := LeaseInfo{TimeoutMillis: 0}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin dump transaction: %w", err)
	}
	lease, err = acquireDumpLease(ctx, tx, leaseTimeoutMillis(), errLog)
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if lease != nil {
		runner = tx
		leaseInfo = LeaseInfo{Held: true, TimeoutMillis: lease.timeoutMillis}
		defer func() { _ = tx.Rollback(ctx) }() // releases the lease
	} else {
		_ = tx.Rollback(ctx)
	}

	manifest := Manifest{
		Version:    manifestVersion,
		CreatedAt:  time.Now().UTC(),
		Tables:     Tables,
		KVSections: []string{kvSrcmapSection},
		Lease:      &leaseInfo,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest marshal: %w", err)
	}
	if err := writeEntry(tw, manifestName, raw); err != nil {
		return err
	}

	// Collect per-table errors rather than abort — a broken read on one table
	// shouldn't lose the rest of the backup. Per-table results are recorded in a
	// trailing entry so a partial dump is detectable on restore, and a failed
	// table is OMITTED (not written as an empty .jsonl that masquerades as a
	// complete, empty table).
	var firstErr error
	results := make([]TableResult, 0, len(Tables)+1)
	for _, table := range Tables {
		rows, err := dumpTable(ctx, runner, tw, table)
		if err != nil {
			fmt.Fprintf(errLog, "backup: table %s: %v\n", table, err)
			results = append(results, TableResult{Table: table, OK: false, Error: err.Error()})
			if firstErr == nil {
				firstErr = fmt.Errorf("table %s: %w", table, err)
			}
			continue
		}
		results = append(results, TableResult{Table: table, Rows: rows, OK: true})
	}

	// F45 app half: the KV srcmap domain rides in the same archive, the
	// same results machinery vouching for its completeness.
	kvRows, err := dumpKVSrcmap(ctx, runner, tw)
	if err != nil {
		fmt.Fprintf(errLog, "backup: kv srcmap: %v\n", err)
		results = append(results, TableResult{Table: kvSrcmapResultTable, OK: false, Error: err.Error()})
		if firstErr == nil {
			firstErr = fmt.Errorf("kv srcmap: %w", err)
		}
	} else {
		results = append(results, TableResult{Table: kvSrcmapResultTable, Rows: kvRows, OK: true})
	}

	// The consistency claim on the manifest ("lease.held") is only honest
	// if every read happened inside the window; expiry means blocked
	// writers woke mid-dump, so fail instead of labeling a mixed-moment
	// archive as one moment. A failed dump here leaves no results entry,
	// which restore rejects (F44).
	if err := checkLeaseWindow(lease); err != nil {
		fmt.Fprintf(errLog, "backup: %v\n", err)
		return err
	}

	// Audit F43: the completion record is load-bearing (restore rejects
	// archives without it since F44) — a failure to marshal or write it
	// must fail the backup, not vanish behind the per-table errors.
	raw, err = json.MarshalIndent(results, "", "  ")
	if err != nil {
		return errors.Join(firstErr, fmt.Errorf("results marshal: %w", err))
	}
	if err := writeEntry(tw, resultsName, raw); err != nil {
		return errors.Join(firstErr, err)
	}
	return firstErr
}

// dumpTable streams one table's rows to a securely-created temp file first,
// then copies that file into the tar entry. Nucleus's on-disk temp file (not
// an in-memory buffer) keeps memory bounded regardless of table size — a
// table larger than available RAM used to OOM the process because every row
// was accumulated in one growing []byte before the tar entry was written
// (writeEntry needs the total size upfront, which the tar format requires
// declared in the header before any data bytes).
func dumpTable(ctx context.Context, r pgxRunner, tw *tar.Writer, table string) (int64, error) {
	// Nucleus ships everything as text via SimpleProtocol. Read raw bytes so
	// we sidestep pgx's built-in type decoding (which chokes on Nucleus's
	// JSONB representation). Rows are serialized as {column: "textvalue"|null}.
	//
	// A ReplacingMergeTree is captured collapsed: `SELECT *` returns every
	// superseded version, and restore re-inserted all of them, so a backup
	// carried (and a restore recreated) the same duplicate rows the read path
	// now has to work around — a soft-deleted webhook came back alive if an
	// older enabled='true' version happened to be re-read first.
	sel, err := latestSelect(ctx, r, table)
	if err != nil {
		return 0, err
	}
	rows, err := r.Query(ctx, sel, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "unknown table") {
			return 0, nil
		}
		return 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	tmp, err := os.CreateTemp("", "observe-backup-"+table+"-*.jsonl")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	bw := bufio.NewWriter(tmp)
	fields := rows.FieldDescriptions()
	var n int64
	for rows.Next() {
		raw := rows.RawValues()
		row := make(map[string]any, len(fields))
		for i, f := range fields {
			if raw[i] == nil {
				row[string(f.Name)] = nil
			} else {
				row[string(f.Name)] = string(raw[i])
			}
		}
		enc, err := json.Marshal(row)
		if err != nil {
			return 0, fmt.Errorf("marshal row: %w", err)
		}
		if _, err := bw.Write(enc); err != nil {
			return 0, fmt.Errorf("write temp row: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return 0, fmt.Errorf("write temp row: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("flush temp file: %w", err)
	}

	info, err := tmp.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat temp file: %w", err)
	}
	hdr := &tar.Header{
		Name: table + ".jsonl",
		// AUD-045 (round 2): SQL exports carry password hashes and other
		// sensitive records; an ordinary extraction must not leave them
		// group/world-readable.
		Mode:    0o600,
		Size:    info.Size(),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, fmt.Errorf("tar header %s: %w", table, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek temp file: %w", err)
	}
	if _, err := io.Copy(tw, tmp); err != nil {
		return 0, fmt.Errorf("tar write %s: %w", table, err)
	}
	return n, nil
}

func writeEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600, // AUD-045: manifest/results are metadata, same posture
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}

// latestSelect returns the statement that dumps one table. For a plain table
// that is `SELECT * FROM t`. For a ReplacingMergeTree whose ORDER BY key is
// registered in internal/query, it is the argMax collapse — one row per key,
// carrying the highest-version value of every other column.
//
// The column list is discovered at runtime rather than hard-coded: several of
// these tables gained columns in later migrations (cron_monitors.ping_token in
// 026, sessions.release_tag in 019), and a backup that silently dropped a
// column would be worse than one that carries duplicates. Nucleus has no
// pg_catalog, so the lookup goes through information_schema. If the lookup
// returns nothing — the table does not exist on this instance, or carries no
// `version` column — the plain `SELECT *` is used and dumpTable's existing
// missing-table handling applies.
func latestSelect(ctx context.Context, r pgxRunner, table string) (string, error) {
	keys := query.Keys(table)
	if len(keys) == 0 {
		return "SELECT * FROM " + table, nil
	}
	cols, err := tableColumns(ctx, r, table)
	if err != nil || len(cols) == 0 {
		// Not a hard failure: fall back to the uncollapsed dump rather than
		// lose the table from the archive.
		return "SELECT * FROM " + table, nil
	}
	isKey := make(map[string]bool, len(keys))
	for _, k := range keys {
		isKey[k] = true
	}
	var hasVersion bool
	var nonKey []string
	for _, c := range cols {
		if c == "version" {
			hasVersion = true
		}
		if !isKey[c] {
			nonKey = append(nonKey, c)
		}
	}
	if !hasVersion {
		return "SELECT * FROM " + table, nil
	}
	for _, k := range keys {
		if !contains(cols, k) {
			// The registered key does not match this instance's schema; a
			// GROUP BY on a missing column would silently group everything
			// into one row, so refuse the collapse rather than corrupt.
			return "SELECT * FROM " + table, nil
		}
	}
	return query.LatestSelect(table, nonKey, ""), nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func tableColumns(ctx context.Context, r pgxRunner, table string) ([]string, error) {
	// information_schema is read through the dump's runner (the lease's
	// point-in-time view) with the same raw SimpleProtocol posture as the
	// table dumps — one scan loop, no SDK model needed on a pgx.Tx.
	rows, err := r.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = $1`,
		pgx.QueryExecModeSimpleProtocol, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var name *string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name != nil && *name != "" {
			out = append(out, *name)
		}
	}
	return out, rows.Err()
}
