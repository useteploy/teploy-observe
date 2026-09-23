package query

// O04 storage-free semantics tests: the deterministic funnel walk, the
// entity dispatch, and the retention grid math. These bind the HAND-
// COMPUTED literals pinned in internal/session/reference_test.go (the
// O04 oracle tables) to the implementation without needing a database;
// the Nucleus-gated twin (funnel_retention_nucleus_test.go) proves the
// SQL fetch + wiring against the same tables.
//
// TDD note: this file was written first and failed to compile against
// the pre-O04 code (walkFunnelEvents / entityKeyOf / buildRetentionCohorts
// did not exist; the old walk had no window, no exclusions, no tie-break).

import (
	"fmt"
	"testing"
	"time"
)

func fe(id, sess, visit, distinct, eventType, pathname string, ts time.Time) funnelEvent {
	return funnelEvent{
		EventID:    id,
		SessionID:  sess,
		VisitID:    visit,
		DistinctID: distinct,
		EventType:  eventType,
		Pathname:   pathname,
		Timestamp:  ts.UnixMilli(),
	}
}

func at(h, m int) time.Time { return time.Date(2026, 7, 10, h, m, 0, 0, time.UTC) }

func step(v string) FunnelStep { return FunnelStep{Type: "event", Value: v} }

// Oracle tie pin: two events at the same stored timestamp order by
// (timestamp, event_id) ASCENDING, so "ev-001" (signup) sequences before
// "ev-002" (purchase) even though the purchase was ingested first.
// A flipped tie-break must fail this test (mutation check).
func TestFunnelWalk_TimestampTieBreaksByEventID(t *testing.T) {
	events := []funnelEvent{
		fe("ev-002", "s", "v", "", "purchase", "", at(10, 0)),
		fe("ev-001", "s", "v", "", "signup", "", at(10, 0)),
	}
	got := walkFunnelEvents(events, []FunnelStep{step("signup"), step("purchase")}, 0, nil)
	if got[0] != 1 || got[1] != 1 {
		t.Fatalf("tie order (ts, event_id ASC): counts = %v, want [1 1]", got)
	}
}

// Distinct events at the same ts can chain in event_id order (strictly
// increasing total order, never the same event twice).
func TestFunnelWalk_SameTimestampChainInEventIDOrder(t *testing.T) {
	events := []funnelEvent{
		fe("ev-01", "s", "v", "", "signup", "", at(10, 0)),
		fe("ev-02", "s", "v", "", "purchase", "", at(10, 0)),
	}
	got := walkFunnelEvents(events, []FunnelStep{step("signup"), step("purchase")}, 0, nil)
	if got[0] != 1 || got[1] != 1 {
		t.Fatalf("same-ts chain: counts = %v, want [1 1]", got)
	}
}

// Conversion window edges: the boundary ts == e0 + W is IN; W+1 is OUT.
// Window is measured from the entity's FIRST step-0 event.
func TestFunnelWalk_ConversionWindowEdges(t *testing.T) {
	const w = 10 * time.Minute
	base := at(10, 0)
	cases := []struct {
		name   string
		offset time.Duration
		want   [2]int
	}{
		{"exactly at window edge", w, [2]int{1, 1}},
		{"one ms past the edge", w + time.Millisecond, [2]int{1, 0}},
		{"well inside", w - time.Millisecond, [2]int{1, 1}},
	}
	for _, tc := range cases {
		events := []funnelEvent{
			fe("e0", "s", "v", "", "signup", "", base),
			fe("e1", "s", "v", "", "purchase", "", base.Add(tc.offset)),
		}
		got := walkFunnelEvents(events, []FunnelStep{step("signup"), step("purchase")}, w.Milliseconds(), nil)
		if got[0] != tc.want[0] || got[1] != tc.want[1] {
			t.Fatalf("%s: counts = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Unbounded (window <= 0): anything within the fetched range counts.
	events := []funnelEvent{
		fe("e0", "s", "v", "", "signup", "", base),
		fe("e1", "s", "v", "", "purchase", "", base.Add(72*time.Hour)),
	}
	if got := walkFunnelEvents(events, []FunnelStep{step("signup"), step("purchase")}, 0, nil); got[1] != 1 {
		t.Fatalf("unbounded window: counts = %v, want step-2 conversion", got)
	}
}

// Repeated steps: [A, A] needs two distinct A events; conversion keys on
// first progression (the greedy earliest chain), and intervening events
// of other types never block.
func TestFunnelWalk_RepeatedStepsAndFirstProgression(t *testing.T) {
	one := []funnelEvent{fe("a1", "s", "v", "", "A", "", at(10, 0))}
	if got := walkFunnelEvents(one, []FunnelStep{step("A"), step("A")}, 0, nil); got[0] != 1 || got[1] != 0 {
		t.Fatalf("one A event, steps [A A]: counts = %v, want [1 0]", got)
	}
	three := []funnelEvent{
		fe("a1", "s", "v", "", "A", "", at(10, 0)),
		fe("x1", "s", "v", "", "X", "", at(10, 1)),
		fe("a2", "s", "v", "", "A", "", at(10, 2)),
	}
	if got := walkFunnelEvents(three, []FunnelStep{step("A"), step("A")}, 0, nil); got[0] != 1 || got[1] != 1 {
		t.Fatalf("two A events with X between, steps [A A]: counts = %v, want [1 1]", got)
	}
	// Greedy earliest chain: B@0 then B@2 both exist; conversion counted
	// once, keyed on the first progression.
	bb := []funnelEvent{
		fe("b1", "s", "v", "", "B", "", at(10, 0)),
		fe("a1", "s", "v", "", "A", "", at(10, 1)),
		fe("b2", "s", "v", "", "B", "", at(10, 2)),
	}
	if got := walkFunnelEvents(bb, []FunnelStep{step("B"), step("B")}, 0, nil); got[0] != 1 || got[1] != 1 {
		t.Fatalf("steps [B B] over B,A,B: counts = %v, want [1 1] (once, first progression)", got)
	}
}

// Exclusions: the first exclusion-matching event strictly after the
// step-0 event terminates progression; exclusion events at or before the
// step-0 event never disqualify; the check applies to events after entry
// even when they also match a later step.
func TestFunnelWalk_Exclusions(t *testing.T) {
	steps := []FunnelStep{step("signup"), step("purchase")}
	ex := []FunnelStep{step("refund")}

	after := []funnelEvent{
		fe("e0", "s", "v", "", "signup", "", at(10, 0)),
		fe("ex", "s", "v", "", "refund", "", at(10, 5)),
		fe("e1", "s", "v", "", "purchase", "", at(10, 10)),
	}
	if got := walkFunnelEvents(after, steps, 0, ex); got[0] != 1 || got[1] != 0 {
		t.Fatalf("exclusion after entry: counts = %v, want [1 0]", got)
	}
	before := []funnelEvent{
		fe("ex", "s", "v", "", "refund", "", at(9, 55)),
		fe("e0", "s", "v", "", "signup", "", at(10, 0)),
		fe("e1", "s", "v", "", "purchase", "", at(10, 10)),
	}
	if got := walkFunnelEvents(before, steps, 0, ex); got[0] != 1 || got[1] != 1 {
		t.Fatalf("exclusion before entry: counts = %v, want [1 1] (no effect)", got)
	}
	// An exclusion event sharing the step-0 timestamp: entry is pinned to
	// the step-0 match itself; a LATER event matching an exclusion (even
	// one that also matches step 1) blocks.
	selfEx := []funnelEvent{
		fe("e0", "s", "v", "", "signup", "", at(10, 0)),
		fe("e1", "s", "v", "", "signup", "", at(10, 5)),
		fe("e2", "s", "v", "", "purchase", "", at(10, 10)),
	}
	if got := walkFunnelEvents(selfEx, steps, 0, []FunnelStep{step("signup")}); got[0] != 1 || got[1] != 0 {
		t.Fatalf("exclusion matching a re-fired step: counts = %v, want [1 0]", got)
	}
}

// Entity dispatch. Mutation check: hard-wiring the dispatch to
// session_id fails the person-mode table (scenario C twin below).
func TestEntityKeyOf_Dispatch(t *testing.T) {
	e := fe("e1", "sess-1", "visit-1", "person-1", "A", "", at(10, 0))
	if k, ok := entityKeyOf(EntityVisitorEstimate, e); !ok || k != "sess-1" {
		t.Fatalf("visitor-estimate: key = %q ok=%v, want sess-1", k, ok)
	}
	if k, ok := entityKeyOf(EntityVisit, e); !ok || k != "visit-1" {
		t.Fatalf("visit: key = %q ok=%v, want visit-1", k, ok)
	}
	if k, ok := entityKeyOf(EntityPerson, e); !ok || k != "person-1" {
		t.Fatalf("person: key = %q ok=%v, want person-1", k, ok)
	}
	anon := fe("e2", "sess-1", "visit-1", "", "A", "", at(10, 0))
	if _, ok := entityKeyOf(EntityPerson, anon); ok {
		t.Fatal("person mode must exclude anonymous events (distinct_id == \"\")")
	}
	for _, m := range []string{EntityVisitorEstimate, EntityVisit} {
		if _, ok := entityKeyOf(m, anon); !ok {
			t.Fatalf("%s mode must include anonymous events", m)
		}
	}
	if err := ValidateEntity("person"); err != nil {
		t.Fatalf("person must validate: %v", err)
	}
	if err := ValidateEntity(""); err != nil {
		t.Fatalf("empty entity (default) must validate: %v", err)
	}
	if err := ValidateEntity("user"); err == nil {
		t.Fatal("unknown entity must be rejected")
	}
}

// Scenario A twin (synthetic ids; identity derivation is pinned in the
// session oracle and end-to-end in the Nucleus test): one estimate, two
// visits, one person over the identified rows.
func TestGroupFunnelByEntity_ScenarioA(t *testing.T) {
	sess, siteSalt := "sess-a", "person-a"
	rows := []funnelEvent{
		fe("a-1", sess, "visit-10h", "", "pageview", "/", at(10, 0)),
		fe("a-2", sess, "visit-10h", siteSalt, "signup", "", at(10, 5)),
		fe("a-3", sess, "visit-11h", siteSalt, "purchase", "", at(11, 30)),
		fe("a-4", sess, "visit-11h", "", "pageview", "/", at(11, 45)),
	}
	pv := FunnelStep{Type: "page", Value: "/"}

	est := groupFunnelByEntity(rows, EntityVisitorEstimate)
	if len(est) != 1 || len(est[sess]) != 4 {
		t.Fatalf("visitor-estimate groups = %+v, want 1 entity / 4 events", entityGroupSizes(est))
	}
	vis := groupFunnelByEntity(rows, EntityVisit)
	if len(vis) != 2 || len(vis["visit-10h"]) != 2 || len(vis["visit-11h"]) != 2 {
		t.Fatalf("visit groups = %+v, want 2 entities / 2 events each", entityGroupSizes(vis))
	}
	per := groupFunnelByEntity(rows, EntityPerson)
	if len(per) != 1 || len(per[siteSalt]) != 2 {
		t.Fatalf("person groups = %+v, want 1 entity / 2 identified events", entityGroupSizes(per))
	}

	// The hand-computed funnel tables from the oracle:
	type wantRow struct {
		mode   string
		steps  []FunnelStep
		counts [2]int
	}
	for _, w := range []wantRow{
		{EntityVisitorEstimate, []FunnelStep{pv, step("purchase")}, [2]int{1, 1}},
		{EntityVisit, []FunnelStep{pv, step("purchase")}, [2]int{2, 0}},
		{EntityPerson, []FunnelStep{pv, step("purchase")}, [2]int{0, 0}},
		{EntityVisitorEstimate, []FunnelStep{step("signup"), step("purchase")}, [2]int{1, 1}},
		{EntityVisit, []FunnelStep{step("signup"), step("purchase")}, [2]int{1, 0}},
		{EntityPerson, []FunnelStep{step("signup"), step("purchase")}, [2]int{1, 1}},
	} {
		g := groupFunnelByEntity(rows, w.mode)
		var counts [2]int
		for _, evs := range g {
			c := walkFunnelEvents(evs, w.steps, 0, nil)
			for i := 0; i < 2; i++ {
				if c[i] > 0 {
					counts[i]++
				}
			}
		}
		if counts != w.counts {
			t.Errorf("scenario A funnel %v via %s: counts = %v, want %v", stepNames(w.steps), w.mode, counts, w.counts)
		}
	}
}

// Scenario C twin (cross-device): person mode converts across what
// session/visit modes split.
func TestGroupFunnelByEntity_ScenarioC_CrossDevice(t *testing.T) {
	rows := []funnelEvent{
		fe("c-1", "sess-laptop", "visit-laptop", "user-1", "signup", "", time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)),
		fe("c-2", "sess-phone", "visit-phone", "user-1", "purchase", "", time.Date(2026, 7, 11, 20, 0, 0, 0, time.UTC)),
	}
	steps := []FunnelStep{step("signup"), step("purchase")}
	for _, w := range []struct {
		mode   string
		counts [2]int
	}{
		{EntityVisitorEstimate, [2]int{1, 0}},
		{EntityVisit, [2]int{1, 0}},
		{EntityPerson, [2]int{1, 1}},
	} {
		var counts [2]int
		for _, evs := range groupFunnelByEntity(rows, w.mode) {
			c := walkFunnelEvents(evs, steps, 0, nil)
			for i := 0; i < 2; i++ {
				if c[i] > 0 {
					counts[i]++
				}
			}
		}
		if counts != w.counts {
			t.Errorf("scenario C funnel via %s: counts = %v, want %v", w.mode, counts, w.counts)
		}
	}
}

// Retention grid math (storage-free twin of the oracle's retention
// tables): complete-column truncation, the incomplete flag, period-0
// definition, and the epoch-aligned bucket keys.
func TestBuildRetentionCohorts_IncompletePeriods(t *testing.T) {
	day := int64(86400000)
	from := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)

	// Person-mode scenario C: one entity, first seen Jul 10 09:00,
	// active Jul 10 and Jul 11.
	person := &retentionEntity{
		firstTS: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC).UnixMilli(),
		buckets: map[int64]bool{},
	}
	for _, ts := range []time.Time{
		time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 20, 0, 0, 0, time.UTC),
	} {
		person.buckets[(ts.UnixMilli()/day)*day] = true
	}

	// Both days complete: [100, 100].
	to := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	got := buildRetentionCohorts(map[string]*retentionEntity{"p": person}, from, to, day, EntityPerson)
	if len(got) != 1 || got[0].CohortSize != 1 || len(got[0].Periods) != 2 ||
		got[0].Periods[0] != 100 || got[0].Periods[1] != 100 || got[0].IncompletePeriods != 0 {
		t.Fatalf("complete grid: got %+v, want size 1 periods [100 100] incomplete 0", got)
	}

	// Range ends mid-Jul-11: the Jul 11 column is incomplete — excluded,
	// flagged. The Jul 10 cohort row keeps only period 0.
	to = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	got = buildRetentionCohorts(map[string]*retentionEntity{"p": person}, from, to, day, EntityPerson)
	if len(got) != 1 || len(got[0].Periods) != 1 || got[0].Periods[0] != 100 || got[0].IncompletePeriods != 1 {
		t.Fatalf("incomplete grid: got %+v, want periods [100] incomplete 1", got)
	}

	// An entity first seen inside the incomplete current period: cohort
	// row exists (size counted) but reports no periods.
	late := &retentionEntity{
		firstTS: time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC).UnixMilli(),
		buckets: map[int64]bool{(time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC).UnixMilli() / day) * day: true},
	}
	got = buildRetentionCohorts(map[string]*retentionEntity{"p": person, "q": late}, from, to, day, EntityPerson)
	if len(got) != 2 {
		t.Fatalf("cohorts = %d, want 2", len(got))
	}
	if got[1].CohortDate != "2026-07-11" || got[1].CohortSize != 1 || len(got[1].Periods) != 0 || got[1].IncompletePeriods != 1 {
		t.Fatalf("current-period cohort: got %+v, want no periods, incomplete 1 (the current period), size counted", got[1])
	}

	// Retention never counts a return before cohort entry: period 0 is
	// the cohort's own bucket (100 by construction).
	if got[0].Periods[0] != 100 {
		t.Fatalf("period 0 = %v, want 100 by construction", got[0].Periods[0])
	}
}

// Period bucket keys are absolute UTC-epoch multiples of the period
// length. For 7-day periods that means buckets start on Thursday
// (1970-01-01); a Friday event cohorts on the Thursday before it. Pinned
// so nobody mistakes the keys for calendar-Monday weeks.
func TestBuildRetentionCohorts_WeekBucketsAlignToEpochThursday(t *testing.T) {
	week := int64(7 * 86400000)
	friday := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) // a Friday
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	e := &retentionEntity{firstTS: friday.UnixMilli(), buckets: map[int64]bool{(friday.UnixMilli() / week) * week: true}}
	got := buildRetentionCohorts(map[string]*retentionEntity{"e": e}, from, to, week, EntityVisitorEstimate)
	if len(got) != 1 || got[0].CohortDate != "2026-07-09" {
		t.Fatalf("week bucket = %+v, want cohort date 2026-07-09 (Thursday)", got)
	}
}

func entityGroupSizes(g map[string][]funnelEvent) map[string]int {
	out := make(map[string]int, len(g))
	for k, v := range g {
		out[k] = len(v)
	}
	return out
}

func stepNames(steps []FunnelStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = fmt.Sprintf("%s:%s", s.Type, s.Value)
	}
	return out
}
