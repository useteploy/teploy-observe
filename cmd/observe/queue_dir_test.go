package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The legacy ./queue fallback is kept, but choosing it must be visible: it
// depends on the working directory the process happened to start in.
func TestResolveQueueDir(t *testing.T) {
	logged := func() (*slog.Logger, *bytes.Buffer) {
		var b bytes.Buffer
		return slog.New(slog.NewTextHandler(&b, nil)), &b
	}

	t.Run("explicit queue dir wins, silently", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.Mkdir("queue", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OBSERVE_QUEUE_DIR", "/explicit/q")
		t.Setenv("OBSERVE_DATA_DIR", "")
		l, b := logged()
		if got := resolveQueueDir(l); got != "/explicit/q" {
			t.Fatalf("got %q", got)
		}
		if b.Len() != 0 {
			t.Fatalf("unexpected log: %s", b)
		}
	})

	t.Run("data dir set ignores a working-directory queue", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.Mkdir("queue", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OBSERVE_QUEUE_DIR", "")
		t.Setenv("OBSERVE_DATA_DIR", "/srv/observe")
		l, b := logged()
		if got := resolveQueueDir(l); got != filepath.Join("/srv/observe", "queue") {
			t.Fatalf("got %q", got)
		}
		if b.Len() != 0 {
			t.Fatalf("unexpected log: %s", b)
		}
	})

	t.Run("no legacy queue uses the default data dir", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("OBSERVE_QUEUE_DIR", "")
		t.Setenv("OBSERVE_DATA_DIR", "")
		l, b := logged()
		if got := resolveQueueDir(l); got != filepath.Join("./data", "queue") {
			t.Fatalf("got %q", got)
		}
		if b.Len() != 0 {
			t.Fatalf("unexpected log: %s", b)
		}
	})

	t.Run("legacy queue in the working directory is kept and logged", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.Mkdir("queue", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OBSERVE_QUEUE_DIR", "")
		t.Setenv("OBSERVE_DATA_DIR", "")
		l, b := logged()
		if got := resolveQueueDir(l); got != "queue" {
			t.Fatalf("got %q", got)
		}
		out := b.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "legacy ingest WAL directory") || !strings.Contains(out, "queue_dir=queue") {
			t.Fatalf("missing or unclear log line: %s", out)
		}
	})
}
