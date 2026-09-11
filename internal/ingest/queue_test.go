package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testQueue(t *testing.T) *DiskQueue {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Long fsync interval so the background loop never races the test's own
	// checkpoint; maxBytes huge so maybeCompact never truncates mid-test.
	q, err := NewDiskQueue(t.TempDir(), "events", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	return q
}

func ev(id string) Event { return Event{EventID: id, SiteID: "s1", Timestamp: 1} }

// TestDiskQueue_CheckpointBoundsToInsertedBatch is the H7 regression: the old
// Checkpoint() advanced to the live offset, so events appended after a batch was
// taken (but before it was checkpointed) were marked durable and dropped on
// crash. Checkpoint(target) must only mark the inserted batch durable.
func TestDiskQueue_CheckpointBoundsToInsertedBatch(t *testing.T) {
	q := testQueue(t)

	// Batch one: events 1..3. Capture the offset after the 3rd — that's the
	// checkpoint target for this batch.
	var batchTarget int64
	for _, id := range []string{"e1", "e2", "e3"} {
		off, err := q.Append(ev(id))
		if err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		batchTarget = off
	}
	// Events 4..5 arrive AFTER the batch was taken but before its checkpoint.
	for _, id := range []string{"e4", "e5"} {
		if _, err := q.Append(ev(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	// Only the first batch was inserted into the DB.
	if err := q.Checkpoint(batchTarget); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a crash + restart: reopen and replay.
	q2 := testQueue2(t, q)
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	got := ids(pending)
	want := []string{"e4", "e5"}
	if !equal(got, want) {
		t.Fatalf("pending after reopen = %v, want %v (events past the checkpointed batch must survive)", got, want)
	}
}

// TestDiskQueue_CheckpointMonotonic verifies a stale/out-of-order checkpoint
// cannot roll the durability point backward.
func TestDiskQueue_CheckpointMonotonic(t *testing.T) {
	q := testQueue(t)

	var off1, offLast int64
	for i, id := range []string{"e1", "e2", "e3"} {
		off, err := q.Append(ev(id))
		if err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		if i == 0 {
			off1 = off
		}
		offLast = off
	}

	// Checkpoint everything, then a stale checkpoint to an earlier offset.
	if err := q.Checkpoint(offLast); err != nil {
		t.Fatalf("checkpoint last: %v", err)
	}
	if err := q.Checkpoint(off1); err != nil {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	q2 := testQueue2(t, q)
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after full checkpoint = %v, want empty (stale checkpoint must not roll back)", ids(pending))
	}
}

// TestDiskQueue_CheckpointClampsToOffset verifies a target beyond the written
// region is clamped, never skipping past un-written bytes.
func TestDiskQueue_CheckpointClampsToOffset(t *testing.T) {
	q := testQueue(t)
	off, err := q.Append(ev("e1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Checkpoint(off + 10_000); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2 := testQueue2(t, q)
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want empty", ids(pending))
	}
}

// TestDiskQueue_ConcurrentAppendCheckpoint is the H8 race regression: Checkpoint
// used to flush the bufio.Writer without the mutex, racing Append. Run with
// -race to catch the data race.
func TestDiskQueue_ConcurrentAppendCheckpoint(t *testing.T) {
	q := testQueue(t)
	defer q.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if _, err := q.Append(ev("e")); err != nil {
				t.Errorf("append: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if err := q.Checkpoint(q.Offset()); err != nil {
				t.Errorf("checkpoint: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestDiskQueue_AppendAfterCheckpointIsStillFlushed is the observe-01
// regression: the checkpoint-file write used to run after q.mu was released,
// so an Append landing between the flush and the write was marked clean
// (dirtySinceFlush=false) and the background fsync loop then SKIPPED bytes it
// had never flushed. An append after a checkpoint must reach the file within
// one fsyncInterval without any help from Close.
func TestDiskQueue_AppendAfterCheckpointIsStillFlushed(t *testing.T) {
	interval := 25 * time.Millisecond
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "events", interval, 1<<30, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}

	off, err := q.Append(ev("e1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Checkpoint(off); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := q.Append(ev("e2")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Quiet period: only the fsync loop can flush now. Then simulate a
	// crash by reading the file as it sits on disk — no Close().
	time.Sleep(5 * interval)
	raw, err := os.ReadFile(filepath.Join(dir, "events", "current.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	_ = q.Close()
	if !bytes.Contains(raw, []byte(`"event_id":"e2"`)) {
		t.Fatalf("append after a checkpoint was not flushed by the fsync loop within %s: %q", interval, raw)
	}
}

// TestDiskQueue_ConcurrentAppendCheckpointNoStrandedBytes pins the observe-01
// invariant: once appends and checkpoints have both quiesced, every byte past
// the checkpoint must still be awaiting a flush (dirty). A cleared dirty flag
// with bytes past the checkpoint means the fsync loop would skip them.
func TestDiskQueue_ConcurrentAppendCheckpointNoStrandedBytes(t *testing.T) {
	q := testQueue(t)
	defer q.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1500; i++ {
			if _, err := q.Append(ev("e")); err != nil {
				t.Errorf("append: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 150; i++ {
			if err := q.Checkpoint(q.Offset()); err != nil {
				t.Errorf("checkpoint: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	q.mu.Lock()
	dirty := q.dirtySinceFlush
	cp, off := q.checkpoint, q.offset
	q.mu.Unlock()
	if cp < off && !dirty {
		t.Fatalf("bytes past the checkpoint (offset %d, checkpoint %d) are not flagged dirty — the fsync loop would skip them", off, cp)
	}
}

// TestDiskQueue_CompactRotationKeepsNewRecordsReplayable is the observe-02
// rotation regression: compaction used to truncate the log and only then
// remove the checkpoint file, so a crash in between left a durable checkpoint
// pointing past an empty/replacement log. Rotation must clear the checkpoint
// first; a crash at either boundary still leaves new valid records replayable.
func TestDiskQueue_CompactRotationKeepsNewRecordsReplayable(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const maxBytes = 512
	q, err := NewDiskQueue(dir, "events", time.Hour, maxBytes, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	// Append past maxBytes, then checkpoint everything: this is the
	// checkpoint that triggers compaction.
	var off int64
	for i := 0; q.Offset() <= maxBytes; i++ {
		off, err = q.Append(ev(fmt.Sprintf("old%d", i)))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := q.Checkpoint(off); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if got := q.Offset(); got != 0 {
		t.Fatalf("compaction must reset the log, offset=%d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "events", "checkpoint")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("compaction must remove the checkpoint file, stat err=%v", err)
	}
	// New records land in the fresh log and must survive a restart.
	if _, err := q.Append(ev("post1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := q.Append(ev("post2")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2 := testQueue2(t, q)
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got, want := ids(pending), []string{"post1", "post2"}; !equal(got, want) {
		t.Fatalf("pending after rotation = %v, want %v", got, want)
	}
}

// TestDiskQueue_OpenClampsCheckpointPastLog is the observe-02 load-time
// guard: a checkpoint pointing beyond the actual log (the on-disk state a
// crash mid-rotation, or anything else, can leave) must never be replayed
// from — that seeks into mid-record bytes and parses garbage. It replays from
// the start instead.
func TestDiskQueue_OpenClampsCheckpointPastLog(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "events")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Poison state: empty log, checkpoint far past its length.
	if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte("500"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "events", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("open with poisoned checkpoint: %v", err)
	}
	q.mu.Lock()
	cp := q.checkpoint
	q.mu.Unlock()
	if cp != 0 {
		t.Fatalf("checkpoint past log length must clamp to 0 on load, got %d", cp)
	}
	// Grow the fresh log past the stale offset: replay must return exactly
	// the new records, each parsing cleanly — never a merged mid-record read.
	var appended []string
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("new%d", i)
		if _, err := q.Append(ev(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		appended = append(appended, id)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2 := testQueue2(t, q)
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got := ids(pending); !equal(got, appended) {
		t.Fatalf("pending = %v, want %v (stale checkpoint must not eat into fresh records)", got, appended)
	}
}

// --- helpers ---

// testQueue2 reopens the same directory the given (closed) queue used, simulating
// a process restart against the persisted WAL + checkpoint.
func testQueue2(t *testing.T, prev *DiskQueue) *DiskQueue {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// prev.dir is "<tmp>/events"; reopen with the parent + name to hit the same path.
	q, err := NewDiskQueue(filepath.Dir(prev.dir), "events", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("reopen DiskQueue: %v", err)
	}
	return q
}

func ids(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
