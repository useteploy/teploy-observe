package views

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// saved_views as 006_wave1 declares it, with the BIGINT version 024 converted
// it to.
const viewColumns = `(
	view_id        TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	view_config    JSONB,
	created_by     TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestDeleteRemovesTheViewAndItsRows.
//
// Delete used to append a blank tombstone via `INSERT INTO saved_views SELECT
// ... FROM saved_views WHERE view_id = $1`. That both doubled the row count on
// every call and left a row List still returned — a nameless, config-less entry
// in the saved-views list that no user could remove. It now hard-deletes.
//
// Without the fix this fails on the very first assertion: List still returns
// the deleted view.
func TestDeleteRemovesTheViewAndItsRows(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "saved_views", viewColumns,
		"(tenant_id, site_id, view_id)", "version")

	svc := NewViewService(db)
	const site = "viewsite"

	v, err := svc.Create(ctx, site, "Slow requests", `{"type":"trace_funnel","ops":["a","b"]}`, "tyler")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second view must survive the first one's delete.
	keep, err := svc.Create(ctx, site, "Errors", `{"type":"trace_funnel","ops":["c","d"]}`, "tyler")
	if err != nil {
		t.Fatalf("create keep: %v", err)
	}

	if err := svc.Delete(ctx, v.ViewID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ViewID != keep.ViewID {
		t.Fatalf("List returned %d views after deleting one of two, want just %q — a delete that leaves a readable row", len(list), keep.ViewID)
	}
	if n := viewRows(ctx, t, db, v.ViewID); n != 0 {
		t.Fatalf("the deleted view still has %d row(s) on disk, want 0", n)
	}
	if n := viewRows(ctx, t, db, keep.ViewID); n != 1 {
		t.Fatalf("the surviving view has %d row(s), want 1", n)
	}
}

func viewRows(ctx context.Context, t *testing.T, db *nucleus.Client, viewID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM saved_views WHERE view_id = $1`, viewID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
