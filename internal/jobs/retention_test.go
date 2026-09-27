package jobs

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func connect(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
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

type countRow struct {
	N int64 `db:"n"`
}

// TestRetentionDeletesOldKeepsRecent is the regression for finding #2. The old
// DELETE compared CAST(col AS BIGINT) against a quoted text literal, which
// Nucleus evaluated lexicographically — so it matched NOTHING and every TTL was
// inert. The fix binds an int64 cutoff against the BIGINT column directly.
func TestRetentionDeletesOldKeepsRecent(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	site := "rettest_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now().UnixMilli()
	const day = int64(24 * 60 * 60 * 1000)
	oldTs := now - 40*day // older than the 30-day policy
	newTs := now - 1*day  // inside retention

	ins := func(id string, ts int64) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, site_id, session_id, visit_id, timestamp) VALUES ($1,$2,$3,$4,$5)`,
			id, site, "s", "v", ts,
		); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	ins(site+"_old", oldTs)
	ins(site+"_new", newTs)

	svc := NewRetentionServiceWithPolicies(db, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]RetentionPolicy{{Table: "events", Column: "timestamp", Days: 30}})
	if err := svc.RunCleanup(ctx); err != nil {
		t.Fatalf("RunCleanup: %v", err)
	}

	count := func(ts int64) int64 {
		rows, err := nucleus.Query[countRow](ctx, db.SQL(),
			`SELECT COUNT(*) AS n FROM events WHERE site_id = $1 AND timestamp = $2`, site, ts)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return rows[0].N
	}
	if c := count(oldTs); c != 0 {
		t.Errorf("old row survived retention (got %d, want 0) — TTL inert regression", c)
	}
	if c := count(newTs); c != 1 {
		t.Errorf("recent row wrongly deleted (got %d, want 1)", c)
	}
}

// TestDefaultLedgerPolicies pins the decided ledger windows (2026-09-23):
// error_inbox/replay_batches 14d default, derived_outbox processed-only
// pruning, notification_outbox delivered-or-suppressed-only pruning (O10,
// same decided posture). The outbox policies MUST carry their guards —
// without them the plain column comparison would also delete pending and
// dead-lettered intents (processed_at = 0 < any cutoff).
func TestDefaultLedgerPolicies(t *testing.T) {
	p := DefaultLedgerPolicies(14, 14, 7)
	if len(p) != 4 {
		t.Fatalf("want 4 ledger policies, got %d", len(p))
	}
	byTable := map[string]RetentionPolicy{}
	for _, x := range p {
		byTable[x.Table] = x
	}
	if x := byTable["error_inbox"]; x.Column != "applied_at" || x.Days != 14 || x.ExtraWhere != "" {
		t.Errorf("error_inbox policy wrong: %+v", x)
	}
	if x := byTable["replay_batches"]; x.Column != "first_seen" || x.Days != 14 || x.ExtraWhere != "" {
		t.Errorf("replay_batches policy wrong: %+v", x)
	}
	x := byTable["derived_outbox"]
	if x.Column != "processed_at" || x.Days != 7 || x.ExtraWhere != "processed_at > 0" {
		t.Errorf("derived_outbox policy wrong: %+v", x)
	}
	if x := byTable["notification_outbox"]; x.Column != "created_at" || x.Days != 7 || x.ExtraWhere != "(delivered_at > 0 OR suppressed_at > 0)" {
		t.Errorf("notification_outbox policy wrong: %+v", x)
	}
}

// TestRetentionPrunesProcessedOutboxIntentsNotDeadLetters is the O01
// slice 5 acceptance: the derived_outbox policy deletes a PROCESSED intent
// past its window and leaves a DEAD-LETTERED intent of the same age in
// place — the dead letters are the operator's queue and are never
// auto-pruned.
func TestRetentionPrunesProcessedOutboxIntentsNotDeadLetters(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	now := time.Now().UnixMilli()
	const day = int64(24 * 60 * 60 * 1000)
	old := now - 40*day // far past any sane window
	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	ins := func(id string, processedAt int64) {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO derived_outbox (id, kind, site_id, payload, created_at, attempts, next_attempt_at, processed_at, version)
			 VALUES ($1,'rollup','rettest',$2,$3,5,0,$4,1)`,
			id, runID, old, processedAt,
		); err != nil {
			t.Fatalf("insert outbox intent: %v", err)
		}
	}
	ins(runID+"_processed", old) // processed long ago
	ins(runID+"_dead", 0)        // attempt budget exhausted, never processed

	svc := NewRetentionServiceWithPolicies(db, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]RetentionPolicy{{Table: "derived_outbox", Column: "processed_at", Days: 7, ExtraWhere: "processed_at > 0"}})
	if err := svc.RunCleanup(ctx); err != nil {
		t.Fatalf("RunCleanup: %v", err)
	}

	count := func(id string) int64 {
		rows, err := nucleus.Query[countRow](ctx, db.SQL(),
			`SELECT COUNT(*) AS n FROM derived_outbox WHERE id = $1`, id)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return rows[0].N
	}
	if c := count(runID + "_processed"); c != 0 {
		t.Errorf("processed intent past its window survived (got %d, want 0)", c)
	}
	if c := count(runID + "_dead"); c != 1 {
		t.Errorf("dead-lettered intent was auto-pruned (got %d, want 1) — dead letters are the operator's queue", c)
	}
}

// Trace expiry must use the configured window without erasing the price catalog
// needed to explain historical cost estimates.
func TestLLMRetentionExpiresOldTracesPreservesRecentAndCatalog(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	site := "llm_ret_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now().UnixMilli()
	const day = int64(24 * 60 * 60 * 1000)
	for _, row := range []struct {
		id string
		ts int64
	}{{"old", now - 8*day}, {"recent", now - 6*day}} {
		if _, err := db.SQL().Exec(ctx, `INSERT INTO llm_traces (trace_id, site_id, timestamp, prompt, completion) VALUES ($1,$2,$3,$4,$5)`, site+row.id, site, row.ts, "retained input", "retained output"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().Exec(ctx, `INSERT INTO llm_model_prices (model_prefix, created_at, valid_from) VALUES ($1,$2,$3)`, site, now-40*day, now-40*day); err != nil {
		t.Fatal(err)
	}
	policies := DefaultPolicies(30, 365, 7)
	var selected []RetentionPolicy
	for _, p := range policies {
		if p.Table == "llm_traces" {
			selected = append(selected, p)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("expected exactly one LLM policy, got %d", len(selected))
	}
	svc := NewRetentionServiceWithPolicies(db, slog.New(slog.NewTextHandler(io.Discard, nil)), selected)
	if err := svc.RunCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id   string
		want int64
	}{{"old", 0}, {"recent", 1}} {
		got, err := nucleus.Query[countRow](ctx, db.SQL(), `SELECT COUNT(*) AS n FROM llm_traces WHERE trace_id=$1`, site+row.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].N != row.want {
			t.Errorf("%s trace count=%v, want %d", row.id, got, row.want)
		}
	}
	got, err := nucleus.Query[countRow](ctx, db.SQL(), `SELECT COUNT(*) AS n FROM llm_model_prices WHERE model_prefix=$1`, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].N != 1 {
		t.Fatalf("price catalog was removed: %v", got)
	}
}
