package errors

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

func TestOBS83StartupPoisonQuarantineIsDurableBeforeCheckpoint(t *testing.T) {
	q, err := ingest.NewDiskQueue(t.TempDir(), "errors", time.Hour, 1<<20, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := q.AppendRecords([]json.RawMessage{json.RawMessage(`{"site_id":"","body":null}`)}); err != nil {
		t.Fatal(err)
	}
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	if err := b.AttachQueue(q); err != nil {
		t.Fatal(err)
	}
	if b.Stats().Quarantined != 1 {
		t.Fatalf("poison count = %+v", b.Stats())
	}
	data, err := os.ReadFile(filepath.Join(q.Dir(), "quarantine.log"))
	if err != nil || len(data) == 0 {
		t.Fatalf("spool = %q, %v", data, err)
	}
	remaining := 0
	if err := q.StreamRecordFrames(func(r []json.RawMessage, _ int64) error { remaining += len(r); return nil }); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("durably quarantined record replayed: %d", remaining)
	}
}

func TestOBS83FailedStartupQuarantinePreservesWAL(t *testing.T) {
	q, err := ingest.NewDiskQueue(t.TempDir(), "errors", time.Hour, 1<<20, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := q.AppendRecords([]json.RawMessage{json.RawMessage(`{"site_id":"","body":null}`)}); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(q.Dir(), "quarantine.log")
	if err := os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	if err := b.AttachQueue(q); err == nil {
		t.Fatal("undurable poison was accepted")
	}
	if b.queue != nil || b.Stats().Quarantined != 0 {
		t.Fatalf("failed attach installed final disposition: %+v", b.Stats())
	}
	remaining := 0
	if err := q.StreamRecordFrames(func(r []json.RawMessage, _ int64) error { remaining += len(r); return nil }); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("failed spool lost WAL record: %d", remaining)
	}
	if err := os.Remove(spool); err != nil {
		t.Fatal(err)
	}
	if err := b.AttachQueue(q); err != nil {
		t.Fatalf("repair did not permit recovery: %v", err)
	}
}

func TestOBS83RuntimePoisonRemainsPendingOnSpoolFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "quarantine.log"), 0700); err != nil {
		t.Fatal(err)
	}
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	b.quarantineDir = dir
	if b.applyOne(context.Background(), bufferedError{Body: []byte(`{invalid`), Site: "site"}) {
		t.Fatal("spool failure became final disposition")
	}
	if b.Stats().Quarantined != 0 {
		t.Fatal("undurable quarantine counted as final")
	}
}
