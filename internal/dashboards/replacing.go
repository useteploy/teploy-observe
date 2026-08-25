package dashboards

import "github.com/useteploy/teploy-observe/internal/query"

// dashboards and dashboard_panels are ReplacingMergeTrees — keyed on
// (tenant_id, site_id, dashboard_id) and (tenant_id, dashboard_id, panel_id)
// respectively — whose rows are rewritten rather than updated. Nucleus does not
// reliably collapse the superseded versions, so every access has to be
// explicit:
//
//   - a READ collapses with argMax over version, otherwise it returns one row
//     per surviving version and an arbitrary one wins.
//   - a WRITE reads through that same collapse before it inserts, so exactly one
//     row is written. Delete and DeletePanel were shaped as
//     `INSERT INTO t SELECT ... FROM t WHERE id = $1`, which inserts one row per
//     row already present — the physical count doubles on every call.
//
// `name` (dashboards) and `title` (panels) are rewritten by the soft-delete, so
// the tombstone filter must run OUTSIDE the collapse; filtering inside would
// match the superseded row and resurrect a deleted dashboard.
var dashboardCols = []string{
	"name", "description", "created_by", "created_at", "version",
}

var panelCols = []string{
	"panel_type", "title", "query_type", "query_config",
	"position_x", "position_y", "width", "height", "version",
}

func dashboardsLatest(where string) string {
	return query.LatestRows("dashboards", dashboardCols, where) + " AS dashboards"
}

func panelsLatest(where string) string {
	return query.LatestRows("dashboard_panels", panelCols, where) + " AS dashboard_panels"
}
