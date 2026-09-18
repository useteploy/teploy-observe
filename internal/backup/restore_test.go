package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func writeTarArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func validManifest(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: Tables})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRestore_RejectsUnknownTable ensures a table name outside the backup
// allowlist is rejected during validation — before any database write is
// attempted (nil db proves it: the validation pass alone must reject this).
func TestRestore_RejectsUnknownTable(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:               validManifest(t),
		"admin_users_shadow.jsonl": []byte(`{"id":"x"}` + "\n"),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "not in the backup allowlist") {
		t.Fatalf("expected unknown-table rejection, got %v", err)
	}
}

// TestRestore_RejectsMalformedRow ensures a structurally invalid JSON row is
// caught during validation (nil db proves no insert was attempted).
func TestRestore_RejectsMalformedRow(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  validManifest(t),
		"sites.jsonl": []byte(`{"site_id": "a", "not closed`),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "decode row") {
		t.Fatalf("expected malformed-row rejection, got %v", err)
	}
}

// TestRestore_RejectsUnsafeColumnName ensures a row whose JSON keys don't
// match the safe-identifier charset is rejected — those keys are
// interpolated directly into an INSERT statement, so this is a SQL-injection
// boundary, not just a correctness check.
func TestRestore_RejectsUnsafeColumnName(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  validManifest(t),
		"sites.jsonl": []byte(`{"site_id; DROP TABLE sites;--": "a"}` + "\n"),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "unsafe column name") {
		t.Fatalf("expected unsafe-column rejection, got %v", err)
	}
}

// TestRestore_RejectsMissingManifest ensures an archive with no manifest at
// all is rejected (nil db proves no insert was attempted, even though this
// archive's one row would otherwise be well-formed).
func TestRestore_RejectsMissingManifest(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "no manifest found") {
		t.Fatalf("expected missing-manifest rejection, got %v", err)
	}
}

// TestRestore_RejectsUnsupportedVersion ensures a manifest declaring an
// incompatible version is rejected.
func TestRestore_RejectsUnsupportedVersion(t *testing.T) {
	raw, _ := json.Marshal(Manifest{Version: manifestVersion + 99, CreatedAt: time.Now(), Tables: Tables})
	archive := writeTarArchive(t, map[string][]byte{manifestName: raw})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected version rejection, got %v", err)
	}
}

// TestRestore_ValidationCatchesLateRowBeforeAnyApply is the sharpest proof of
// the OBS-020/021 fix: a malformed row in the SECOND of two tables (i.e.
// appearing well into the archive, after an earlier table's rows would, in
// the old streaming-apply design, already have been inserted) still means
// nothing is ever applied — validation runs over the WHOLE archive first.
// nil db proves this: if the old table's rows had been applied before the
// second table's bad row was hit, this test would panic on a nil pointer
// dereference instead of returning a clean validation error.
func TestRestore_ValidationCatchesLateRowBeforeAnyApply(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  validManifest(t),
		"sites.jsonl": []byte(`{"site_id": "good-row"}` + "\n"),
		"users.jsonl": []byte(`{"id": "bad-row", unterminated`),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "decode row") {
		t.Fatalf("expected the second table's malformed row to abort validation before any apply, got %v", err)
	}
}

// validResults builds a completion record for the given tables (all OK).
func validResults(t *testing.T, rs []TableResult) []byte {
	t.Helper()
	raw, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRestore_RejectsManifestOnlyArchive is the audit F44 regression: a
// manifest-only (or boundary-truncated) archive passed the old validation
// because every check was conditional on the entries present. The completion
// record is now required, so truncation anywhere before it fails preflight.
func TestRestore_RejectsManifestOnlyArchive(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName: validManifest(t),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "missing its completion record") {
		t.Fatalf("expected missing-completion-record rejection, got %v", err)
	}
}

// TestRestore_RejectsRowCountMismatch: a completion record claiming rows the
// archive does not hold (reassembled or edited archive) is rejected.
func TestRestore_RejectsRowCountMismatch(t *testing.T) {
	manifest := func(t *testing.T) []byte {
		raw, err := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: []string{"sites"}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}(t)
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  manifest,
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: 7, OK: true}}),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected row-count reconciliation rejection, got %v", err)
	}
}

// TestRestore_RejectsMissingTableWithClaimedRows: the completion record says
// a table was dumped with rows, but the archive lacks the entry.
func TestRestore_RejectsMissingTableWithClaimedRows(t *testing.T) {
	manifest := func(t *testing.T) []byte {
		raw, err := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: []string{"sites"}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}(t)
	archive := writeTarArchive(t, map[string][]byte{
		manifestName: manifest,
		resultsName:  validResults(t, []TableResult{{Table: "sites", Rows: 3, OK: true}}),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "missing from the archive") {
		t.Fatalf("expected missing-table rejection, got %v", err)
	}
}

// TestRestore_RejectsDuplicateTableEntry: two .jsonl entries for one table
// (an archive reassembled by concatenation) are rejected.
func TestRestore_RejectsDuplicateTableEntry(t *testing.T) {
	sites := []byte(`{"site_id": "a"}` + "\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{manifestName, "sites.jsonl", "sites.jsonl"} {
		data := validManifest(t)
		if name == "sites.jsonl" {
			data = sites
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := Restore(context.Background(), nil, bytes.NewReader(buf.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "duplicate entries") {
		t.Fatalf("expected duplicate-entry rejection, got %v", err)
	}
}

// TestRestore_RejectsUndeclaredTable: an observed table the manifest does
// not declare is rejected even though it is allowlisted.
func TestRestore_RejectsUndeclaredTable(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  validManifest(t),
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: 1, OK: true}}),
	})
	// Replace the manifest with one declaring no tables at all.
	empty, _ := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now()})
	entries := map[string][]byte{
		manifestName:  empty,
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: 1, OK: true}}),
	}
	_ = archive
	archive = writeTarArchive(t, entries)
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("expected undeclared-table rejection, got %v", err)
	}
}

func miniManifest(t *testing.T, tables ...string) []byte {
	t.Helper()
	raw, err := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: tables})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// AUD-040 (round 2): a manifest declaring sites and admin_users with only
// sites' data/results used to pass validation — the declared-to-result
// direction was missing, so the omitted table restored as a silent no-op.
func TestRestore_RejectsDeclaredTableWithoutResult(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  miniManifest(t, "sites", "admin_users"),
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: 1, OK: true}}),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "admin_users") {
		t.Fatalf("expected declared-table-without-result rejection naming admin_users, got %v", err)
	}
}

// AUD-040: a result for a table the manifest does not declare is a
// reassembled/edited archive.
func TestRestore_RejectsUndeclaredResult(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  miniManifest(t, "sites"),
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName: validResults(t, []TableResult{
			{Table: "sites", Rows: 1, OK: true},
			{Table: "admin_users", Rows: 0, OK: true},
		}),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("expected undeclared-result rejection, got %v", err)
	}
}

// AUD-040: negative row counts are corruption, not data.
func TestRestore_RejectsNegativeRowCount(t *testing.T) {
	archive := writeTarArchive(t, map[string][]byte{
		manifestName:  miniManifest(t, "sites"),
		"sites.jsonl": []byte(`{"site_id": "a"}` + "\n"),
		resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: -3, OK: true}}),
	})
	err := Restore(context.Background(), nil, bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("expected negative-count rejection, got %v", err)
	}
}

// AUD-041 (round 2): null, empty objects, and trailing documents are
// rejected in preflight — they used to decode successfully and fail only
// mid-apply, after earlier tables had committed.
func TestRestore_RejectsMalformedRowShapes(t *testing.T) {
	for name, line := range map[string]string{
		"null row":        "null\n",
		"empty object":    "{}\n",
		"trailing doc":    `{"site_id":"a"} {"site_id":"b"}` + "\n",
		"array row":       `[1,2]` + "\n",
		"unsafe column":   `{"site_id\":\"a\",\"x-y\":1}` + "\n",
	} {
		archive := writeTarArchive(t, map[string][]byte{
			manifestName:  miniManifest(t, "sites"),
			"sites.jsonl": []byte(line),
			resultsName:   validResults(t, []TableResult{{Table: "sites", Rows: 1, OK: true}}),
		})
		err := Restore(context.Background(), nil, bytes.NewReader(archive))
		if err == nil {
			t.Fatalf("%s must be rejected in preflight", name)
		}
	}
}

// AUD-041: numeric lexemes survive decode exactly (float64 used to round
// BIGINT-scale literals).
func TestDecodeRestoreRow_PreservesBigNumbers(t *testing.T) {
	row, err := decodeRestoreRow([]byte(`{"n": 9007199254740993, "s": "x"}`))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := row["n"].(json.Number)
	if !ok || n.String() != "9007199254740993" {
		t.Fatalf("big integer must round-trip exactly, got %#v", row["n"])
	}
	if got := formatValue(row["n"], false); got != "9007199254740993" {
		t.Fatalf("formatValue must emit the exact lexeme, got %#v", got)
	}
}
