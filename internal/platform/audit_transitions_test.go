package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/incidents"
)

func TestCronTransitionsRetryLookupAndPartialTargetAdmission(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			db := o10Fixture(t)
			clk := newO10Clock()
			e := newO10Engine(t, db, &o10Receiver{}, clk)
			if _, err := e.hooks.Create(context.Background(), e.site, "second", "http", e.server.URL, ""); err != nil {
				t.Fatal(err)
			}
			cron := CronMonitorRef{CronID: e.site, SiteID: e.site, Name: "cron"}
			ctx := context.Background()
			calls := 0
			if failAt == 0 {
				e.alerts.listHooksHook = func(context.Context, string, string) ([]Webhook, error) { return nil, errors.New("lookup failed") }
			} else {
				e.notify.enqueueHook = func(NotificationIntent) error {
					calls++
					if calls == failAt {
						return errors.New("target admission failed")
					}
					return nil
				}
			}
			if _, _, err := e.alerts.EnsureCronMissed(ctx, cron, 300); err == nil {
				t.Fatal("expected opening failure")
			}
			if active, err := e.inc.ActiveByRule(ctx, "cron:"+cron.CronID); err != nil || len(active) != 0 {
				t.Fatalf("orphaned incident: %+v %v", active, err)
			}
			if rows := e.intents(t, "cron:"+cron.CronID); len(rows) != 0 {
				t.Fatalf("partial opening admitted: %+v", rows)
			}
			e.alerts.listHooksHook = nil
			e.notify.enqueueHook = nil
			inc, created, err := e.alerts.EnsureCronMissed(ctx, cron, 300)
			if err != nil || !created {
				t.Fatalf("retry opening: %+v %v %v", inc, created, err)
			}
			if rows := e.intents(t, "cron:"+cron.CronID); len(rows) != 2 {
				t.Fatalf("missing opening targets: %+v", rows)
			}
			// Fail the recovery seam, then restart the services. The close must roll
			// back, so the new process discovers the obligation through active state.
			calls = 0
			if failAt == 0 {
				e.alerts.listHooksHook = func(context.Context, string, string) ([]Webhook, error) { return nil, errors.New("lookup failed") }
			} else {
				e.notify.enqueueHook = func(NotificationIntent) error {
					calls++
					if calls == failAt {
						return errors.New("recovery admission failed")
					}
					return nil
				}
			}
			if _, err := e.alerts.RecoverCron(ctx, cron, 300); err == nil {
				t.Fatal("expected recovery failure")
			}
			active, err := e.inc.ActiveByRule(ctx, "cron:"+cron.CronID)
			if err != nil || len(active) != 1 {
				t.Fatalf("failed recovery closed: %+v %v", active, err)
			}
			if rows := e.intents(t, "cron:"+cron.CronID); len(rows) != 2 {
				t.Fatalf("partial recovery admitted: %+v", rows)
			}
			e.alerts, e.inc, e.notify, e.hooks = o10Bind(db, clk)
			if n, err := e.alerts.RecoverCron(ctx, cron, 300); err != nil || n != 1 {
				t.Fatalf("restart recovery: %d %v", n, err)
			}
			if n, err := e.alerts.RecoverCron(ctx, cron, 300); err != nil || n != 0 {
				t.Fatalf("duplicate recovery: %d %v", n, err)
			}
			rows := e.intents(t, "cron:"+cron.CronID)
			if len(rows) != 4 {
				t.Fatalf("targets lost/duplicated: %+v", rows)
			}
			for _, row := range rows {
				if row.IncidentID != inc.IncidentID {
					t.Fatalf("correlation lost: %+v", row)
				}
			}
		})
	}
}

func TestAlertRecoveryCarriesEveryIncidentAndAtomicAdmission(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	e := newO10Engine(t, db, &o10Receiver{}, clk)
	ctx := context.Background()
	rule := e.rule(t, "error_count", "gt", 1, 5, 0, 1)
	var ids []string
	for i := 0; i < 2; i++ {
		inc, err := e.inc.Create(ctx, incidents.CreateInput{SiteID: e.site, Title: "historical duplicate", RuleID: rule.RuleID}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inc.IncidentID)
	}
	e.alerts.listHooksHook = func(context.Context, string, string) ([]Webhook, error) { return nil, errors.New("lookup failed") }
	if err := e.alerts.evaluateHealthy(ctx, rule, ruleState{State: StateFiring}, 0, 10, clk.now()); err == nil {
		t.Fatal("lost lookup error")
	}
	active, err := e.inc.ActiveByRule(ctx, rule.RuleID)
	if err != nil || len(active) != 2 {
		t.Fatalf("lookup failure closed incident: %+v %v", active, err)
	}
	e.alerts.listHooksHook = nil
	if err := e.alerts.evaluateHealthy(ctx, rule, ruleState{State: StateFiring}, 0, 10, clk.now()); err != nil {
		t.Fatal(err)
	}
	rows := e.intents(t, rule.RuleID)
	if len(rows) != 2 {
		t.Fatalf("recoveries: %+v", rows)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		var p NotificationPayload
		if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
			t.Fatal(err)
		}
		if p.IncidentID == "" || p.IncidentID != row.IncidentID {
			t.Fatalf("payload lost id: %+v %+v", p, row)
		}
		seen[p.IncidentID] = true
	}
	e.drain(t)
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("incident %s omitted", id)
		}
		tl, err := e.inc.Timeline(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		notified := 0
		for _, event := range tl {
			if event.Kind == incidents.EventNotified {
				notified++
			}
		}
		if notified != 1 {
			t.Fatalf("recovery delivery timeline %s: %+v", id, tl)
		}
	}
}

type auditRoundTripper func(*http.Request) (*http.Response, error)

func (f auditRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotifierStopCancelsFirstSendWithoutAdmittingRestOfBatch(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	e := newO10Engine(t, db, &o10Receiver{}, clk)
	var calls atomic.Int32
	started := make(chan struct{})
	e.notify.client = &http.Client{Transport: auditRoundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	for i := 0; i < 3; i++ {
		o10Enqueue(t, e.notify, e.site, e.server.URL)
	}
	e.notify.Start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.notify.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown sent %d requests", calls.Load())
	}
	for _, row := range e.intents(t, "o10-notify-rule") {
		if row.DeliveredAt != 0 || row.Attempts != 0 {
			t.Fatalf("unsent/canceled row consumed: %+v", row)
		}
	}
}

func TestNotifierTimelineFailureRollsBackDispositionAndRepairsOnRetry(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		t.Run(fmt.Sprint(suppressed), func(t *testing.T) {
			db := o10Fixture(t)
			clk := newO10Clock()
			recv := &o10Receiver{}
			e := newO10Engine(t, db, recv, clk)
			o10Enqueue(t, e.notify, e.site, e.server.URL)
			rows := e.intents(t, "o10-notify-rule")
			row := rows[0]
			ctx := context.Background()
			real := e.notify.recorder
			e.notify.recorder = func(context.Context, *nucleus.SQLModel, string, string, int64, string, string, string) error {
				return errors.New("timeline write failure")
			}
			var err error
			if suppressed {
				err = e.notify.markSuppressed(ctx, &row, clk.now())
			} else {
				_, err = e.notify.DrainDue(ctx)
			}
			if err == nil {
				t.Fatal("timeline error was swallowed")
			}
			rows = e.intents(t, "o10-notify-rule")
			if rows[0].DeliveredAt != 0 || rows[0].SuppressedAt != 0 {
				t.Fatalf("terminal mark without timeline: %+v", rows)
			}
			e.notify.recorder = real
			if suppressed {
				if err := e.notify.markSuppressed(ctx, &rows[0], clk.now()); err != nil {
					t.Fatal(err)
				}
			} else {
				e.drain(t)
				if recv.unique() != 1 {
					t.Fatalf("retry changed delivery identity")
				}
			}
			tl, err := e.inc.Timeline(ctx, row.IncidentID)
			if err != nil || len(tl) != 1 {
				t.Fatalf("lost/duplicate disposition event: %+v %v", tl, err)
			}
		})
	}
}

func TestCanceledNotificationRequestDoesNotStartHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	client := &http.Client{}
	// Custom transports may ignore cancellation; admission is checked separately
	// by processOne and DrainDue. The request itself must carry the canceled ctx.
	client.Transport = auditRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		calls++
		return nil, errors.New("unexpected send")
	})
	if err := postSignedJSON(ctx, client, "id", "https://example.test", "", nil); err == nil || calls != 0 {
		t.Fatalf("unbound request: calls=%d err=%v", calls, err)
	}
}

func TestNotifierLockWaitHonorsCancellation(t *testing.T) {
	n := &Notifier{}
	n.processMu.Lock()
	defer n.processMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := n.DrainDue(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v", err)
	}
}
