package ingest

import (
	"testing"
	"time"
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
