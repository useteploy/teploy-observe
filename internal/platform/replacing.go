package platform

import "github.com/useteploy/teploy-observe/internal/query"

// alert_rules and webhooks are ReplacingMergeTree tables, and Delete on both is
// a soft delete: it writes a new version with enabled='false'. Nucleus does not
// reliably collapse the superseded row, so `WHERE enabled = 'true'` read against
// the raw table still matches the pre-delete version — a deleted alert rule kept
// evaluating and a deleted webhook kept receiving payloads, forever.
//
// Both halves have to go through the collapse: reads select the highest version
// per key and filter enabled AFTER it, and the soft-delete itself reads through
// the collapse so it writes exactly one row rather than one per existing version.

var alertRuleCols = []string{
	"name", "metric", "operator", "threshold", "window_minutes",
	"check_interval", "cooldown", "enabled", "created_by", "created_at", "version",
}

func alertRulesLatest(where string) string {
	return query.LatestRows("alert_rules", alertRuleCols, where) + " AS alert_rules"
}

var webhookCols = []string{
	"name", "webhook_type", "url", "secret", "enabled", "created_at", "version",
}

func webhooksLatest(where string) string {
	return query.LatestRows("webhooks", webhookCols, where) + " AS webhooks"
}
