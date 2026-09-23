package platform

// O10 engine acceptance (the programme's alerting line): fault-simulated
// suites at the real engine + the real Nucleus fixture. Every test names
// the contract line it guards. Nucleus-gated: self-migrating, skips without
// a reachable engine (fails when OBSERVE_REQUIRE_NUCLEUS=1, via DSN).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/incidents"
)

// TestO10_SustainedFaultSendsExactlyOneOpeningNotification is THE
// acceptance line: a simulated sustained fault sends ONE appropriate
// opening notification. The repeat policy is explicit and say-so: a
// still-firing rule repeats only past its cooldown (minutes, default 5);
// three consecutive evaluations inside the cooldown produce exactly one
// notification, one incident, one history row.
func TestO10_SustainedFaultSendsExactlyOneOpeningNotification(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 2, 5, 5, 1)
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)

	for i := 0; i < 3; i++ {
		e.check(t) // sustained: still breaching on every tick
		clk.advance(time.Second)
	}
	e.drain(t)

	intents := e.intents(t, rule.RuleID)
	if len(intents) != 1 {
		t.Fatalf("sustained fault inside cooldown: want exactly 1 notification intent, got %d", len(intents))
	}
	if intents[0].Kind != NotifyIncidentOpened {
		t.Fatalf("intent kind = %q, want %q", intents[0].Kind, NotifyIncidentOpened)
	}
	if got := recv.count(); got != 1 {
		t.Fatalf("receiver POSTs = %d, want exactly 1 (one opening notification, no storm)", got)
	}
	p := payloadOf(t, intents[0])
	if p.Kind != NotifyIncidentOpened || p.RuleID != rule.RuleID || p.SiteID != e.site {
		t.Fatalf("opening payload wrong: %+v", p)
	}
	if p.Severity != "critical" || p.Value != 3 || p.Message == "" || p.IncidentID == "" {
		t.Fatalf("opening payload not self-describing: %+v", p)
	}

	// One incident, still open, linked from the edges.
	active, err := e.inc.ActiveByRule(context.Background(), rule.RuleID)
	if err != nil || len(active) != 1 {
		t.Fatalf("open incidents for rule = %d (err %v), want 1", len(active), err)
	}
	edges := e.evalRows(t, rule.RuleID)
	if len(edges) != 3 {
		t.Fatalf("evaluation rows = %d, want 3 (state persists per tick, not just current)", len(edges))
	}
	for i, edge := range edges {
		if edge.To != StateFiring {
			t.Fatalf("edge %d to_state = %q, want firing", i, edge.To)
		}
		if edge.IncidentID != active[0].IncidentID {
			t.Fatalf("edge %d incident_id = %q, want the open incident %q", i, edge.IncidentID, active[0].IncidentID)
		}
	}
	if edges[0].From != "" || edges[1].From != StateFiring || edges[2].From != StateFiring {
		t.Fatalf("edge sequence wrong: %+v", edges)
	}
}

// TestO10_RecoveryAndAcknowledgementRecorded: the fault clears (data
// present and in bounds - not a data gap), the incident closes, the
// recovery notification goes out, and an ack taken mid-incident survives
// the close on both the incident and its timeline.
func TestO10_RecoveryAndAcknowledgementRecorded(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 2, 5, 5, 1)
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)
	e.check(t)
	e.drain(t)

	active, err := e.inc.ActiveByRule(context.Background(), rule.RuleID)
	if err != nil || len(active) != 1 {
		t.Fatalf("incident did not open (err %v, n %d)", err, len(active))
	}
	incidentID := active[0].IncidentID

	// Acknowledge mid-incident.
	if err := e.inc.Ack(context.Background(), incidentID, "tyler"); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// The fault clears: errors gone, traffic present (a healthy decision,
	// not a no-data gap).
	e.clearErrors(t)
	e.seedEvents(t, 5, 30*time.Second)
	clk.advance(2 * time.Second)
	e.check(t)
	e.drain(t)

	if got := recv.count(); got != 2 {
		t.Fatalf("receiver POSTs = %d, want 2 (opening + recovery)", got)
	}
	intents := e.intents(t, rule.RuleID)
	if len(intents) != 2 || intents[1].Kind != NotifyIncidentRecovered {
		t.Fatalf("intents after recovery = %+v, want opening + recovered", intents)
	}
	if p := payloadOf(t, intents[1]); !strings.Contains(p.Message, "RECOVERED") {
		t.Fatalf("recovery payload message = %q", p.Message)
	}

	// Incident closed, ack preserved through the close, timeline complete.
	got, err := e.inc.Get(context.Background(), incidentID)
	if err != nil || got == nil {
		t.Fatalf("read incident: %v %v", got, err)
	}
	if got.EndedAt == 0 {
		t.Fatalf("incident not closed on recovery")
	}
	if got.AcknowledgedAt == 0 || got.AcknowledgedBy != "tyler" {
		t.Fatalf("ack lost on close: at=%d by=%q", got.AcknowledgedAt, got.AcknowledgedBy)
	}
	timeline, err := e.inc.Timeline(context.Background(), incidentID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	kinds := map[string]int{}
	for _, ev := range timeline {
		kinds[ev.Kind]++
	}
	for _, want := range []string{incidents.EventOpened, incidents.EventAck, incidents.EventRecovered, incidents.EventNotified} {
		if kinds[want] == 0 {
			t.Fatalf("timeline missing %q event: %+v", want, timeline)
		}
	}
	if kinds[incidents.EventAck] != 1 {
		t.Fatalf("timeline ack events = %d, want exactly 1 (ack is idempotent)", kinds[incidents.EventAck])
	}

	// The recovery edge is recorded with the healthy decision.
	edges := e.evalRows(t, rule.RuleID)
	last := edges[len(edges)-1]
	if last.To != StateHealthy || last.From != StateFiring {
		t.Fatalf("recovery edge = %q -> %q, want firing -> healthy", last.From, last.To)
	}
}

// TestO10_NoDataIsDistinctFromHealthy pins the no-data contract: a window
// with zero data points is a DISTINCT, labeled state - never silently
// treated as passing. It opens nothing, recovers nothing. The error_rate
// case is load-bearing: the old code returned 0% for an empty window, which
// read as healthy.
func TestO10_NoDataIsDistinctFromHealthy(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 0, 5, 5, 1)
	// No events at all: zero data points.
	e.check(t)

	edges := e.evalRows(t, rule.RuleID)
	if len(edges) != 1 || edges[0].To != StateNoData {
		t.Fatalf("empty window edge = %+v, want one no_data edge", edges)
	}
	if !strings.Contains(edges[0].Detail, "minimum") {
		t.Fatalf("no_data detail should label the sample gap: %q", edges[0].Detail)
	}
	if n := len(e.intents(t, rule.RuleID)); n != 0 {
		t.Fatalf("no_data enqueued %d notifications, want 0", n)
	}
	if got := recv.count(); got != 0 {
		t.Fatalf("no_data delivered %d notifications, want 0", got)
	}
	if active, _ := e.inc.ActiveByRule(context.Background(), rule.RuleID); len(active) != 0 {
		t.Fatalf("no_data opened an incident")
	}

	// Data present, in bounds: a real healthy DECISION, distinct edge.
	e.seedEvents(t, 4, time.Minute)
	clk.advance(time.Second)
	e.check(t)
	edges = e.evalRows(t, rule.RuleID)
	if len(edges) != 2 || edges[1].From != StateNoData || edges[1].To != StateHealthy {
		t.Fatalf("edges after healthy data = %+v, want no_data -> healthy", edges)
	}

	// The error_rate silent-zero bug: on a site with NO traffic the rate is
	// no_data, not a healthy 0 percent. (A site WITH traffic and zero
	// errors is a real healthy decision - that stays healthy.)
	rateRule, err := e.alerts.CreateRule(context.Background(), AlertRule{
		SiteID: o10Site(t), Name: "rate on a silent site", Metric: "error_rate",
		Operator: "gt", Threshold: 10, WindowMinutes: 5, Cooldown: 5, MinSamples: 1,
		Severity: "warning", CreatedBy: "o10",
	})
	if err != nil {
		t.Fatalf("create rate rule: %v", err)
	}
	clk.advance(time.Second)
	e.check(t)
	rateEdges := e.evalRows(t, rateRule.RuleID)
	if len(rateEdges) != 1 || rateEdges[0].To != StateNoData {
		t.Fatalf("error_rate on empty window = %+v, want no_data (never a silent 0 percent)", rateEdges)
	}
}

// TestO10_MinSamplesGateBlocksFiring: a rule cannot fire below its minimum
// sample count even when the value breaches - and reaching the minimum
// fires on the next evaluation.
func TestO10_MinSamplesGateBlocksFiring(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 1, 5, 5, 10)
	e.seedEvents(t, 4, time.Minute) // samples 4 < minimum 10
	e.seedErrors(t, 3, time.Minute) // value 3 breaches threshold 1
	e.check(t)

	edges := e.evalRows(t, rule.RuleID)
	if len(edges) != 1 || edges[0].To != StateNoData {
		t.Fatalf("below-minimum edge = %+v, want no_data", edges)
	}
	if !strings.Contains(edges[0].Detail, "4 of 10") {
		t.Fatalf("below-minimum detail = %q, want the counts named", edges[0].Detail)
	}
	if n := len(e.intents(t, rule.RuleID)); n != 0 {
		t.Fatalf("below-minimum fired %d notifications, want 0", n)
	}

	// Traffic grows past the minimum: the same breach now fires.
	e.seedEvents(t, 8, 30*time.Second)
	clk.advance(time.Second)
	e.check(t)
	edges = e.evalRows(t, rule.RuleID)
	if len(edges) != 2 || edges[1].To != StateFiring {
		t.Fatalf("at-minimum edge = %+v, want firing", edges)
	}
	if active, _ := e.inc.ActiveByRule(context.Background(), rule.RuleID); len(active) != 1 {
		t.Fatalf("at-minimum evaluation did not open the incident")
	}
}

// TestO10_CooldownRepeatPolicyOnStillFiringRule pins the repeat policy (the
// say-so of the acceptance): a still-firing rule repeats only past its
// cooldown (minutes) counted from the LAST notification, opening included.
func TestO10_CooldownRepeatPolicyOnStillFiringRule(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 2, 5, 1, 1) // cooldown 1 minute
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)

	e.check(t) // opening
	clk.advance(30 * time.Second)
	e.check(t) // still firing, inside cooldown: no repeat
	intents := e.intents(t, rule.RuleID)
	if len(intents) != 1 {
		t.Fatalf("inside cooldown: %d intents, want 1 (opening only)", len(intents))
	}
	clk.advance(31 * time.Second) // now past the 1-minute cooldown
	e.check(t)
	intents = e.intents(t, rule.RuleID)
	if len(intents) != 2 {
		t.Fatalf("past cooldown: %d intents, want 2 (opening + repeat)", len(intents))
	}
	if intents[1].Kind != NotifyIncidentRepeat {
		t.Fatalf("second intent kind = %q, want %q", intents[1].Kind, NotifyIncidentRepeat)
	}
	e.drain(t)
	if got := recv.count(); got != 2 {
		t.Fatalf("receiver POSTs = %d, want 2", got)
	}
	if p := payloadOf(t, intents[1]); !strings.Contains(p.Message, "STILL FIRING") {
		t.Fatalf("repeat payload message = %q", p.Message)
	}
	// One incident throughout - repeats never re-open.
	if active, _ := e.inc.ActiveByRule(context.Background(), rule.RuleID); len(active) != 1 {
		t.Fatalf("repeats changed the open-incident count: %d", len(active))
	}
}

// TestO10_EvaluationStateSurvivesRestart: state lives in the engine, not
// the process. A fresh service set over the same database (the restarted
// process) reads the firing state from the ledger and does NOT re-notify.
func TestO10_EvaluationStateSurvivesRestart(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	rule := e.rule(t, "error_count", "gt", 2, 5, 5, 1)
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)
	e.check(t)
	e.drain(t)
	if got := recv.count(); got != 1 {
		t.Fatalf("pre-restart POSTs = %d, want 1", got)
	}

	// The restarted process: fresh service instances, same engine.
	e.rebind()
	clk.advance(2 * time.Second)
	e.check(t)
	e.drain(t)

	if got := recv.count(); got != 1 {
		t.Fatalf("post-restart POSTs = %d, want 1 (no duplicate opening after restart)", got)
	}
	if n := len(e.intents(t, rule.RuleID)); n != 1 {
		t.Fatalf("post-restart intents = %d, want 1", n)
	}
	edges := e.evalRows(t, rule.RuleID)
	if len(edges) != 2 || edges[1].From != StateFiring || edges[1].To != StateFiring {
		t.Fatalf("post-restart edges = %+v, want firing -> firing (state read from the ledger)", edges)
	}
	if active, _ := e.inc.ActiveByRule(context.Background(), rule.RuleID); len(active) != 1 {
		t.Fatalf("post-restart open incidents = %d, want 1", len(active))
	}
}

// TestO10_MaintenanceSuppressesDeliveryNotEvaluation: an active maintenance
// window suppresses the notification DELIVERY visibly while the evaluation
// still records and the incident still opens.
func TestO10_MaintenanceSuppressesDeliveryNotEvaluation(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	recv := &o10Receiver{}
	e := newO10Engine(t, db, recv, clk)

	if _, err := e.notify.maintenance.Create(context.Background(), e.site,
		clk.now().Add(-time.Minute).UnixMilli(), clk.now().Add(time.Hour).UnixMilli(),
		"deploy window", "o10"); err != nil {
		t.Fatalf("create maintenance window: %v", err)
	}

	rule := e.rule(t, "error_count", "gt", 2, 5, 5, 1)
	e.seedEvents(t, 10, time.Minute)
	e.seedErrors(t, 3, time.Minute)
	e.check(t)
	e.drain(t)

	if got := recv.count(); got != 0 {
		t.Fatalf("maintenance window delivered %d notifications, want 0", got)
	}
	intents := e.intents(t, rule.RuleID)
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1 (the notification was still enqueued)", len(intents))
	}
	if intents[0].SuppressedAt == 0 || intents[0].SuppressedReason == "" {
		t.Fatalf("suppression not visible on the row: %+v", intents[0])
	}
	if intents[0].DeliveredAt != 0 || intents[0].Attempts != 0 {
		t.Fatalf("suppressed intent was worked instead of suppressed: %+v", intents[0])
	}

	// Evaluation recorded, incident open: maintenance is a delivery
	// decision, not an evaluation one.
	edges := e.evalRows(t, rule.RuleID)
	if len(edges) != 1 || edges[0].To != StateFiring {
		t.Fatalf("edges under maintenance = %+v, want the firing edge recorded", edges)
	}
	if active, _ := e.inc.ActiveByRule(context.Background(), rule.RuleID); len(active) != 1 {
		t.Fatalf("incident count under maintenance = %d, want 1 (still opens)", len(active))
	}
	stats := e.notify.Stats(context.Background())
	if stats.ByKind[NotifyIncidentOpened].Suppressed != 1 {
		t.Fatalf("healthz counters do not show the suppression: %+v", stats.ByKind)
	}
}

// TestO10_EngineDefaultsSaySo documents in executable form what the
// notification policy IS: cooldown minutes default to 5 through the CRUD
// path, and min_samples defaults to 1.
func TestO10_EngineDefaultsSaySo(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	alerts, _, _, _ := o10Bind(db, clk)
	rule, err := alerts.CreateRule(context.Background(), AlertRule{
		SiteID: o10Site(t), Name: "defaults", Metric: "error_count",
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if rule.Cooldown != 5 || rule.MinSamples != 1 || rule.Severity != "warning" {
		t.Fatalf("defaults: cooldown=%d min_samples=%d severity=%q; the repeat policy is one notification per cooldown (minutes, default 5), min gate 1 sample",
			rule.Cooldown, rule.MinSamples, rule.Severity)
	}
}
