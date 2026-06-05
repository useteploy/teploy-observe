package tracing

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

func connectDB(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return ctx, db, func() {
		db.Close()
		cancel()
	}
}

// TestRED_FromRawSpans_CountsAllBatches guards finding #7. RED metrics must come
// from raw spans, NOT the per-batch service_stats rollup — that rollup wrote one
// row per ingest batch into a ReplacingMergeTree keyed on (service, op, bucket),
// so read-time dedup kept only the newest version and earlier batches' counts
// were silently dropped (undercount). Spans from two "batches" in one window
// must sum to the full total.
func TestRED_FromRawSpans_CountsAllBatches(t *testing.T) {
	ctx, db, done := connectDB(t)
	defer done()

	site := "redtest_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const svc = "svcRED"
	base := time.Now().Add(-1 * time.Hour)
	st := base.UnixMilli()

	ins := func(spanID string, durMs int64, status string) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO spans (trace_id, span_id, parent_span_id, tenant_id, site_id, service_name, operation_name, start_time, end_time, duration_ms, status_code)
			 VALUES ($1,$2,'','default',$3,$4,'GET /x',$5,$6,$7,$8)`,
			"t-"+spanID, spanID, site, svc, st, st+durMs, durMs, status,
		); err != nil {
			t.Fatalf("insert span: %v", err)
		}
	}
	// batch 1: 2 spans; batch 2: 3 spans (one error). Total: 5 requests, 1 error.
	ins("a", 10, "ok")
	ins("b", 20, "ok")
	ins("c", 30, "ok")
	ins("d", 40, "error")
	ins("e", 50, "ok")

	q := NewQueryService(db)
	out, err := q.ListServices(ctx, site, base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}

	var got *ServiceSummary
	for i := range out {
		if out[i].ServiceName == svc {
			got = &out[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("service %q not found in %d results", svc, len(out))
	}
	if got.RequestCount != 5 {
		t.Errorf("request_count=%d, want 5 (RED must count all batches, not just the newest rollup version)", got.RequestCount)
	}
	if got.ErrorCount != 1 {
		t.Errorf("error_count=%d, want 1", got.ErrorCount)
	}
	if got.P99 < 10 {
		t.Errorf("p99=%d, want a real percentile from raw durations (>0)", got.P99)
	}
}
