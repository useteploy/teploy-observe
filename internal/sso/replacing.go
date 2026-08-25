package sso

import "github.com/useteploy/teploy-observe/internal/query"

// sso_configs is a ReplacingMergeTree keyed on (tenant_id, sso_id) — it is
// instance-wide, so the key carries no site_id — and Enable rewrites `enabled`
// as a new version. Nucleus does not reliably collapse the superseded row, so a
// read of the raw table returns an arbitrary version and List could report a
// live SSO provider as disabled (or the reverse). The write was shaped as
// `INSERT INTO sso_configs SELECT ... FROM sso_configs`, which inserts one row
// per version already present and doubles the row count on every call.
//
// The read collapses by argMax and Enable reads through the same collapse, so
// it writes exactly one row.
var ssoCols = []string{
	"provider", "entity_id", "sso_url", "certificate", "attribute_map",
	"enabled", "created_at", "version",
}

// ssoConfigsLatest renders the collapsed derived table, aliased `sso_configs`
// so the surrounding query reads unchanged. where is applied before the
// collapse, so pass only version-stable predicates (the key itself); `enabled`
// is rewritten between versions and must be filtered outside.
func ssoConfigsLatest(where string) string {
	return query.LatestRows("sso_configs", ssoCols, where) + " AS sso_configs"
}
