package platform

// O10 notification-outbox oracle: retries, visible exponential backoff, the
// bounded attempt budget ending in a durable dead letter, exactly-once
// restart semantics via the delivery-id dedupe marker, and healthz
// counters. Nucleus-gated, same fixture posture as the engine suites.

import (
	"context"
	"testing"
	"time"
)

// o10Enqueue enqueues one delivered-to-receiver intent directly (the engine
// path is covered by the engine suites; these tests pin the drain).
func o10Enqueue(t *testing.T, n *Notifier, site, url string) string {
	t.Helper()
	id, err := n.enqueueTx(context.Background(), n.db.SQL(), NotificationIntent{
		Kind: NotifyIncidentOpened, RuleID: "o10-notify-rule", IncidentID: "o10-incident",
		SiteID: site, WebhookID: "o10-hook", TargetType: "http", TargetURL: url,
		Payload: BuildNotificationPayload(NotifyIncidentOpened, AlertRule{
			RuleID: "o10-notify-rule", SiteID: site, Name: "retry rule", Metric: "error_count",
			Threshold: 2, Severity: "critical",
		}, 7, 30, StateHealthy, StateFiring, "o10-incident", time.Now()),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// TestO10_NotifierRetriesWithBackoffThenDeadLetters: a failing provider is
// retried with VISIBLE exponential backoff under a bounded attempt budget;
// the budget's end is a durable dead letter with last_error kept - never a
// storm (POSTs are bounded by the budget), never a silent drop, never
// retried again even far into the future.
func TestO10_NotifierRetriesWithBackoffThenDeadLetters(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{failFirst: 1 << 30} // always failing
	e := newO10Engine(t, db, recv, clk)
	e.notify.WithMaxAttempts(3).WithBackoffBase(10 * time.Second)

	id := o10Enqueue(t, e.notify, e.site, e.server.URL)

	// Attempt 1 fails; backoff schedules +10s.
	e.drain(t)
	if got := recv.count(); got != 1 {
		t.Fatalf("POSTs after attempt 1 = %d, want 1", got)
	}
	rows := e.intents(t, "o10-notify-rule")
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("intent row missing: %+v", rows)
	}
	if rows[0].Attempts != 1 || rows[0].DeliveredAt != 0 {
		t.Fatalf("after attempt 1: %+v", rows[0])
	}
	gap1 := rows[0].NextAttemptAt - clk.now().UnixMilli()
	if gap1 != 10_000 {
		t.Fatalf("backoff after attempt 1 = %dms, want 10000 (base, visible)", gap1)
	}

	// Attempt 2 fails; backoff doubles to +20s.
	clk.advance(11 * time.Second)
	e.drain(t)
	if got := recv.count(); got != 2 {
		t.Fatalf("POSTs after attempt 2 = %d, want 2", got)
	}
	rows = e.intents(t, "o10-notify-rule")
	if rows[0].Attempts != 2 {
		t.Fatalf("attempts after attempt 2 = %d, want 2", rows[0].Attempts)
	}
	gap2 := rows[0].NextAttemptAt - clk.now().UnixMilli()
	if gap2 != 20_000 {
		t.Fatalf("backoff after attempt 2 = %dms, want 20000 (doubled)", gap2)
	}

	// Attempt 3 hits the budget: durable dead letter, last_error kept.
	clk.advance(21 * time.Second)
	e.drain(t)
	if got := recv.count(); got != 3 {
		t.Fatalf("POSTs = %d, want exactly 3 (bounded by the attempt budget - no storm)", got)
	}
	rows = e.intents(t, "o10-notify-rule")
	if rows[0].NextAttemptAt != -1 {
		t.Fatalf("dead letter sentinel = %d, want -1", rows[0].NextAttemptAt)
	}
	if rows[0].LastError == "" {
		t.Fatalf("dead letter lost last_error")
	}
	if rows[0].DeliveredAt != 0 {
		t.Fatalf("dead-lettered row claims delivery")
	}

	// Far into the future: a dead letter is never selected again.
	clk.advance(24 * time.Hour)
	e.drain(t)
	if got := recv.count(); got != 3 {
		t.Fatalf("POSTs after dead-letter = %d, want 3 (dead is durable)", got)
	}
	stats := e.notify.Stats(context.Background())
	if stats.ByKind[NotifyIncidentOpened].DeadLettered != 1 || stats.ByKind[NotifyIncidentOpened].Delivered != 0 {
		t.Fatalf("healthz counters wrong: %+v", stats.ByKind[NotifyIncidentOpened])
	}
}

// TestO10_NotifierSucceedsAfterRetries: a provider that recovers mid-retry
// sequence gets delivered - POSTs land once per attempt (3), the row shows
// the two failed attempts, and the healthz counters show the delivery.
func TestO10_NotifierSucceedsAfterRetries(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{failFirst: 2} // fails twice, then healthy
	e := newO10Engine(t, db, recv, clk)
	e.notify.WithMaxAttempts(5).WithBackoffBase(10 * time.Second)

	o10Enqueue(t, e.notify, e.site, e.server.URL)

	e.drain(t)
	clk.advance(11 * time.Second)
	e.drain(t)
	clk.advance(21 * time.Second)
	e.drain(t)

	if got := recv.count(); got != 3 {
		t.Fatalf("POSTs = %d, want 3 (two failures + the successful retry)", got)
	}
	rows := e.intents(t, "o10-notify-rule")
	if len(rows) != 1 || rows[0].DeliveredAt == 0 || rows[0].Attempts != 2 {
		t.Fatalf("row after recovery: %+v (want delivered with attempts=2)", rows)
	}
	stats := e.notify.Stats(context.Background())
	if stats.ByKind[NotifyIncidentOpened].Delivered != 1 || stats.ByKind[NotifyIncidentOpened].Pending != 0 {
		t.Fatalf("healthz counters wrong: %+v", stats.ByKind[NotifyIncidentOpened])
	}
}

// TestO10_NotifierRestartIsExactlyOnceByDeliveryMarker: kill-the-process
// mid-drain (the POST left, the disposition mark did not - the skipMark
// seam is exactly that window) and restart. The drain is at-least-once, so
// the POST is repeated - under the SAME X-Observe-Delivery id, which is
// what makes the delivery exactly-once at the receiver: unique ids stay at
// 1 while raw POSTs reach 2. The terminal state is durable: delivered.
func TestO10_NotifierRestartIsExactlyOnceByDeliveryMarker(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	id := o10Enqueue(t, e.notify, e.site, e.server.URL)

	// The crash window: delivered, then the process dies before the mark.
	e.notify.skipMark = true
	if _, err := e.notify.DrainDue(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := recv.count(); got != 1 {
		t.Fatalf("POSTs before crash = %d, want 1", got)
	}
	rows := e.intents(t, "o10-notify-rule")
	if rows[0].DeliveredAt != 0 {
		t.Fatalf("crash simulation marked the row delivered - the seam is wrong")
	}

	// The restarted process: fresh notifier over the same engine state.
	e.rebind()
	clk.advance(time.Second)
	e.drain(t)

	if got := recv.count(); got != 2 {
		t.Fatalf("POSTs after restart = %d, want 2 (at-least-once resend)", got)
	}
	if got := recv.unique(); got != 1 {
		t.Fatalf("unique delivery ids after restart = %d, want 1 (exactly-once by the dedupe marker)", got)
	}
	if ids := recv.ids(); ids[0] != id || ids[1] != id {
		t.Fatalf("delivery ids changed across the restart: %v", ids)
	}
	rows = e.intents(t, "o10-notify-rule")
	if rows[0].DeliveredAt == 0 {
		t.Fatalf("terminal state lost after restart")
	}
	// And the terminal state stops further drains cold.
	e.drain(t)
	if got := recv.count(); got != 2 {
		t.Fatalf("POSTs after terminal mark = %d, want 2 (no re-delivery of a delivered intent)", got)
	}
}

// TestO10_MaintenanceWindowScopesBySite: a window suppresses only its site
// (or every site when site_id is empty); other sites deliver.
func TestO10_MaintenanceWindowScopesBySite(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	other := o10Site(t)
	o10Cleanup(t, db, other)
	now := clk.now()
	if _, err := e.notify.maintenance.Create(context.Background(), other,
		now.Add(-time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli(), "other site", "o10"); err != nil {
		t.Fatalf("create window: %v", err)
	}

	// A notification for the engine's site: the OTHER site's window must
	// not suppress it.
	o10Enqueue(t, e.notify, e.site, e.server.URL)
	e.drain(t)
	if got := recv.count(); got != 1 {
		t.Fatalf("POSTs with a foreign window = %d, want 1 (windows scope by site)", got)
	}

	// A global window (site_id empty) suppresses every site.
	if _, err := e.notify.maintenance.Create(context.Background(), "",
		now.Add(-time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli(), "global", "o10"); err != nil {
		t.Fatalf("create global window: %v", err)
	}
	o10Enqueue(t, e.notify, e.site, e.server.URL)
	e.drain(t)
	if got := recv.count(); got != 1 {
		t.Fatalf("POSTs under a global window = %d, want 1 (no new delivery)", got)
	}
	stats := e.notify.Stats(context.Background())
	if stats.ByKind[NotifyIncidentOpened].Suppressed != 1 {
		t.Fatalf("healthz counters do not show the suppression: %+v", stats.ByKind[NotifyIncidentOpened])
	}
}
