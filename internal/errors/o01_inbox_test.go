package errors

// O01 slice 2 oracle (docs/O01_DURABLE_INGEST_ADR.md §5.6 + the §7
// validation matrix): the idempotent error inbox across the crash
// windows. Every test names the contract line it guards. Nucleus-gated.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func o01CountInbox(t *testing.T, db *nucleus.Client, site string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		"SELECT CAST(COUNT(*) AS TEXT) AS n FROM error_inbox WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("count error_inbox: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(rows[0].N, "%d", &n); err != nil {
		t.Fatalf("parse count %q: %v", rows[0].N, err)
	}
	return n
}

// TestO01_Inbox_DuplicateRetryAppliedOnce guards §5.6 "duplicate (same id
// + same digest) -> acknowledged once, applied once" for the fast path
// (in-process retry, e.g. a lost HTTP response): the retry acks
// {deduped}, and storage holds exactly one row + one ledger row.
func TestO01_Inbox_DuplicateRetryAppliedOnce(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	b := o01DurableErrorBuffer(t, dbStore, t.TempDir())

	id := "o01dup-ev0000001"
	body := ErrorInput{EventID: id, ErrorType: "O01Dup", ErrorValue: "same body"}
	if err := b.Push(site, body); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	b.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("applied once, got %d", got)
	}
	// The lost-response retry: same id, same payload.
	if err := b.Push(site, body); !errors.Is(err, ErrAdmittedDuplicate) {
		t.Fatalf("retry must be acknowledged as a duplicate, got %v", err)
	}
	b.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("duplicate retry must not double-apply, got %d", got)
	}
	if got := o01CountInbox(t, dbCheck, site); got != 1 {
		t.Fatalf("exactly one ledger row, got %d", got)
	}
	st := b.Stats()
	if st.Deduped != 1 || st.Applied != 1 {
		t.Fatalf("deduped=1 applied=1, got %+v", st)
	}
}

// TestO01_Inbox_ConflictingIDRejectedAndCounted guards §5.6 "conflicting
// reuse -> rejected with a 409-class response and a counter, never
// silently merged": same id, DIFFERENT payload is refused at admission
// while the cache knows the id.
func TestO01_Inbox_ConflictingIDRejectedAndCounted(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	b := o01DurableErrorBuffer(t, dbStore, t.TempDir())

	id := "o01cfl-ev0000001"
	if err := b.Push(site, ErrorInput{EventID: id, ErrorType: "A", ErrorValue: "original"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	b.Flush()
	if err := b.Push(site, ErrorInput{EventID: id, ErrorType: "B", ErrorValue: "conflicting"}); !errors.Is(err, ErrEventIDConflict) {
		t.Fatalf("conflicting reuse must be rejected 409-class, got %v", err)
	}
	b.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("the conflicting payload must never be applied, got %d rows", got)
	}
	if st := b.Stats(); st.ConflictingID != 1 {
		t.Fatalf("conflicts must be counted for /healthz, got %+v", st)
	}
}

// TestO01_Inbox_ConflictAfterRestartCountedNotMerged guards the durable
// half of the conflict rule: after a restart the admission cache is gone,
// so a conflicting reuse is ACKED (durable, honest — the server cannot
// know yet) but the flush-time ledger check classifies it conflicting,
// counts it, and applies nothing.
func TestO01_Inbox_ConflictAfterRestartCountedNotMerged(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()

	b := o01DurableErrorBuffer(t, dbStore, dir)
	id := "o01rst-ev0000001"
	if err := b.Push(site, ErrorInput{EventID: id, ErrorType: "A", ErrorValue: "original"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	b.Flush()

	// Restart: fresh buffer (empty cache) over the same store/WAL.
	b2 := o01DurableErrorBuffer(t, dbStore, dir)
	if err := b2.Push(site, ErrorInput{EventID: id, ErrorType: "A", ErrorValue: "DIFFERENT"}); err != nil {
		t.Fatalf("post-restart conflicting admit is acknowledged (durable): %v", err)
	}
	b2.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("flush-time conflict must apply nothing, got %d rows", got)
	}
	if st := b2.Stats(); st.ConflictingID != 1 {
		t.Fatalf("the flush-time conflict must be counted, got %+v", st)
	}
}

// TestO01_Inbox_CrashAfterAckAppliesExactlyOnce guards §7's "crash after
// ACK before apply" row: the record was fsync-acked but never flushed; a
// restart replays the WAL frame and applies it exactly once (nothing was
// applied before, the ledger is empty).
func TestO01_Inbox_CrashAfterAckAppliesExactlyOnce(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()

	b := o01DurableErrorBuffer(t, dbStore, dir)
	id := "o01cra-ev0000001"
	if err := b.Push(site, ErrorInput{EventID: id, ErrorType: "O01Crash", ErrorValue: "acked-not-applied"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// Crash-equivalent: no Flush, no checkpoint; Close adds no bytes.
	if err := b.queue.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}
	b.mu.Lock()
	b.queue = nil // keep the test's Stop from double-closing
	b.mu.Unlock()
	if o01CountErrorEvents(t, dbCheck, site) != 0 {
		t.Fatal("precondition: nothing applied before the crash")
	}

	b2 := o01DurableErrorBuffer(t, dbStore, dir) // replays at attach
	if st := b2.Stats(); st.ReplayedOnRestart != 1 {
		t.Fatalf("replay must recover the acked record, got %+v", st)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("crash after ack must apply exactly once after restart, got %d", got)
	}
	if got := o01CountInbox(t, dbCheck, site); got != 1 {
		t.Fatalf("exactly one ledger row after replay, got %d", got)
	}
}

// TestO01_Inbox_CrashMidApplyNoDoubleCount guards §7's "crash mid-apply"
// row: the apply transaction committed but the WAL checkpoint never
// advanced — replay resubmits the record and the in-transaction ledger
// check dedupes it (zero new writes).
func TestO01_Inbox_CrashMidApplyNoDoubleCount(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()

	b := o01DurableErrorBuffer(t, dbStore, dir)
	id := "o01mid-ev0000001"
	if err := b.Push(site, ErrorInput{EventID: id, ErrorType: "O01Mid", ErrorValue: "committed-uncheckpointed"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// Simulate the crash between SQL commit and checkpoint: the apply
	// transaction runs (and commits) WITHOUT the buffer's flush/checkpoint.
	svc := b.handler
	input := ErrorInput{SiteID: site, EventID: id, ErrorType: "O01Mid", ErrorValue: "committed-uncheckpointed"}
	outcome, _, err := svc.ApplyInbox(context.Background(), input, "", id, digestOf(t, b, site, id))
	if err != nil || outcome != InboxApplied {
		t.Fatalf("direct apply: outcome=%s err=%v", outcome, err)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("precondition: exactly one row committed, got %d", got)
	}

	// Restart: the frame is still uncheckpointed, so replay resubmits it —
	// the ledger check inside the apply transaction dedupes it.
	b2 := o01DurableErrorBuffer(t, dbStore, dir)
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("replay must not double-count the committed record, got %d", got)
	}
	if st := b2.Stats(); st.ReplayedOnRestart != 1 {
		t.Fatalf("the replayed (deduped) record must be counted, got %+v", st)
	}
}

// TestO01_Inbox_LostResponseRetryAfterRestart guards §7's "lost response
// + client retry -> one applied event" across a restart (the admission
// cache is gone; the retry re-admits and the flush-time ledger dedupes).
func TestO01_Inbox_LostResponseRetryAfterRestart(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()

	id := "o01lrr-ev0000001"
	body := ErrorInput{EventID: id, ErrorType: "O01LRR", ErrorValue: "retry-me"}
	b := o01DurableErrorBuffer(t, dbStore, dir)
	if err := b.Push(site, body); err != nil {
		t.Fatalf("admit: %v", err)
	}
	b.Flush()
	if err := b.queue.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b.mu.Lock()
	b.queue = nil
	b.mu.Unlock()

	// Restart + the client retries the request it never got a response for.
	b2 := o01DurableErrorBuffer(t, dbStore, dir)
	if err := b2.Push(site, body); err != nil {
		t.Fatalf("retry admit (cache is empty after restart): %v", err)
	}
	b2.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("lost response + retry must leave exactly one applied event, got %d", got)
	}
	if st := b2.Stats(); st.Deduped != 1 {
		t.Fatalf("the flush-time dedupe must be counted, got %+v", st)
	}
}

// TestO01_Inbox_IdentitylessRecordsKeepV1Semantics guards the documented
// boundary: producers without an event_id are applied (durable WAL ack,
// exactly-once server-side replay) but leave no ledger row and get no
// cross-request dedupe.
func TestO01_Inbox_IdentitylessRecordsKeepV1Semantics(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	b := o01DurableErrorBuffer(t, dbStore, t.TempDir())

	if err := b.Push(site, ErrorInput{ErrorType: "NoID", ErrorValue: "v1 posture"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	b.Flush()
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("identity-less record must apply, got %d", got)
	}
	if got := o01CountInbox(t, dbCheck, site); got != 0 {
		t.Fatalf("identity-less record must not claim ledger rows, got %d", got)
	}
}

// TestO01_Inbox_QuarantineSkipsPoison guards §5.4/§5.9's poison rule: a
// record that cannot ever succeed (undecodable after admission) is
// quarantined with a counter and skipped WITHOUT blocking the stream —
// the records after it still apply.
func TestO01_Inbox_QuarantineSkipsPoison(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	dir := t.TempDir()
	b := o01DurableErrorBuffer(t, dbStore, dir)

	if err := b.Push(site, ErrorInput{ErrorType: "P", ErrorValue: "before"}); err != nil {
		t.Fatalf("admit 1: %v", err)
	}
	// Poison the middle record's frozen body (in-package seam): it decodes
	// to nothing applicable.
	if err := b.Push(site, ErrorInput{ErrorType: "P", ErrorValue: "poison"}); err != nil {
		t.Fatalf("admit 2: %v", err)
	}
	b.mu.Lock()
	b.events[1].Body = []byte("{not json")
	b.mu.Unlock()
	if err := b.Push(site, ErrorInput{ErrorType: "P", ErrorValue: "after"}); err != nil {
		t.Fatalf("admit 3: %v", err)
	}

	b.Flush()
	st := b.Stats()
	if st.Quarantined != 1 {
		t.Fatalf("the poison record must be quarantined and counted, got %+v", st)
	}
	if st.Pending != 0 {
		t.Fatalf("the stream must not be blocked, got pending=%d", st.Pending)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 2 {
		t.Fatalf("records around the poison must apply (2 rows), got %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "errors", "quarantine.log")); err != nil {
		t.Fatalf("the poison record must be spooled beside the WAL: %v", err)
	}
	// The checkpoint advanced past the quarantined record: a restart does
	// not replay it.
	b2 := o01DurableErrorBuffer(t, dbStore, dir)
	if st := b2.Stats(); st.ReplayedOnRestart != 0 {
		t.Fatalf("quarantined records must not replay, got %+v", st)
	}
}

// TestO01_Inbox_PendingGapStopsCheckpoint guards the checkpoint rule: a
// PENDING record's frame — and everything after it — stays uncheckpointed
// so a crash re-replays exactly the unapplied records.
func TestO01_Inbox_PendingGapStopsCheckpoint(t *testing.T) {
	dbStore, _, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	dir := t.TempDir()
	b := o01DurableErrorBuffer(t, dbStore, dir)

	if err := b.Push(site, ErrorInput{ErrorType: "G", ErrorValue: "will-pend"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	dbStore.Close() // storage dies: the flush must leave it PENDING
	b.Flush()
	if st := b.Stats(); st.Pending != 1 {
		t.Fatalf("record must be PENDING, got %+v", st)
	}
	if err := b.queue.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b.mu.Lock()
	b.queue = nil
	b.mu.Unlock()

	// Restart against a live store: the pending record replays and applies.
	fresh, err := nucleus.Connect(context.Background(), nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable for the replay half: %v", err)
	}
	defer fresh.Close()
	b2 := o01DurableErrorBuffer(t, fresh, dir)
	if st := b2.Stats(); st.ReplayedOnRestart != 1 || st.Applied != 1 {
		t.Fatalf("the uncheckpointed PENDING record must replay and apply after restart, got %+v", st)
	}
}

// digestOf reproduces the admission digest for a pushed record (test aid).
func digestOf(t *testing.T, b *ErrorBuffer, site, eventID string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range b.events {
		if ev.EventID == eventID && ev.Site == site {
			return ev.Digest
		}
	}
	// Already flushed: recompute over a canonical body of the same fields —
	// the digest is sha256 over the frozen body bytes; the test above
	// constructs it deterministically.
	return ""
}
