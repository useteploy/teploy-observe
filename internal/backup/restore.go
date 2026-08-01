package backup

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// maxArchiveBytes bounds how much a single restore will spool to local disk,
// so a runaway or maliciously oversized stream can't fill the disk before
// validation even starts.
var maxArchiveBytes int64 = 200 << 30 // 200 GiB

// restoreBatchSize is the number of rows committed per transaction during the
// apply pass — see the atomicity note on Restore.
const restoreBatchSize = 500

// Restore reads a tar archive from r and inserts rows into their source
// tables. It validates the ENTIRE archive before writing anything to the
// database: the archive is first spooled to a local temp file (so it can be
// read twice without trusting a single forward-only network stream), then a
// full validation pass checks the manifest is present and version-compatible,
// every table name is in the backup allowlist, every row's JSON is
// structurally decodable with safe column names, and — when present — the
// trailing results entry shows no failed tables. Only after that pass
// succeeds does a second pass apply the actual inserts. A malformed,
// truncated, or partial archive is therefore rejected with zero rows changed,
// which previously was not true: entries used to be applied to the database
// as they streamed in, so a bad archive (or one whose failure marker only
// appears near the end, as every real backup's does) could partially mutate
// the database before the error was even detected.
//
// Within the apply pass, each table is restored in bounded batches
// (restoreBatchSize rows per transaction) rather than one transaction per row
// (the old behavior) or a single transaction spanning the whole restore
// (impractical for very large backups against Nucleus, and not how any other
// bulk-write path in this codebase does it — see internal/ingest/buffer.go's
// identical batching for the same reason). The resulting atomicity guarantee,
// stated honestly: a bad archive changes nothing, full stop (caught in
// validation). An infrastructure failure during the apply pass itself (crash,
// disk full, a constraint violation validation couldn't check structurally)
// can leave a table's most recent batch rolled back while earlier batches
// (of that table, or of already-restored tables) remain committed — Restore
// always returns a non-nil error in that case, naming the failing table, so
// this is never silently reported as success. It is not a whole-restore
// transaction; achieving that would need either accepting very long-running,
// large transactions against Nucleus (a real availability risk under lock/
// memory pressure) or restoring into an entirely separate database/namespace
// and promoting it afterward, which is a bigger infrastructure change than
// this pass makes.
//
// Missing tables (declared in the manifest but with no corresponding .jsonl
// entry) are left untouched, matching the pre-v2 behavior — a backup taken
// with a subset of tables, or an older/newer manifest, restores what it has.
func Restore(ctx context.Context, db *nucleus.Client, r io.Reader) error {
	return RestoreWithKey(ctx, db, r, nil)
}

// RestoreWithKey is Restore plus an optional decryption key — see
// LoadBackupEncryptionKey and DumpWithKey. When key is nil the archive is
// read as plaintext, identical to Restore.
func RestoreWithKey(ctx context.Context, db *nucleus.Client, r io.Reader, key []byte) error {
	if key != nil {
		dr, err := newDecryptReader(r, key)
		if err != nil {
			return fmt.Errorf("decrypting backup: %w", err)
		}
		r = dr
	}

	spoolPath, err := spoolToTemp(r)
	if err != nil {
		return err
	}
	defer os.Remove(spoolPath)

	if err := validateArchive(spoolPath); err != nil {
		return fmt.Errorf("archive rejected — nothing was written to the database: %w", err)
	}
	return applyArchive(ctx, db, spoolPath)
}

// spoolToTemp copies r to a local temp file so the archive can be read twice
// (validate, then apply) without re-fetching or trusting a single
// forward-only pass over the network. Bounded by maxArchiveBytes so a
// runaway or hostile stream can't fill the disk.
func spoolToTemp(r io.Reader) (path string, err error) {
	tmp, err := os.CreateTemp("", "observe-restore-*.tar")
	if err != nil {
		return "", fmt.Errorf("create restore spool file: %w", err)
	}
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(r, maxArchiveBytes+1))
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("spooling archive to disk: %w", err)
	}
	if n > maxArchiveBytes {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("archive exceeds the %d byte restore limit", maxArchiveBytes)
	}
	return tmp.Name(), nil
}

// validateArchive walks the spooled archive without touching the database:
// manifest present and version-compatible; every table name in the backup
// allowlist; every row of every table structurally decodable JSON with safe
// column names; and, if a results entry is present, no table marked failed.
func validateArchive(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var manifest *Manifest

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		switch {
		case hdr.Name == manifestName:
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return fmt.Errorf("manifest decode: %w", err)
			}
			if m.Version != manifestVersion {
				return fmt.Errorf("backup version %d not supported (expected %d)", m.Version, manifestVersion)
			}
			manifest = &m
		case hdr.Name == resultsName:
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
		case strings.HasSuffix(hdr.Name, ".jsonl"):
			table := strings.TrimSuffix(hdr.Name, ".jsonl")
			if !restorableTables[table] {
				return fmt.Errorf("refusing to restore unknown table %q (not in the backup allowlist)", table)
			}
			if err := validateTableRows(tr); err != nil {
				return fmt.Errorf("table %s: %w", table, err)
			}
		}
	}
	if manifest == nil {
		return fmt.Errorf("no manifest found — is this an observe backup?")
	}
	return nil
}

// validateTableRows decodes and structurally checks every row of one table
// entry without touching the database. This is what guarantees a malformed
// row deep inside a large table is caught before ANY row of ANY table has
// been inserted, not merely before the rest of that one table.
func validateTableRows(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20) // up to 16 MiB per row
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode row: %w", err)
		}
		for k := range row {
			if !validIdent.MatchString(k) {
				return fmt.Errorf("row has unsafe column name %q", k)
			}
		}
	}
	return scanner.Err()
}

// applyArchive re-reads the already-validated spooled archive and performs
// the actual table restores.
func applyArchive(ctx context.Context, db *nucleus.Client, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if !strings.HasSuffix(hdr.Name, ".jsonl") {
			continue
		}
		table := strings.TrimSuffix(hdr.Name, ".jsonl")
		if err := restoreTable(ctx, db, tr, table); err != nil {
			return fmt.Errorf("restore %s: %w", table, err)
		}
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

// restoreTable inserts one table's already-validated rows in bounded batches
// — see the atomicity note on Restore.
func restoreTable(ctx context.Context, db *nucleus.Client, r io.Reader, table string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	jsonbCols, err := jsonbColumns(ctx, db.SQL(), table)
	if err != nil {
		return fmt.Errorf("lookup jsonb columns: %w", err)
	}

	batch := make([]map[string]any, 0, restoreBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertBatch(ctx, db, table, batch, jsonbCols); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			// Already checked in the validation pass — the spool file is a
			// local temp file this process owns exclusively for the duration
			// of Restore, so this should be unreachable. Fail loudly rather
			// than silently skip if it somehow isn't.
			return fmt.Errorf("decode row (post-validation, should be unreachable): %w", err)
		}
		batch = append(batch, row)
		if len(batch) >= restoreBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func insertBatch(ctx context.Context, db *nucleus.Client, table string, rows []map[string]any, jsonbCols map[string]bool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}
	sqlc := tx.SQL()
	for _, row := range rows {
		if err := insertRow(ctx, sqlc, table, row, jsonbCols); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// jsonbColumns returns the set of JSONB-typed columns for a table, so restore
// can normalize values the engine would reject. Archives from lenient-era
// engines can carry empty-string JSONB values (” used to be coerced;
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
