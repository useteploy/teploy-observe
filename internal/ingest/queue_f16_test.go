package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F16: maxBytes is a CAP enforced by rolling — appends that would push the
// active segment past it land in a fresh numbered segment, and replay walks
// every segment in order after a restart.
func TestDiskQueue_RollEnforcesSegmentCapAndCrossSegmentReplay(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const maxBytes = 600
	q, err := NewDiskQueue(dir, "events", time.Hour, maxBytes, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	var appended []string
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("e%02d", i)
		if _, err := q.Append(ev(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		appended = append(appended, id)
	}
	stats := q.Stats()
	if stats.Segments < 3 {
		t.Fatalf("expected the queue to roll across >=3 segments, got %d (bytes=%d)", stats.Segments, stats.Bytes)
	}
	for i := 0; i < len(q.segments)-1; i++ {
		if q.segments[i].size > maxBytes {
			t.Fatalf("sealed segment %d exceeds the cap: %d > %d", q.segments[i].idx, q.segments[i].size, maxBytes)
		}
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
		t.Fatalf("cross-segment replay lost or reordered events: got %d events, want %d", len(got), len(appended))
	}
}

// F16: a checkpoint written after a roll uses the JSON segment-scoped form
// and replays exactly the uncheckpointed tail across the segment boundary.
func TestDiskQueue_CheckpointAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const maxBytes = 600
	q, err := NewDiskQueue(dir, "events", time.Hour, maxBytes, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	var appended []string
	var midTarget int64
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("e%02d", i)
		off, err := q.Append(ev(id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		appended = append(appended, id)
		if i == 19 {
			midTarget = off
		}
	}
	if err := q.Checkpoint(midTarget); err != nil {
		t.Fatalf("checkpoint mid-backlog: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events", "checkpoint"))
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	var cf struct {
		Segment int64 `json:"segment"`
		Offset  int64 `json:"offset"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("post-roll checkpoint must be the JSON segment form, got %q: %v", raw, err)
	}
	if cf.Segment == 0 {
		t.Fatalf("checkpoint should reference a numbered segment after a roll, got %+v", cf)
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
	if got := ids(pending); !equal(got, appended[20:]) {
		t.Fatalf("segment-scoped checkpoint replay = %v, want %v", got, appended[20:])
	}
}

// F16: acknowledged sealed segments are deleted once the checkpoint moves
// into a later segment.
func TestDiskQueue_AcknowledgedSegmentsAreDeleted(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const maxBytes = 600
	q, err := NewDiskQueue(dir, "events", time.Hour, maxBytes, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	var off int64
	var target int64
	sawRoll := false
	for i := 0; i < 40; i++ {
		off, err = q.Append(ev(fmt.Sprintf("e%02d", i)))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		q.mu.Lock()
		rolled := q.activeIdx >= 1
		q.mu.Unlock()
		if rolled && !sawRoll {
			sawRoll = true
			// First frame that landed in wal-000001: checkpointing here
			// puts the durability point strictly past the legacy segment.
			target = off
		}
	}
	if !sawRoll {
		t.Fatalf("expected a roll within 40 events at maxBytes=%d", maxBytes)
	}
	before := q.Stats().Segments
	if before < 3 {
		t.Fatalf("expected >=3 segments before GC, got %d", before)
	}
	if err := q.Checkpoint(target); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if got := q.Stats().Segments; got != before-1 {
		t.Fatalf("acknowledged segments must be deleted: %d remain, want %d", got, before-1)
	}
	// The events in the deleted segment are checkpointed — committed — so
	// replay must return only the tail past the checkpoint.
	pending, err := q.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	gotIDs := ids(pending)
	if len(gotIDs) == 0 || gotIDs[0] == "e00" || gotIDs[len(gotIDs)-1] != "e39" {
		t.Fatalf("pending after segment GC = %v…%v, want the uncheckpointed tail through e39 (e00's segment is checkpointed)", gotIDs[0], gotIDs[len(gotIDs)-1])
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Reopen: the deleted segment stays deleted and the checkpoint still
	// resolves (it names a segment that exists).
	q2 := testQueue2(t, q)
	defer q2.Close()
	if got := q2.Stats().Segments; got != before-1 {
		t.Fatalf("segments after reopen = %d, want %d", got, before-1)
	}
}

// F16: the disk high-water is enforced by dropping the oldest segment —
// loudly, and counted, when that segment still held unacknowledged events.
// Admission keeps working; the crash-recovery copy of the dropped events is
// what is lost.
//
// O01 era 2 (2026-09-22, ADR §5.2): this delete-at-breach behavior now
// lives ONLY in explicit lossy mode (WithLossySync) — durable mode refuses
// admission instead (pinned in o01_group_commit_test.go). The test opts
// into lossy to keep pinning the F16 semantics where they survive.
func TestDiskQueue_HighWaterBreachDropsOldestSegmentLoudly(t *testing.T) {
	dir := t.TempDir()
	var logBuf memorySink
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	const maxBytes = 600
	const maxTotal = 3 * maxBytes
	q, err := NewDiskQueue(dir, "events", time.Hour, maxBytes, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	q.WithMaxTotalBytes(maxTotal)
	q.WithLossySync()
	var appended []string
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("e%03d", i)
		if _, err := q.Append(ev(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		appended = append(appended, id)
	}
	stats := q.Stats()
	if stats.Bytes > maxTotal+maxBytes {
		t.Fatalf("high-water not enforced: %d bytes on disk (cap %d)", stats.Bytes, maxTotal)
	}
	if stats.DroppedUnackedSegments == 0 || stats.DroppedUnackedBytes == 0 {
		t.Fatalf("breach must be counted: %+v", stats)
	}
	if !logBuf.has("high-water breached") {
		t.Fatalf("breach must be logged loudly, got: %s", logBuf.String())
	}
	// Availability: admission keeps working past the breach.
	if _, err := q.Append(ev("post-breach")); err != nil {
		t.Fatalf("append after breach: %v", err)
	}
	// Replay returns only what physically survived; the oldest events are
	// gone from the WAL (they were never checkpointed — that is the
	// documented breach cost).
	pending, err := q.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	got := ids(pending)
	if equal(got, appended) {
		t.Fatalf("expected the breached segments' events to be gone from replay")
	}
	if got[len(got)-1] != "post-breach" {
		t.Fatalf("post-breach append must survive replay, got tail %v", got[len(got)-1])
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// F16: streaming replay hands the consumer one WAL frame at a time and
// never materializes the backlog; a consumer error aborts the walk.
func TestDiskQueue_StreamPendingFramesAndAbort(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "events", time.Hour, 1<<30, logger)
	if err != nil {
		t.Fatalf("NewDiskQueue: %v", err)
	}
	defer q.Close()
	if _, err := q.AppendBatch([]Event{ev("a1"), ev("a2")}); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if _, err := q.Append(ev("b1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	var frames [][]string
	err = q.StreamPending(func(events []Event) error {
		frames = append(frames, ids(events))
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(frames) != 2 || !equal(frames[0], []string{"a1", "a2"}) || !equal(frames[1], []string{"b1"}) {
		t.Fatalf("frames = %v, want [[a1 a2] [b1]]", frames)
	}
	boom := errors.New("consumer failed")
	if err := q.StreamPending(func(events []Event) error { return boom }); err == nil {
		t.Fatal("consumer error must abort the stream")
	}
}

// F16: a never-rolled queue keeps the exact legacy on-disk layout — a
// pre-F16 binary could open it — and an F16 binary opens a legacy layout
// (decimal checkpoint + current.log) transparently.
func TestDiskQueue_LegacyLayoutCompat(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "events", time.Hour, 1<<30, logger)
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
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Decimal checkpoint (not the JSON segment form) + all data in
	// current.log — byte-compatible with a pre-F16 binary.
	raw, err := os.ReadFile(filepath.Join(dir, "events", "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "{") {
		t.Fatalf("never-rolled queue must keep the legacy decimal checkpoint, got %q", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "events", "wal-000001.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("never-rolled queue must not create numbered segments, stat err=%v", err)
	}
	// A numbered segment appears only after a roll.
	q2 := testQueue2(t, q)
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if got := ids(pending); !equal(got, []string{"e2"}) {
		t.Fatalf("legacy layout replay = %v, want [e2]", got)
	}
}

// memorySink is a tiny io.Writer for capturing slog output in tests.
type memorySink struct{ data []byte }

func (m *memorySink) Write(p []byte) (int, error) {
	m.data = append(m.data, p...)
	return len(p), nil
}

func (m *memorySink) String() string { return string(m.data) }

func (m *memorySink) has(substr string) bool {
	return strings.Contains(m.String(), substr)
}

// TO-012: segment bases are immutable for the queue's lifetime. Repro: an
// acknowledged-segment GC (from an earlier Checkpoint) used to rebase the
// survivors' bases while q.offset and outstanding checkpoint targets stayed
// in the original coordinates — a later Checkpoint of a target captured
// after the rebase mapped it FORWARD by the removed size, marking
// uncommitted records as durable (they never replayed after a crash).
func TestDiskQueue_GCDoesNotRebaseOutstandingCheckpointTargets(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const segBytes = 400
	q, err := NewDiskQueue(dir, "events", time.Hour, segBytes, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	defer q.Close()

	// Append until the first roll, remembering the event that landed in
	// segment 1 — checkpointing ITS end puts cpSeg=1 and GCs segment 0.
	var rolledAt, t1 int64
	for i := 0; ; i++ {
		off, err := q.Append(ev(fmt.Sprintf("gc%03d", i)))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		q.mu.Lock()
		rolled := q.activeIdx >= 1
		q.mu.Unlock()
		if rolled {
			rolledAt, t1 = int64(i), off
			break
		}
	}
	if err := q.Checkpoint(t1); err != nil {
		t.Fatalf("checkpoint t1: %v", err)
	}
	q.mu.Lock()
	segsAfterGC := len(q.segments)
	q.mu.Unlock()
	if segsAfterGC < 1 {
		t.Fatal("expected surviving segments after acknowledged GC")
	}

	// More uncommitted appends; capture their end as the flush target.
	var target int64
	for i := int(rolledAt) + 1; i < int(rolledAt)+30; i++ {
		target, err = q.Append(ev(fmt.Sprintf("gc%03d", i)))
		if err != nil {
			t.Fatalf("append tail: %v", err)
		}
	}
	// Events appended AFTER the target was captured: they are not part of
	// the batch being flushed, must NOT be covered by its checkpoint, and
	// must replay after a crash.
	var postTarget string
	for i := int(rolledAt) + 30; i < int(rolledAt)+36; i++ {
		if _, err = q.Append(ev(fmt.Sprintf("gc%03d", i))); err != nil {
			t.Fatalf("append post-target: %v", err)
		}
		postTarget = fmt.Sprintf("gc%03d", i)
	}
	if err := q.Checkpoint(target); err != nil {
		t.Fatalf("checkpoint target: %v", err)
	}
	// Crash: only the events AFTER `target` are uncommitted — they must
	// ALL replay. Under the old base rebasing, the checkpoint mapped
	// forward by the removed segment's size and skipped the first of them.
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	q2, err := NewDiskQueue(dir, "events", time.Hour, segBytes, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	got := ids(pending)
	if len(got) == 0 {
		t.Fatal("uncommitted tail must replay")
	}
	wantFirst := fmt.Sprintf("gc%03d", rolledAt+30)
	if got[0] != wantFirst || got[len(got)-1] != postTarget {
		t.Fatalf("post-GC replay = %v…%v, want %v…%v (a rebased checkpoint skipped the uncommitted records right after the target)",
			got[0], got[len(got)-1], wantFirst, postTarget)
	}
}

// TO-013: construction never deletes UNACKNOWLEDGED recovery data. A
// backlog above the configured total cap must still be present (and
// replayable) after NewDiskQueueWithLimits — the high-water is a roll-time
// policy, not a startup purge.
func TestDiskQueue_ConstructionPreservesOverCapBacklog(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const segBytes = 400
	// Write a multi-segment backlog whose total exceeds the cap we will
	// pass on reopen.
	q, err := NewDiskQueueWithLimits(dir, "events", time.Hour, segBytes, 1<<20, logger)
	if err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	for i := 0; i < 30; i++ {
		if _, err := q.Append(ev(fmt.Sprintf("cap%02d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	total := q.Stats().Bytes
	if q.Stats().Segments < 2 {
		t.Fatalf("expected a multi-segment backlog, got %d segment(s)", q.Stats().Segments)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen with a total cap BELOW the backlog: every segment (all
	// unacknowledged — nothing was checkpointed) must survive.
	q2, err := NewDiskQueueWithLimits(dir, "events", time.Hour, segBytes, total-1, logger)
	if err != nil {
		t.Fatalf("reopen under cap: %v", err)
	}
	defer q2.Close()
	pending, err := q2.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	got := ids(pending)
	if len(got) != 30 {
		t.Fatalf("construction must preserve the unacknowledged backlog: got %d of 30 events", len(got))
	}
}

// TO-015: a negative JSON checkpoint offset is corruption, not a position.
func TestDiskQueue_NegativeJSONCheckpointOffsetRefused(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := NewDiskQueue(dir, "events", time.Hour, 1<<20, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := q.Append(ev("neg-1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events", "checkpoint"),
		[]byte(`{"segment":0,"offset":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiskQueue(dir, "events", time.Hour, 1<<20, logger); err == nil {
		t.Fatal("a negative JSON checkpoint offset must be refused at open")
	}
}
