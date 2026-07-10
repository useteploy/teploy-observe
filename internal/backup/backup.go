// Package backup implements streaming backup and restore of all Observe tables.
//
// Backup produces a tar archive (written to io.Writer) containing one JSONL
// file per table. Each line is a single row as JSON — no compression here, so
// the caller can pipe through gzip/zstd for on-disk storage.
//
//	observe backup > out.tar           # raw tar
//	observe backup | zstd > out.tar.zst # compressed
//	zstdcat out.tar.zst | observe restore
package backup

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-dev/neutron-go/nucleus"
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

func dumpTable(ctx context.Context, db *nucleus.Client, tw *tar.Writer, table string) (int64, error) {
	// Nucleus ships everything as text via SimpleProtocol. Read raw bytes so
	// we sidestep pgx's built-in type decoding (which chokes on Nucleus's
	// JSONB representation). Rows are serialized as {column: "textvalue"|null}.
	rows, err := db.Pool().Query(ctx, "SELECT * FROM "+table, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "unknown table") {
			return 0, nil
		}
		return 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	var buf []byte
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
		buf = append(buf, enc...)
		buf = append(buf, '\n')
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate: %w", err)
	}
	return n, writeEntry(tw, table+".jsonl", buf)
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

// Restore reads a tar archive from r and inserts rows into their source tables.
// Missing tables (not in the archive) are left untouched. Existing rows are
// inserted via INSERT without ON CONFLICT — callers should restore into an
// empty Nucleus instance or accept duplicates on ReplacingMergeTree tables.
func Restore(ctx context.Context, db *nucleus.Client, r io.Reader) error {
	tr := tar.NewReader(r)
	var manifest *Manifest

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Name == manifestName {
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return fmt.Errorf("manifest decode: %w", err)
			}
			if m.Version != manifestVersion {
				return fmt.Errorf("backup version %d not supported (expected %d)", m.Version, manifestVersion)
			}
			manifest = &m
			continue
		}
		if hdr.Name == resultsName {
			// The results entry is trailing, so we error AFTER restoring the
			// present tables — but that still turns a silently-partial restore
			// into a loud failure the operator must acknowledge (the failed
			// tables were omitted from the archive, not restored as empty).
			// Legacy archives without this entry are unaffected.
			var results []TableResult
			if err := json.NewDecoder(tr).Decode(&results); err != nil {
				return fmt.Errorf("results decode: %w", err)
			}
			var failed []string
			for _, r := range results {
				if !r.OK {
					failed = append(failed, r.Table)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("backup is partial — these tables failed to dump and are missing: %s", strings.Join(failed, ", "))
			}
			continue
		}
		if !strings.HasSuffix(hdr.Name, ".jsonl") {
			continue
		}
		table := strings.TrimSuffix(hdr.Name, ".jsonl")
		if err := restoreTable(ctx, db, tr, table); err != nil {
			return fmt.Errorf("restore %s: %w", table, err)
		}
	}
	if manifest == nil {
		return fmt.Errorf("no manifest found — is this an observe backup?")
	}
	return nil
}

// validIdent matches a safe SQL identifier (table or column). Restore data
// comes from an attacker-controllable tar archive, and table/column names are
// interpolated into INSERT statements (not bindable as params), so they MUST be
// validated against an allowlist + charset or the archive can inject SQL.
var validIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

var restorableTables = func() map[string]bool {
	m := make(map[string]bool, len(Tables))
	for _, t := range Tables {
		m[t] = true
	}
	return m
}()

func restoreTable(ctx context.Context, db *nucleus.Client, r io.Reader, table string) error {
	if !restorableTables[table] {
		return fmt.Errorf("refusing to restore unknown table %q (not in the backup allowlist)", table)
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20) // up to 16 MiB per row
	sqlc := db.SQL()

	jsonbCols, err := jsonbColumns(ctx, sqlc, table)
	if err != nil {
		return fmt.Errorf("lookup jsonb columns: %w", err)
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode row: %w", err)
		}
		if err := insertRow(ctx, sqlc, table, row, jsonbCols); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// jsonbColumns returns the set of JSONB-typed columns for a table, so restore
// can normalize values the engine would reject. Archives from lenient-era
// engines can carry empty-string JSONB values ('' used to be coerced;
// Postgres-parity engines reject it with "invalid input syntax for type
// json"), and those rows must restore as SQL NULL instead of failing.
func jsonbColumns(ctx context.Context, sqlc *nucleus.SQLModel, table string) (map[string]bool, error) {
	type colRow struct {
		ColumnName string `db:"column_name"`
	}
	rows, err := nucleus.Query[colRow](ctx, sqlc,
		`SELECT column_name FROM information_schema.columns
		 WHERE table_name = $1 AND UPPER(data_type) = 'JSONB'`, table)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rows))
	for _, r := range rows {
		set[r.ColumnName] = true
	}
	return set, nil
}

func insertRow(ctx context.Context, sqlc *nucleus.SQLModel, table string, row map[string]any, jsonbCols map[string]bool) error {
	cols := make([]string, 0, len(row))
	for k := range row {
		if !validIdent.MatchString(k) {
			return fmt.Errorf("refusing to restore row with unsafe column name %q", k)
		}
		cols = append(cols, k)
	}
	// Stable order for reproducibility.
	sortStrings(cols)

	placeholders := make([]string, len(cols))
	values := make([]any, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		values[i] = formatValue(row[c], jsonbCols[c])
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		table,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
	)
	_, err := sqlc.Exec(ctx, query, values...)
	return err
}

func formatValue(v any, isJSONB bool) any {
	// Nucleus's pgwire wants text for BIGINT/JSONB columns. json.Unmarshal
	// gives us string|float64|bool|map|slice|nil — map/slice need JSON text.
	switch val := v.(type) {
	case map[string]any, []any:
		raw, _ := json.Marshal(val)
		return string(raw)
	case string:
		// Lenient-era archives carry '' for JSONB columns; strict engines
		// reject it. Restore as NULL (the modern write path's equivalent).
		if isJSONB && val == "" {
			return nil
		}
		return v
	default:
		return v
	}
}

// sortStrings — avoid pulling the sort package into one tiny helper.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
