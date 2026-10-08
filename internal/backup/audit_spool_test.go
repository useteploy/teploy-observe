package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type kvSpoolRunner struct {
	payload string
	mutate  bool
	gets    int
}
type kvSpoolRow func(...any) error

func (f kvSpoolRow) Scan(dst ...any) error { return f(dst...) }
func (r *kvSpoolRunner) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("unused")
}
func (r *kvSpoolRunner) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("unused")
}
func (r *kvSpoolRunner) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	return kvSpoolRow(func(dst ...any) error {
		switch {
		case strings.Contains(q, "KV_KEYS"):
			keys := make([]string, 32)
			for i := range keys {
				keys[i] = fmt.Sprintf("srcmap:site:r:f%03d", i)
			}
			b, _ := json.Marshal(keys)
			raw := string(b)
			*(dst[0].(**string)) = &raw
		case strings.Contains(q, "KV_GET"):
			r.gets++
			raw := r.payload
			if r.mutate {
				raw += fmt.Sprint(r.gets)
			}
			*(dst[0].(**string)) = &raw
		case strings.Contains(q, "KV_SMEMBERS"), strings.Contains(q, "KV_ZRANGE"):
			*(dst[0].(*string)) = "[]"
		default:
			return fmt.Errorf("unexpected query %s", q)
		}
		return nil
	})
}

func TestKVNamespaceSnapshotsSpoolPayloadsAndCleanUp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	runner := &kvSpoolRunner{payload: strings.Repeat("x", 256<<10)}
	snap, err := stableKVSnapshot(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.keys) != 32 {
		t.Fatalf("keys=%d", len(snap.keys))
	}
	info, err := os.Stat(snap.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 8<<20 || info.Mode().Perm() != 0600 {
		t.Fatalf("invalid spool: %v", info)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "observe-backup-kv-srcmap-*"))
	if len(files) != 1 {
		t.Fatalf("unused snapshots leaked: %v", files)
	}
	f, err := os.Open(snap.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var count int
	if err := eachKVLine(f, func(line kvLine) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 32 {
		t.Fatalf("lost rows: %d", count)
	}
	os.Remove(snap.path)
	runner.mutate = true
	if _, err := stableKVSnapshot(context.Background(), runner); err == nil {
		t.Fatal("changing payload accepted")
	}
	files, _ = filepath.Glob(filepath.Join(tmp, "observe-backup-kv-srcmap-*"))
	if len(files) != 0 {
		t.Fatalf("failed convergence leaked spools: %v", files)
	}
}
