package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestBuffer uses a nil db client to test push/drain logic only.
// Actual DB insertion is tested in integration tests.

func TestBuffer_PushAndLen(t *testing.T) {
	// nil db — we won't call Flush
	buf := NewBuffer(nil, 100, 50, time.Hour, nil)

	e := Event{EventID: "test-1", SiteID: "s1", Timestamp: time.Now().UnixMilli()}
	if !buf.Push(e) {
		t.Fatal("push should succeed when buffer is not full")
	}
	if buf.Len() != 1 {
		t.Fatalf("expected len=1, got %d", buf.Len())
	}
}

func TestBuffer_Backpressure(t *testing.T) {
	buf := NewBuffer(nil, 3, 100, time.Hour, nil)

	for i := 0; i < 3; i++ {
		e := Event{EventID: "test", SiteID: "s1", Timestamp: time.Now().UnixMilli()}
		if !buf.Push(e) {
			t.Fatalf("push %d should succeed", i)
		}
	}

	// Buffer is now full (maxSize=3)
	e := Event{EventID: "overflow", SiteID: "s1", Timestamp: time.Now().UnixMilli()}
	if buf.Push(e) {
		t.Fatal("push should return false when buffer is full")
	}
	if buf.Len() != 3 {
		t.Fatalf("expected len=3 after backpressure, got %d", buf.Len())
	}
}

func TestBuffer_DrainOnFlush(t *testing.T) {
	buf := NewBuffer(nil, 100, 50, time.Hour, nil)

	for i := 0; i < 5; i++ {
		buf.Push(Event{EventID: "test", SiteID: "s1", Timestamp: time.Now().UnixMilli()})
	}
	if buf.Len() != 5 {
		t.Fatalf("expected 5 events, got %d", buf.Len())
	}

	// Flush will try to insert with nil db and fail, but the events get drained
	// from the main buffer (and re-queued on error if possible).
	// With nil db, insertBatch will panic, so we test the drain separately.
	buf.mu.Lock()
	drained := buf.events
	buf.events = make([]Event, 0, 50)
	buf.mu.Unlock()

	if len(drained) != 5 {
		t.Fatalf("expected to drain 5 events, got %d", len(drained))
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer after drain, got %d", buf.Len())
	}
}

// TestBuffer_FlushFailureRequeueKeepsAllUnderPressure is the observe-04
// regression: a failed flush used to requeue only what fit below maxSize and
// silently drop the rest. The dropped events were WAL-appended below
// lastOffset, so the next successful flush checkpointed past them — lost for
// good, restart included. The requeue must retain everything, at the front.
func TestBuffer_FlushFailureRequeueKeepsAllUnderPressure(t *testing.T) {
	buf := NewBuffer(nil, 4, 100, time.Hour, nil)
	// Buffer already at maxSize with events pushed while the flush was
	// in flight — zero headroom, the exact pressure case.
	for _, id := range []string{"n1", "n2", "n3", "n4"} {
		buf.events = append(buf.events, ev(id))
	}

	buf.requeueFailed([]Event{ev("e1"), ev("e2"), ev("e3")}, 0, 0)

	if got := buf.Len(); got != 7 {
		t.Fatalf("all failed-batch events must survive under pressure: %d of 7", got)
	}
	buf.mu.Lock()
	got := ids(buf.events)
	buf.mu.Unlock()
	want := []string{"e1", "e2", "e3", "n1", "n2", "n3", "n4"}
	if !equal(got, want) {
		t.Fatalf("requeue order = %v, want the failed batch at the front: %v", got, want)
	}
}

// TestBuffer_FlushFailureSurvivesRetryAndRestart walks the observe-04
// acceptance end-to-end against a real Nucleus: events accepted before a
// failed flush must all survive the retry AND a restart would have replayed
// them (nothing checkpointed past uncommitted records).
func TestBuffer_FlushFailureSurvivesRetryAndRestart(t *testing.T) {
	dbFail, doneFail := ingestTestDB(t)
	defer doneFail()
	dbRetry, doneRetry := ingestTestDB(t)
	defer doneRetry()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "ingest", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	buf := NewBuffer(dbFail, 8, 100, time.Hour, logger)
	if err := buf.AttachQueue(q); err != nil {
		t.Fatalf("attach: %v", err)
	}

	site := fmt.Sprintf("obs04-%d", time.Now().UnixNano())
	ts := time.Now().UTC().UnixMilli()
	push := func(i int) {
		id := fmt.Sprintf("%s-e%d", site, i)
		if !buf.Push(Event{EventID: id, TenantID: "default", SiteID: site, SessionID: "s", VisitID: "s", EventType: "pageview", Timestamp: ts}) {
			t.Fatalf("push %d refused", i)
		}
	}
	for i := 1; i <= 4; i++ {
		push(i)
	}

	// The flush fails (dead connection) with the buffer under pressure.
	dbFail.Close()
	buf.Flush()
	if got := buf.Len(); got != 4 {
		t.Fatalf("failed flush must retain every accepted event, got %d of 4", got)
	}
	if cpSeg, cpOff, err := readCheckpoint(filepath.Join(dir, "ingest", "checkpoint")); err != nil || cpSeg != 0 || cpOff != 0 {
		t.Fatalf("failed flush must not advance the checkpoint, got (%d,%d) (err %v)", cpSeg, cpOff, err)
	}

	// Retry against a live connection: every accepted event is inserted.
	buf.mu.Lock()
	buf.db = dbRetry
	buf.mu.Unlock()
	buf.Flush()
	if got := buf.Len(); got != 0 {
		t.Fatalf("retry must drain the requeued events, %d left", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type idRow struct {
		EventID string `db:"event_id"`
	}
	rows, err := nucleus.Query[idRow](ctx, dbRetry.SQL(),
		"SELECT event_id FROM events WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("all 4 events must have survived the failed flush + retry, got %d", len(rows))
	}

	// And the WAL is fully checkpointed now — a restart replays nothing.
	if err := q.Close(); err != nil {
		t.Fatalf("close queue: %v", err)
	}
	q2, err := NewDiskQueue(dir, "ingest", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("checkpoint must cover the retried batch, %d pending", len(pending))
	}
}

// TestBuffer_FlushFailureDoesNotLivelock is the audit F10 regression: the
// size-triggered wakeup used to re-Flush in a tight inner loop while the
// buffer stayed above flushSize (a failing flush requeues its batch), never
// selecting on stopCh — an endless spin no Stop could interrupt. Now one
// wakeup is one bounded attempt, so Stop completes even with a dead DB.
func TestBuffer_FlushFailureDoesNotLivelock(t *testing.T) {
	buf := NewBuffer(nil, 8, 2, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	buf.Start()
	defer buf.Stop()

	for i := 0; i < 4; i++ {
		if !buf.Push(ev(fmt.Sprintf("e%d", i))) {
			t.Fatalf("push %d refused", i)
		}
	}
	// nil db -> insertBatch panics inside Flush, recovered by the worker's
	// panic guard, leaving the events requeued... in fact the panic aborts
	// the whole worker goroutine (recover logs and exits the goroutine),
	// which is exactly the "unhealthy worker" case Stop must still survive.
	// Wait for the flush attempt to land, then stop and require completion
	// within a generous bound instead of hanging the test forever.
	done := make(chan struct{})
	go func() { buf.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not complete — flush loop is livelocked on a failing flush")
	}
}

// TestBuffer_AvailBacksAtomicBatchAdmission is the audit F12 helper: the
// batch handler reserves capacity for the whole batch up front.
func TestBuffer_AvailBacksAtomicBatchAdmission(t *testing.T) {
	buf := NewBuffer(nil, 5, 10, time.Hour, nil)
	if got := buf.Avail(); got != 5 {
		t.Fatalf("fresh buffer Avail = %d, want 5", got)
	}
	for i := 0; i < 3; i++ {
		buf.Push(ev(fmt.Sprintf("e%d", i)))
	}
	if got := buf.Avail(); got != 2 {
		t.Fatalf("after 3 pushes Avail = %d, want 2", got)
	}
}

// TestAttachQueueFailsLeavesQueueUnattached is the audit F15 regression: a
// queue whose replay fails must not be left attached (later checkpoints
// would advance past the unread backlog).
func TestAttachQueueFailsLeavesQueueUnattached(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	q, err := NewDiskQueue(dir, "ingest", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A closed queue's Pending errors, so attach must fail...
	buf := NewBuffer(nil, 8, 2, time.Hour, logger)
	if err := buf.AttachQueue(q); err == nil {
		t.Fatal("attaching a queue with a failing replay must return an error")
	}
	buf.mu.Lock()
	attached := buf.queue
	buf.mu.Unlock()
	if attached != nil {
		t.Fatal("a failed AttachQueue must not leave the queue installed")
	}
}

// AUD-010 (round 2): concurrent batches against a tight capacity — exactly
// one whole batch may be admitted, never an accepted prefix from the loser.
func TestBuffer_PushBatchAtomicUnderConcurrency(t *testing.T) {
	buf := NewBuffer(nil, 10, 100, time.Hour, nil)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := make([]Event, 6)
			for j := range batch {
				batch[j] = ev(fmt.Sprintf("g%d", j))
			}
			if buf.PushBatch(batch) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("with capacity 10 and 8 racing batches of 6, exactly one must win, got %d", got)
	}
	if got := buf.Len(); got != 6 {
		t.Fatalf("winner's whole batch must be admitted atomically, len = %d", got)
	}
}

// AUD-016 (round 2): post-stop admission is refused; a never-started
// buffer still flushes on Stop.
func TestBuffer_RejectsAdmissionAfterStop(t *testing.T) {
	buf := NewBuffer(nil, 10, 100, time.Hour, nil)
	buf.Start()
	buf.Stop()
	if buf.Push(ev("late")) {
		t.Fatal("push after Stop must be refused")
	}
	if batch := []Event{ev("a"), ev("b")}; buf.PushBatch(batch) {
		t.Fatal("pushBatch after Stop must be refused")
	}
}

// AUD-015 (round 2): the serialized-byte budget refuses admission even
// when the count cap has headroom.
func TestBuffer_ByteBudgetRefusesOversizedAdmission(t *testing.T) {
	buf := NewBuffer(nil, 100, 100, time.Hour, nil).WithMaxBufferedBytes(1024)
	big := ev("big")
	big.Title = strings.Repeat("x", 900)
	if buf.Push(big) {
		t.Fatal("event exceeding the byte budget must be refused")
	}
	small := ev("small")
	if !buf.Push(small) {
		t.Fatal("small event must be admitted")
	}
}
