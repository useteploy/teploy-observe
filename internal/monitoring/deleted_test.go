package monitoring

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// The two monitor tables as their migrations declare them: 005_features for the
// bodies, 026_cron_ping_token for cron_monitors.ping_token.
const uptimeColumns = `(
	monitor_id      TEXT NOT NULL,
	tenant_id       TEXT NOT NULL DEFAULT 'default',
	site_id         TEXT NOT NULL,
	name            TEXT NOT NULL DEFAULT '',
	url             TEXT NOT NULL DEFAULT '',
	method          TEXT NOT NULL DEFAULT 'GET',
	interval_secs   TEXT NOT NULL DEFAULT '60',
	expected_status TEXT NOT NULL DEFAULT '200',
	enabled         TEXT NOT NULL DEFAULT 'true',
	created_at      TEXT NOT NULL,
	version         BIGINT NOT NULL DEFAULT 0
)`

const cronColumns = `(
	cron_id        TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	slug           TEXT NOT NULL,
	schedule       TEXT NOT NULL DEFAULT '',
	grace_period   TEXT NOT NULL DEFAULT '300',
	enabled        TEXT NOT NULL DEFAULT 'true',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0,
	ping_token     TEXT NOT NULL DEFAULT ''
)`

func monitorTestDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	return db
}

// TestDeletedUptimeMonitorStopsBeingListed covers the soft-delete cluster: the
// delete writes a new version with enabled='false', and with the superseded row
// still readable, `WHERE enabled = 'true'` matched it — so a deleted monitor
// stayed in the list and RunChecks kept issuing HTTP requests against a target
// the user had removed. Without the fix, ListMonitors still returns it.
func TestDeletedUptimeMonitorStopsBeingListed(t *testing.T) {
	db := monitorTestDB(t)
	nucleustest.AsPlainMergeTree(t, db, "uptime_monitors", uptimeColumns,
		"(tenant_id, site_id, monitor_id)", "version")
	svc := NewUptimeService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx := context.Background()

	const site = "delsite"
	m, err := svc.CreateMonitor(ctx, Monitor{SiteID: site, Name: "n", URL: "https://example.com"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteMonitor(ctx, site, m.MonitorID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err := svc.ListMonitors(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, got := range list {
		if got.MonitorID == m.MonitorID {
			t.Fatalf("deleted monitor still listed — enabled was filtered before the collapse")
		}
	}
}

// TestDeletedCronPingTokenIsRejected is the security-relevant one: the ping
// token is the credential that authorizes a heartbeat, and deleting the monitor
// is the only way to revoke it. The lookup filtered enabled='true' against the
// raw table, so the pre-delete version kept authenticating the token forever.
func TestDeletedCronPingTokenIsRejected(t *testing.T) {
	db := monitorTestDB(t)
	nucleustest.AsPlainMergeTree(t, db, "cron_monitors", cronColumns,
		"(tenant_id, site_id, cron_id)", "version")
	svc := NewCronService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx := context.Background()

	const site = "cronsite"
	c, err := svc.CreateCron(ctx, CronMonitor{SiteID: site, Name: "nightly", Slug: "nightly"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.RecordCheckinByToken(ctx, c.PingToken, "ok", 1); err != nil {
		t.Fatalf("check-in before delete should succeed: %v", err)
	}
	if err := svc.DeleteCron(ctx, site, c.CronID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.RecordCheckinByToken(ctx, c.PingToken, "ok", 1); !errors.Is(err, ErrCronNotFound) {
		t.Fatalf("a deleted monitor's ping token still authenticates: err=%v, want ErrCronNotFound", err)
	}
	if err := svc.RecordCheckin(ctx, site, c.Slug, "ok", 1); !errors.Is(err, ErrCronNotFound) {
		t.Fatalf("a deleted monitor still accepts slug check-ins: err=%v, want ErrCronNotFound", err)
	}
	crons, err := svc.ListCrons(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, got := range crons {
		if got.CronID == c.CronID {
			t.Fatalf("deleted cron monitor still listed")
		}
	}
	missed, err := svc.CheckMissed(ctx)
	if err != nil {
		t.Fatalf("check missed: %v", err)
	}
	for _, got := range missed {
		if got.CronID == c.CronID {
			t.Fatalf("deleted cron monitor still alerts on missed runs")
		}
	}
}
