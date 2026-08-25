package dashboards

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// dashboards and dashboard_panels as 005_features declares them, with the
// BIGINT version 024 converted them to.
const dashboardColumns = `(
	dashboard_id   TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	description    TEXT NOT NULL DEFAULT '',
	created_by     TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

const panelColumns = `(
	panel_id       TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	dashboard_id   TEXT NOT NULL,
	panel_type     TEXT NOT NULL DEFAULT 'metric',
	title          TEXT NOT NULL DEFAULT '',
	query_type     TEXT NOT NULL DEFAULT '',
	query_config   JSONB,
	position_x     TEXT NOT NULL DEFAULT '0',
	position_y     TEXT NOT NULL DEFAULT '0',
	width          TEXT NOT NULL DEFAULT '6',
	height         TEXT NOT NULL DEFAULT '4',
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestDeleteWritesOneRowAndReadsResolveLatest.
//
// Delete was `INSERT INTO dashboards SELECT ... FROM dashboards WHERE
// dashboard_id = $1`, so the physical count doubled every call — and Delete is
// idempotent from the UI's point of view, so a repeated request repeats the
// doubling. Repeating it three times is the shape that separates the two: buggy
// reaches 8 rows, fixed reaches 4.
//
// The reads have to agree throughout: List and Get filter the tombstone
// OUTSIDE the collapse, because filtering the empty name before it would match
// the superseded pre-delete row and resurrect the dashboard.
func TestDeleteWritesOneRowAndReadsResolveLatest(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "dashboards", dashboardColumns,
		"(tenant_id, site_id, dashboard_id)", "version")

	svc := NewDashboardService(db)
	const site = "dashsite"

	d, err := svc.Create(ctx, site, "Ops", "", "tyler")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d dashboards after one Create, want 1", len(list))
	}

	for i := 1; i <= 3; i++ {
		if err := svc.Delete(ctx, d.DashboardID); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if got, want := countRows(ctx, t, db, "dashboards", "dashboard_id", d.DashboardID), int64(i+1); got != want {
			t.Fatalf("after %d Delete call(s) the table holds %d rows, want %d — the write re-inserted one row per existing version", i, got, want)
		}
		list, err := svc.List(ctx, site)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("List returned %d dashboards after Delete, want 0 — it resolved a superseded version", len(list))
		}
		got, err := svc.Get(ctx, d.DashboardID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Fatalf("Get returned a deleted dashboard (%q)", got.Name)
		}
	}
}

// TestDeletePanelWritesOneRowAndListPanelsResolvesLatest covers the panel half:
// DeletePanel had the same doubling shape, and ListPanels resolved the latest
// version through a correlated `version = (SELECT MAX(version) ...)` subquery
// that the argMax collapse replaces.
func TestDeletePanelWritesOneRowAndListPanelsResolvesLatest(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "dashboard_panels", panelColumns,
		"(tenant_id, dashboard_id, panel_id)", "version")

	svc := NewDashboardService(db)
	const dash = "dash-1"

	p, err := svc.AddPanel(ctx, dash, Panel{PanelType: "metric", Title: "Pageviews", QueryType: "pageviews"})
	if err != nil {
		t.Fatalf("add panel: %v", err)
	}

	// An edit must win over the row it supersedes.
	p.DashboardID = dash
	p.Title = "Pageviews (7d)"
	if err := svc.UpdatePanel(ctx, *p); err != nil {
		t.Fatalf("update panel: %v", err)
	}
	panels, err := svc.ListPanels(ctx, dash)
	if err != nil {
		t.Fatalf("list panels: %v", err)
	}
	if len(panels) != 1 {
		t.Fatalf("ListPanels returned %d panels, want 1 — one row per surviving version", len(panels))
	}
	if panels[0].Title != "Pageviews (7d)" {
		t.Fatalf("ListPanels reported title %q after the edit — it resolved a superseded version", panels[0].Title)
	}

	for i := 1; i <= 3; i++ {
		if err := svc.DeletePanel(ctx, p.PanelID); err != nil {
			t.Fatalf("delete panel %d: %v", i, err)
		}
		if got, want := countRows(ctx, t, db, "dashboard_panels", "panel_id", p.PanelID), int64(i+2); got != want {
			t.Fatalf("after %d DeletePanel call(s) the table holds %d rows, want %d — the write re-inserted one row per existing version", i, got, want)
		}
	}
}

func countRows(ctx context.Context, t *testing.T, db *nucleus.Client, table, keyCol, key string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM `+table+` WHERE `+keyCol+` = $1`, key)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
