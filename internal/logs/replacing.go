package logs

import "github.com/useteploy/teploy-observe/internal/query"

// log_pipelines is a ReplacingMergeTree and Delete is a soft delete
// (enabled='false' at a new version). Nucleus does not reliably collapse the
// superseded row, so `WHERE enabled = 'true'` read against the raw table still
// matched the pre-delete version: a deleted pipeline kept processing — and
// kept dropping — every log line for its site. Reads collapse to the highest
// version per key and filter enabled after it; Delete reads through the same
// collapse so it writes exactly one row.
var pipelineCols = []string{
	"name", "priority", "rules", "enabled", "created_at", "version",
}

func pipelinesLatest(where string) string {
	return query.LatestRows("log_pipelines", pipelineCols, where) + " AS log_pipelines"
}
