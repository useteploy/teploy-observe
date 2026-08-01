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
