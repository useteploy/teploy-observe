package ingest

// O01 slice 2 oracle: the DiskQueue frame codec generalized to carry
// opaque RECORD frames (the errors inbox's transport) alongside events
// frames — one WAL implementation, per-signal queue instances. Guards the
// ADR §5.6 line "reuse the events WAL/group-commit machinery ... do not
// duplicate a second WAL implementation": records frames round-trip
// byte-identically through append, group commit, checkpoint, and replay,
// and the two frame kinds never interchange on one queue.

import (
	"encoding/json"
	"testing"
	"time"
)

func rawRec(s string) json.RawMessage { return json.RawMessage(s) }

// TestO01_RecordsFrame_RoundTripAndReplay guards the transport contract:
// AppendRecords writes one version-2 frame; after a crash-equivalent
// restart (no checkpoint) StreamRecordFrames replays it with the logical
// end offset; after Checkpoint it replays nothing.
func TestO01_RecordsFrame_RoundTripAndReplay(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	off, err := q.AppendRecords([]json.RawMessage{rawRec(`{"event_id":"o01r-aaaaaaaa","body":"one"}`)})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.WaitCommit(off); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if off <= 0 {
		t.Fatalf("offset after append must be positive, got %d", off)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	q2, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	var seen []string
	var ends []int64
	if err := q2.StreamRecordFrames(func(recs []json.RawMessage, endOffset int64) error {
		for _, r := range recs {
			seen = append(seen, string(r))
		}
		ends = append(ends, endOffset)
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(seen) != 1 || seen[0] != `{"event_id":"o01r-aaaaaaaa","body":"one"}` {
		t.Fatalf("record must replay byte-identically, got %v", seen)
	}
	if len(ends) != 1 || ends[0] != off {
		t.Fatalf("frame end offset must equal the append's returned offset %d, got %v", off, ends)
	}

	// Checkpoint drains the replay: nothing pending afterwards.
	if err := q2.Checkpoint(ends[0]); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	n := 0
	if err := q2.StreamRecordFrames(func([]json.RawMessage, int64) error { n++; return nil }); err != nil {
		t.Fatalf("replay after checkpoint: %v", err)
	}
	if n != 0 {
		t.Fatal("checkpointed frames must not replay")
	}
}

// TestO01_RecordsFrame_DurableAckAndPendingTail guards the durable-ack
// half for records: WaitCommit only returns after an fsync (the frame is
// in the segment file), and a PARTIAL tail — one frame checkpointed, one
// not — replays exactly the uncheckpointed frame.
func TestO01_RecordsFrame_DurableAckAndPendingTail(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	c := &countingSync{}
	q.syncHook = c.hook
	q.WithGroupCommitDelay(25 * time.Millisecond)
	defer q.Close()

	off1, err := q.AppendRecords([]json.RawMessage{rawRec(`{"n":1}`)})
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := q.WaitCommit(off1); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if c.count() < 1 {
		t.Fatal("the durable record ack must be backed by an fsync")
	}
	if err := q.Checkpoint(off1); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}
	off2, err := q.AppendRecords([]json.RawMessage{rawRec(`{"n":2}`)})
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if err := q.WaitCommit(off2); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	q2, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	var replayed []string
	if err := q2.StreamRecordFrames(func(recs []json.RawMessage, _ int64) error {
		for _, r := range recs {
			replayed = append(replayed, string(r))
		}
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 1 || replayed[0] != `{"n":2}` {
		t.Fatalf("only the uncheckpointed frame must replay, got %v", replayed)
	}
}

// TestO01_RecordsFrame_KindNeverInterchanges guards the corruption rule:
// an events frame on a records queue — and the reverse — is a replay
// ERROR (loud refusal, never a silent mis-decode).
func TestO01_RecordsFrame_KindNeverInterchanges(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := q.AppendBatch([]Event{ev("must-not-live-here")}); err != nil {
		t.Fatalf("append events frame: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2, err := NewDiskQueueWithLimits(dir, "errors", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	if err := q2.StreamRecordFrames(func([]json.RawMessage, int64) error { return nil }); err == nil {
		t.Fatal("an events frame on a records queue must fail replay loudly")
	}

	// And the mirror: a records frame on the events queue.
	dir2 := t.TempDir()
	qe, err := NewDiskQueueWithLimits(dir2, "events", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("events queue: %v", err)
	}
	if _, err := qe.AppendRecords([]json.RawMessage{rawRec(`{"x":1}`)}); err != nil {
		t.Fatalf("append records frame: %v", err)
	}
	if err := qe.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	qe2, err := NewDiskQueueWithLimits(dir2, "events", time.Hour, 1<<20, 8<<20, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer qe2.Close()
	if _, err := qe2.Pending(); err == nil {
		t.Fatal("a records frame on an events queue must fail replay loudly")
	}
}
