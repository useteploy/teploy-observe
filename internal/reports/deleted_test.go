package reports

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// report_schedules as 006_wave1 declares it.
const scheduleColumns = `(
	schedule_id    TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	frequency      TEXT NOT NULL DEFAULT 'weekly',
	recipients     TEXT NOT NULL DEFAULT '',
	enabled        TEXT NOT NULL DEFAULT 'true',
	last_sent      TEXT NOT NULL DEFAULT '0',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestDeletedScheduleStopsBeingRead covers report_schedules, where the same
// soft-delete shape produced two distinct symptoms: a deleted schedule still
// matched `enabled = 'true'` and kept emailing, and a live schedule was read
// once per surviving version, so one send cycle mailed every recipient several
// times. RunScheduled needs SMTP, so this asserts on the query the scheduler
// runs rather than on delivery.
func TestDeletedScheduleStopsBeingRead(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "report_schedules", scheduleColumns,
		"(tenant_id, site_id, schedule_id)", "version")

	svc := NewReportService(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx := context.Background()
	const site = "reportsite"

	live, err := svc.Create(ctx, site, "weekly", "weekly", "a@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gone, err := svc.Create(ctx, site, "old", "weekly", "b@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, gone.ScheduleID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// A send bumps last_sent as a new version; two of them leave three
	// versions of the live schedule behind. last_sent doubles as the version,
	// so it must move forward from the creation timestamp.
	base := time.Now().UTC().UnixMilli() + 1000
	for i := 0; i < 2; i++ {
		if err := svc.markSent(ctx, live.ScheduleID, base+int64(i)); err != nil {
			t.Fatalf("mark sent: %v", err)
		}
	}
	wantLastSent := strconv.FormatInt(base+1, 10)

	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var liveSeen int
	for _, s := range list {
		if s.ScheduleID == gone.ScheduleID {
			t.Fatalf("deleted schedule still listed — enabled was filtered before the collapse")
		}
		if s.ScheduleID == live.ScheduleID {
			liveSeen++
			if s.LastSent != wantLastSent {
				t.Fatalf("last_sent is %q, want %s — an older version won", s.LastSent, wantLastSent)
			}
		}
	}
	if liveSeen != 1 {
		t.Fatalf("the live schedule appears %d times, want 1 — every send would mail that many times", liveSeen)
	}
}
