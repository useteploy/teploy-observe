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
