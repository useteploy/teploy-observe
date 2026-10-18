package platform

// O10 tails oracle: per-severity webhook routing and cron-heartbeat
// notifications through the durable outbox - at the real engine,
// fail-not-skip under OBSERVE_REQUIRE_NUCLEUS.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestO10SeverityRouting: a webhook with a severities filter receives only
// notifications at those severities; an unfiltered webhook keeps receiving
// everything. Both directions pinned through the real evaluation engine.
func TestO10SeverityRouting(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	// Two sinks: one pages only on critical, one receives everything.
	criticalOnly, err := e.hooks.Create(context.Background(), e.site, "crit-sink", "http", e.server.URL, "critical", "")
	if err != nil {
		t.Fatalf("create crit webhook: %v", err)
	}
	allSink, err := e.hooks.Create(context.Background(), e.site, "all-sink", "http", e.server.URL, "", "")
	if err != nil {
		t.Fatalf("create all webhook: %v", err)
	}

	// A WARNING rule fires: only the unfiltered sink is notified.
	warnRule, err := e.alerts.CreateRule(context.Background(), AlertRule{
		SiteID: e.site, Name: "o10 warn rule", Metric: "error_count", Operator: "gt",
		Threshold: 0, WindowMinutes: 5, Cooldown: 0, MinSamples: 1,
		Severity: "warning", CreatedBy: "o10",
	})
	if err != nil {
		t.Fatalf("create warning rule: %v", err)
	}
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)
	e.check(t)
	e.drain(t)

	intents := e.dbIntentsByWebhook(t, warnRule.RuleID)
	if intents[criticalOnly.WebhookID] != 0 {
		t.Fatalf("critical-only webhook received %d warning intents, want 0", intents[criticalOnly.WebhookID])
	}
	if intents[allSink.WebhookID] == 0 {
		t.Fatalf("unfiltered webhook received no warning intents")
	}

	// A CRITICAL rule fires: both sinks are notified.
	critRule, err := e.alerts.CreateRule(context.Background(), AlertRule{
		SiteID: e.site, Name: "o10 crit rule", Metric: "error_count", Operator: "gt",
		Threshold: 0, WindowMinutes: 5, Cooldown: 0, MinSamples: 1,
		Severity: "critical", CreatedBy: "o10",
	})
	if err != nil {
		t.Fatalf("create critical rule: %v", err)
	}
	e.seedErrors(t, 5, time.Minute)
	e.check(t)
	e.drain(t)

	intents = e.dbIntentsByWebhook(t, critRule.RuleID)
	if intents[criticalOnly.WebhookID] == 0 {
		t.Fatalf("critical-only webhook received no critical intents")
	}
	if intents[allSink.WebhookID] == 0 {
		t.Fatalf("unfiltered webhook received no critical intents")
	}
}

// TestO10CronNotificationsThroughOutbox: the missed-cron path enqueues one
// durable intent per severity-matched webhook atomically with the incident, the drain
// delivers it with the stable delivery id, and the payload receivers parse
// is the same NotificationPayload shape alerts use.
func TestO10CronNotificationsThroughOutbox(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	cronRef := CronMonitorRef{CronID: "cron-xyz", SiteID: e.site, Name: "nightly backup", Slug: "nightly-backup"}
	inc, created, err := e.alerts.EnsureCronMissed(context.Background(), cronRef, 300)
	if err != nil || !created {
		t.Fatalf("cron transition: %+v %v %v", inc, created, err)
	}
	incidentID := inc.IncidentID
	if _, created, err := e.alerts.EnsureCronMissed(context.Background(), cronRef, 300); err != nil || created {
		t.Fatalf("repeat missed: %v %v", created, err)
	}
	if rows := e.intents(t, "cron:"+cronRef.CronID); len(rows) != 1 {
		t.Fatalf("opening intents: %+v", rows)
	}

	// Before the drain the intent is durable: a REBOUND notifier (the
	// restarted process) reads and delivers it.
	e.rebindNotifyOnly()
	e.drain(t)

	if recv.count() != 1 {
		t.Fatalf("POSTs = %d, want 1", recv.count())
	}
	if recv.unique() != 1 {
		t.Fatalf("unique delivery ids = %d, want 1", recv.unique())
	}
	var p NotificationPayload
	if err := json.Unmarshal([]byte(recv.body(0)), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.Kind != NotifyCronMissed || p.RuleName != "nightly backup" || p.RuleID != "cron:"+cronRef.CronID {
		t.Fatalf("payload = %+v, want the cron missed shape", p)
	}
	if p.Severity != "warning" || p.Message == "" {
		t.Fatalf("payload carries no severity/message: %+v", p)
	}

	// The production recovery operation owns the close and correlated intent.
	if count, err := e.alerts.RecoverCron(context.Background(), cronRef, 300); err != nil || count != 1 {
		t.Fatalf("recover: %d %v", count, err)
	}
	if count, err := e.alerts.RecoverCron(context.Background(), cronRef, 300); err != nil || count != 0 {
		t.Fatalf("repeat recovery: %d %v", count, err)
	}

	e.drain(t)
	if recv.count() != 2 {
		t.Fatalf("POSTs after recovery = %d, want 2", recv.count())
	}
	var p2 NotificationPayload
	if err := json.Unmarshal([]byte(recv.body(1)), &p2); err != nil {
		t.Fatalf("decode recovery payload: %v", err)
	}
	if p.IncidentID != incidentID || p2.IncidentID != incidentID {
		t.Fatalf("incident correlation lost: %+v %+v", p, p2)
	}
	if p2.Kind != NotifyCronRecovered {
		t.Fatalf("recovery payload kind = %s", p2.Kind)
	}
}

// TestNormalizeSeverities: the create-time validation table.
func TestNormalizeSeverities(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"critical", "critical", true},
		{"Critical, WARNING", "critical,warning", true},
		{"warning,warning", "warning", true},
		{" info , critical ,", "info,critical", true},
		{"fatal", "", false},
		{"warning,fatal", "", false},
	}
	for _, c := range cases {
		got, err := NormalizeSeverities(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Fatalf("NormalizeSeverities(%q) = (%q, %v), want (%q, ok=%v)", c.in, got, err, c.want, c.ok)
		}
	}
	if !MatchesSeverity("", "anything") {
		t.Fatalf("empty filter must match every severity")
	}
	if MatchesSeverity("critical,error", "warning") {
		t.Fatalf("filtered webhook matched an unlisted severity")
	}
	if !MatchesSeverity("critical,error", "error") {
		t.Fatalf("filtered webhook did not match a listed severity")
	}
}

// dbIntentsByWebhook counts the collapsed intents a rule enqueued, keyed by
// webhook id (what the routing decision durably froze).
func (e *o10Engine) dbIntentsByWebhook(t *testing.T, ruleID string) map[string]int {
	t.Helper()
	rows := e.intents(t, ruleID)
	out := map[string]int{}
	for _, r := range rows {
		out[r.WebhookID]++
	}
	return out
}

// TestO10KindsFilterGovernsCronPaths (SF2, review round 2): the documented
// MatchesKind contract says a NON-EMPTY kinds filter is exact and governs
// every kind. The cron paths ignored it: a webhook that opted into exactly
// issue_new,issue_regression was still receiving cron_missed. The
// transition must route only webhooks whose filter lists cron_missed (an
// empty filter keeps the pre-066 kinds, including cron_missed).
func TestO10KindsFilterGovernsCronPaths(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	e := newO10Engine(t, db, &o10Receiver{}, clk)

	issueOnly, err := e.hooks.Create(context.Background(), e.site, "issue-only-sink", "http", e.server.URL, "", "issue_new,issue_regression")
	if err != nil {
		t.Fatalf("create issue-only webhook: %v", err)
	}
	cronSink, err := e.hooks.Create(context.Background(), e.site, "cron-sink", "http", e.server.URL, "", "cron_missed,cron_recovered")
	if err != nil {
		t.Fatalf("create cron webhook: %v", err)
	}

	cronRef := CronMonitorRef{CronID: "cron-kinds", SiteID: e.site, Name: "nightly backup", Slug: "nightly-backup"}
	if _, _, err := e.alerts.EnsureCronMissed(context.Background(), cronRef, 300); err != nil {
		t.Fatalf("cron transition: %v", err)
	}
	intents := e.dbIntentsByWebhook(t, "cron:"+cronRef.CronID)
	if intents[issueOnly.WebhookID] != 0 {
		t.Fatalf("webhook with kinds=issue_new,issue_regression received %d cron_missed intents, want 0 (a non-empty filter is exact)", intents[issueOnly.WebhookID])
	}
	if intents[cronSink.WebhookID] != 1 {
		t.Fatalf("webhook with kinds=cron_missed,cron_recovered received %d cron_missed intents, want 1", intents[cronSink.WebhookID])
	}

	// Recovery routes through the same filter.
	if _, err := e.alerts.RecoverCron(context.Background(), cronRef, 300); err != nil {
		t.Fatalf("recover: %v", err)
	}
	intents = e.dbIntentsByWebhook(t, "cron:"+cronRef.CronID)
	if intents[issueOnly.WebhookID] != 0 {
		t.Fatalf("webhook with kinds=issue_new,issue_regression received %d cron intents after recovery, want 0", intents[issueOnly.WebhookID])
	}
	if intents[cronSink.WebhookID] != 2 {
		t.Fatalf("cron-listing webhook received %d cron intents after recovery, want 2 (missed + recovered)", intents[cronSink.WebhookID])
	}
}

// TestO10KindsFilterGovernsIncidentPaths: the same contract on the alert
// engine's incident_opened / incident_recovered edges.
func TestO10KindsFilterGovernsIncidentPaths(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	e := newO10Engine(t, db, &o10Receiver{}, clk)

	issueOnly, err := e.hooks.Create(context.Background(), e.site, "issue-only-sink", "http", e.server.URL, "", "issue_new")
	if err != nil {
		t.Fatalf("create issue-only webhook: %v", err)
	}
	incidentSink, err := e.hooks.Create(context.Background(), e.site, "incident-sink", "http", e.server.URL, "", "incident_opened,incident_recovered")
	if err != nil {
		t.Fatalf("create incident webhook: %v", err)
	}

	rule := e.rule(t, "error_count", "gt", 0, 5, 0, 1)
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)
	e.check(t)

	intents := e.dbIntentsByWebhook(t, rule.RuleID)
	if intents[issueOnly.WebhookID] != 0 {
		t.Fatalf("webhook with kinds=issue_new received %d incident_opened intents, want 0", intents[issueOnly.WebhookID])
	}
	if intents[incidentSink.WebhookID] != 1 {
		t.Fatalf("webhook listing incident_opened received %d intents, want 1", intents[incidentSink.WebhookID])
	}
}

// rebindNotifyOnly swaps just the notifier (the restart between enqueue and
// drain - the intent must survive it in the table).
func (e *o10Engine) rebindNotifyOnly() {
	_, _, notify, _ := o10Bind(e.db, e.clock)
	e.notify = notify
}
