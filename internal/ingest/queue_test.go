package ingest

import (
	"io"
	"log/slog"
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
