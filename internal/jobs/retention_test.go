package jobs

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

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
