package reports

import (
	"context"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestOBS45ReportRefusesUnavailableMeasurements(t *testing.T) {
	db, err := nucleus.Connect(context.Background(), nucleustest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	svc := NewReportService(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched := ReportSchedule{SiteID: "absent-report-" + time.Now().Format("150405.000000"), Frequency: "daily"}
	data, err := svc.gatherData(context.Background(), sched)
	if err != nil {
		t.Fatal(err)
	}
	if data.Pageviews != "0" || data.Visitors != "0" || data.Errors != "0" {
		t.Fatalf("empty success: %+v", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.gatherData(ctx, sched); err == nil {
		t.Fatal("canceled measurement returned a successful report")
	}
}
