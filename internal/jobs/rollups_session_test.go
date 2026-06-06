package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestRunSessionRollup_PreservesLongSessions is the regression for the HIGH
// finding that sessions longer than the 30-minute rollup window were truncated:
// the rollup only read the tail and overwrote the complete row with a partial
// one. Two events 45 minutes apart must yield first_ts at the earlier event,
// 2 pageviews, and is_bounce=false.
func TestRunSessionRollup_PreservesLongSessions(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	site := fmt.Sprintf("test-roll-%d", time.Now().UnixNano())
	session := "sess-" + site
	now := time.Now().UTC()
	early := now.Add(-45 * time.Minute).UnixMilli() // outside the 30-min window
	late := now.Add(-5 * time.Minute).UnixMilli()   // inside the window

	insert := func(eid string, ts int64, path string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname)
			 VALUES ($1, 'default', $2, $3, $4, 'pageview', $5, $6)`,
			eid, site, session, session, ts, path)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	insert("e1-"+site, early, "/landing")
	insert("e2-"+site, late, "/checkout")

	r := NewRollupService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := r.RunSessionRollup(ctx); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	type srow struct {
		FirstTs    int64  `db:"first_ts"`
		Pageviews  int64  `db:"pageviews"`
		EntryURL   string `db:"entry_url"`
		IsBounce   string `db:"is_bounce"`
	}
	rows, err := nucleus.Query[srow](ctx, db.SQL(),
		`SELECT first_ts, pageviews, COALESCE(entry_url,'') AS entry_url, is_bounce
		 FROM sessions WHERE site_id = $1 AND session_id = $2
		 ORDER BY version DESC LIMIT 1`, site, session)
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected a session row")
	}
	got := rows[0]
	if got.FirstTs != early {
		t.Fatalf("first_ts: want %d (earliest event), got %d", early, got.FirstTs)
	}
	if got.Pageviews != 2 {
		t.Fatalf("pageviews: want 2 (full session), got %d", got.Pageviews)
	}
	if got.EntryURL != "/landing" {
		t.Fatalf("entry_url: want /landing, got %q", got.EntryURL)
	}
	// Boolean may serialize as "f"/"false" depending on column type.
	if got.IsBounce == "true" || got.IsBounce == "t" {
		t.Fatalf("is_bounce: want false for 2-event session, got %q", got.IsBounce)
	}
}
