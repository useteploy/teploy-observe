package integrations

import "github.com/useteploy/teploy-observe/internal/query"

// integrations is a ReplacingMergeTree and Delete is a soft delete: a new
// version with enabled='false'. Nucleus does not reliably collapse the
// superseded row, so `WHERE enabled = 'true'` read against the raw table still
// matched the pre-delete version and a deleted integration kept receiving every
// alert. Reads collapse to the highest version per key and filter enabled after
// it; Delete reads through the same collapse so it writes exactly one row.
var integrationCols = []string{
	"name", "int_type", "config", "enabled", "created_at", "version",
}

func integrationsLatest(where string) string {
	return query.LatestRows("integrations", integrationCols, where) + " AS integrations"
}
