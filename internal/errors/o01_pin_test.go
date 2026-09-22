package errors

// O01 pinning oracle (docs/O01_DURABLE_INGEST_ADR.md §1.2, §2.2). Pins
// the CURRENT errors-signal semantics: the HTTP ack (ErrorBuffer.Push
// returning true, which errorIngestHandler turns into 200 OK) is backed
// by MEMORY ONLY, and a storage failure at flush time DROPS the acked
// record (logged, never requeued, never retried, no producer identity).
// Guards the ADR §5.6 error-inbox line and §5.4 backpressure line: when
// the durable error inbox lands, this test must be updated deliberately
// in the same change (dedupe + no-silent-drop) and the era recorded in
// the ADR.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

type syncSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

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

func TestO01_ErrorAckIsMemoryOnlyAndFlushFailureDrops(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	dbStore, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer dbStore.Close()
	dbCheck, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer dbCheck.Close()

	site := fmt.Sprintf("o01-err-%d", time.Now().UnixNano())
	svc := NewService(dbStore, NewIssueService(dbStore), NewSearchService(dbStore), nil)
	sink := &syncSink{}
	b := NewErrorBuffer(svc, 100, 10, time.Hour, slog.New(slog.NewTextHandler(sink, nil)))

	// Healthy store: Push (the ack basis) then Flush applies exactly once.
	if !b.Push(site, ErrorInput{ErrorType: "O01Pin", ErrorValue: "stored"}) {
		t.Fatal("Push must admit — this true is what errorIngestHandler turns into 200 OK")
	}
	if queued, _ := b.Stats(); queued != 1 {
		t.Fatalf("acked record must be memory-queued, got %d", queued)
	}
	if o01CountErrorEvents(t, dbCheck, site) != 0 {
		t.Fatal("record must NOT be stored at ack time — the errors path has no synchronous storage")
	}
	b.Flush()
	if queued, bytes := b.Stats(); queued != 0 || bytes != 0 {
		t.Fatalf("healthy flush must drain the queue, got queued=%d bytes=%d", queued, bytes)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("healthy flush must apply the acked record exactly once, got %d rows", got)
	}

	// Dead store: the record is ACKED (Push true, 200-equivalent), then
	// the flush fails and the record is DROPPED — never requeued, never
	// retried, absent from storage. This is the disposition ADR §5.4/§5.6
	// replaces; the pin makes the change deliberate.
	dbStore.Close()
	if !b.Push(site, ErrorInput{ErrorType: "O01Pin", ErrorValue: "dropped"}) {
		t.Fatal("Push must still admit — the buffer cannot see the storage failure")
	}
	b.Flush()
	if queued, bytes := b.Stats(); queued != 0 || bytes != 0 {
		t.Fatalf("CURRENT-semantics pin: a failed flush DROPS the record (budget released, nothing requeued), got queued=%d bytes=%d", queued, bytes)
	}
	if got := o01CountErrorEvents(t, dbCheck, site); got != 1 {
		t.Fatalf("the acked-then-failed record must be absent from storage (1 pre-existing row expected), got %d", got)
	}
	if !strings.Contains(sink.String(), "error flush failed") {
		t.Fatal("the drop must at least be logged — silent loss is not today's contract either")
	}
}
