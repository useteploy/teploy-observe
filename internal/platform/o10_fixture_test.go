package platform

// O10 shared test fixtures: a migrated scratch Nucleus, a frozen clock, a
// counting fake webhook receiver, and readers that observe exactly what
// production queries observe (the argMax collapses). The fault-simulated
// suites live in o10_alerts_engine_test.go and o10_notify_outbox_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/incidents"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func o10Fixture(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// o10Site mints a unique site id so the shared scratch fixture's accumulated
// rows from earlier runs never bleed into per-test counts.
func o10Site(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("o10-%d", time.Now().UnixNano())
}

// o10Cleanup removes this test's rows from the tables keyed by site_id.
// maintenance_windows also sweeps GLOBAL windows (site_id '') - a leftover
// global window from an earlier run suppresses every later test's
// deliveries. Incidents and their events are keyed by per-test
// rule/incident ids and are asserted only through those ids, so they need
// no cross-test sweep.
func o10Cleanup(t *testing.T, db *nucleus.Client, site string) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"alert_rules", "alert_history", "alert_evaluations", "notification_outbox",
		"webhooks", "events", "error_events",
	} {
		if _, err := db.SQL().Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE site_id = $1", table), site); err != nil {
			t.Fatalf("cleanup %s: %v", table, err)
		}
	}
	if _, err := db.SQL().Exec(ctx,
		`DELETE FROM maintenance_windows WHERE site_id = $1 OR site_id = ''`, site); err != nil {
		t.Fatalf("cleanup maintenance_windows: %v", err)
	}
}

// o10Clock is a frozen, manually advanced clock shared by the engine and the
// notifier under test, so cooldowns, backoffs and windows are deterministic.
type o10Clock struct {
	mu sync.Mutex
	ms int64
}

func newO10Clock() *o10Clock {
	return &o10Clock{ms: time.Now().UTC().UnixMilli()}
}

func (c *o10Clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms).UTC()
}

func (c *o10Clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += d.Milliseconds()
}

// o10Receiver is the fake webhook provider: it counts POSTs, records every
// X-Observe-Delivery id in order, and can fail the first N requests with a
// 500 to simulate a failing provider.
type o10Receiver struct {
	mu        sync.Mutex
	posts     int
	delivered []string
	bodies    []string
	failFirst int
}

func (r *o10Receiver) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := req.Header.Get("X-Observe-Delivery")
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.posts++
		n := r.posts
		r.delivered = append(r.delivered, id)
		r.bodies = append(r.bodies, string(body))
		r.mu.Unlock()
		if r.failFirst > 0 && n <= r.failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (r *o10Receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posts
}

// unique counts distinct delivery ids - the receiver-side dedupe an
// exactly-once consumer performs on the X-Observe-Delivery marker.
func (r *o10Receiver) unique() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range r.delivered {
		seen[id] = true
	}
	return len(seen)
}

func (r *o10Receiver) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.delivered...)
}

func (r *o10Receiver) body(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[i]
}

// o10Engine wires the real services against the real engine: the alert
// service (evaluation state machine), the incident service, the durable
// notifier (plain client so httptest's 127.0.0.1 is diallable - the same
// swap webhooks_test makes), and the site's webhook pointed at the receiver.
type o10Engine struct {
	db      *nucleus.Client
	site    string
	clock   *o10Clock
	alerts  *AlertService
	inc     *incidents.Service
	notify  *Notifier
	hooks   *WebhookService
	recv    *o10Receiver
	server  *httptest.Server
}

// o10Bind builds a fresh service set over the engine (the "restarted
// process"): new instances, same database. The notifier gets a plain client
// so httptest's 127.0.0.1 is diallable - the same swap webhooks_test makes.
func o10Bind(db *nucleus.Client, clk *o10Clock) (*AlertService, *incidents.Service, *Notifier, *WebhookService) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inc := incidents.NewService(db)
	hooks := NewWebhookService(db, logger)
	maint := NewMaintenanceService(db, logger)
	notify := NewNotifier(db, logger, maint, func(ctx context.Context, incidentID, kind, actor, detail string) error {
		return inc.RecordEvent(ctx, incidentID, kind, actor, detail)
	})
	notify.client = &http.Client{Timeout: 5 * time.Second}
	alerts := NewAlertService(db, logger, hooks, inc, notify)
	alerts.now = clk.now
	notify.now = clk.now
	return alerts, inc, notify, hooks
}

func newO10Engine(t *testing.T, db *nucleus.Client, recv *o10Receiver, clk *o10Clock) *o10Engine {
	t.Helper()
	site := o10Site(t)
	alerts, inc, notify, hooks := o10Bind(db, clk)

	srv := httptest.NewServer(recv.handler())
	t.Cleanup(srv.Close)
	// Sweep any earlier run's rows for this site, then create the sink.
	o10Cleanup(t, db, site)
	if _, err := hooks.Create(context.Background(), site, "o10-sink", "http", srv.URL, ""); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{
			"alert_rules", "alert_history", "alert_evaluations", "notification_outbox",
			"webhooks", "events", "error_events",
		} {
			_, _ = db.SQL().Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE site_id = $1", table), site)
		}
		_, _ = db.SQL().Exec(ctx, `DELETE FROM maintenance_windows WHERE site_id = $1 OR site_id = ''`, site)
	})
	return &o10Engine{
		db: db, site: site, clock: clk, alerts: alerts, inc: inc,
		notify: notify, hooks: hooks, recv: recv, server: srv,
	}
}

// rule creates an alert rule through the production CRUD path.
func (e *o10Engine) rule(t *testing.T, metric, operator string, threshold float64, windowMins, cooldown, minSamples int) AlertRule {
	t.Helper()
	rule, err := e.alerts.CreateRule(context.Background(), AlertRule{
		SiteID: e.site, Name: "o10 rule " + metric, Metric: metric,
		Operator: operator, Threshold: threshold,
		WindowMinutes: windowMins, Cooldown: cooldown, MinSamples: minSamples,
		Severity: "critical", CreatedBy: "o10",
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return *rule
}

// seedEvents inserts n plain events; seedErrors inserts n error events. All
// carry timestamps inside the rule window relative to the frozen clock.
func (e *o10Engine) seedEvents(t *testing.T, n int, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	ts := e.clock.now().Add(-age).UnixMilli()
	for i := 0; i < n; i++ {
		if _, err := e.db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp)
			 VALUES ($1, 'default', $2, $3, $4, 'pageview', $5)`,
			genID(), e.site, genID(), genID(), fmt.Sprintf("%d", ts+int64(i)),
		); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
}

func (e *o10Engine) seedErrors(t *testing.T, n int, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	ts := e.clock.now().Add(-age).UnixMilli()
	for i := 0; i < n; i++ {
		if _, err := e.db.SQL().Exec(ctx,
			`INSERT INTO error_events (error_id, tenant_id, site_id, session_id, issue_id, group_hash, timestamp)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
			genID(), e.site, genID(), genID(), genID(), fmt.Sprintf("%d", ts+int64(i)),
		); err != nil {
			t.Fatalf("seed error %d: %v", i, err)
		}
	}
}

func (e *o10Engine) clearErrors(t *testing.T) {
	t.Helper()
	if _, err := e.db.SQL().Exec(context.Background(),
		`DELETE FROM error_events WHERE site_id = $1`, e.site); err != nil {
		t.Fatalf("clear errors: %v", err)
	}
}

// evalRows returns the rule's transition edges oldest first.
func (e *o10Engine) evalRows(t *testing.T, ruleID string) []struct {
	From, To, Detail, IncidentID string
	Samples                      int64
} {
	t.Helper()
	rows, err := nucleus.Query[struct {
		FromState  string `db:"from_state"`
		ToState    string `db:"to_state"`
		Detail     string `db:"detail"`
		IncidentID string `db:"incident_id"`
		Samples    string `db:"samples"`
	}](context.Background(), e.db.SQL(),
		`SELECT from_state, to_state, detail, incident_id, CAST(samples AS TEXT) AS samples
		 FROM alert_evaluations WHERE rule_id = $1
		 ORDER BY evaluated_at ASC, eval_id ASC`, ruleID)
	if err != nil {
		t.Fatalf("read evaluations: %v", err)
	}
	out := make([]struct {
		From, To, Detail, IncidentID string
		Samples                      int64
	}, 0, len(rows))
	for _, r := range rows {
		var s int64
		fmt.Sscanf(r.Samples, "%d", &s)
		out = append(out, struct {
			From, To, Detail, IncidentID string
			Samples                      int64
		}{r.FromState, r.ToState, r.Detail, r.IncidentID, s})
	}
	return out
}

// intents returns the rule's collapsed notification rows (what every
// production reader sees).
func (e *o10Engine) intents(t *testing.T, ruleID string) []notifyRow {
	t.Helper()
	rows, err := nucleus.Query[notifyRow](context.Background(), e.db.SQL(),
		`SELECT id, kind, rule_id, incident_id, site_id, webhook_id, target_type, target_url, secret, payload,
			created_at, attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		 FROM (`+notifyCollapseSelect("rule_id = $1")+`)
		 ORDER BY created_at ASC, id ASC`, ruleID)
	if err != nil {
		t.Fatalf("read intents: %v", err)
	}
	return rows
}

func (e *o10Engine) check(t *testing.T) {
	t.Helper()
	if err := e.alerts.CheckRules(context.Background()); err != nil {
		t.Fatalf("CheckRules: %v", err)
	}
}

// rebind swaps the engine's services for fresh instances over the same
// database, clock and site - the restarted process reading persisted state.
func (e *o10Engine) rebind() {
	alerts, inc, notify, _ := o10Bind(e.db, e.clock)
	e.alerts, e.inc, e.notify = alerts, inc, notify
}

func (e *o10Engine) drain(t *testing.T) {
	t.Helper()
	if _, err := e.notify.DrainDue(context.Background()); err != nil {
		t.Fatalf("DrainDue: %v", err)
	}
}

// payloadOf decodes an intent's frozen payload.
func payloadOf(t *testing.T, row notifyRow) NotificationPayload {
	t.Helper()
	var p NotificationPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}
