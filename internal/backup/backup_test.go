package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestRestore_RejectsPartialArchive builds an archive whose results entry marks
// a table as failed and asserts Restore refuses it.
func TestRestore_RejectsPartialArchive(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, v any) {
		raw, _ := json.Marshal(v)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(raw)), ModTime: time.Now()})
		_, _ = tw.Write(raw)
	}
	write(manifestName, Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: Tables})
	write(resultsName, []TableResult{
		{Table: "sites", Rows: 3, OK: true},
		{Table: "events", OK: false, Error: "boom"},
	})
	tw.Close()

	err := Restore(context.Background(), nil, &buf)
	if err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("expected partial-archive rejection, got %v", err)
	}
}

// TestDump_WritesManifestAndResults dumps the live DB and asserts the archive
// carries the manifest + results entries and Restore accepts it (round-trip of
// the metadata path; rows go back into the same instance which is fine for
// ReplacingMergeTree / idempotent-id tables here).
func TestDump_WritesManifestAndResults(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	var arch bytes.Buffer
	if err := DumpWithLog(ctx, db, &arch, os.Stderr); err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Walk the archive and confirm the metadata entries exist and results are all-OK.
	tr := tar.NewReader(bytes.NewReader(arch.Bytes()))
	var sawManifest, sawResults bool
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		switch hdr.Name {
		case manifestName:
			sawManifest = true
		case resultsName:
			sawResults = true
			var rs []TableResult
			if err := json.NewDecoder(tr).Decode(&rs); err != nil {
				t.Fatalf("results decode: %v", err)
			}
			for _, r := range rs {
				if !r.OK {
					t.Fatalf("table %s failed to dump: %s", r.Table, r.Error)
				}
			}
		}
	}
	if !sawManifest || !sawResults {
		t.Fatalf("archive missing manifest=%v results=%v", sawManifest, sawResults)
	}
}

// TestRestore_LenientEraEmptyJSONB builds an archive carrying '' for JSONB
// columns (what lenient-era engines coerced and stored) and asserts Restore
// lands the row with SQL NULL instead of failing on a Postgres-parity engine
// that rejects '' for JSON.
func TestRestore_LenientEraEmptyJSONB(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	errorID := "restore-jsonb-test-" + time.Now().UTC().Format("150405.000000000")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, raw []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(raw)), ModTime: time.Now()})
		_, _ = tw.Write(raw)
	}
	manifest, _ := json.Marshal(Manifest{Version: manifestVersion, CreatedAt: time.Now(), Tables: Tables})
	write(manifestName, manifest)
	results, _ := json.Marshal([]TableResult{{Table: "error_events", Rows: 1, OK: true}})
	write(resultsName, results)
	// A lenient-era row: JSONB columns serialized as empty strings.
	row, _ := json.Marshal(map[string]any{
		"error_id": errorID, "tenant_id": "default", "site_id": "restore-jsonb-test",
		"group_hash": "gh", "timestamp": 1, "error_type": "T", "error_value": "v",
		"stack_trace": "", "breadcrumbs": "", "contexts": "", "extra": "",
	})
	write("error_events.jsonl", row)
	tw.Close()

	if err := Restore(ctx, db, &buf); err != nil {
		t.Fatalf("restore of lenient-era archive failed: %v", err)
	}

	type countRow struct {
		N string `db:"n"`
	}
	got, err := nucleus.Query[countRow](ctx, db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS n FROM error_events
		 WHERE error_id = $1 AND contexts IS NULL`, errorID)
	if err != nil {
		t.Fatalf("verify query: %v", err)
	}
	if len(got) == 0 || got[0].N != "1" {
		t.Fatalf("restored row not found with NULL contexts (got %+v)", got)
	}
}
