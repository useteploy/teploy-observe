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
	criticalOnly, err := e.hooks.Create(context.Background(), e.site, "crit-sink", "http", e.server.URL, "critical")
	if err != nil {
		t.Fatalf("create crit webhook: %v", err)
	}
	allSink, err := e.hooks.Create(context.Background(), e.site, "all-sink", "http", e.server.URL, "")
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
// durable intent per severity-matched webhook (created-gated), the drain
// delivers it with the stable delivery id, and the payload receivers parse
// is the same NotificationPayload shape alerts use.
func TestO10CronNotificationsThroughOutbox(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	cronRef := CronMonitorRef{CronID: "cron-xyz", SiteID: e.site, Name: "nightly backup", Slug: "nightly-backup"}
	incidentID := "inc-cron-1"

	// The main.go wiring: enqueue on the missed transition (created-gated).
	id, err := e.notify.Enqueue(context.Background(), NotificationIntent{
		Kind: NotifyCronMissed, RuleID: "cron:" + cronRef.CronID, IncidentID: incidentID,
		SiteID: e.site, WebhookID: "hook-1", TargetType: "http",
		TargetURL: e.server.URL, Secret: "",
		Payload: BuildCronPayload(NotifyCronMissed, cronRef, incidentID, 300, clk.now()),
	})
	if err != nil {
		t.Fatalf("enqueue cron missed: %v", err)
	}
	if id == "" {
		t.Fatalf("enqueue returned no delivery id")
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

	// The recovery side: same outbox, same shape.
	if _, err := e.notify.Enqueue(context.Background(), NotificationIntent{
		Kind: NotifyCronRecovered, RuleID: "cron:" + cronRef.CronID, IncidentID: incidentID,
		SiteID: e.site, WebhookID: "hook-1", TargetType: "http",
		TargetURL: e.server.URL, Secret: "",
		Payload: BuildCronPayload(NotifyCronRecovered, cronRef, incidentID, 0, clk.now()),
	}); err != nil {
		t.Fatalf("enqueue cron recovered: %v", err)
	}
	e.drain(t)
	if recv.count() != 2 {
		t.Fatalf("POSTs after recovery = %d, want 2", recv.count())
	}
	var p2 NotificationPayload
	if err := json.Unmarshal([]byte(recv.body(1)), &p2); err != nil {
		t.Fatalf("decode recovery payload: %v", err)
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

// rebindNotifyOnly swaps just the notifier (the restart between enqueue and
// drain - the intent must survive it in the table).
func (e *o10Engine) rebindNotifyOnly() {
	_, _, notify, _ := o10Bind(e.db, e.clock)
	e.notify = notify
}
