package ingest

// O01 slice 1 oracle (docs/O01_DURABLE_INGEST_ADR.md §5.1-§5.3, §5.10):
// group commit before acknowledgment, admission refusal at the disk
// high-water, explicit lossy mode, and the visible counters. Each test
// names the destination-contract line it guards. The pre-slice pins in
// o01_pin_test.go were updated deliberately in the same change as the
// implementation; this file pins the NEW semantics.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
)

// countingSync is the fsync seam for the group-commit tests: every fsync of
// a WAL segment goes through DiskQueue.syncActiveLocked, which tests route
// through this hook so the grouping (N events in a window -> M fsyncs,
// M << N) is assertable rather than assumed.
type countingSync struct {
	mu     sync.Mutex
	n      int
	failAt int // 1-based; when n reaches failAt the sync fails (0 = never)
}

func (c *countingSync) hook(f *os.File) error {
	c.mu.Lock()
	c.n++
	fail := c.failAt != 0 && c.n >= c.failAt
	c.mu.Unlock()
	if fail {
		return fmt.Errorf("countingSync: injected fsync failure #%d", c.n)
	}
	return f.Sync()
}

func (c *countingSync) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// durableTestQueue builds a durable-mode queue with an fsync counting seam.
func durableTestQueue(t *testing.T, dir string, maxBytes, maxTotal int64, delay time.Duration) (*DiskQueue, *countingSync) {
	t.Helper()
	q, err := NewDiskQueueWithLimits(dir, "events", time.Hour, maxBytes, maxTotal, quietLogger())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	c := &countingSync{}
	q.syncHook = c.hook
	if delay > 0 {
		q.WithGroupCommitDelay(delay)
	}
	return q, c
}

// appendDurable appends one batch and waits for the group commit covering it
// — the exact sequence Buffer.PushBatch performs in durable mode, and the
// condition under which the HTTP handler sends 200 OK.
func appendDurable(t *testing.T, q *DiskQueue, id string) {
	t.Helper()
	off, err := q.AppendBatch([]Event{ev(id)})
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	if err := q.WaitCommit(off); err != nil {
		t.Fatalf("group commit for %s: %v", id, err)
	}
}

// TestO01_GroupCommit_AckWaitsForFsync guards ADR §5.1: "the HTTP 200 is
// sent only after the fsync covering the request's frame returns". WaitCommit
// returning nil means the frame's bytes are in the segment FILE (fsynced),
// not in the userspace bufio.
func TestO01_GroupCommit_AckWaitsForFsync(t *testing.T) {
	dir := t.TempDir()
	q, c := durableTestQueue(t, dir, 1<<20, 8<<20, 25*time.Millisecond)
	defer q.Close()

	appendDurable(t, q, "o01-acked-fsynced")

	if c.count() < 1 {
		t.Fatal("the durable ack must be backed by at least one fsync")
	}
	if raw := readSegmentRaw(t, dir, "events"); !frameContains(raw, "o01-acked-fsynced") {
		t.Fatal("durable-mode violation: WaitCommit returned but the frame is not in the WAL file — " +
			"the ack would be memory-only (the pre-O01 condition)")
	}
}

// TestO01_GroupCommit_AckSurvivesCrashEquivalentRestart guards ADR §5.1's
// recovery line + §7's "crash after ACK" row: because the ack now implies a
// completed fsync, a crash at the moment of the ack loses nothing — Close()
// adds no bytes after a synced ack, so it is crash-equivalent, and a fresh
// process replays the acked event.
func TestO01_GroupCommit_AckSurvivesCrashEquivalentRestart(t *testing.T) {
	dir := t.TempDir()
	q, _ := durableTestQueue(t, dir, 1<<20, 8<<20, 25*time.Millisecond)

	appendDurable(t, q, "o01-acked-recoverable")

	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2, err := NewDiskQueueWithLimits(dir, "events", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got := ids(pending); !equal(got, []string{"o01-acked-recoverable"}) {
		t.Fatalf("crash at ack-time must leave the acked event replayable, got %v", got)
	}
}

// TestO01_GroupCommit_CoalescesConcurrentAppends guards the group-commit
// purpose itself (ADR §5.1; the contract's "M fsyncs where M << N"): many
// concurrent durable appends inside one commit window share ONE flush+fsync
// — concurrent request pressure must not become an fsync-per-event storm.
func TestO01_GroupCommit_CoalescesConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	const n = 40
	// Window comfortably wider than goroutine startup skew so the whole
	// burst lands in one group.
	q, c := durableTestQueue(t, dir, 1<<30, 8<<30, 60*time.Millisecond)
	defer q.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			off, err := q.AppendBatch([]Event{ev(fmt.Sprintf("o01-gc-%02d", i))})
			if err != nil {
				errCh <- fmt.Errorf("append %d: %w", i, err)
				return
			}
			if err := q.WaitCommit(off); err != nil {
				errCh <- fmt.Errorf("commit %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	fsyncs := c.count()
	if fsyncs < 1 {
		t.Fatal("the group must be backed by at least one fsync")
	}
	if fsyncs > n/5 {
		t.Fatalf("group commit did not coalesce: %d fsyncs for %d events in one window (want M << N)", fsyncs, n)
	}
	// Every acked event must actually be durable — grouping never drops a
	// waiter's coverage.
	raw := readSegmentRaw(t, dir, "events")
	for i := 0; i < n; i++ {
		if !frameContains(raw, fmt.Sprintf("o01-gc-%02d", i)) {
			t.Fatalf("event %d acked but not in the WAL file", i)
		}
	}
}

// TestO01_Durable_HighWaterRefusesInsteadOfDeleting guards ADR §5.2: under
// durable mode a roll that would breach the total-bytes budget REFUSES
// admission (retryable, counted) instead of deleting an uncheckpointed
// segment — every previously acked event keeps its crash-recovery copy, and
// admission resumes once the checkpoint advances.
func TestO01_Durable_HighWaterRefusesInsteadOfDeleting(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 600
	const maxTotal = 3 * maxBytes
	q, _ := durableTestQueue(t, dir, maxBytes, maxTotal, 10*time.Millisecond)
	defer q.Close()

	var admitted []string
	refused := 0
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("o01-hw-%03d", i)
		off, err := q.AppendBatch([]Event{ev(id)})
		if errors.Is(err, ErrWALHighWaterRefused) {
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		if err := q.WaitCommit(off); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
		admitted = append(admitted, id)
	}
	if refused == 0 {
		t.Fatal("the high-water must eventually be refused in durable mode")
	}
	stats := q.Stats()
	if stats.RefusedHighWater == 0 {
		t.Fatalf("refusals must be counted for /healthz, got %+v", stats)
	}
	if stats.DroppedUnackedSegments != 0 || stats.DroppedUnackedBytes != 0 {
		t.Fatalf("durable mode must never delete uncheckpointed segments, got %+v", stats)
	}
	// Nothing acked was silently lost: every admitted event is still
	// replayable (nothing was checkpointed).
	pending, err := q.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got := ids(pending); !equal(got, admitted) {
		t.Fatalf("durable-mode breach must retain every acked event: got %d of %d", len(got), len(admitted))
	}

	// The refusal is retryable, not latched: once the checkpoint advances
	// (flush drained the backlog), admission resumes.
	if err := q.Checkpoint(q.Offset()); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := q.AppendBatch([]Event{ev("o01-post-checkpoint")}); err != nil {
		t.Fatalf("admission must resume after the checkpoint advances: %v", err)
	}
}

// TestO01_Lossy_ModeKeepsFastAckAndDeletion guards ADR §5.3: lossy mode
// (explicit opt-in) restores the pre-O01 semantics exactly — acks return
// without waiting for any fsync, and the disk high-water deletes the oldest
// uncheckpointed segment (counted) instead of refusing.
func TestO01_Lossy_ModeKeepsFastAckAndDeletion(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 600
	const maxTotal = 3 * maxBytes
	q, c := durableTestQueue(t, dir, maxBytes, maxTotal, 10*time.Millisecond)
	defer q.Close()
	q.WithLossySync()

	if got := q.Stats().Mode; got != "lossy" {
		t.Fatalf("mode must be visible in stats, got %q", got)
	}

	off, err := q.AppendBatch([]Event{ev("o01-lossy-fast")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.WaitCommit(off); err != nil {
		t.Fatalf("lossy WaitCommit must not wait or fail: %v", err)
	}
	if c.count() != 0 {
		t.Fatal("lossy ack must not fsync — that is the mode's declared loss budget")
	}
	if raw := readSegmentRaw(t, dir, "events"); frameContains(raw, "o01-lossy-fast") {
		t.Fatal("lossy fast-ack pinned: the frame must still be userspace-only at ack time (fsyncInterval is 1h here)")
	}

	// High-water under lossy mode: keep admitting by deleting the oldest
	// uncheckpointed segment, loudly counted.
	for i := 0; i < 200; i++ {
		if _, err := q.AppendBatch([]Event{ev(fmt.Sprintf("o01-lossy-%03d", i))}); err != nil {
			t.Fatalf("lossy append %d: %v (lossy mode must keep admitting past the breach)", i, err)
		}
	}
	stats := q.Stats()
	if stats.DroppedUnackedSegments == 0 || stats.RefusedHighWater != 0 {
		t.Fatalf("lossy breach must delete-and-count, never refuse: %+v", stats)
	}
	if stats.UnsyncedEvents <= 0 {
		t.Fatalf("the potentially-lost window must be visible as a gauge, got %+v", stats)
	}
}

// TestO01_Lossy_BufferCounters guards ADR §5.3/§5.10 at the buffer boundary:
// in lossy mode events are accepted but NEVER counted as durably acked —
// the accepted-vs-durable gap is the visible loss budget.
func TestO01_Lossy_BufferCounters(t *testing.T) {
	dir := t.TempDir()
	q, _ := durableTestQueue(t, dir, 1<<20, 8<<20, 10*time.Millisecond)
	defer q.Close()
	q.WithLossySync()

	buf := NewBuffer(nil, 1000, 100, time.Hour, quietLogger())
	if err := buf.AttachQueue(q); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := buf.PushBatch([]Event{ev("o01-lossy-ack")}); err != nil {
		t.Fatalf("push: %v", err)
	}
	st := buf.Stats()
	if st.Accepted != 1 || st.DurablyAcked != 0 {
		t.Fatalf("lossy mode: accepted=1 durably_acked=0, got %+v", st)
	}
	if raw := readSegmentRaw(t, dir, "events"); frameContains(raw, "o01-lossy-ack") {
		t.Fatal("lossy buffer ack must not have fsynced")
	}
}

// TestO01_Durable_BufferCounters guards ADR §5.1/§5.10 at the buffer
// boundary: PushBatch returning nil in durable mode means accepted AND
// durably acked — the frame is in the file at return time (this is the
// deliberate flip of the pre-slice pin).
func TestO01_Durable_BufferCounters(t *testing.T) {
	dir := t.TempDir()
	q, c := durableTestQueue(t, dir, 1<<20, 8<<20, 25*time.Millisecond)
	defer q.Close()

	buf := NewBuffer(nil, 1000, 100, time.Hour, quietLogger())
	if err := buf.AttachQueue(q); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := buf.PushBatch([]Event{ev("o01-durable-ack")}); err != nil {
		t.Fatalf("push: %v", err)
	}
	st := buf.Stats()
	if st.Accepted != 1 || st.DurablyAcked != 1 {
		t.Fatalf("durable mode: accepted=1 durably_acked=1, got %+v", st)
	}
	if c.count() < 1 {
		t.Fatal("the buffer ack must be backed by an fsync")
	}
	if raw := readSegmentRaw(t, dir, "events"); !frameContains(raw, "o01-durable-ack") {
		t.Fatal("durable buffer ack: frame must be in the WAL file at PushBatch return")
	}
}

// TestO01_SyncFailureRefusesAdmission guards ADR §5.4 (F14 posture extended
// to the group commit): a failing fsync fails the waiting ack AND latches the
// queue — the process never answers 200 for bytes it could not make durable.
func TestO01_SyncFailureRefusesAdmission(t *testing.T) {
	dir := t.TempDir()
	q, c := durableTestQueue(t, dir, 1<<20, 8<<20, 10*time.Millisecond)
	defer q.Close()

	off, err := q.AppendBatch([]Event{ev("o01-sync-fail")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	c.mu.Lock()
	c.failAt = 1 // the group's fsync fails
	c.mu.Unlock()
	if err := q.WaitCommit(off); err == nil {
		t.Fatal("an fsync failure must fail the ack, not acknowledge undurably")
	}
	// The queue is latched (F14): the next append refuses outright.
	if _, err := q.AppendBatch([]Event{ev("o01-after-latch")}); err == nil {
		t.Fatal("a failed group fsync must latch the queue against further appends")
	}
	if q.LastError() == nil {
		t.Fatal("the latch must be visible at LastError for /healthz")
	}
}

// TestO01_Handler_RefusalStatusSplit guards the handler mapping: capacity
// refusals stay 429 (today's consumer contract), durability refusals
// (latched WAL, high-water) are 503 — the retryable OTLP convention.
func TestO01_Handler_RefusalStatusSplit(t *testing.T) {
	ctx := withTestUA(context.Background())

	// Capacity: buffer of one, already full.
	full := NewBuffer(nil, 1, 100, time.Hour, nil)
	if err := full.Push(ev("occupant")); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	h := Handler(full, "salt", nil)
	_, err := h(ctx, IngestInput{SiteID: "s1", EventType: "pageview"})
	var appErr *neutron.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusTooManyRequests {
		t.Fatalf("capacity refusal must stay 429, got %v", err)
	}

	// Durability: a queue whose fsync fails latches; the next handler call
	// must surface 503, never 200-with-memory-only-ack.
	dir := t.TempDir()
	q, c := durableTestQueue(t, dir, 1<<20, 8<<20, 10*time.Millisecond)
	defer q.Close()
	buf := NewBuffer(nil, 1000, 100, time.Hour, quietLogger())
	if err := buf.AttachQueue(q); err != nil {
		t.Fatalf("attach: %v", err)
	}
	h2 := Handler(buf, "salt", nil)
	if _, err := h2(ctx, IngestInput{SiteID: "s1", EventType: "pageview"}); err != nil {
		t.Fatalf("healthy durable admit: %v", err)
	}
	c.mu.Lock()
	c.failAt = 1
	c.mu.Unlock()
	_, err = h2(ctx, IngestInput{SiteID: "s1", EventType: "pageview"})
	if !errors.As(err, &appErr) || appErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("durability refusal must be 503, got %v", err)
	}
}

// TestO01_RetryAfterOnUnavailable guards the Retry-After half of the refusal
// convention (OTLP handlers set it on their 503s; the events group gets it
// from this middleware so producers can honor it).
func TestO01_RetryAfterOnUnavailable(t *testing.T) {
	mw := RetryAfterOnUnavailable(5 * time.Second)
	makeHandler := func(code int) http.Handler {
		return mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
	}
	for _, code := range []int{200, 429, 500} {
		rr := httptest.NewRecorder()
		makeHandler(code).ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/events", nil))
		if got := rr.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After must not be set on %d, got %q", code, got)
		}
	}
	rr := httptest.NewRecorder()
	makeHandler(http.StatusServiceUnavailable).ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/events", nil))
	if got := rr.Header().Get("Retry-After"); got != strconv.Itoa(5) {
		t.Fatalf("503 must carry Retry-After: 5, got %q", got)
	}
}

// TestO01_AttachQueueCountsReplayed guards ADR §5.10's
// replayed-on-restart counter: recovery of fsynced-but-uncheckpointed
// segments at boot is visible at the buffer's stats surface. Nucleus-gated:
// recovery commits through insertBatch.
func TestO01_AttachQueueCountsReplayed(t *testing.T) {
	db, done := ingestTestDB(t)
	defer done()

	dir := t.TempDir()
	q, err := NewDiskQueueWithLimits(dir, "ingest", time.Hour, 1<<30, 8<<30, quietLogger())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	off, err := q.AppendBatch([]Event{ev("o01-replayed")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.WaitCommit(off); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Crash-equivalent: the frame is fsynced, Close adds nothing.
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	q2, err := NewDiskQueueWithLimits(dir, "ingest", time.Hour, 1<<30, 8<<30, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	buf := NewBuffer(db, 1000, 100, time.Hour, quietLogger())
	if err := buf.AttachQueue(q2); err != nil {
		t.Fatalf("attach (recovery): %v", err)
	}
	if st := buf.Stats(); st.ReplayedOnRestart != 1 {
		t.Fatalf("replayed_on_restart must count recovered events, got %+v", st)
	}
}
