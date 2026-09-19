package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// F12 durable dedupe boundary, executed against a live Nucleus: events
// carrying producer-stable ids are inserted exactly once no matter how many
// times a flush processes them - the response-lost client retry, the
// restart-then-retry, and the WAL-replay case all reduce to "the flush path
// sees the same event_id twice".
func testBufferDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func producerEvent(id, site string, ts int64) Event {
	return Event{
		EventID:   id,
		TenantID:  "default",
		SiteID:    site,
		SessionID: "sess-f12",
		VisitID:   "visit-f12",
		EventType: "pageview",
		Timestamp: ts,
		URL:       "https://example.com/f12",
	}
}

// TestFlushDuplicateProducerIDsInsertedOnce is the retry-safety proof for
// the events path: submit a batch, flush (commit), then re-submit the SAME
// event ids as a client retry would (the retry's events carry fresh server
// timestamps, exactly as re-preparing produces) and flush again. The stored
// row count must not change, and events_recent must stay in lockstep.
func TestFlushDuplicateProducerIDsInsertedOnce(t *testing.T) {
	db := testBufferDB(t)
	buf := NewBuffer(db, 1000, 1000, time.Hour, slog.Default())
	ctx := context.Background()
	site := fmt.Sprintf("f12-site-%d", time.Now().UnixNano())

	// Ids unique per run: the flush filter dedupes by event_id GLOBALLY
	// (producer ids are unique in practice), so a literal id reused across
	// test runs would be dropped as a duplicate of the previous run's row.
	id1 := fmt.Sprintf("retry-id-%d-1", time.Now().UnixNano())
	id2 := fmt.Sprintf("retry-id-%d-2", time.Now().UnixNano())
	first := []Event{
		producerEvent(id1, site, time.Now().UnixMilli()),
		producerEvent(id2, site, time.Now().UnixMilli()),
	}
	if !buf.PushBatch(first) {
		t.Fatal("first admission failed")
	}
	buf.Flush()

	// The retry: same producer event ids, new admission timestamps (the
	// server re-stamps at prepare time). This is the exact shape of a
	// response-lost retry that slipped past the admission cache.
	retry := []Event{
		producerEvent(id1, site, time.Now().Add(time.Second).UnixMilli()),
		producerEvent(id2, site, time.Now().Add(time.Second).UnixMilli()),
	}
	if !buf.PushBatch(retry) {
		t.Fatal("retry admission failed")
	}
	buf.Flush()

	countEvents := func(q string, args ...any) int {
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
	var events, recent int
	events = countEvents("SELECT CAST(COUNT(*) AS TEXT) AS n FROM events WHERE site_id = $1 AND event_id IN ($2, $3)",
		site, id1, id2)
	recent = countEvents("SELECT CAST(COUNT(*) AS TEXT) AS n FROM events_recent WHERE site_id = $1 AND event_id IN ($2, $3)",
		site, id1, id2)
	if events != 2 || recent != 2 {
		t.Fatalf("duplicates must be dropped at flush: events=%d events_recent=%d (want 2/2)", events, recent)
	}
}

// TestFlushMixedDuplicateAndFresh: a retried batch that ALSO carries new
// events (producer re-queued and appended) must insert only the new ones -
// the filter drops the known ids without failing the chunk.
func TestFlushMixedDuplicateAndFresh(t *testing.T) {
	db := testBufferDB(t)
	buf := NewBuffer(db, 1000, 1000, time.Hour, slog.Default())
	ctx := context.Background()
	site := fmt.Sprintf("f12-mix-%d", time.Now().UnixNano())

	dupID := fmt.Sprintf("mix-id-%d-1", time.Now().UnixNano())
	freshID := fmt.Sprintf("mix-id-%d-2", time.Now().UnixNano())
	first := []Event{producerEvent(dupID, site, time.Now().UnixMilli())}
	if !buf.PushBatch(first) {
		t.Fatal("first admission failed")
	}
	buf.Flush()

	mixed := []Event{
		producerEvent(dupID, site, time.Now().UnixMilli()),   // duplicate
		producerEvent(freshID, site, time.Now().UnixMilli()), // fresh
	}
	if !buf.PushBatch(mixed) {
		t.Fatal("second admission failed")
	}
	buf.Flush()

	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		"SELECT CAST(COUNT(*) AS TEXT) AS n FROM events WHERE site_id = $1 AND event_id IN ($2, $3)",
		site, dupID, freshID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(rows[0].N, "%d", &n); err != nil {
		t.Fatalf("count parse: %v", err)
	}
	if n != 2 {
		t.Fatalf("mixed retry must yield exactly the 2 distinct events, got %d", n)
	}
}

// TO-018: the durable event-id dedupe is SITE-SCOPED. A producer-chosen
// event id is only an identity within its owner site: the same id under a
// second site must store BOTH records, while a same-site retry still
// deduplicates.
func TestFlush_CrossSiteEventIDsAreDistinct(t *testing.T) {
	db := testBufferDB(t)
	defer db.Close()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	buf := NewBuffer(db, 1000, 100, time.Hour, logger)

	siteA := fmt.Sprintf("to018a-%d", time.Now().UnixNano())
	siteB := fmt.Sprintf("to018b-%d", time.Now().UnixNano())
	sharedID := "evt-shared-" + siteA
	ts := time.Now().UTC().UnixMilli()

	first := Event{EventID: sharedID, TenantID: "default", SiteID: siteA, SessionID: "s", VisitID: "s", EventType: "pageview", Timestamp: ts}
	if !buf.PushBatch([]Event{first}) {
		t.Fatal("site A admission failed")
	}
	buf.Flush()

	// Same id, different site: must store a SECOND record.
	second := first
	second.SiteID = siteB
	if !buf.PushBatch([]Event{second}) {
		t.Fatal("site B admission failed")
	}
	buf.Flush()

	// Same id, same site A again: the durable dedupe drops it.
	if !buf.PushBatch([]Event{first}) {
		t.Fatal("site A retry admission failed")
	}
	buf.Flush()

	count := func(site string) int {
		rows, err := nucleus.Query[struct {
			N string `db:"n"`
		}](ctx, db.SQL(),
			"SELECT CAST(COUNT(*) AS TEXT) AS n FROM events WHERE event_id = $1 AND site_id = $2",
			sharedID, site)
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
	if got := count(siteA); got != 1 {
		t.Fatalf("site A: want exactly 1 record, got %d", got)
	}
	if got := count(siteB); got != 1 {
		t.Fatalf("site B: the same event id under another site must store its own record, got %d", got)
	}
}
