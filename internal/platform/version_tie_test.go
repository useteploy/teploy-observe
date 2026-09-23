package platform

import (
	"context"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// alert_rules and webhooks as 004_platform + 049 declare them.
const alertRuleColumns = `(
	rule_id        TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	metric         TEXT NOT NULL,
	operator       TEXT NOT NULL DEFAULT 'gt',
	threshold      TEXT NOT NULL DEFAULT '0',
	window_minutes TEXT NOT NULL DEFAULT '5',
	check_interval TEXT NOT NULL DEFAULT '60',
	cooldown       TEXT NOT NULL DEFAULT '300',
	min_samples    TEXT NOT NULL DEFAULT '1',
	severity       TEXT NOT NULL DEFAULT 'warning',
	enabled        TEXT NOT NULL DEFAULT 'true',
	created_by     TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

const webhookColumns = `(
	webhook_id     TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	webhook_type   TEXT NOT NULL DEFAULT 'http',
	url            TEXT NOT NULL DEFAULT '',
	secret         TEXT NOT NULL DEFAULT '',
	enabled        TEXT NOT NULL DEFAULT 'true',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestSameMillisecondCreateDeleteTombstoneWins is the version-tie regression
// for the platform soft deletes (the 70f6eff defect): CreateRule/Create and
// DeleteRule/Delete both stamp version from the clock, so an immediate
// create+delete (the UI's "add then remove" path, no sleeps) ties and argMax
// can resolve the live row — a deleted alert rule keeps evaluating, a deleted
// webhook keeps receiving payloads. The fix stamps the tombstone
// GREATEST(now, version + 1) so it always strictly exceeds the row it kills.
func TestSameMillisecondCreateDeleteTombstoneWins(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "alert_rules", alertRuleColumns,
		"(tenant_id, site_id, rule_id)", "version")
	nucleustest.AsPlainMergeTree(t, db, "webhooks", webhookColumns,
		"(tenant_id, site_id, webhook_id)", "version")

	alerts := NewAlertService(db, nil, nil, nil, nil)
	hooks := NewWebhookService(db, nil)
	const site = "platform-tie-site"

	for i := 0; i < 5; i++ {
		rule, err := alerts.CreateRule(ctx, AlertRule{SiteID: site, Name: "tie", Metric: "error_count"})
		if err != nil {
			t.Fatalf("iter %d create rule: %v", i, err)
		}
		if err := alerts.DeleteRule(ctx, rule.RuleID); err != nil {
			t.Fatalf("iter %d delete rule: %v", i, err)
		}
		rules, err := alerts.ListRules(ctx, site)
		if err != nil {
			t.Fatalf("iter %d list rules: %v", i, err)
		}
		for _, r := range rules {
			if r.RuleID == rule.RuleID {
				t.Fatalf("iter %d: a deleted rule still lists as enabled — the tombstone tied or lost the version collapse", i)
			}
		}

		hook, err := hooks.Create(ctx, site, "tie", "http", "https://example.com/hook")
		if err != nil {
			t.Fatalf("iter %d create webhook: %v", i, err)
		}
		if err := hooks.Delete(ctx, hook.WebhookID); err != nil {
			t.Fatalf("iter %d delete webhook: %v", i, err)
		}
		hookList, err := hooks.List(ctx, site)
		if err != nil {
			t.Fatalf("iter %d list webhooks: %v", i, err)
		}
		for _, h := range hookList {
			if h.WebhookID == hook.WebhookID {
				t.Fatalf("iter %d: a deleted webhook still lists as enabled — the tombstone tied or lost the version collapse", i)
			}
		}
	}
}
