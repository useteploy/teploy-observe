package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	manifest, err := validateArchive(spoolPath)
	if err != nil {
		return fmt.Errorf("archive rejected — nothing was written to the database: %w", err)
	}
	// AUD-044 (round 2): restore APPENDS. Applied over a populated target it
	// mixes old and new state and duplicates append-only data, and a rerun
	// after a partial apply doubles everything again. Refuse any nonempty
	// restore table up front — restore into a fresh/isolated database (the
	// documented safe procedure), verify, then cut over. The KV namespace
	// emptiness requirement applies only when the archive carries a KV
	// section: a pre-F45 archive restores tables only and must keep
	// working against an instance that legitimately holds source maps.
	if err := requireEmptyRestoreTarget(ctx, db, len(manifest.KVSections) > 0); err != nil {
		return fmt.Errorf("restore target is not empty — nothing was written: %w", err)
	}
	return applyArchive(ctx, db, spoolPath)
}

// requireEmptyRestoreTarget fails when any restorable table already holds
// rows, or — for archives that carry KV sections — when the KV srcmap
// namespace is nonempty (AUD-044 + F45: the KV section appends like the
// tables do, so a populated namespace would mix old and restored source
// maps). A count check races concurrent writers; restore is an offline
// operation run against a quiesced instance, which the emptiness
// requirement now makes explicit rather than accidental.
func requireEmptyRestoreTarget(ctx context.Context, db *nucleus.Client, kvSections bool) error {
	for _, table := range Tables {
		rows, err := nucleus.Query[struct {
			N int64 `db:"n"`
		}](ctx, db.SQL(), "SELECT COUNT(*) AS n FROM "+table)
		if err != nil {
			// A table the migrations do not create here cannot hold data.
			if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "unknown table") {
				continue
			}
			return fmt.Errorf("inspect target %s: %w", table, err)
		}
		if len(rows) > 0 && rows[0].N > 0 {
			return fmt.Errorf("table %s holds %d rows — restore appends and must run against an empty (isolated) target", table, rows[0].N)
		}
	}
	if !kvSections {
		return nil
	}
	// KV_KEYS cannot see collection keys (upstream; see kvsrcmap.go), so
	// this check covers the string blobs and — via the dump's own
	// site-derivation shape — any site that still has blobs. Orphaned
	// set/zset indexes without blobs are invisible to it; the upstream
	// KV_KEYS fix closes that.
	keys, err := kvListKeys(ctx, db.Pool(), kvSrcmapPattern)
	if err != nil {
		return fmt.Errorf("inspect kv target: %w", err)
	}
	if len(keys) > 0 {
		return fmt.Errorf("kv namespace %s holds %d key(s) — restore appends and must run against an empty (isolated) target", kvSrcmapPattern, len(keys))
	}
	return nil
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
// column names; and — the audit F44 completeness contract — exactly one
// manifest, exactly one trailing results record, no duplicate table entries,
// every observed table declared in the manifest, and every table the results
// claim to have dumped with rows actually present with exactly that many
// rows. A manifest-only or boundary-truncated archive therefore fails
// preflight instead of restoring as a silent partial. Returns the validated
// manifest so the caller can gate target requirements on what the archive
// actually carries.
func validateArchive(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var manifest *Manifest
	var results []TableResult
	manifestCount, resultsCount := 0, 0
	observed := map[string]int64{}
	declared := map[string]bool{}
	kvDeclared := map[string]bool{}
	kvObserved := map[string]int64{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		switch {
		case hdr.Name == manifestName:
			manifestCount++
			if manifestCount > 1 {
				return nil, fmt.Errorf("archive contains more than one manifest")
			}
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("manifest decode: %w", err)
			}
			if m.Version != manifestVersion {
				return nil, fmt.Errorf("backup version %d not supported (expected %d)", m.Version, manifestVersion)
			}
			manifest = &m
		case hdr.Name == resultsName:
			resultsCount++
			if resultsCount > 1 {
				return nil, fmt.Errorf("archive contains more than one completion record")
			}
			if err := json.NewDecoder(tr).Decode(&results); err != nil {
				return nil, fmt.Errorf("results decode: %w", err)
			}
		case hdr.Name == kvSrcmapEntryName:
			// F45: the KV srcmap section is its own entry, validated with
			// the same strictness as table rows — shape-checked line by
			// line, duplicate keys rejected, and the count reconciled
			// against the completion record below.
			if _, dup := kvObserved[kvSrcmapSection]; dup {
				return nil, fmt.Errorf("archive contains duplicate entries for the kv srcmap section")
			}
			rows, err := validateKVRows(tr)
			if err != nil {
				return nil, fmt.Errorf("kv srcmap section: %w", err)
			}
			kvObserved[kvSrcmapSection] = rows
		case strings.HasSuffix(hdr.Name, ".jsonl"):
			table := strings.TrimSuffix(hdr.Name, ".jsonl")
			if !restorableTables[table] {
				return nil, fmt.Errorf("refusing to restore unknown table %q (not in the backup allowlist)", table)
			}
			if _, dup := observed[table]; dup {
				return nil, fmt.Errorf("archive contains duplicate entries for table %q", table)
			}
			rows, err := validateTableRows(tr)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", table, err)
			}
			observed[table] = rows
		}
	}
	if manifest == nil {
		return nil, fmt.Errorf("no manifest found — is this an observe backup?")
	}
	// F44: the completion record is written last by every dump this code has
	// produced for years; an archive without it is either truncated before
	// the dump finished or predates the format, and both must be rejected
	// rather than restored as an unknown-quality partial.
	if resultsCount == 0 {
		return nil, fmt.Errorf("archive is missing its completion record — it is truncated or from an unsupported old format; refusing to guess")
	}

	for _, name := range manifest.Tables {
		if _, dup := declared[name]; dup {
			return nil, fmt.Errorf("manifest declares table %q twice", name)
		}
		declared[name] = true
	}
	// F45: KV sections declared by the manifest form their own declaration
	// domain, with the same bijection contract as tables.
	for _, section := range manifest.KVSections {
		if _, dup := kvDeclared[section]; dup {
			return nil, fmt.Errorf("manifest declares kv section %q twice", section)
		}
		if _, known := kvSections[section]; !known {
			return nil, fmt.Errorf("manifest declares unknown kv section %q", section)
		}
		kvDeclared[section] = true
	}
	var failed []string
	byTable := make(map[string]TableResult, len(results))
	for _, r := range results {
		isKV := r.Table == kvSrcmapResultTable
		if !restorableTables[r.Table] && !isKV {
			return nil, fmt.Errorf("completion record names unknown table %q", r.Table)
		}
		if isKV {
			if !kvDeclared[kvSrcmapSection] {
				return nil, fmt.Errorf("completion record lists the kv srcmap section, which the manifest does not declare")
			}
		} else if !declared[r.Table] {
			// AUD-040 (round 2): a result for a table the manifest does not
			// declare means the archive was reassembled or edited.
			return nil, fmt.Errorf("completion record lists table %q which the manifest does not declare", r.Table)
		}
		if r.Rows < 0 {
			return nil, fmt.Errorf("completion record for %q has a negative row count (%d)", r.Table, r.Rows)
		}
		if _, dup := byTable[r.Table]; dup {
			return nil, fmt.Errorf("completion record lists table %q twice", r.Table)
		}
		if !r.OK {
			failed = append(failed, r.Table)
			// A failed table has no usable entry — still record it so the
			// declared-coverage check below sees the result and the failed
			// list is reported instead of a confusing missing-result error.
			byTable[r.Table] = r
			continue
		}
		byTable[r.Table] = r
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("backup is partial — these tables failed to dump and are missing: %s", strings.Join(failed, ", "))
	}

	// AUD-040 (round 2): every manifest-declared table must carry a
	// completion result — a manifest declaring sites and admin_users with
	// only sites' data/results used to pass, silently leaving
	// admin_users untouched while restore reported success.
	for _, name := range manifest.Tables {
		if _, ok := byTable[name]; !ok {
			return nil, fmt.Errorf("declared table %q has no completion result — the archive is incomplete", name)
		}
	}

	// Observed tables must be declared, and row counts must reconcile with
	// the completion record (truncation inside a table entry is caught by
	// the tar reader; reassembled or edited archives are caught here).
	for table, rows := range observed {
		if !declared[table] {
			return nil, fmt.Errorf("archive contains table %q which its manifest does not declare", table)
		}
		r, ok := byTable[table]
		if !ok {
			return nil, fmt.Errorf("completion record is missing table %q", table)
		}
		if rows != r.Rows {
			return nil, fmt.Errorf("table %q is incomplete: completion record says %d rows, archive holds %d", table, r.Rows, rows)
		}
	}
	// A table the dump claims produced rows MUST have an entry; one with
	// zero rows legitimately has none (the source instance lacked it). The
	// kv srcmap result is excluded here — the kv block below owns its
	// three-way reconciliation.
	for table, r := range byTable {
		if table == kvSrcmapResultTable {
			continue
		}
		if r.Rows > 0 {
			if _, present := observed[table]; !present {
				return nil, fmt.Errorf("table %q is missing from the archive (completion record says %d rows)", table, r.Rows)
			}
		}
	}

	// F45: the kv srcmap section gets the same three-way reconciliation —
	// declared implies a completion result, an observed entry must be
	// declared, and the entry's line count must equal the result's.
	if kvDeclared[kvSrcmapSection] {
		r, ok := byTable[kvSrcmapResultTable]
		if !ok {
			return nil, fmt.Errorf("declared kv section %q has no completion result — the archive is incomplete", kvSrcmapSection)
		}
		if rows, present := kvObserved[kvSrcmapSection]; present {
			if rows != r.Rows {
				return nil, fmt.Errorf("kv section %q is incomplete: completion record says %d keys, archive holds %d", kvSrcmapSection, r.Rows, rows)
			}
		} else if r.Rows > 0 {
			return nil, fmt.Errorf("kv section %q is missing from the archive (completion record says %d keys)", kvSrcmapSection, r.Rows)
		}
	}
	for section := range kvObserved {
		if !kvDeclared[section] {
			return nil, fmt.Errorf("archive contains kv section %q which its manifest does not declare", section)
		}
	}
	return manifest, nil
}

// validateKVRows shape-checks every line of the kv srcmap entry (F45) with
// the same preflight posture as validateTableRows: a malformed key anywhere
// fails the whole archive before any row of any table is applied.
func validateKVRows(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), maxKVValueBytes+(64<<20))
	var n int64
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		n++
		row, err := decodeKVRow(line)
		if err != nil {
			return n, fmt.Errorf("decode row: %w", err)
		}
		if seen[row.Key] {
			return n, fmt.Errorf("duplicate key %q", row.Key)
		}
		seen[row.Key] = true
	}
	return n, scanner.Err()
}

// validateTableRows decodes and structurally checks every row of one table
// entry without touching the database, returning the row count for the
// completeness reconciliation. This is what guarantees a malformed row deep
// inside a large table is caught before ANY row of ANY table has been
// inserted, not merely before the rest of that one table.
func validateTableRows(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20) // up to 16 MiB per row
	var n int64
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		n++
		if _, err := decodeRestoreRow(line); err != nil {
			return n, fmt.Errorf("decode row: %w", err)
		}
	}
	return n, scanner.Err()
}

// decodeRestoreRow decodes one restore line with the STRICT decoder shared
// by the validation and apply passes (AUD-041, round 2): json.Number keeps
// numeric lexemes exact (float64 decoding silently rounded BIGINT-scale
// literals), trailing JSON is rejected, and null/empty/non-object rows are
// rejected instead of surfacing as mid-apply failures after earlier tables
// have already committed.
func decodeRestoreRow(line []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var row map[string]any
	if err := dec.Decode(&row); err != nil {
		return nil, err
	}
	if len(row) == 0 {
		return nil, errors.New("restore row must be a nonempty object (null and {} are not rows)")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON after restore row")
	}
	for k := range row {
		if !validIdent.MatchString(k) {
			return nil, fmt.Errorf("row has unsafe column name %q", k)
		}
	}
	return row, nil
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
		if hdr.Name == kvSrcmapEntryName {
			if err := applyKVSection(ctx, db, tr); err != nil {
				return fmt.Errorf("restore kv srcmap: %w", err)
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
		row, err := decodeRestoreRow(line)
		if err != nil {
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
	// Nucleus's pgwire wants text for BIGINT/JSONB columns. decodeRestoreRow
	// (UseNumber) gives us string|json.Number|bool|map|slice|nil — map/slice
	// need JSON text, and json.Number passes through as its exact lexeme
	// (AUD-041: float64 decoding used to round BIGINT-scale literals).
	switch val := v.(type) {
	case map[string]any, []any:
		raw, _ := json.Marshal(val)
		return string(raw)
	case json.Number:
		return val.String()
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
