package ingest

// O01 pinning oracle (docs/O01_DURABLE_INGEST_ADR.md references this
// file). These tests pin the WAL/ack semantics so a later implementation
// slice cannot silently regress them. Each test names the
// destination-contract line it guards; a slice that changes semantics must
// UPDATE the corresponding test deliberately in the same change and record
// the era in the ADR.
//
// ERA 2 (2026-09-22, slice 1 — group commit): durable mode is the default.
// The buffer-level ack (PushBatch nil) now WAITS for the group fsync, and
// the disk high-water REFUSES instead of deleting (lossy mode is the
// explicit opt-in that keeps era-1 semantics). The three era-1 pins were
// updated in the same change as the implementation:
//
//   - WALAckedAppendIsMemoryOnlyUntilPeriodicSync  -> WALAppendWithoutWaitIsNotADurabilityBoundary
//   - WALPeriodicSyncMakesAppendRecoverable        -> kept (fsyncLoop is now the backstop)
//   - BufferAckOrderReturnsBeforeAnyStorage        -> BufferAckOrderWaitsForGroupCommit

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readSegmentRaw reads the active segment file WITHOUT going through the
// DiskQueue (whose Pending/StreamPending would flush its userspace
// buffer first). This is the crash-equivalent view: what a process
// restart (or a post-mortem disk image) would actually see.
func readSegmentRaw(t *testing.T, dir, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name, legacySegmentName))
	if err != nil {
		t.Fatalf("read segment raw: %v", err)
	}
	return raw
}

func frameContains(raw []byte, id string) bool {
	var probe struct {
		Events []Event `json:"events"`
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		for _, e := range probe.Events {
			if e.EventID == id {
				return true
			}
		}
	}
	return false
}

// TestO01_WALAppendWithoutWaitIsNotADurabilityBoundary pins the ASYNC
// primitive: AppendBatch enqueues into the 64 KiB userspace bufio and
// returns — the bytes are in NEITHER the file NOR the OS page cache until
// a sync path runs. In era 2 this is still true BY DESIGN: the durability
// boundary is the caller's WaitCommit (group commit), and lossy mode
// (ADR §5.3) deliberately rides this unsynced window as its declared loss
// budget. Nobody may treat AppendBatch's success alone as durability.
//
// Guards ADR §5.1's "the periodic loop remains a backstop, not the
// durability boundary" line (inverted: the enqueue is explicitly NOT the
// boundary). Era-1 form (WALAckedAppendIsMemoryOnlyUntilPeriodicSync)
// pinned the ACK on this window; era 2 moved the ack behind WaitCommit —
// see TestO01_GroupCommit_AckWaitsForFsync in o01_group_commit_test.go.
func TestO01_WALAppendWithoutWaitIsNotADurabilityBoundary(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Interval long enough that no periodic sync fires during the test —
	// the window under inspection is "append succeeded, sync not yet due".
	q, err := NewDiskQueueWithLimits(dir, "events", time.Hour, 1<<20, 8<<20, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	defer q.Close()

	off, err := q.AppendBatch([]Event{ev("o01-acked-not-synced")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if off == 0 {
		t.Fatal("append must advance the WAL offset")
	}

	// The queue BELIEVES the record exists (offset advanced). The disk
	// does not have it — an ack issued here (the lossy-mode window) is not
	// backed by anything.
	if raw := readSegmentRaw(t, dir, "events"); frameContains(raw, "o01-acked-not-synced") {
		t.Fatal("pin violated: the appended frame reached the segment file " +
			"before any flush/sync — AppendBatch must stay an async enqueue, or " +
			"the lossy-mode loss-budget documentation (ADR §5.3) is a lie")
	}
}

// TestO01_WALPeriodicSyncMakesAppendRecoverable pins the OTHER edge of the
// same window: once the periodic fsync loop has run, the frame is in the
// file, and a process-equivalent restart (re-open) recovers it as pending
// replay output.
//
// Guards ADR §5.1's "recovery-tested fsync/checkpoint semantics" line: the
// group-commit committer must preserve recoverability (it reuses the same
// flush+fsync under the same lock as this loop and Checkpoint). In era 2
// the loop is the BACKSTOP for bytes nobody waits on (lossy mode, or a
// waiter lost to shutdown) — still load-bearing.
func TestO01_WALPeriodicSyncMakesAppendRecoverable(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueueWithLimits(dir, "events", 50*time.Millisecond, 1<<20, 8<<20, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := q.AppendBatch([]Event{ev("o01-synced")}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Wait for the periodic loop to flush+fsync (bounded; it ticks every
	// 50 ms). Poll the RAW file — Pending() would flush userspace bytes
	// itself and prove nothing about the loop.
	deadline := time.Now().Add(5 * time.Second)
	synced := false
	for time.Now().Before(deadline) {
		if frameContains(readSegmentRaw(t, dir, "events"), "o01-synced") {
			synced = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !synced {
		t.Fatal("periodic fsync loop did not land the frame within the bound")
	}

	// Crash-equivalent restart: nothing was appended after the sync, so
	// Close()'s graceful flush adds no bytes — closing here is
	// indistinguishable from a crash that happened after the sync. A
	// fresh process then reopens the same WAL and replays the
	// uncheckpointed record.
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2, err := NewDiskQueueWithLimits(dir, "events", time.Hour, 1<<20, 8<<20, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got := ids(pending); !equal(got, []string{"o01-synced"}) {
		t.Fatalf("restart must replay the synced-but-uncheckpointed record, got %v", got)
	}
}

// TestO01_BufferAckOrderWaitsForGroupCommit pins the ACK ORDERING of the
// analytics HTTP boundary end to end at the buffer level: PushBatch
// returning nil is exactly the condition under which the handler sends
// 200 OK (internal/ingest/handler.go). At that instant the event is (a)
// in the in-memory buffer, (b) IN THE WAL FILE ON DISK — the group-commit
// fsync covering its frame completed before the return — and (c) not yet
// in the database (flush is still asynchronous).
//
// Guards ADR §5.1 ("the HTTP 200 is sent only after the fsync covering the
// request's frame returns"). Era-1 form (BufferAckOrderReturnsBeforeAny-
// Storage) asserted (b) was FALSE at ack time; era 2 flips it — this is
// the deliberate pin update recorded in the ADR's era log.
func TestO01_BufferAckOrderWaitsForGroupCommit(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueueWithLimits(dir, "events", time.Hour, 1<<20, 8<<20, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	defer q.Close()

	buf := NewBuffer(nil, 1000, 100, time.Hour, logger) // nil db: SQL is never reachable in this test
	if err := buf.AttachQueue(q); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := buf.PushBatch([]Event{ev("o01-ack-order")}); err != nil {
		t.Fatal("PushBatch must admit — this nil is what the handler turns into 200 OK")
	}
	if got := buf.Len(); got != 1 {
		t.Fatalf("admitted event must be memory-buffered, got %d", got)
	}
	if raw := readSegmentRaw(t, dir, "events"); !frameContains(raw, "o01-ack-order") {
		t.Fatal("durable-ack pin violated: at PushBatch return the frame was NOT in the WAL file — " +
			"an ack issued now would be memory-only (the era-1 condition). If the ordering changed " +
			"deliberately, update this pin and record the era in docs/O01_DURABLE_INGEST_ADR.md")
	}
	// The event is pending (admitted, not applied) — the state ADR §5.10
	// counts independently. No Stop() here: the buffer's shutdown flush
	// would hit the nil db; nothing was Started, so there is no worker to
	// leak.
}
