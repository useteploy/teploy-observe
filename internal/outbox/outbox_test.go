package outbox

// O01 slice 3 oracle (docs/O01_DURABLE_INGEST_ADR.md section 5.7): the
// derived-work outbox worker. Every test names the contract line it guards.
// Nucleus-gated: self-migrating, skips without a reachable engine.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func o01OutboxFixture(t *testing.T) *nucleus.Client {
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
	return db
}

// o01CollapsedRow reads one intent through the argMax collapse — the same
// form the worker's queries use, so the test observes exactly what a reader
// sees after version-rewriting marks.
func o01CollapsedRow(t *testing.T, db *nucleus.Client, id string) (attempts, processedAt, nextAttemptAt int64, lastError string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := nucleus.Query[struct {
		Attempts      string `db:"attempts"`
		ProcessedAt   string `db:"processed_at"`
		NextAttemptAt string `db:"next_attempt_at"`
		LastError     string `db:"last_error"`
	}](ctx, db.SQL(),
		`SELECT CAST(attempts AS TEXT) AS attempts,
			CAST(processed_at AS TEXT) AS processed_at,
			CAST(next_attempt_at AS TEXT) AS next_attempt_at,
			last_error
		 FROM (
			SELECT tenant_id, id,
				argMax(attempts, version) AS attempts,
				argMax(processed_at, version) AS processed_at,
				argMax(next_attempt_at, version) AS next_attempt_at,
				argMax(last_error, version) AS last_error,
				MAX(version) AS version
			FROM derived_outbox WHERE id = $1
			GROUP BY tenant_id, id
		 )`, id)
	if err != nil {
		t.Fatalf("read collapsed outbox row: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no outbox row for id %s", id)
	}
	fmt.Sscanf(rows[0].Attempts, "%d", &attempts)
	fmt.Sscanf(rows[0].ProcessedAt, "%d", &processedAt)
	fmt.Sscanf(rows[0].NextAttemptAt, "%d", &nextAttemptAt)
	return attempts, processedAt, nextAttemptAt, rows[0].LastError
}

// o01RunID makes intent kinds unique per test run: the shared scratch
// fixture accumulates rows across runs, and Stats counts per kind — a
// per-run kind keeps the assertions exact against an aged store.
func o01RunID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func o01RawRowCount(t *testing.T, db *nucleus.Client, site string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(), "SELECT CAST(COUNT(*) AS TEXT) AS n FROM derived_outbox WHERE site_id = $1", site)
	if err != nil {
		t.Fatalf("count derived_outbox: %v", err)
	}
	var n int
	fmt.Sscanf(rows[0].N, "%d", &n)
	return n
}

// TestO01_Outbox_ProcessAndMark guards section 5.7 "an idempotent worker
// drains due intents ... marks processed": a due intent reaches its handler
// with the frozen payload verbatim, and the row is marked processed through a
// version-rewriting insert.
func TestO01_Outbox_ProcessAndMark(t *testing.T) {
	db := o01OutboxFixture(t)
	site := fmt.Sprintf("o01-obx-%d", time.Now().UnixNano())

	kind := "test-ok-" + o01RunID()
	var mu sync.Mutex
	var gotID, gotSite string
	var gotPayload []byte
	s := New(db, slog.New(slog.DiscardHandler))
	s.Register(kind, func(ctx context.Context, intentID, siteID string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		gotID, gotSite, gotPayload = intentID, siteID, append([]byte(nil), payload...)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := s.Enqueue(ctx, db.SQL(), site, kind, map[string]int{"n": 3})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("enqueue returned an empty intent id")
	}

	// The drain is global by design (one worker, every site); against the
	// shared fixture it may also carry leftovers from earlier runs, so the
	// exact evidence is the per-intent assertions below.
	n, err := s.ProcessDue(ctx)
	if err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if n < 1 {
		t.Fatalf("ProcessDue processed %d intents, want >= 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotID != id || gotSite != site {
		t.Fatalf("handler got (id=%q site=%q), want (%q %q)", gotID, gotSite, id, site)
	}
	if string(gotPayload) != `{"n":3}` {
		t.Fatalf("handler payload = %q, want the frozen JSON verbatim", gotPayload)
	}

	attempts, processedAt, _, lastErr := o01CollapsedRow(t, db, id)
	if processedAt == 0 {
		t.Fatal("intent not marked processed (processed_at = 0)")
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d on a successful first pass, want 0", attempts)
	}
	if lastErr != "" {
		t.Fatalf("last_error = %q on a successful intent, want empty", lastErr)
	}

	st := s.Stats(ctx)
	ks := st.Kinds[kind]
	if ks.Processed != 1 || ks.Pending != 0 || ks.DeadLettered != 0 || ks.Failed != 0 {
		t.Fatalf("stats for %s = %+v, want processed=1 pending=0 dead=0 failed=0", kind, ks)
	}
}

// TestO01_Outbox_FailRetryThenDeadLetter guards section 5.7 "retries with
// backoff on failure, dead-letters (keeps the row with last_error + stops
// retrying) after N attempts": attempts advance one per pass, last_error
// records the failure, and once the budget is spent the row stops being
// selected while staying readable with its error.
func TestO01_Outbox_FailRetryThenDeadLetter(t *testing.T) {
	db := o01OutboxFixture(t)
	site := fmt.Sprintf("o01-obx-dl-%d", time.Now().UnixNano())

	kind := "test-fail-" + o01RunID()
	s := New(db, slog.New(slog.DiscardHandler)).
		WithMaxAttempts(3).
		WithBackoffBase(0) // retries immediately due
	s.Register(kind, func(ctx context.Context, intentID, siteID string, payload []byte) error {
		return errors.New("boom")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := s.Enqueue(ctx, db.SQL(), site, kind, "payload")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	for pass := 1; pass <= 3; pass++ {
		n, err := s.ProcessDue(ctx)
		if err != nil {
			t.Fatalf("ProcessDue pass %d: %v", pass, err)
		}
		if n < 1 {
			t.Fatalf("ProcessDue pass %d processed %d intents, want >= 1 (retry must be re-selected)", pass, n)
		}
		attempts, processedAt, _, lastErr := o01CollapsedRow(t, db, id)
		if attempts != int64(pass) {
			t.Fatalf("after pass %d: attempts = %d, want %d", pass, attempts, pass)
		}
		if processedAt != 0 {
			t.Fatalf("after failed pass %d: processed_at = %d, want 0", pass, processedAt)
		}
		if lastErr == "" {
			t.Fatalf("after failed pass %d: last_error is empty", pass)
		}
	}

	// Budget spent: the row is a dead letter — excluded from due selection,
	// kept with its error, and counted.
	if n, _ := s.ProcessDue(ctx); n != 0 {
		t.Fatalf("a dead-lettered intent was selected again (processed %d)", n)
	}
	attempts, processedAt, nextAt, lastErr := o01CollapsedRow(t, db, id)
	if attempts != 3 || processedAt != 0 {
		t.Fatalf("dead letter row = (attempts=%d processed_at=%d), want (3, 0)", attempts, processedAt)
	}
	if nextAt != -1 {
		t.Fatalf("dead letter next_attempt_at = %d, want the -1 sentinel (durably dead)", nextAt)
	}
	if lastErr == "" {
		t.Fatal("dead letter row lost last_error")
	}

	st := s.Stats(ctx)
	ks := st.Kinds[kind]
	if ks.DeadLettered != 1 {
		t.Fatalf("dead_lettered = %d, want 1", ks.DeadLettered)
	}
	if ks.Pending != 0 {
		t.Fatalf("pending = %d on an exhausted intent, want 0", ks.Pending)
	}
	if ks.Failed < 3 {
		t.Fatalf("failed = %d, want >= 3 failed attempts", ks.Failed)
	}
}

// TestO01_Outbox_StartupResume guards section 5.7 "on startup, unprocessed
// intents resume": an intent committed by one process (which then died
// before deriving) is drained by a FRESH store over the same database —
// crash-after-commit-before-derive = at-least-once derive.
func TestO01_Outbox_StartupResume(t *testing.T) {
	db := o01OutboxFixture(t)
	site := fmt.Sprintf("o01-obx-resume-%d", time.Now().UnixNano())

	kind := "test-ok-" + o01RunID()
	var calls int
	var mu sync.Mutex
	origin := New(db, slog.New(slog.DiscardHandler))
	origin.Register(kind, func(ctx context.Context, intentID, siteID string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := origin.Enqueue(ctx, db.SQL(), site, kind, "frozen")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// No ProcessDue: the originating process "crashes" with the intent
	// committed but underived.

	restarted := New(db, slog.New(slog.DiscardHandler))
	restarted.Register(kind, func(ctx context.Context, intentID, siteID string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if string(payload) != `"frozen"` {
			t.Errorf("resumed payload = %q, want the frozen original", payload)
		}
		return nil
	})
	if n, err := restarted.ProcessDue(ctx); err != nil || n < 1 {
		t.Fatalf("resume drain = (%d, %v), want (>=1, nil)", n, err)
	}
	_, processedAt, _, _ := o01CollapsedRow(t, db, id)
	if processedAt == 0 {
		t.Fatal("resumed intent not marked processed")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("handler called %d times across restart, want exactly 1 derive", calls)
	}
}

// TestO01_Outbox_EnqueueRollsBackWithOriginator guards the programme
// contract line "an outbox committed with their originating state": an
// enqueue inside a rolled-back transaction leaves no row — intents can never
// orphan ahead of the state they describe.
func TestO01_Outbox_EnqueueRollsBackWithOriginator(t *testing.T) {
	db := o01OutboxFixture(t)
	site := fmt.Sprintf("o01-obx-rb-%d", time.Now().UnixNano())
	s := New(db, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.Enqueue(ctx, tx.SQL(), site, "test-ok", "doomed"); err != nil {
		t.Fatalf("enqueue inside tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := o01RawRowCount(t, db, site); got != 0 {
		t.Fatalf("rolled-back tx left %d outbox rows, want 0", got)
	}
}

// TestO01_Outbox_UnknownKindFailsLoud guards "an intent whose kind has no
// handler ... dead-letters rather than vanishing": wiring mistakes surface
// as dead letters with last_error, never as silent no-ops.
func TestO01_Outbox_UnknownKindFailsLoud(t *testing.T) {
	db := o01OutboxFixture(t)
	site := fmt.Sprintf("o01-obx-unk-%d", time.Now().UnixNano())
	kind := "never-registered-" + o01RunID()
	s := New(db, slog.New(slog.DiscardHandler)).WithMaxAttempts(1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := s.Enqueue(ctx, db.SQL(), site, kind, nil)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.ProcessDue(ctx); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	attempts, _, _, lastErr := o01CollapsedRow(t, db, id)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (single-attempt budget)", attempts)
	}
	if lastErr == "" {
		t.Fatal("unhandled kind left no last_error")
	}
	st := s.Stats(ctx)
	if st.Kinds[kind].DeadLettered != 1 {
		t.Fatalf("unhandled kind not dead-lettered: %+v", st.Kinds[kind])
	}
}

// TestO01_Outbox_BackoffSchedule is storage-free: the first retry waits the
// base, each later retry doubles, and the cap holds. A zero base means
// immediately due (the red/green tests rely on it).
func TestO01_Outbox_BackoffSchedule(t *testing.T) {
	s := New(nil, slog.New(slog.DiscardHandler)).WithBackoffBase(time.Second).WithBackoffCap(5 * time.Second)
	cases := []struct {
		attempts int64
		want     time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 5 * time.Second},
		{9, 5 * time.Second},
	}
	for _, c := range cases {
		if got := s.backoff(c.attempts); got != c.want {
			t.Fatalf("backoff(attempts=%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
	zero := New(nil, slog.New(slog.DiscardHandler)).WithBackoffBase(0)
	if got := zero.backoff(1); got != 0 {
		t.Fatalf("backoff with zero base = %v, want 0 (immediately due)", got)
	}
}
