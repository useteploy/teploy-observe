package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestTraceErrors_ExactMatchWithWindowFallback is the regression test for
// audit #347: two traces overlapping in time must not cross-contaminate
// each other's error lists once errors carry a trace_id. Errors without
// trace context keep the timestamp-window fallback.
//
// Connects to the same live-nucleus DSN as the errors-package integration
// tests; skips cleanly when nucleus isn't running.
func TestTraceErrors_ExactMatchWithWindowFallback(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()

	// The dev DB may predate migration 029 — apply its ALTER idempotently.
	for _, col := range []string{"trace_id", "span_id"} {
		if _, err := db.SQL().Exec(ctx,
			fmt.Sprintf(`ALTER TABLE error_events ADD COLUMN IF NOT EXISTS %s TEXT NOT NULL DEFAULT ''`, col),
		); err != nil {
			t.Fatalf("add %s column: %v", col, err)
		}
	}

	uniq := func() string {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	siteID := "trace-corr-test-" + uniq()
	traceA := uniq()
	traceB := uniq()

	// Two traces overlapping in time: A spans [1000, 2000], B spans [1500, 2500].
	base := time.Now().UnixMilli()
	plantSpan := func(traceID string, start, end int64) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO spans (trace_id, span_id, site_id, operation_name, start_time, end_time)
			 VALUES ($1, $2, $3, 'test-op', $4, $5)`,
			traceID, uniq(), siteID, base+start, base+end,
		); err != nil {
			t.Fatalf("plant span: %v", err)
		}
	}
	plantSpan(traceA, 1000, 2000)
	plantSpan(traceB, 1500, 2500)

	plantError := func(errorID, traceID string, ts int64) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO error_events (error_id, site_id, group_hash, timestamp, trace_id)
			 VALUES ($1, $2, 'gh', $3, $4)`,
			errorID, siteID, base+ts, traceID,
		); err != nil {
			t.Fatalf("plant error %s: %v", errorID, err)
		}
	}
	plantError("e-tagged-a", traceA, 1600)   // tagged A, inside both windows
	plantError("e-untagged", "", 1700)       // no trace context, inside both windows
	plantError("e-tagged-b", traceB, 1800)   // tagged B, inside both windows
	plantError("e-tagged-a-late", traceA, 9000) // tagged A, OUTSIDE A's window

	q := NewQueryService(db)

	got := func(traceID string) map[string]bool {
		hits, err := q.TraceErrors(ctx, traceID, siteID)
		if err != nil {
			t.Fatalf("TraceErrors(%s): %v", traceID, err)
		}
		set := map[string]bool{}
		for _, h := range hits {
			set[h.ErrorID] = true
		}
		return set
	}

	a := got(traceA)
	// Exact matches (even outside the window) + untagged fallback; NOT B's error.
	for _, want := range []string{"e-tagged-a", "e-tagged-a-late", "e-untagged"} {
		if !a[want] {
			t.Errorf("trace A missing %s (got %v)", want, a)
		}
	}
	if a["e-tagged-b"] {
		t.Errorf("trace A cross-contaminated with trace B's error: %v", a)
	}

	b := got(traceB)
	for _, want := range []string{"e-tagged-b", "e-untagged"} {
		if !b[want] {
			t.Errorf("trace B missing %s (got %v)", want, b)
		}
	}
	if b["e-tagged-a"] || b["e-tagged-a-late"] {
		t.Errorf("trace B cross-contaminated with trace A's errors: %v", b)
	}
}
