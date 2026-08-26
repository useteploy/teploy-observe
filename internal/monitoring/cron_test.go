package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func cronTestDB(t *testing.T) (*nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS cron_monitors (
			cron_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default', site_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '', slug TEXT NOT NULL, schedule TEXT NOT NULL DEFAULT '',
			grace_period TEXT NOT NULL DEFAULT '300', enabled TEXT NOT NULL DEFAULT 'true',
			created_at TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 0
		) WITH (engine = 'replacing_mergetree', version_column = 'version') ORDER BY (tenant_id, site_id, cron_id)`,
		`CREATE TABLE IF NOT EXISTS cron_checkins (
			checkin_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default', cron_id TEXT NOT NULL,
			site_id TEXT NOT NULL, timestamp BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'ok',
			duration_ms TEXT NOT NULL DEFAULT '0'
		) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, cron_id, timestamp)`,
		`ALTER TABLE cron_monitors ADD COLUMN IF NOT EXISTS ping_token TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.SQL().Exec(ctx, ddl); err != nil {
			db.Close()
			cancel()
			t.Fatalf("ensure schema: %v", err)
		}
	}
	return db, func() { db.Close(); cancel() }
}

// TestCheckMissed_DetectsAndRecovers is the regression for the HIGH finding that
// missed-cron detection was dead code: a cron past its grace with no check-in
// must be reported; a token check-in must clear it.
func TestCheckMissed_DetectsAndRecovers(t *testing.T) {
	db, done := cronTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewCronService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	site := fmt.Sprintf("test-cron-%d", time.Now().UnixNano())
	created, err := svc.CreateCron(ctx, CronMonitor{SiteID: site, Name: "nightly", GracePeriod: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.PingToken == "" {
		t.Fatal("expected a ping token at creation")
	}

	// No check-in; wait past the 1s grace.
	time.Sleep(1100 * time.Millisecond)
	missed, err := svc.CheckMissed(ctx)
	if err != nil {
		t.Fatalf("check missed: %v", err)
	}
	if !containsCron(missed, created.CronID) {
		t.Fatalf("expected cron %s to be reported missed", created.CronID)
	}

	// Check in via token; it must no longer be missed.
	if err := svc.RecordCheckinByToken(ctx, created.PingToken, "ok", 0); err != nil {
		t.Fatalf("checkin by token: %v", err)
	}
	missed, err = svc.CheckMissed(ctx)
	if err != nil {
		t.Fatalf("check missed 2: %v", err)
	}
	if containsCron(missed, created.CronID) {
		t.Fatalf("cron %s should not be missed after check-in", created.CronID)
	}
}

// TestRecordCheckinByToken_UnknownToken returns ErrCronNotFound.
func TestRecordCheckinByToken_UnknownToken(t *testing.T) {
	db, done := cronTestDB(t)
	defer done()
	svc := NewCronService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc.RecordCheckinByToken(context.Background(), "nope-not-a-real-token", "ok", 0); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

func containsCron(crons []CronMonitor, id string) bool {
	for _, c := range crons {
		if c.CronID == id {
			return true
		}
	}
	return false
}

// TestCheckMissed_HonoursTheSchedulePeriod is the database-level regression for
// the incident flood.
//
// The monitor below runs hourly and has a one-second grace. It checked in a
// moment ago, so it is not due for another hour and must NOT be reported
// missed. The old detector compared against the grace period alone and reported
// it as soon as that second elapsed — and since a check-in then closed the
// incident and the next second reopened it, it produced one incident per cron
// run indefinitely. The live instance reached 12,398 incident rows from ten
// monitors that way, which is what filled the analytics chart with markers.
//
// With internal/monitoring/monitoring.go's CheckMissed reverted to the
// count-within-grace form this fails with "reported missed 1.1s after checking
// in".
func TestCheckMissed_HonoursTheSchedulePeriod(t *testing.T) {
	db, done := cronTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewCronService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	site := fmt.Sprintf("test-sched-%d", time.Now().UnixNano())
	hourly, err := svc.CreateCron(ctx, CronMonitor{
		SiteID: site, Name: "hourly", Slug: "hourly",
		Schedule: "0 * * * *", GracePeriod: 1,
	})
	if err != nil {
		t.Fatalf("create hourly: %v", err)
	}
	if err := svc.RecordCheckinByToken(ctx, hourly.PingToken, "ok", 0); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	// A monitor with no schedule to read still alerts on grace alone, which is
	// what proves the sleep below is long enough to matter.
	graceOnly, err := svc.CreateCron(ctx, CronMonitor{
		SiteID: site, Name: "gracely", Slug: "gracely", GracePeriod: 1,
	})
	if err != nil {
		t.Fatalf("create grace-only: %v", err)
	}
	if err := svc.RecordCheckinByToken(ctx, graceOnly.PingToken, "ok", 0); err != nil {
		t.Fatalf("checkin grace-only: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)

	missed, err := svc.CheckMissed(ctx)
	if err != nil {
		t.Fatalf("check missed: %v", err)
	}
	if containsCron(missed, hourly.CronID) {
		t.Fatal("an hourly cron was reported missed 1.1s after checking in — the detector is judging it on its grace period alone, which reopens an incident between every pair of runs")
	}
	if !containsCron(missed, graceOnly.CronID) {
		t.Fatal("a monitor with an unreadable schedule must still alert on grace alone")
	}
}
