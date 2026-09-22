package errors

// O01 pinning oracle (docs/O01_DURABLE_INGEST_ADR.md §1.2, §2.2, §5.4,
// §5.6). ERA 3 (2026-09-22, implementation slice 2): the errors 200 is
// DURABLY ADMITTED — Push returning nil (what errorIngestHandler turns
// into 200 OK) means the record's WAL frame is fsynced in the errors WAL
// (group commit, same discipline as the events slice-1 ack) — and a
// storage failure at flush time leaves the record PENDING (requeued,
// retried, never dropped). The era-1/2 pins (memory-only ack,
// drop-on-flush-failure) were updated deliberately in the same change as
// the implementation; the mutation checks recorded in the ADR prove this
// file fails if either half regresses.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func o01CountErrorEvents(t *testing.T, db *nucleus.Client, site string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		"SELECT CAST(COUNT(*) AS TEXT) AS n FROM error_events WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("count error_events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("count returned %d rows", len(rows))
	}
	var n int
	if _, err := fmt.Sscanf(rows[0].N, "%d", &n); err != nil {
		t.Fatalf("parse count %q: %v", rows[0].N, err)
	}
	return n
}

// o01ErrorsFixture brings a scratch Nucleus to the current schema and
// returns two clients (store + check) plus the test site.
func o01ErrorsFixture(t *testing.T) (dbStore, dbCheck *nucleus.Client, site string) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbStore, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, dbStore); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbCheck, err = nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	return dbStore, dbCheck, fmt.Sprintf("o01-err-%d", time.Now().UnixNano())
}

// o01DurableErrorBuffer builds a buffer with a durable errors WAL in dir.
func o01DurableErrorBuffer(t *testing.T, dbStore *nucleus.Client, dir string) *ErrorBuffer {
	t.Helper()
	svc := NewService(dbStore, NewIssueService(dbStore), NewSearchService(dbStore), nil)
	q, err := ingest.NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, discardLogger())
	if err != nil {
		t.Fatalf("errors WAL: %v", err)
	}
	q.WithGroupCommitDelay(10 * time.Millisecond)
	b := NewErrorBuffer(svc, 100, 10, time.Hour, discardLogger())
	if err := b.AttachQueue(q); err != nil {
		t.Fatalf("attach errors WAL: %v", err)
	}
	return b
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// walFileContains reports whether the frame payload is present in the
// errors WAL's on-disk segment file — the evidence that Push's nil return
// is backed by a flush+fsync, not the userspace bufio (O01 §5.1's
// ack-ordering proof, the slice-1 technique applied to the errors queue).
func walFileContains(t *testing.T, dir, substr string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "errors", "current.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read errors WAL: %v", err)
	}
	return bytes.Contains(raw, []byte(substr))
}

// TestO01_ErrorAckIsDurableAndFlushFailurePends is the era-3 pin:
//  1. healthy store: Push (the ack basis) returns nil AFTER the record's
//     WAL frame is fsynced; nothing is stored at ack time; Flush applies
//     exactly once.
//  2. dead store: Push still returns nil (the WAL decouples the ack from
//     the database), and the flush failure leaves the record PENDING —
//     requeued, retried, never dropped (the disposition §5.4 replaced).
func TestO01_ErrorAckIsDurableAndFlushFailurePends(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()
	b := o01DurableErrorBuffer(t, dbStore, dir)

	// Healthy store: the ack is durable BEFORE any storage happens.
	evID := "o01pin-ack0001"
	if err := b.Push(site, ErrorInput{EventID: evID, ErrorType: "O01Pin", ErrorValue: "stored"}); err != nil {
		t.Fatalf("Push must admit durably — this nil is what errorIngestHandler turns into 200 OK: %v", err)
	}
	if !walFileContains(t, dir, evID) {
		t.Fatal("durable-ack violation: Push returned nil but the frame is not in the WAL FILE — " +
			"the ack would be memory/buffer-only (the pre-O01 condition)")
	}
	if o01CountErrorEvents(t, dbCheck, site) != 0 {
		t.Fatal("record must NOT be stored at ack time — the errors path applies asynchronously")
	}
	b.Flush()
	if st := b.Stats(); st.Pending != 0 {
		t.Fatalf("healthy flush must drain the queue, got pending=%d", st.Pending)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("healthy flush must apply the acked record exactly once, got %d rows", got)
	}

	// Dead store: the ack STILL works (WAL-backed), and the flush failure
	// PENDS the record instead of dropping it.
	dbStore.Close()
	evID2 := "o01pin-pend0002"
	if err := b.Push(site, ErrorInput{EventID: evID2, ErrorType: "O01Pin", ErrorValue: "pending"}); err != nil {
		t.Fatalf("Push must admit durably even with the store down — the WAL is the ack basis: %v", err)
	}
	if !walFileContains(t, dir, evID2) {
		t.Fatal("the second ack must also be fsync-backed")
	}
	b.Flush()
	st := b.Stats()
	if st.Pending != 1 {
		t.Fatalf("a failed flush must leave the record PENDING (requeued, retried), got pending=%d — "+
			"a drop would be the era-1/2 disposition this pin replaced", st.Pending)
	}
	if st.FlushFailing != true {
		t.Fatal("the pending state must be visible (flush_failing) for /healthz degradation")
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("the acked-then-failed record must not be stored yet (1 pre-existing row expected), got %d", got)
	}

	// Store restored (new client on the same DSN): the PENDING record is
	// retried and applied — never silently dropped.
	fresh, err := nucleus.Connect(context.Background(), nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable for the recovery half: %v", err)
	}
	defer fresh.Close()
	b.handler = NewService(fresh, NewIssueService(fresh), NewSearchService(fresh), nil)
	b.Flush()
	if st := b.Stats(); st.Pending != 0 {
		t.Fatalf("the PENDING record must be retried to application, got pending=%d", st.Pending)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 2 {
		t.Fatalf("the retried record must apply exactly once, got %d rows", got)
	}
	// Counters reconcile (§5.10): 2 accepted, 2 durably acked, 2 applied,
	// 1 of them after a PENDING retry.
	st = b.Stats()
	if st.Accepted != 2 || st.DurablyAcked != 2 || st.Applied != 2 {
		t.Fatalf("counters must reconcile: accepted=2 durably_acked=2 applied=2, got %+v", st)
	}
}

// TestO01_ErrorAdmissionRefusals pins the refusal classes at the buffer
// boundary (§5.4): capacity 429-class, identity 400-class, conflict
// 409-class, duplicate = deduped re-ack.
func TestO01_ErrorAdmissionRefusals(t *testing.T) {
	b := NewErrorBuffer(nil, 1, 100, time.Hour, discardLogger())
	in := ErrorInput{EventID: "o01ref-id000001", ErrorType: "T"}
	if err := b.Push("s", in); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := b.Push("s", ErrorInput{ErrorType: "T"}); !errors.Is(err, ErrErrorBufferFull) {
		t.Fatalf("capacity refusal must stay the 429 class, got %v", err)
	}
	if err := b.Push("s", ErrorInput{EventID: "bad id!", ErrorType: "T"}); !errors.Is(err, ErrInvalidEventID) {
		t.Fatalf("malformed identity must be the 400 class, got %v", err)
	}
	// Same id, same body -> deduped re-ack (NOT a capacity refusal, even
	// though the buffer is full — the record is already admitted).
	b2 := NewErrorBuffer(nil, 10, 100, time.Hour, discardLogger())
	if err := b2.Push("s", in); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := b2.Push("s", in); !errors.Is(err, ErrAdmittedDuplicate) {
		t.Fatalf("identical retry must be the deduped re-ack, got %v", err)
	}
	// Same id, different body -> 409-class conflict, counted.
	if err := b2.Push("s", ErrorInput{EventID: in.EventID, ErrorType: "Different"}); !errors.Is(err, ErrEventIDConflict) {
		t.Fatalf("conflicting reuse must be the 409 class, got %v", err)
	}
	if st := b2.Stats(); st.ConflictingID != 1 {
		t.Fatalf("conflicts must be counted, got %+v", st)
	}
}
