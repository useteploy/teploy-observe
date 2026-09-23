package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// TestReplayDedup_DropsAlreadyCommitted is the regression for exactly-once event
// counting across a crash: a WAL replay of an event already committed to the DB
// must be dropped, while a genuinely-new pending event is kept.
func TestReplayDedup_DropsAlreadyCommitted(t *testing.T) {
	db, done := ingestTestDB(t)
	defer done()
	ctx := context.Background()

	site := fmt.Sprintf("replay-%d", time.Now().UnixNano())
	committedID := "evt-committed-" + site
	freshID := "evt-fresh-" + site
	ts := time.Now().UTC().UnixMilli()

	// Simulate a committed event already in the DB (flushed before the crash).
	_, err := db.SQL().Exec(ctx,
		`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp)
		 VALUES ($1, 'default', $2, 's', 's', 'pageview', $3)`,
		committedID, site, ts)
	if err != nil {
		t.Fatalf("seed committed event: %v", err)
	}

	// A WAL queue holding BOTH the committed event (would double-count) and a
	// genuinely-new one (must survive).
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q1, err := NewDiskQueue(dir, "ingest", time.Second, 1<<20, logger)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	for _, e := range []Event{
		{EventID: committedID, TenantID: "default", SiteID: site, SessionID: "s", VisitID: "s", EventType: "pageview", Timestamp: ts},
		{EventID: freshID, TenantID: "default", SiteID: site, SessionID: "s", VisitID: "s", EventType: "pageview", Timestamp: ts},
	} {
		if _, err := q1.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Simulate a crash: close (fsyncs to disk) without checkpointing, then a
	// fresh process reopens the same WAL and replays.
	_ = q1.Close()
	q2, err := NewDiskQueue(dir, "ingest", time.Second, 1<<20, logger)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}

	buf := NewBuffer(db, 1000, 100, time.Hour, logger)
	if err := buf.AttachQueue(q2); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// TO-014: recovery is write-through — the fresh event is committed to
	// the events table during attach (not staged in the in-memory buffer),
	// the committed one is deduped, and nothing stays buffered.
	if got := buf.Len(); got != 0 {
		t.Fatalf("write-through recovery must leave the in-memory buffer empty, got %d events", got)
	}
	countRows := func(q string, args ...any) int {
		rows, err := nucleus.Query[struct {
			N string `db:"n"`
		}](ctx, db.SQL(), q, args...)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("count: expected 1 row, got %d", len(rows))
		}
		var n int
		if _, err := fmt.Sscanf(rows[0].N, "%d", &n); err != nil {
			t.Fatalf("count parse %q: %v", rows[0].N, err)
		}
		return n
	}
	for _, tc := range []struct {
		id   string
		want int
	}{
		{committedID, 1},
		{freshID, 1},
	} {
		got := countRows("SELECT CAST(COUNT(*) AS TEXT) AS n FROM events WHERE event_id = $1", tc.id)
		if got != tc.want {
			t.Fatalf("event %s: want %d row(s) after recovery, got %d", tc.id, tc.want, got)
		}
	}

	// The recovery checkpoint advanced: reopening the queue again must
	// replay nothing.
	_ = q2.Close()
	q3, err := NewDiskQueue(dir, "ingest", time.Second, 1<<20, logger)
	if err != nil {
		t.Fatalf("reopen after recovery: %v", err)
	}
	defer q3.Close()
	buf3 := NewBuffer(db, 1000, 100, time.Hour, logger)
	if err := buf3.AttachQueue(q3); err != nil {
		t.Fatalf("second attach: %v", err)
	}
	if got := buf3.Len(); got != 0 {
		t.Fatalf("a checkpointed recovery must not replay again, got %d events", got)
	}
}
