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
var Tables = []string{
	"sites",
	"admin_users",
	"api_keys",
	"users",
	"events",
	"events_recent",
	"sessions",
	"stats_hourly",
	"error_events",
	"issues",
	"logs",
	"spans",
	"service_stats",
	"replay_sessions",
	"replay_events",
	"feature_flags",
	"experiments",
	"surveys",
	"alert_rules",
	"alert_history",
	"webhooks",
	"integrations",
	"dashboards",
	"dashboard_panels",
	"reports",
	"monitors",
	"crons",
	"goals",
	"share_tokens",
	"report_schedules",
}

// Manifest is the first entry in a backup archive — a small JSON file that
// describes the archive so restore can validate compatibility.
type Manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Tables    []string  `json:"tables"`
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

func dumpTar(ctx context.Context, db *nucleus.Client, w io.Writer, errLog io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	manifest := Manifest{
		Version:   manifestVersion,
		CreatedAt: time.Now().UTC(),
		Tables:    Tables,
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
	results := make([]TableResult, 0, len(Tables))
	for _, table := range Tables {
		rows, err := dumpTable(ctx, db, tw, table)
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
	if raw, err := json.MarshalIndent(results, "", "  "); err == nil {
		_ = writeEntry(tw, resultsName, raw)
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
func dumpTable(ctx context.Context, db *nucleus.Client, tw *tar.Writer, table string) (int64, error) {
	// Nucleus ships everything as text via SimpleProtocol. Read raw bytes so
	// we sidestep pgx's built-in type decoding (which chokes on Nucleus's
	// JSONB representation). Rows are serialized as {column: "textvalue"|null}.
	//
	// A ReplacingMergeTree is captured collapsed: `SELECT *` returns every
	// superseded version, and restore re-inserted all of them, so a backup
	// carried (and a restore recreated) the same duplicate rows the read path
	// now has to work around — a soft-deleted webhook came back alive if an
	// older enabled='true' version happened to be re-read first.
	sel, err := latestSelect(ctx, db, table)
	if err != nil {
		return 0, err
	}
	rows, err := db.Pool().Query(ctx, sel, pgx.QueryExecModeSimpleProtocol)
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
		Name:    table + ".jsonl",
		Mode:    0644,
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
		Mode:    0644,
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
func latestSelect(ctx context.Context, db *nucleus.Client, table string) (string, error) {
	keys := query.Keys(table)
	if len(keys) == 0 {
		return "SELECT * FROM " + table, nil
	}
	cols, err := tableColumns(ctx, db, table)
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

type columnRow struct {
	ColumnName string `db:"column_name"`
}

func tableColumns(ctx context.Context, db *nucleus.Client, table string) ([]string, error) {
	rows, err := nucleus.Query[columnRow](ctx, db.SQL(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = $1`,
		table)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ColumnName != "" {
			out = append(out, r.ColumnName)
		}
	}
	return out, nil
}
