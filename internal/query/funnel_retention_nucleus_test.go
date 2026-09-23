package query

// O04 Nucleus-gated integration: the executable binding of the O03
// scenario oracle (internal/session/reference_test.go, O04 tables) to
// the real Funnel/Retention SQL paths. Expected counts are HAND-WRITTEN
// here and in the oracle — never computed by calling the implementation
// twice. Entity ids are derived through the REAL production functions
// (session.ID, session.VisitID, identity.HashDistinctID) exactly as the
// ingest path would derive them, so the seeds are era-1-honest.
//
// Self-skips without a live Nucleus (nucleustest.DSN), same as every
// other DB-backed suite in this package.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/session"
)

type o04Seed struct {
	db   *nucleus.Client
	site string
}

func o04Connect(t *testing.T) (*o04Seed, context.Context) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	t.Cleanup(func() { db.Close() })
	return &o04Seed{db: db, site: fmt.Sprintf("test_o04_%d", time.Now().UnixNano())}, ctx
}

// event seeds one analytics row with production-derived era-1 ids.
func (s *o04Seed) event(t *testing.T, ctx context.Context, ip, ua, salt, siteSalt, rawID, eventID, eventType, pathname string, stored time.Time) (sessionID, visitID, distinctID string) {
	t.Helper()
	sessionID = session.ID(s.site, ip, ua, salt)
	visitID = session.VisitID(sessionID, stored)
	distinctID = identity.HashDistinctID(rawID, siteSalt)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id,
			event_type, timestamp, pathname, distinct_id, properties)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, 'null')`,
		eventID, s.site, sessionID, visitID, eventType, stored.UnixMilli(), pathname, distinctID,
	)
	if err != nil {
		t.Fatalf("seed event %s: %v", eventID, err)
	}
	return
}

// sessionRow seeds the sessions rollup (cohort entry source for the
// default visitor-estimate retention path).
func (s *o04Seed) sessionRow(t *testing.T, ctx context.Context, sessionID string, firstTS time.Time) {
	t.Helper()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO sessions (tenant_id, site_id, session_id, first_ts, last_ts, version)
		 VALUES ('default', $1, $2, $3, $3, 1)`,
		s.site, sessionID, firstTS.UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed session %s: %v", sessionID, err)
	}
}

func o04Counts(t *testing.T, svc *StatsService, ctx context.Context, site string, from, to time.Time, entity string, steps []FunnelStep, opts FunnelOptions) []int {
	t.Helper()
	opts.Entity = entity
	res, err := svc.FunnelWithOptions(ctx, site, from, to, steps, opts)
	if err != nil {
		t.Fatalf("funnel (%s): %v", entity, err)
	}
	out := make([]int, len(res))
	for i, r := range res {
		out[i] = r.Visitors
		if r.Entity != entityLabel(entity) {
			t.Fatalf("funnel row %d entity label = %q, want %q", i, r.Entity, entityLabel(entity))
		}
		if r.EntityLimitation == "" {
			t.Fatalf("funnel row %d: entity_limitation must be present (O03 D2)", i)
		}
	}
	return out
}

// Scenario a/b/c entity tables, end to end. The literals mirror
// TestReference_O04_* in internal/session — one source of truth for the
// numbers, asserted twice.
func TestO04_FunnelEntityModes_Scenarios(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)

	const (
		salt     = "global-salt-o04"
		siteSalt = "site-salt-o04"
	)
	july := func(d, h, m int) time.Time { return time.Date(2026, 7, d, h, m, 0, 0, time.UTC) }
	pv := FunnelStep{Type: "page", Value: "/"}
	signup, purchase := step("signup"), step("purchase")

	// Scenario A: one browser, anon -> login -> logout.
	siteA := s.site + "_a"
	sA := &o04Seed{db: s.db, site: siteA}
	sA.event(t, ctx, "203.0.113.7", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36", salt, siteSalt, "", "a-1", "pageview", "/", july(10, 10, 0))
	sA.event(t, ctx, "203.0.113.7", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36", salt, siteSalt, "user-1", "a-2", "signup", "", july(10, 10, 5))
	sA.event(t, ctx, "203.0.113.7", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36", salt, siteSalt, "user-1", "a-3", "purchase", "", july(10, 11, 30))
	sA.event(t, ctx, "203.0.113.7", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36", salt, siteSalt, "", "a-4", "pageview", "/", july(10, 11, 45))

	from, to := july(10, 0, 0), july(11, 0, 0)
	for _, w := range []struct {
		entity string
		steps  []FunnelStep
		want   []int
	}{
		{EntityVisitorEstimate, []FunnelStep{pv, purchase}, []int{1, 1}},
		{EntityVisit, []FunnelStep{pv, purchase}, []int{2, 0}},
		{EntityPerson, []FunnelStep{pv, purchase}, []int{0, 0}},
		{EntityVisitorEstimate, []FunnelStep{signup, purchase}, []int{1, 1}},
		{EntityVisit, []FunnelStep{signup, purchase}, []int{1, 0}},
		{EntityPerson, []FunnelStep{signup, purchase}, []int{1, 1}},
		{"", []FunnelStep{pv, purchase}, []int{1, 1}}, // empty = default = visitor-estimate
	} {
		got := o04Counts(t, svc, ctx, siteA, from, to, w.entity, w.steps, FunnelOptions{})
		for i := range got {
			if got[i] != w.want[i] {
				t.Errorf("scenario A funnel %v entity=%q: counts = %v, want %v", stepNames(w.steps), w.entity, got, w.want)
				break
			}
		}
	}

	// Scenario B: NAT pair, distinct UAs (chrome converts, firefox does not).
	siteB := s.site + "_b"
	sB := &o04Seed{db: s.db, site: siteB}
	chrome := "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/121 Safari/537.36"
	firefox := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"
	sB.event(t, ctx, "198.51.100.25", chrome, salt, siteSalt, "", "b-1", "pageview", "/", july(10, 10, 0))
	sB.event(t, ctx, "198.51.100.25", chrome, salt, siteSalt, "", "b-2", "purchase", "", july(10, 10, 10))
	sB.event(t, ctx, "198.51.100.25", firefox, salt, siteSalt, "", "b-3", "pageview", "/", july(10, 10, 2))
	for _, w := range []struct {
		entity string
		want   []int
	}{
		{EntityVisitorEstimate, []int{2, 1}},
		{EntityVisit, []int{2, 1}},
		{EntityPerson, []int{0, 0}},
	} {
		got := o04Counts(t, svc, ctx, siteB, from, to, w.entity, []FunnelStep{pv, purchase}, FunnelOptions{})
		for i := range got {
			if got[i] != w.want[i] {
				t.Errorf("scenario B funnel entity=%q: counts = %v, want %v", w.entity, got, w.want)
				break
			}
		}
	}

	// Scenario C: one person, two devices — the cross-device conversion
	// exists ONLY in person mode.
	siteC := s.site + "_c"
	sC := &o04Seed{db: s.db, site: siteC}
	laptopSess, _, _ := sC.event(t, ctx, "203.0.113.9", "Mozilla/5.0 (Macintosh) Chrome/120", salt, siteSalt, "user-1", "c-1", "signup", "", july(10, 9, 0))
	phoneSess, _, _ := sC.event(t, ctx, "198.51.100.88", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Safari/604.1", salt, siteSalt, "user-1", "c-2", "purchase", "", july(11, 20, 0))
	for _, w := range []struct {
		entity string
		want   []int
	}{
		{EntityVisitorEstimate, []int{1, 0}},
		{EntityVisit, []int{1, 0}},
		{EntityPerson, []int{1, 1}},
	} {
		got := o04Counts(t, svc, ctx, siteC, july(10, 0, 0), july(12, 0, 0), w.entity, []FunnelStep{signup, purchase}, FunnelOptions{})
		for i := range got {
			if got[i] != w.want[i] {
				t.Errorf("scenario C funnel entity=%q: counts = %v, want %v", w.entity, got, w.want)
				break
			}
		}
	}

	// Retention, scenario C shape, per entity mode. The sessions rollup
	// is seeded for the two estimates (the default path's cohort source).
	sC.sessionRow(t, ctx, laptopSess, july(10, 9, 0))
	sC.sessionRow(t, ctx, phoneSess, july(11, 20, 0))
	for _, w := range []struct {
		entity     string
		wantDate   []string
		wantSize   []int
		wantPeriod [][]float64
		wantIncomp []int
	}{
		{
			EntityVisitorEstimate,
			[]string{"2026-07-10", "2026-07-11"},
			[]int{1, 1},
			[][]float64{{100, 0}, {100}},
			[]int{0, 0},
		},
		{
			EntityVisit,
			[]string{"2026-07-10", "2026-07-11"},
			[]int{1, 1},
			[][]float64{{100, 0}, {100}},
			[]int{0, 0},
		},
		{
			EntityPerson,
			[]string{"2026-07-10"},
			[]int{1},
			[][]float64{{100, 100}},
			[]int{0},
		},
	} {
		cohorts, err := svc.RetentionWithOptions(ctx, siteC, july(10, 0, 0), july(12, 0, 0), 1, RetentionOptions{Entity: w.entity})
		if err != nil {
			t.Fatalf("retention (%s): %v", w.entity, err)
		}
		if len(cohorts) != len(w.wantDate) {
			t.Fatalf("retention (%s): %d cohorts, want %d", w.entity, len(cohorts), len(w.wantDate))
		}
		for i, c := range cohorts {
			if c.CohortDate != w.wantDate[i] || c.CohortSize != w.wantSize[i] || c.IncompletePeriods != w.wantIncomp[i] || len(c.Periods) != len(w.wantPeriod[i]) {
				t.Fatalf("retention (%s) cohort %d = %+v, want date %s size %d periods %v incomplete %d",
					w.entity, i, c, w.wantDate[i], w.wantSize[i], w.wantPeriod[i], w.wantIncomp[i])
			}
			for p, v := range c.Periods {
				if v != w.wantPeriod[i][p] {
					t.Fatalf("retention (%s) cohort %d period %d = %v, want %v", w.entity, i, p, v, w.wantPeriod[i][p])
				}
			}
			if c.Entity != entityLabel(w.entity) || c.EntityLimitation == "" {
				t.Fatalf("retention (%s) cohort %d: entity label missing", w.entity, i)
			}
		}
	}
}

// Window edges, exclusions, and range edges end to end through the SQL
// fetch. Window boundary: ts == e0 + W is IN, +1ms OUT. Range: an event
// exactly at `from` is IN, exactly at `to` is OUT.
func TestO04_FunnelWindowExclusionRangeEdges(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)
	site := s.site + "_wx"

	const salt = "salt-wx"
	base := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	edgeIn := &o04Seed{db: s.db, site: site + "in"}
	edgeOut := &o04Seed{db: s.db, site: site + "out"}
	edgeIn.event(t, ctx, "203.0.113.31", "UA-wx", salt, "", "", "w-1", "signup", "", base)
	edgeIn.event(t, ctx, "203.0.113.31", "UA-wx", salt, "", "", "w-2", "purchase", "", base.Add(10*time.Minute))
	edgeOut.event(t, ctx, "203.0.113.32", "UA-wx", salt, "", "", "w-3", "signup", "", base)
	edgeOut.event(t, ctx, "203.0.113.32", "UA-wx", salt, "", "", "w-4", "purchase", "", base.Add(10*time.Minute+time.Millisecond))

	from, to := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	w := (10 * time.Minute).Milliseconds()
	if got := o04Counts(t, svc, ctx, edgeIn.site, from, to, "", []FunnelStep{step("signup"), step("purchase")}, FunnelOptions{ConversionWindowMs: w}); got[1] != 1 {
		t.Fatalf("window edge exactly at W: step-2 = %d, want 1 (inclusive)", got[1])
	}
	if got := o04Counts(t, svc, ctx, edgeOut.site, from, to, "", []FunnelStep{step("signup"), step("purchase")}, FunnelOptions{ConversionWindowMs: w}); got[1] != 0 {
		t.Fatalf("window edge W+1ms: step-2 = %d, want 0", got[1])
	}

	// Exclusion barrier after entry.
	exSite := &o04Seed{db: s.db, site: site + "ex"}
	exSite.event(t, ctx, "203.0.113.33", "UA-wx", salt, "", "", "x-1", "signup", "", base)
	exSite.event(t, ctx, "203.0.113.33", "UA-wx", salt, "", "", "x-2", "refund", "", base.Add(5*time.Minute))
	exSite.event(t, ctx, "203.0.113.33", "UA-wx", salt, "", "", "x-3", "purchase", "", base.Add(8*time.Minute))
	if got := o04Counts(t, svc, ctx, exSite.site, from, to, "", []FunnelStep{step("signup"), step("purchase")}, FunnelOptions{Exclusions: []FunnelStep{step("refund")}}); got[0] != 1 || got[1] != 0 {
		t.Fatalf("exclusion barrier: counts = %v, want [1 0]", got)
	}

	// Range edges: exactly-at-from in, exactly-at-to out.
	rSite := &o04Seed{db: s.db, site: site + "rg"}
	rSite.event(t, ctx, "203.0.113.34", "UA-wx", salt, "", "", "r-1", "signup", "", from)
	rSite.event(t, ctx, "203.0.113.34", "UA-wx", salt, "", "", "r-2", "purchase", "", to)
	if got := o04Counts(t, svc, ctx, rSite.site, from, to, "", []FunnelStep{step("signup"), step("purchase")}, FunnelOptions{}); got[0] != 1 || got[1] != 0 {
		t.Fatalf("range edges: counts = %v, want [1 0] (at-from IN, at-to OUT)", got)
	}
}

// Late-arrival truth (pinned): the funnel orders by the STORED
// timestamp, which era-1 sets to ingestion time. A late-delivered event
// re-sequences to its ingestion position, so the funnel [B, A] below
// does NOT convert even though B truly happened before A. Under event
// time it would convert — that divergence is the O03 implementation
// dependency (wire event-time, ADR D8), recorded in AUDIT_OPEN.md.
func TestO04_FunnelLateArrivalOrdersByStoredTimestamp(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)

	// Same fingerprint, so every mode groups them as one entity.
	s.event(t, ctx, "203.0.113.62", "Mozilla/5.0 Chrome/121", "salt-late", "", "", "l-1", "A", "", time.Date(2026, 7, 10, 9, 59, 0, 0, time.UTC))
	s.event(t, ctx, "203.0.113.62", "Mozilla/5.0 Chrome/121", "salt-late", "", "", "l-2", "B", "", time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC))

	from := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	for _, entity := range []string{EntityVisitorEstimate, EntityVisit} {
		if got := o04Counts(t, svc, ctx, s.site, from, to, entity, []FunnelStep{step("B"), step("A")}, FunnelOptions{}); got[0] != 1 || got[1] != 0 {
			t.Fatalf("late arrival (%s): counts = %v, want [1 0] on stored/ingestion order", entity, got)
		}
	}
}

// Retention knobs end to end: cohort entry event + return event, and
// the incomplete-current-period exclusion + flag. Person mode, one
// entity: signup Jul 10 (cohort entry), purchase Jul 10, purchase
// Jul 11 09:00; range ends Jul 11 12:00 so the Jul 11 column is
// incomplete. With return_event=purchase the day-2 return is attributed
// to the signup cohort: [100] + incomplete 1 (the Jul 11 column exists
// but has not elapsed). Without the knobs the same shape comes from
// first-activity cohorts.
func TestO04_RetentionEntryReturnEventsAndIncompleteFlag(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)
	seed := &o04Seed{db: s.db, site: s.site + "_er"}

	const (
		ip       = "203.0.113.71"
		ua       = "UA-er"
		salt     = "salt-er"
		siteSalt = "site-salt-er"
	)
	july := func(d, h, m int) time.Time { return time.Date(2026, 7, d, h, m, 0, 0, time.UTC) }
	site := seed.site
	seed.event(t, ctx, ip, ua, salt, siteSalt, "user-9", "er-1", "signup", "", july(10, 9, 0))
	seed.event(t, ctx, ip, ua, salt, siteSalt, "user-9", "er-2", "purchase", "", july(10, 10, 0))
	seed.event(t, ctx, ip, ua, salt, siteSalt, "user-9", "er-3", "purchase", "", july(11, 9, 0))
	// An anonymous entity that only ever browses: cohort member under
	// first-activity cohorts, NOT a member when cohort_event=signup.
	seed.event(t, ctx, ip, ua+"-anon", salt, siteSalt, "", "er-4", "pageview", "/", july(10, 12, 0))

	from, to := july(10, 0, 0), july(11, 12, 0)

	// Person mode + entry/return events: one cohort (the identified
	// person), one complete column, one incomplete.
	cohorts, err := svc.RetentionWithOptions(ctx, site, from, to, 1, RetentionOptions{
		Entity:      EntityPerson,
		CohortEvent: "signup",
		ReturnEvent: "purchase",
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if len(cohorts) != 1 || cohorts[0].CohortDate != "2026-07-10" || cohorts[0].CohortSize != 1 {
		t.Fatalf("person + signup cohort: %+v, want one Jul-10 cohort of size 1", cohorts)
	}
	if len(cohorts[0].Periods) != 1 || cohorts[0].Periods[0] != 100 || cohorts[0].IncompletePeriods != 1 {
		t.Fatalf("person + signup cohort grid: %+v, want [100] incomplete 1", cohorts[0])
	}

	// Visitor-estimate, first-activity cohorts (default path via the
	// sessions rollup — seeded below): two estimates (different UAs),
	// both first seen Jul 10, so ONE Jul-10 cohort of size 2. Both were
	// active Jul 10 (p0 = 100); the Jul 11 column is incomplete at
	// to = Jul 11 12:00, so it is excluded and flagged — the identified
	// estimate's morning return surfaces only once the day completes.
	estSess := session.ID(site, ip, ua, "salt-er")
	estSessAnon := session.ID(site, ip, ua+"-anon", "salt-er")
	seed.sessionRow(t, ctx, estSess, july(10, 9, 0))
	seed.sessionRow(t, ctx, estSessAnon, july(10, 12, 0))
	est, err := svc.RetentionWithOptions(ctx, site, from, to, 1, RetentionOptions{})
	if err != nil {
		t.Fatalf("retention (estimate): %v", err)
	}
	if len(est) != 1 || est[0].CohortDate != "2026-07-10" || est[0].CohortSize != 2 {
		t.Fatalf("estimate cohorts: %+v, want one Jul-10 cohort of size 2 (two estimates)", est)
	}
	if len(est[0].Periods) != 1 || est[0].Periods[0] != 100 || est[0].IncompletePeriods != 1 {
		t.Fatalf("estimate grid: %+v, want [100] incomplete 1 (Jul 11 column not yet elapsed)", est[0])
	}
}

// Unknown entity values are rejected with a 400-shaped error, never
// silently degraded to the default.
func TestO04_UnknownEntityRejected(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)
	from := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	if _, err := svc.FunnelWithOptions(ctx, s.site, from, to, []FunnelStep{step("a")}, FunnelOptions{Entity: "user"}); err == nil {
		t.Fatal("funnel: unknown entity must be rejected")
	}
	if _, err := svc.RetentionWithOptions(ctx, s.site, from, to, 1, RetentionOptions{Entity: "session"}); err == nil {
		t.Fatal("retention: unknown entity must be rejected")
	}
}

// Default-path identity: the pre-O04 entry points produce exactly the
// options-based zero-value call, including the honest default label.
func TestO04_DefaultsUnchanged(t *testing.T) {
	s, ctx := o04Connect(t)
	svc := NewStatsService(s.db)
	from := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	steps := []FunnelStep{step("signup")}

	a, err := svc.Funnel(ctx, s.site, from, to, steps)
	if err != nil {
		t.Fatalf("Funnel: %v", err)
	}
	b, err := svc.FunnelWithOptions(ctx, s.site, from, to, steps, FunnelOptions{})
	if err != nil {
		t.Fatalf("FunnelWithOptions: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("default drift: %d vs %d results", len(a), len(b))
	}
	for i := range a {
		if a[i].Visitors != b[i].Visitors || a[i].Conversion != b[i].Conversion || a[i].DropOff != b[i].DropOff {
			t.Fatalf("default drift at step %d: %+v vs %+v", i, a[i], b[i])
		}
		if b[i].Entity != EntityVisitorEstimate {
			t.Fatalf("default entity label = %q, want %q", b[i].Entity, EntityVisitorEstimate)
		}
	}
	ra, err := svc.Retention(ctx, s.site, from, to, 1)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	rb, err := svc.RetentionWithOptions(ctx, s.site, from, to, 1, RetentionOptions{})
	if err != nil {
		t.Fatalf("RetentionWithOptions: %v", err)
	}
	if (ra == nil) != (rb == nil) || len(ra) != len(rb) {
		t.Fatalf("retention default drift: %d vs %d cohorts", len(ra), len(rb))
	}
}
