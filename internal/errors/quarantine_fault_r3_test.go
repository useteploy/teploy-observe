package errors

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type r3FaultFile struct {
	*os.File
	fault string
}

func (f *r3FaultFile) Write(p []byte) (int, error) {
	if f.fault == "short" {
		return f.File.Write(p[:len(p)/2])
	}
	if f.fault == "write" {
		_, _ = f.File.Write(p[:len(p)/2])
		return len(p) / 2, io.ErrClosedPipe
	}
	return f.File.Write(p)
}
func (f *r3FaultFile) Sync() error {
	if f.fault == "file-sync" {
		return stderrors.New("injected file sync failure")
	}
	return f.File.Sync()
}
func r3SpoolFault(b *ErrorBuffer, fault string) {
	b.quarantineIO = &quarantineIO{
		open: func(p string) (quarantineFile, error) {
			f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				return nil, err
			}
			return &r3FaultFile{f, fault}, nil
		},
		syncDir: func(p string) error {
			if fault == "dir-sync" {
				return stderrors.New("injected directory sync failure")
			}
			return syncQuarantineDir(p)
		},
	}
}
func TestOBS83RepeatedSpoolFaultKeepsMixedFrameAcrossSecondReopen(t *testing.T) {
	for _, fault := range []string{"short", "write", "file-sync", "dir-sync", "full"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			q := r3Queue(t, root)
			// Body is valid envelope JSON but invalid ErrorInput; runtime poison.
			poison, _ := json.Marshal(errorRecord{SiteID: "site", Body: json.RawMessage(`42`)})
			if _, err := q.AppendRecords([]json.RawMessage{r3Envelope(t, "final"), poison}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(q.Dir(), "quarantine.log")
			if fault == "full" {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(maxQuarantineBytes); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			makeBuffer := func() *ErrorBuffer {
				b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
				b.apply = func(_ context.Context, ev bufferedError) bool {
					var in ErrorInput
					if err := json.Unmarshal(ev.Body, &in); err != nil {
						return b.quarantine(ev, err)
					}
					return true
				}
				r3SpoolFault(b, fault)
				return b
			}
			b := makeBuffer()
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				b.Flush()
			}
			if b.Stats().Pending != 1 || b.Stats().Quarantined != 0 || r3Remaining(t, q) != 2 {
				t.Fatalf("fault reached FINAL: %+v", b.Stats())
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			wantSize := int64(0)
			if fault == "full" {
				wantSize = maxQuarantineBytes
			}
			if info.Size() != wantSize {
				t.Fatalf("failed retries consumed spool: %d", info.Size())
			}
			for restart := 0; restart < 2; restart++ {
				if err := q.Close(); err != nil {
					t.Fatal(err)
				}
				q = r3Queue(t, root)
				b = makeBuffer()
				if err := b.AttachQueue(q); err != nil {
					t.Fatal(err)
				}
				if b.Stats().Pending != 1 || r3Remaining(t, q) != 2 {
					t.Fatal("second reopen lost poison obligation")
				}
			}
			defer q.Close()
			b.quarantineIO = nil
			if fault == "full" {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			b.Flush()
			if b.Stats().Pending != 0 || b.Stats().Quarantined != 1 || r3Remaining(t, q) != 0 {
				t.Fatalf("repair failed: %+v", b.Stats())
			}
			info, err = os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private spool: %v %v", info, err)
			}
		})
	}
}

func TestOBS83StartupEnvelopeSpoolFaultRefusesBeforeCheckpoint(t *testing.T) {
	for _, fault := range []string{"short", "write", "file-sync", "dir-sync"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			q := r3Queue(t, root)
			if _, err := q.AppendRecords([]json.RawMessage{r3Envelope(t, "final"), r3Envelope(t, "poison")}); err != nil {
				t.Fatal(err)
			}
			for restart := 0; restart < 3; restart++ {
				b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
				b.apply = func(context.Context, bufferedError) bool { return true }
				r3SpoolFault(b, fault)
				if err := b.AttachQueue(q); err == nil {
					t.Fatal("undurable envelope quarantine attached")
				}
				if b.queue != nil || b.Stats().Quarantined != 0 || b.Stats().Bytes != 0 || r3Remaining(t, q) != 2 {
					t.Fatalf("failed startup retired frame: %+v", b.Stats())
				}
				if err := q.Close(); err != nil {
					t.Fatal(err)
				}
				q = r3Queue(t, root)
			}
			defer q.Close()
			b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
			b.apply = func(context.Context, bufferedError) bool { return true }
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			if b.Stats().Quarantined != 1 || r3Remaining(t, q) != 0 {
				t.Fatalf("startup repair failed: %+v", b.Stats())
			}
		})
	}
}
