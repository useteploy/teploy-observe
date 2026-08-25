package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// TestDeleteRemovesTheExportAndItsRunHistory.
//
// scheduled_exports is a plain mergetree: no version column, so recordRun
// appends one row per run and List keeps the newest per export_id. Delete used
// to append an enabled='false' row via `INSERT INTO scheduled_exports SELECT
// ... FROM scheduled_exports WHERE export_id = $1` with no LIMIT, so it wrote
// one row per row already present — an export with run history doubled on every
// delete, and the history stayed on disk forever with nothing to read it.
//
// Without the fix this fails on the row count: the soft delete leaves rows
// behind rather than removing them.
func TestDeleteRemovesTheExportAndItsRunHistory(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	svc := NewExportService(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	e, err := svc.Create(ctx, CreateInput{
		Name: "nightly", SQL: "SELECT 1", Cron: "@daily",
		Destination: S3Destination{Region: "us-east-1", Bucket: "b"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_exports WHERE export_id = $1`, e.ExportID)
	})

	// recordRun's `ORDER BY updated_at DESC LIMIT 1` is what keeps an
	// INSERT...SELECT-from-the-same-table to one row per call. Three runs must
	// leave four rows, not eight.
	for i := 0; i < 3; i++ {
		if err := svc.recordRun(ctx, e.ExportID, time.Now(), "ok", "", int64(i)); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}
	if got := exportRows(ctx, t, db, e.ExportID); got != 4 {
		t.Fatalf("three recorded runs left %d rows, want 4 — the LIMIT 1 that bounds recordRun is gone", got)
	}

	if err := svc.Delete(ctx, e.ExportID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := exportRows(ctx, t, db, e.ExportID); got != 0 {
		t.Fatalf("the deleted export still has %d row(s) on disk, want 0", got)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, l := range list {
		if l.ExportID == e.ExportID {
			t.Fatal("List returned a deleted export")
		}
	}
}

func exportRows(ctx context.Context, t *testing.T, db *nucleus.Client, exportID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM scheduled_exports WHERE export_id = $1`, exportID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
