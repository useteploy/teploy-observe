package ingest

// O01 pinning oracle (docs/O01_DURABLE_INGEST_ADR.md §8 references this
// file). These tests pin CURRENT WAL/ack semantics so an implementation
// slice cannot silently regress them. Each test names the
// destination-contract line it guards; when a slice from ADR §5 lands,
// it must UPDATE the corresponding test deliberately in the same change
// and record the era in the ADR.

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

// TestO01_WALAckedAppendIsMemoryOnlyUntilPeriodicSync pins TODAY's
// durability gap on the analytics path: AppendBatch returning success
// (the event Buffer.PushBatch needs before the HTTP handler sends 200
// OK) writes the frame into a 64 KiB userspace bufio — the bytes are in
// NEITHER the file NOR the OS page cache until the periodic fsync loop
// (500 ms in production wiring, cmd/observe/main.go) or a flush/
// checkpoint runs. A crash in this window loses an event the producer
// was told was accepted.
//
// Guards ADR §5.1 ("durable mode = bounded group commit before
// acknowledgment"): when group commit lands, the ack-time assertion
// below FLIPS — the frame must be fsynced before AppendBatch's caller
// can acknowledge. Update this test deliberately in that slice.
func TestO01_WALAckedAppendIsMemoryOnlyUntilPeriodicSync(t *testing.T) {
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

	// The queue BELIEVES the record exists (offset advanced) — that
	// belief is the basis of the HTTP 200. The disk does not have it.
	if raw := readSegmentRaw(t, dir, "events"); frameContains(raw, "o01-acked-not-synced") {
		t.Fatal("CURRENT-semantics pin violated: the appended frame reached the segment file " +
			"before any flush/sync — if group-commit-before-ack landed, flip this assertion and " +
			"record the era in docs/O01_DURABLE_INGEST_ADR.md")
	}
	// Documented loss bound (queue.go durability comment): up to one
	// fsyncInterval of acked events, plus anything still in the bufio.
	// There is deliberately no assertion that recovers the record here —
	// TODAY it is gone for a crashed process.
}

// TestO01_WALPeriodicSyncMakesAppendRecoverable pins the OTHER edge of
// the same window: once the periodic fsync loop has run, the frame is in
// the file, and a process-equivalent restart (re-open) recovers it as
// pending replay output. Together with the test above this pins the
// exact boundary of today's durability: sync-or-lose, per interval.
//
// Guards ADR §5.1's "recovery-tested fsync/checkpoint semantics" line:
// group commit must preserve recoverability while moving the boundary
// earlier (to the ack).
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

// TestO01_BufferAckOrderReturnsBeforeAnyStorage pins the ACK ORDERING of
// the analytics HTTP boundary end to end at the buffer level: PushBatch
// returning true is exactly the condition under which the handler sends
// 200 OK (internal/ingest/handler.go). At that instant the event is (a)
// in the in-memory buffer, (b) NOT in the WAL file on disk (userspace
// bufio), and (c) obviously not in the database — no flush has run, and
// none is synchronous with the request.
//
// Guards ADR §5.1 (the ack must move behind the group fsync) and the
// inventory line "200 OK is sent before every durable hop"
// (O01_DURABLE_INGEST_ADR.md §2.1). A slice that makes PushBatch wait
// for durability must update the (b) assertion in the same change.
func TestO01_BufferAckOrderReturnsBeforeAnyStorage(t *testing.T) {
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

	if !buf.PushBatch([]Event{ev("o01-ack-order")}) {
		t.Fatal("PushBatch must admit — this true is what the handler turns into 200 OK")
	}
	if got := buf.Len(); got != 1 {
		t.Fatalf("admitted event must be memory-buffered, got %d", got)
	}
	if raw := readSegmentRaw(t, dir, "events"); frameContains(raw, "o01-ack-order") {
		t.Fatal("CURRENT-semantics pin violated: the frame was durable in the WAL file at ack time — " +
			"if group-commit-before-ack landed, flip this assertion and record the era in the ADR")
	}
	// The event is pending (admitted, not applied) — the state ADR §5.10
	// counts independently. No Stop() here: the buffer's shutdown flush
	// would hit the nil db; nothing was Started, so there is no worker to
	// leak.
}
