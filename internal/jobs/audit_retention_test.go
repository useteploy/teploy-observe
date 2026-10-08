package jobs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

func TestRetentionCutoffOverflowIsNonDestructive(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, unit := range []TimeUnit{UnitMillis, UnitNanos} {
		for _, days := range []int{-1, 0, 100001, 109500, math.MaxInt} {
			if got := unit.CutoffAt(now, days); got != math.MinInt64 {
				t.Fatalf("unit=%d days=%d unsafe cutoff=%d", unit, days, got)
			}
		}
		if got := unit.CutoffAt(now, 100000); got >= unit.CutoffAt(now, 1) {
			t.Fatal("maximum retention points into future")
		}
	}
	if got := UnitNanos.CutoffAt(time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC), 100000); got != math.MinInt64 {
		t.Fatalf("UnixNano overflow accepted: %d", got)
	}
}

func TestRetentionIdentityColumnsExist(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	policies := append(DefaultPolicies(30, 90, 30), DefaultTelemetryPolicies(30, 30, 90)...)
	policies = append(policies, DefaultLedgerPolicies(14, 14, 7)...)
	policies = append(policies, RetentionPolicy{Table: "replay_events", Column: "timestamp", Days: 14})
	for _, p := range policies {
		keys := append(append([]string(nil), retentionKeys[p.Table]...), p.Column)
		if len(keys) == 0 || len(keys) > 8 {
			t.Fatalf("unbounded identity %s", p.Table)
		}
		// Unknown read columns silently become NULL in Nucleus. Probe writes.
		for _, key := range keys {
			if _, err := db.SQL().Exec(ctx, fmt.Sprintf("UPDATE %s SET %s = %s WHERE 1=0", p.Table, key, key)); err != nil {
				t.Fatalf("%s.%s: %v", p.Table, key, err)
			}
		}
	}
}

func TestRetentionLargeTimestampTieStaysBounded(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	site := fmt.Sprintf("ret-tie-%d", time.Now().UnixNano())
	t.Cleanup(func() { db.SQL().Exec(context.Background(), "DELETE FROM events WHERE site_id = $1", site) })
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	for start := 0; start < 6001; start += 250 {
		end := start + 250
		if end > 6001 {
			end = 6001
		}
		values := make([]string, end-start)
		args := make([]any, 0, 3*(end-start))
		for i := start; i < end; i++ {
			n := len(args)
			values[i-start] = fmt.Sprintf("($%d,$%d,$%d)", n+1, n+2, n+3)
			args = append(args, fmt.Sprintf("%s-%05d", site, i), site, old)
		}
		if _, err := db.SQL().Exec(ctx, "INSERT INTO events (event_id,site_id,timestamp) VALUES "+strings.Join(values, ","), args...); err != nil {
			t.Fatal(err)
		}
	}
	total := int64(0)
	for total < 6001 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n, err := deleteRetentionChunk(ctx, tx.SQL(), "events", "site_id = $1 AND timestamp = $2", []any{site, old}, append(retentionKeys["events"], "timestamp"))
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if n <= 0 || n > retentionChunkSize {
			tx.Rollback(ctx)
			t.Fatalf("timestamp tie expanded DELETE to %d", n)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 6001 {
		t.Fatalf("deleted %d", total)
	}
}

func TestReplayPayloadRetentionUsesOwnerAndRemovesOrphans(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	site := fmt.Sprintf("ret-replay-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM replay_events WHERE event_id LIKE $1", site+"%")
		db.SQL().Exec(context.Background(), "DELETE FROM replay_sessions WHERE site_id = $1", site)
	})
	now := time.Now().UTC()
	old := now.Add(-20 * 24 * time.Hour).UnixMilli()
	for _, r := range []struct {
		id string
		ts int64
	}{{"old", old}, {"new", now.UnixMilli()}} {
		if _, err := db.SQL().Exec(ctx, "INSERT INTO replay_sessions (replay_id,site_id,start_time) VALUES ($1,$2,$3)", site+r.id, site, r.ts); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ id, ownerSite string }{{"old", site}, {"new", site}, {"old", ""}, {"new", ""}, {"orphan", site}} {
		eid := site + r.id + "-" + r.ownerSite
		if _, err := db.SQL().Exec(ctx, "INSERT INTO replay_events (event_id,tenant_id,site_id,replay_id,timestamp,event_type,data) VALUES ($1,'default',$2,$3,123,'click','{}')", eid, r.ownerSite, site+r.id); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewRetentionServiceWithPolicies(db, slog.New(slog.NewTextHandler(io.Discard, nil)), []RetentionPolicy{{Table: "replay_sessions", Column: "start_time", Days: 14}})
	if err := svc.RunCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := nucleus.Query[countRow](ctx, db.SQL(), "SELECT COUNT(*) AS n FROM replay_events WHERE event_id LIKE $1", site+"%")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].N != 2 {
		t.Fatalf("old, orphan or legacy recording survived: %+v", rows)
	}
	owners, err := nucleus.Query[countRow](ctx, db.SQL(), "SELECT COUNT(*) AS n FROM replay_sessions WHERE site_id = $1", site)
	if err != nil {
		t.Fatal(err)
	}
	if owners[0].N != 1 {
		t.Fatalf("wrong parent retention: %+v", owners)
	}
}

func TestSessionSummarySurvivesRawRetention(t *testing.T) {
	for _, days := range []int{1, 30} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			ctx, db, done := connect(t)
			defer done()
			now := time.Date(2026, 10, 31, 23, 55, 0, 0, time.UTC)
			site := fmt.Sprintf("ret-summary-%d-%d", days, time.Now().UnixNano())
			sid := site + "-monthly"
			t.Cleanup(func() {
				db.SQL().Exec(context.Background(), "DELETE FROM events WHERE site_id = $1", site)
				db.SQL().Exec(context.Background(), "DELETE FROM sessions WHERE site_id = $1", site)
			})
			early := now.Add(-time.Duration(days)*24*time.Hour - time.Minute).UnixMilli()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			// First rollup runs while both original events are raw and recently touched.
			roll := NewRollupService(db, logger)
			roll.now = func() time.Time { return time.UnixMilli(early).Add(5 * time.Minute) }
			insert := func(id string, ts int64, path, release string) {
				if _, err := db.SQL().Exec(ctx, "INSERT INTO events (event_id,site_id,session_id,timestamp,pathname,release_tag) VALUES ($1,$2,$3,$4,$5,$6)", id, site, sid, ts, path, release); err != nil {
					t.Fatal(err)
				}
			}
			insert(site+"-1", early, "/landing", "release-first")
			insert(site+"-2", early+1, "/second", "release-first")
			if err := roll.RunSessionRollup(ctx); err != nil {
				t.Fatal(err)
			}
			retention := NewRetentionServiceWithPolicies(db, logger, []RetentionPolicy{{Table: "events", Column: "timestamp", Days: days}})
			retention.now = func() time.Time { return now }
			if err := retention.RunCleanup(ctx); err != nil {
				t.Fatal(err)
			}
			insert(site+"-3", now.Add(-time.Minute).UnixMilli(), "/return", "release-later")
			roll.now = func() time.Time { return now }
			if err := roll.RunSessionRollup(ctx); err != nil {
				t.Fatal(err)
			}
			if err := roll.RunSessionRollup(ctx); err != nil {
				t.Fatal(err)
			}
			type summary struct {
				First   int64  `db:"first_ts"`
				Last    int64  `db:"last_ts"`
				Count   int64  `db:"pageviews"`
				Entry   string `db:"entry_url"`
				Exit    string `db:"exit_url"`
				Release string `db:"release_tag"`
				Bounce  string `db:"is_bounce"`
			}
			rows, err := nucleus.Query[summary](ctx, db.SQL(), "SELECT first_ts,last_ts,pageviews,entry_url,exit_url,release_tag,is_bounce FROM sessions WHERE site_id = $1", site)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].First != early || rows[0].Count != 3 || rows[0].Entry != "/landing" || rows[0].Exit != "/return" || rows[0].Release != "release-first" || rows[0].Bounce == "true" {
				t.Fatalf("retained history lost/doubled: %+v", rows)
			}
		})
	}
}

func TestRetentionReplacingCopiesNeverCommitOversizedDelete(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	site := fmt.Sprintf("ret-physical-%d", time.Now().UnixNano())
	t.Cleanup(func() { db.SQL().Exec(context.Background(), "DELETE FROM stats_hourly WHERE site_id = $1", site) })
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	for start := 0; start < 5001; start += 250 {
		end := start + 250
		if end > 5001 {
			end = 5001
		}
		values := make([]string, end-start)
		args := make([]any, 0, 2*(end-start))
		for i := start; i < end; i++ {
			n := len(args)
			values[i-start] = fmt.Sprintf("($%d,$%d,1)", n+1, n+2)
			args = append(args, site, old)
		}
		if _, err := db.SQL().Exec(ctx, "INSERT INTO stats_hourly (site_id,ts_bucket,version) VALUES "+strings.Join(values, ","), args...); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	n, err := deleteRetentionChunk(ctx, tx.SQL(), "stats_hourly", "site_id = $1", []any{site}, append(append([]string(nil), retentionKeys["stats_hourly"]...), "ts_bucket"))
	if err == nil && n > retentionChunkSize {
		t.Fatalf("oversized physical delete accepted: %d", n)
	}
	// With physical SELECT, refused before DELETE; with collapsed SELECT,
	// refused on affected-row check and rolled back, preserving logical data.
	if err != nil && !strings.Contains(err.Error(), "bounded") {
		t.Fatal(err)
	}
}
