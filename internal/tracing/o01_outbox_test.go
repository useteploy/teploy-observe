package tracing

// O01 slice 3 oracle (docs/O01_DURABLE_INGEST_ADR.md section 5.7, the trace
// signal end to end): span commit + derived-work intents are ONE atomic unit,
// the worker derives idempotently from the frozen payload, and a crash
// between commit and derive resumes on the next start. Every test names the
// contract line it guards. Nucleus-gated: self-migrating, skips without a
// reachable engine.

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func o01TraceFixture(t *testing.T) (*nucleus.Client, string) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, fmt.Sprintf("o01-tr-%d", time.Now().UnixNano())
}

// o01Export builds a two-span export for service "o01svc": a 10ms server
// span and a 1500ms child DB span (db.system + db.statement attributes) that
// trips the slow_db_query detector at its 1000ms threshold. Times are fixed
// so the rollup bucket is deterministic.
func o01Export() ExportTraceRequest {
	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	nano := func(d time.Duration) string { return strconv.FormatInt(base.Add(d).UnixNano(), 10) }
	return ExportTraceRequest{
		ResourceSpans: []ResourceSpans{{
			Resource: Resource{Attributes: []KeyValue{
				{Key: "service.name", Value: AnyValue{StringValue: "o01svc"}},
			}},
			ScopeSpans: []ScopeSpans{{Spans: []OTLPSpan{
				{
					TraceID: "o01trace0000000001", SpanID: "o01span00000001",
					Name: "GET /x", Kind: 2,
					StartTimeUnixNano: nano(0), EndTimeUnixNano: nano(10 * time.Millisecond),
					Status: SpanStatus{Code: 1},
				},
				{
					TraceID: "o01trace0000000001", SpanID: "o01span00000002", ParentSpanID: "o01span00000001",
					Name: "db select users", Kind: 3,
					StartTimeUnixNano: nano(10 * time.Millisecond), EndTimeUnixNano: nano(1510 * time.Millisecond),
					Attributes: []KeyValue{
						{Key: "db.system", Value: AnyValue{StringValue: "postgres"}},
						{Key: "db.statement", Value: AnyValue{StringValue: "SELECT * FROM users"}},
					},
					Status: SpanStatus{Code: 1},
				},
			}}},
		}},
	}
}

func o01CountSpans(t *testing.T, db *nucleus.Client, site string) int {
	t.Helper()
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](context.Background(), db.SQL(),
		"SELECT CAST(COUNT(*) AS TEXT) AS n FROM spans WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("count spans: %v", err)
	}
	n, _ := strconv.Atoi(rows[0].N)
	return n
}

func o01CountOutbox(t *testing.T, db *nucleus.Client, site string) int {
	t.Helper()
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](context.Background(), db.SQL(),
		"SELECT CAST(COUNT(*) AS TEXT) AS n FROM derived_outbox WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("count derived_outbox: %v", err)
	}
	n, _ := strconv.Atoi(rows[0].N)
	return n
}

func o01OutboxKinds(t *testing.T, db *nucleus.Client, site string) map[string]bool {
	t.Helper()
	rows, err := nucleus.Query[struct {
		Kind string `db:"kind"`
	}](context.Background(), db.SQL(),
		"SELECT DISTINCT kind FROM derived_outbox WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("read outbox kinds: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Kind] = true
	}
	return out
}

// o01ServiceStat reads the collapsed service_stats row for one
// (service, operation) as the query path would.
func o01ServiceStat(t *testing.T, db *nucleus.Client, site, service, operation string) (reqCount, errCount, durMax string) {
	t.Helper()
	rows, err := nucleus.Query[struct {
		ReqCount string `db:"request_count"`
		ErrCount string `db:"error_count"`
		DurMax   string `db:"duration_max"`
	}](context.Background(), db.SQL(),
		`SELECT request_count, error_count, duration_max FROM (
			SELECT tenant_id, site_id, service_name, operation_name, ts_bucket,
				argMax(request_count, version) AS request_count,
				argMax(error_count, version) AS error_count,
				argMax(duration_max, version) AS duration_max,
				MAX(version) AS version
			FROM service_stats WHERE site_id = $1
			GROUP BY tenant_id, site_id, service_name, operation_name, ts_bucket
		 ) WHERE service_name = $2 AND operation_name = $3`,
		site, service, operation)
	if err != nil {
		t.Fatalf("read service_stats: %v", err)
	}
	if len(rows) == 0 {
		return "", "", ""
	}
	return rows[0].ReqCount, rows[0].ErrCount, rows[0].DurMax
}

// o01PerfIssueCount reads the count of the latest performance_issues row for
// one detector on a site.
func o01PerfIssueCount(t *testing.T, db *nucleus.Client, site, detector string) (count int, found bool) {
	t.Helper()
	rows, err := nucleus.Query[struct {
		Count string `db:"count"`
	}](context.Background(), db.SQL(),
		`SELECT CAST(count AS TEXT) AS count FROM performance_issues
		 WHERE site_id = $1 AND detector_name = $2
		 ORDER BY last_seen DESC LIMIT 1`, site, detector)
	if err != nil {
		t.Fatalf("read performance_issues: %v", err)
	}
	if len(rows) == 0 {
		return 0, false
	}
	fmt.Sscanf(rows[0].Count, "%d", &count)
	return count, true
}

// failingEnqueuer is the seam the atomicity tests use to break the ingest
// transaction at a chosen point: failAt = 1 fails the first (rollup)
// enqueue, 2 the second (detector) one.
type failingEnqueuer struct{ failAt, calls int }

func (f *failingEnqueuer) Enqueue(ctx context.Context, sql *nucleus.SQLModel, siteID, kind string, payload any) (string, error) {
	f.calls++
	if f.calls >= f.failAt {
		return "", fmt.Errorf("injected enqueue failure at call %d", f.calls)
	}
	return fmt.Sprintf("stub-%d", f.calls), nil
}

func (f *failingEnqueuer) ProcessIDs(ctx context.Context, ids ...string) error { return nil }

// TestO01_TraceIngestWritesIntentsInSpanTx guards the programme contract
// line "an outbox committed with their originating state" for the trace
// signal: a successful ingest leaves exactly the rollup + detector intents
// committed alongside the spans, pending at ack time (the derive happens
// after, from the durable intent).
func TestO01_TraceIngestWritesIntentsInSpanTx(t *testing.T) {
	db, site := o01TraceFixture(t)
	svc := NewIngestService(db)
	req := o01Export()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := svc.Ingest(ctx, site, req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if resp.Spans != 2 {
		t.Fatalf("resp.Spans = %d, want 2", resp.Spans)
	}
	if got := o01CountSpans(t, db, site); got != 2 {
		t.Fatalf("spans committed = %d, want 2", got)
	}
	if got := o01CountOutbox(t, db, site); got != 2 {
		t.Fatalf("derived_outbox rows = %d, want 2 (rollup + detector intents committed with the spans)", got)
	}
	kinds := o01OutboxKinds(t, db, site)
	if !kinds["rollup"] || !kinds["detector"] {
		t.Fatalf("intent kinds = %v, want rollup + detector", kinds)
	}
}

// TestO01_TraceTxRollbackLeavesNoSpansNoIntents guards the atomic-unit
// boundary from the enqueue side: when an intent write fails inside the
// ingest transaction, the WHOLE request rolls back — no spans, no intents,
// and the exporter's retry reprocesses cleanly (no half-committed prefix).
func TestO01_TraceTxRollbackLeavesNoSpansNoIntents(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("failAt=%d", failAt), func(t *testing.T) {
			db, site := o01TraceFixture(t)
			svc := NewIngestService(db)
			svc.outbox = &failingEnqueuer{failAt: failAt}
			req := o01Export()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := svc.Ingest(ctx, site, req); err == nil {
				t.Fatal("ingest with a failing enqueue must return an error")
			}
			if got := o01CountSpans(t, db, site); got != 0 {
				t.Fatalf("spans committed = %d, want 0 (rolled back with the failed enqueue)", got)
			}
			if got := o01CountOutbox(t, db, site); got != 0 {
				t.Fatalf("derived_outbox rows = %d, want 0 (no orphan intents)", got)
			}
		})
	}
}

// spansDDL recreates the spans table after a drop (003 shape), so a test can
// force span-insert failures against the real schema.
const spansDDL = `CREATE TABLE IF NOT EXISTS spans (
    trace_id       TEXT NOT NULL,
    span_id        TEXT NOT NULL,
    parent_span_id TEXT NOT NULL DEFAULT '',
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    service_name   TEXT NOT NULL DEFAULT '',
    operation_name TEXT NOT NULL DEFAULT '',
    span_kind      TEXT NOT NULL DEFAULT 'internal',
    start_time     BIGINT NOT NULL,
    end_time       BIGINT NOT NULL,
    duration_ms    BIGINT NOT NULL DEFAULT 0,
    status_code    TEXT NOT NULL DEFAULT 'unset',
    status_message TEXT NOT NULL DEFAULT '',
    attributes     JSONB,
    resource       JSONB,
    events         JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, start_time, trace_id, span_id)`

// TestO01_TraceSpanInsertFailureNoOrphanIntent guards the atomic-unit
// boundary from the span side: a failing span insert rolls the intent writes
// back with it — an intent can never exist for spans that never committed.
func TestO01_TraceSpanInsertFailureNoOrphanIntent(t *testing.T) {
	db, site := o01TraceFixture(t)
	if _, err := db.SQL().Exec(context.Background(), "DROP TABLE IF EXISTS spans"); err != nil {
		t.Fatalf("drop spans: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.SQL().Exec(context.Background(), spansDDL); err != nil {
			t.Fatalf("recreate spans: %v", err)
		}
	})
	svc := NewIngestService(db)
	req := o01Export()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := svc.Ingest(ctx, site, req); err == nil {
		t.Fatal("ingest with the spans table missing must return an error")
	}
	if got := o01CountOutbox(t, db, site); got != 0 {
		t.Fatalf("derived_outbox rows = %d, want 0 (no intents for spans that never committed)", got)
	}
}

// TestO01_TraceCrashResumeDerivesIdempotently guards section 5.7 "crash
// mid-derive = at-least-once derive, idempotent by construction": after a
// crash between the span commit and any derive, a restarted process drains
// the intents and produces the derived values — and deriving the SAME
// intents a second time leaves the derived VALUES unchanged.
func TestO01_TraceCrashResumeDerivesIdempotently(t *testing.T) {
	db, site := o01TraceFixture(t)
	req := o01Export()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Originating process: commits spans + intents, then "crashes" before
	// any derive runs.
	origin := NewIngestService(db)
	if _, err := origin.Ingest(ctx, site, req); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, _, durMax := o01ServiceStat(t, db, site, "o01svc", "db select users"); durMax != "" {
		t.Fatal("precondition: no service_stats row should exist before the worker derives")
	}

	// Restart: a fresh service + outbox over the same database resumes.
	restarted := NewIngestService(db)
	if n, err := restarted.Outbox().ProcessDue(ctx); err != nil {
		t.Fatalf("resume drain: %v", err)
	} else if n < 2 {
		t.Fatalf("resume drain processed %d intents, want >= 2 (rollup + detector)", n)
	}

	reqCount, errCount, durMax := o01ServiceStat(t, db, site, "o01svc", "db select users")
	if reqCount != "1" || errCount != "0" || durMax != "1500" {
		t.Fatalf("service_stats after resume = (%q,%q,%q), want (1,0,1500)", reqCount, errCount, durMax)
	}
	rootReq, _, _ := o01ServiceStat(t, db, site, "o01svc", "GET /x")
	if rootReq != "1" {
		t.Fatalf("GET /x request_count = %q, want 1", rootReq)
	}
	perfCount, found := o01PerfIssueCount(t, db, site, "slow_db_query")
	if !found {
		t.Fatal("slow_db_query issue missing after resume")
	}
	if perfCount != 1 {
		t.Fatalf("slow_db_query count = %d, want 1", perfCount)
	}

	// At-least-once delivery: re-derive the SAME intents (a duplicate
	// delivery, not a new batch) and assert the derived VALUES are stable.
	rows, err := nucleus.Query[struct {
		ID string `db:"id"`
	}](ctx, db.SQL(), "SELECT DISTINCT id FROM derived_outbox WHERE site_id = $1", site)
	if err != nil || len(rows) != 2 {
		t.Fatalf("read intent ids: %v (rows=%d)", err, len(rows))
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if err := restarted.Outbox().ProcessIDs(ctx, ids...); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	reqCount2, _, durMax2 := o01ServiceStat(t, db, site, "o01svc", "db select users")
	if reqCount2 != reqCount || durMax2 != durMax {
		t.Fatalf("service_stats changed on re-derive: (%q,%q) vs (%q,%q)", reqCount2, durMax2, reqCount, durMax)
	}
	perfCount2, _ := o01PerfIssueCount(t, db, site, "slow_db_query")
	if perfCount2 != perfCount {
		t.Fatalf("detector count drifted on re-derive: %d vs %d (KV completion markers must hold)", perfCount2, perfCount)
	}
}

// TestO01_TraceSeedSyncPathDerivesImmediately guards the IngestSync contract
// (the demo seeder): synchronous ingest derives AND marks within the call —
// a fresh dev stack shows data the moment seeding finishes, nothing left
// pending.
func TestO01_TraceSeedSyncPathDerivesImmediately(t *testing.T) {
	db, site := o01TraceFixture(t)
	svc := NewIngestService(db)
	req := o01Export()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := svc.IngestSync(ctx, site, req); err != nil {
		t.Fatalf("IngestSync: %v", err)
	}
	if _, _, durMax := o01ServiceStat(t, db, site, "o01svc", "db select users"); durMax != "1500" {
		t.Fatalf("service_stats after IngestSync = %q, want 1500 (derive is synchronous)", durMax)
	}
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS n FROM (
			SELECT tenant_id, id, argMax(processed_at, version) AS processed_at, MAX(version) AS version
			FROM derived_outbox WHERE site_id = $1 GROUP BY tenant_id, id
		 ) WHERE processed_at = 0`, site)
	if err != nil {
		t.Fatalf("count pending intents: %v", err)
	}
	if rows[0].N != "0" {
		t.Fatalf("pending intents after IngestSync = %s, want 0", rows[0].N)
	}
}

// TestO01_TraceWorkerDeadLettersFailingDerive guards "a bounded dead-letter
// for work whose application keeps failing": with the derived tables gone,
// the trace intents exhaust their attempt budget, stay readable with
// last_error, and are counted per kind at Stats.
func TestO01_TraceWorkerDeadLettersFailingDerive(t *testing.T) {
	db, site := o01TraceFixture(t)
	for _, ddl := range []string{
		"DROP TABLE IF EXISTS service_stats",
		"DROP TABLE IF EXISTS performance_issues",
	} {
		if _, err := db.SQL().Exec(context.Background(), ddl); err != nil {
			t.Fatalf("drop derived table: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, ddl := range []string{
			`CREATE TABLE IF NOT EXISTS service_stats (
				tenant_id      TEXT NOT NULL DEFAULT 'default',
				site_id        TEXT NOT NULL,
				service_name   TEXT NOT NULL,
				operation_name TEXT NOT NULL DEFAULT '',
				ts_bucket      BIGINT NOT NULL,
				request_count  TEXT NOT NULL DEFAULT '0',
				error_count    TEXT NOT NULL DEFAULT '0',
				duration_sum   TEXT NOT NULL DEFAULT '0',
				duration_min   TEXT NOT NULL DEFAULT '0',
				duration_max   TEXT NOT NULL DEFAULT '0',
				p50_ms         TEXT NOT NULL DEFAULT '0',
				p95_ms         TEXT NOT NULL DEFAULT '0',
				p99_ms         TEXT NOT NULL DEFAULT '0',
				version        BIGINT NOT NULL DEFAULT 0
			) WITH (engine = 'replacing_mergetree', version_column = 'version')
			ORDER BY (tenant_id, site_id, service_name, operation_name, ts_bucket)`,
			`CREATE TABLE IF NOT EXISTS performance_issues (
				issue_id       TEXT NOT NULL,
				tenant_id      TEXT NOT NULL DEFAULT 'default',
				site_id        TEXT NOT NULL,
				trace_id       TEXT NOT NULL,
				detector_name  TEXT NOT NULL,
				fingerprint    TEXT NOT NULL,
				title          TEXT NOT NULL,
				description    TEXT NOT NULL DEFAULT '',
				severity       TEXT NOT NULL DEFAULT 'warning',
				count          BIGINT NOT NULL DEFAULT 1,
				first_seen     BIGINT NOT NULL,
				last_seen      BIGINT NOT NULL
			) WITH (engine = 'replacing_mergetree', version_column = 'last_seen')
			ORDER BY (tenant_id, site_id, fingerprint)`,
		} {
			if _, err := db.SQL().Exec(context.Background(), ddl); err != nil {
				t.Fatalf("recreate derived table: %v", err)
			}
		}
	})

	svc := NewIngestService(db)
	svc.Outbox().WithMaxAttempts(2).WithBackoffBase(0)
	req := o01Export()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := svc.Ingest(ctx, site, req); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := svc.Outbox().ProcessDue(ctx); err != nil {
			t.Fatalf("ProcessDue pass %d: %v", pass, err)
		}
	}
	st := svc.Outbox().Stats(ctx)
	for _, kind := range []string{"rollup", "detector"} {
		if st.Kinds[kind].DeadLettered < 1 {
			t.Fatalf("kind %q not dead-lettered after exhausting its budget: %+v", kind, st.Kinds[kind])
		}
	}
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS n FROM (
			SELECT tenant_id, id, argMax(last_error, version) AS last_error, MAX(version) AS version
			FROM derived_outbox WHERE site_id = $1 GROUP BY tenant_id, id
		 ) WHERE last_error = ''`, site)
	if err != nil {
		t.Fatalf("count errored intents: %v", err)
	}
	if rows[0].N != "0" {
		t.Fatalf("%s intents without last_error after dead-lettering, want 0", rows[0].N)
	}
}
