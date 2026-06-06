package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
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

	// Only the fresh event should be queued for replay; the committed one is dropped.
	buf.mu.Lock()
	got := append([]Event(nil), buf.events...)
	buf.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want 1 replayed event (fresh only), got %d: %+v", len(got), got)
	}
	if got[0].EventID != freshID {
		t.Fatalf("replayed wrong event: got %q, want %q", got[0].EventID, freshID)
	}
}
