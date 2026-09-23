package session

// O03 reference oracle — era-1 identity derivation, pinned.
//
// Programme workstream O03 ("One explicit identity and session model")
// requires expected answers locked BEFORE any identity code changes. This
// file is that lock. Each scenario below encodes the CURRENT (era-1)
// behavior of the real derivation functions as executable tables:
//
//   (a) anonymous -> login -> logout under one browser
//   (b) two people behind one NAT (same IP, different UA)
//   (c) one person, two devices (same distinct_id, different fingerprints)
//   (d) month boundary (the documented anonymous-estimate limitation —
//       pinned as behavior, NOT endorsed as person identity)
//   (e) delayed delivery (old event timestamp, late ingestion)
//
// The companion decision record is docs/IDENTITY_MODEL_ADR.md.
//
// ERA RULE: a future, deliberate behavior change (O04 and later) MUST
// update these tables in the same change, with the new era documented in
// the ADR. These tables are never updated "as a fix" to make a new
// implementation pass — a mismatch means either the implementation is
// wrong or the era is being bumped; both are conscious acts.
//
// What era-1 is, precisely:
//
//   session.ID(site, ip, ua, salt)     = uuid(sha256(site+ip+ua+salt+YYYY-MM))
//                                        where YYYY-MM is the UTC month of
//                                        the SERVER CLOCK at processing
//                                        time (ingestion, not event time).
//   VisitID(sessionID, ts)             = uuid(sha256(sessionID:hourBucket))
//                                        where hourBucket is ts truncated
//                                        to an absolute (UTC-epoch) hour.
//                                        The ingest path passes ingestion
//                                        time (internal/ingest/handler.go,
//                                        prepareEvent), never a client
//                                        timestamp — the wire protocol has
//                                        no event-time field at all.
//   distinct_id (persons entity)       = HMAC-SHA256(raw, per-site salt)
//                                        truncated to 16 hex chars
//                                        (internal/identity), independent
//                                        of the global session salt on
//                                        site-backed installs.
//
// Surface mapping pinned by these tables (internal/query):
//   "visitors"  = COUNT(DISTINCT session_id)   (the monthly estimate)
//   "sessions"  = COUNT(DISTINCT visit_id)     (the clock-hour bucket)
//   "persons"   = grouping on distinct_id
//   funnels     group on session_id
//   retention   cohorts on session_id

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/identity"
)

// ----------------------------------------------------------------------------
// Era-1 derivation shims
// ----------------------------------------------------------------------------

// monthKeyedID is the era-1 session derivation with the month key as an
// EXPLICIT input. Production ID() reads the month from the wall clock
// inside the function, so cross-month rows cannot be driven through the
// real function in a test; TestReference_MonthKeyIsTheOnlyClockInput
// below proves production ID() output equals monthKeyedID at the current
// month, which makes monthKeyedID a faithful oracle for other months.
func monthKeyedID(siteID, ip, userAgent, salt, monthKey string) string {
	input := siteID + ip + userAgent + salt + monthKey
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

// currentMonthKey returns the UTC month key the production clock would
// produce right now.
func currentMonthKey() string {
	return time.Now().UTC().Format("2006-01")
}

// distinctIDOf applies the era-1 persons derivation: HMAC of the raw
// identify() value under the per-site salt (empty raw stays empty — an
// anonymous event never carries a person id).
func distinctIDOf(raw, siteSalt string) string {
	return identity.HashDistinctID(raw, siteSalt)
}

// scenarioRow is one event as the derivation sees it. ts is the EVENT
// time the producer experienced; ingestedAt is when the server processed
// it — era-1 uses ONLY ingestedAt (see scenario (e)).
type scenarioRow struct {
	name        string
	site        string
	ip          string
	ua          string
	ts          time.Time // event time as experienced client-side
	ingestedAt  time.Time // server processing time (era-1's only clock)
	salt        string    // global OBSERVE_SESSION_SALT
	rawIdentity string    // raw identify() value, "" for anonymous
	siteSalt    string    // per-site salt for the distinct_id HMAC

	// computed by runScenario
	sessionID  string
	visitID    string
	storedAt   time.Time // the timestamp era-1 actually stores
	distinctID string
}

// surfaceCounts is the query-visible outcome of a scenario: what each
// surface reports for the rows above.
type surfaceCounts struct {
	visitors int // COUNT(DISTINCT session_id) — stats/boards/coverage
	sessions int // COUNT(DISTINCT visit_id)   — stats/boards
	persons  int // COUNT(DISTINCT distinct_id), excluding "" — persons
}

// runScenario derives era-1 IDs for every row exactly as the ingest path
// does: session.ID with the global salt (month = server clock, pinned
// here to the current month — runScenario must not straddle a UTC month
// boundary; TestReference_MonthKeyIsTheOnlyClockInput guards the
// equivalence), VisitID keyed on INGESTION time, stored timestamp =
// ingestion time, distinct_id = per-site-salt HMAC.
func runScenario(t *testing.T, rows []*scenarioRow) {
	t.Helper()
	month := currentMonthKey()
	for _, r := range rows {
		r.sessionID = monthKeyedID(r.site, r.ip, r.ua, r.salt, month)
		r.visitID = VisitID(r.sessionID, r.ingestedAt)
		r.storedAt = r.ingestedAt
		r.distinctID = distinctIDOf(r.rawIdentity, r.siteSalt)
		if month != currentMonthKey() {
			t.Fatalf("UTC month rolled mid-scenario (%s -> %s); rerun the test", month, currentMonthKey())
		}
	}
}

func countDistinctIDs(rows []*scenarioRow, pick func(*scenarioRow) string, skipEmpty bool) int {
	seen := map[string]bool{}
	for _, r := range rows {
		v := pick(r)
		if v == "" && skipEmpty {
			continue
		}
		seen[v] = true
	}
	return len(seen)
}

func surfaceCountsOf(rows []*scenarioRow) surfaceCounts {
	return surfaceCounts{
		visitors: countDistinctIDs(rows, func(r *scenarioRow) string { return r.sessionID }, false),
		sessions: countDistinctIDs(rows, func(r *scenarioRow) string { return r.visitID }, false),
		persons:  countDistinctIDs(rows, func(r *scenarioRow) string { return r.distinctID }, true),
	}
}

// ----------------------------------------------------------------------------
// Clock-input equivalence proof (lets monthKeyedID stand in for ID())
// ----------------------------------------------------------------------------

func TestReference_MonthKeyIsTheOnlyClockInput(t *testing.T) {
	// If production ID() equals monthKeyedID at the current month, then
	// monthKeyedID at other months is exactly what production would
	// derive in those months. A mismatch here means the derivation
	// changed: bump the era, update this file and the ADR consciously.
	for attempt := 0; attempt < 3; attempt++ {
		before := currentMonthKey()
		got := ID("site-o03", "203.0.113.7", "Mozilla/5.0 Chrome/120", "salt-o03")
		want := monthKeyedID("site-o03", "203.0.113.7", "Mozilla/5.0 Chrome/120", "salt-o03", before)
		if got == want {
			return
		}
		if currentMonthKey() != before {
			continue // UTC month rolled between the two calls; retry
		}
		t.Fatalf("era-1 drift: ID()=%s monthKeyedID()=%s — derivation changed, bump the era", got, want)
	}
	t.Fatal("could not compare ID() against monthKeyedID without a UTC month rollover; rerun the test")
}

// Golden literal: era-1 VisitID format is pinned byte-for-byte so a
// formatting change cannot slip under equality-only assertions.
func TestReference_VisitIDGoldenLiteral(t *testing.T) {
	ts := time.Date(2026, 7, 10, 9, 10, 0, 0, time.UTC)
	if got := VisitID("deadbeef-0000-0000-0000-000000000000", ts); got != "7104076e-b717-d844-061c-89a1271e6490" {
		t.Fatalf("VisitID golden literal drifted: %s", got)
	}
}

// ----------------------------------------------------------------------------
// Scenario (a): anonymous -> login -> logout under one browser
// ----------------------------------------------------------------------------

// Era-1 answer: login and logout change NOTHING about the session or
// visit derivation. The fingerprint (site/IP/UA/salt/month) is identical
// across all four phases, so the visitor estimate and visit bucketing
// are unaffected; the person entity (distinct_id) exists only on the two
// identified events. The pre-login and post-logout events belong to no
// person — era-1 has no anonymous-to-person stitching.
func TestReference_ScenarioA_AnonymousLoginLogout(t *testing.T) {
	const (
		site     = "site-a"
		ip       = "203.0.113.7"
		ua       = "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36"
		salt     = "global-salt-a"
		siteSalt = "site-salt-a"
	)
	rows := []*scenarioRow{
		{name: "anon pageview 10:00", site: site, ip: ip, ua: ua, salt: salt, siteSalt: siteSalt,
			ts: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)},
		{name: "login + $identify 10:05", site: site, ip: ip, ua: ua, salt: salt, siteSalt: siteSalt, rawIdentity: "user-1",
			ts: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC)},
		{name: "identified action 11:30", site: site, ip: ip, ua: ua, salt: salt, siteSalt: siteSalt, rawIdentity: "user-1",
			ts: time.Date(2026, 7, 10, 11, 30, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 11, 30, 0, 0, time.UTC)},
		{name: "logout 11:45 (identify cleared)", site: site, ip: ip, ua: ua, salt: salt, siteSalt: siteSalt,
			ts: time.Date(2026, 7, 10, 11, 45, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 11, 45, 0, 0, time.UTC)},
	}
	runScenario(t, rows)

	// One session estimate for the whole arc.
	wantSession := rows[0].sessionID
	for _, r := range rows {
		if r.sessionID != wantSession {
			t.Errorf("%s: session estimate changed across login/logout: %s != %s", r.name, r.sessionID, wantSession)
		}
	}
	// Visits rotate on clock hours only: 10:00+10:05 share a visit,
	// 11:30+11:45 share a later one.
	if rows[0].visitID != rows[1].visitID {
		t.Error("10:00 and 10:05 must share the 10:00-hour visit")
	}
	if rows[2].visitID != rows[3].visitID {
		t.Error("11:30 and 11:45 must share the 11:00-hour visit")
	}
	if rows[0].visitID == rows[2].visitID {
		t.Error("10:00-hour and 11:00-hour must be different visits")
	}
	// Person surface: only the identified rows carry a distinct_id; the
	// anonymous bookends are invisible to Persons (excluded by the
	// non-empty filter in internal/persons).
	if rows[1].distinctID == "" || rows[2].distinctID == "" {
		t.Fatal("identified rows must carry distinct_id")
	}
	if rows[1].distinctID != rows[2].distinctID {
		t.Error("same raw identify value under the same site salt must hash to one person")
	}
	if rows[0].distinctID != "" || rows[3].distinctID != "" {
		t.Error("anonymous rows must carry no distinct_id")
	}
	if got := surfaceCountsOf(rows); got != (surfaceCounts{visitors: 1, sessions: 2, persons: 1}) {
		t.Errorf("surface counts = %+v, want visitors=1 sessions=2 persons=1", got)
	}
}

// ----------------------------------------------------------------------------
// Scenario (b): two people behind one NAT (same IP, different UA)
// ----------------------------------------------------------------------------

// Era-1 answer: the UA string is the ONLY differentiator, and it enters
// the fingerprint byte-exact (no normalization beyond HTTP header
// trimming — internal/ingest/middleware.go stores the raw header). Two
// different browsers behind one NAT are two visitor estimates. Two
// people using the SAME browser behind one NAT are ONE estimate — that
// is the documented failure mode, pinned by the second half of this
// table. UA case differences alone also split the estimate.
func TestReference_ScenarioB_TwoPeopleOneNAT(t *testing.T) {
	const (
		site = "site-b"
		ip   = "198.51.100.25" // the shared NAT egress
		salt = "global-salt-b"
	)
	chrome := "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/121 Safari/537.36"
	firefox := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"

	rows := []*scenarioRow{
		{name: "person A (Chrome) 10:00", site: site, ip: ip, ua: chrome, salt: salt,
			ts: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)},
		{name: "person B (Firefox) 10:02", site: site, ip: ip, ua: firefox, salt: salt,
			ts: time.Date(2026, 7, 10, 10, 2, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 2, 0, 0, time.UTC)},
	}
	runScenario(t, rows)
	if rows[0].sessionID == rows[1].sessionID {
		t.Error("different UA behind one NAT must yield two visitor estimates")
	}
	if got := surfaceCountsOf(rows); got != (surfaceCounts{visitors: 2, sessions: 2, persons: 0}) {
		t.Errorf("distinct-UA NAT surface counts = %+v, want visitors=2 sessions=2 persons=0", got)
	}

	// Same browser, same NAT: ONE estimate for two people. Pinned as the
	// documented limitation of the era-1 fingerprint, not as correct
	// person identity.
	same := []*scenarioRow{
		{name: "person A (Chrome) 10:00", site: site, ip: ip, ua: chrome, salt: salt,
			ts: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)},
		{name: "person C (same Chrome build) 10:05", site: site, ip: ip, ua: chrome, salt: salt,
			ts: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC)},
	}
	runScenario(t, same)
	if same[0].sessionID != same[1].sessionID {
		t.Error("identical fingerprints behind one NAT must share one estimate (era-1 limitation pin)")
	}
	if got := surfaceCountsOf(same); got != (surfaceCounts{visitors: 1, sessions: 1, persons: 0}) {
		t.Errorf("same-UA NAT surface counts = %+v, want visitors=1 sessions=1 persons=0 (merged)", got)
	}

	// Byte-exact UA sensitivity: a case-only difference splits the
	// estimate. The fingerprint does not normalize the UA; ParseUA
	// (browser/OS/device) feeds display columns only, never identity.
	rows[0].sessionID = monthKeyedID(site, ip, chrome, salt, currentMonthKey())
	if monthKeyedID(site, ip, chrome, salt, currentMonthKey()) == monthKeyedID(site, ip, "mozilla/5.0 (windows nt 10.0) applewebkit/537.36 chrome/121 safari/537.36", salt, currentMonthKey()) {
		t.Error("UA case difference must split the era-1 estimate (raw-header pin)")
	}
}

// ----------------------------------------------------------------------------
// Scenario (c): one person, two devices (same distinct_id, different
// fingerprints)
// ----------------------------------------------------------------------------

// Era-1 answer: identify() unifies the PERSON across devices (same raw
// value, same site salt -> one HMAC), but nothing else. Each device is
// its own visitor estimate and its own visits, so session-keyed surfaces
// (funnels, retention, stats) double-count this person; only the Persons
// surface shows one entity.
func TestReference_ScenarioC_OnePersonTwoDevices(t *testing.T) {
	const (
		site     = "site-c"
		salt     = "global-salt-c"
		siteSalt = "site-salt-c"
		raw      = "user-1"
	)
	rows := []*scenarioRow{
		{name: "laptop (home IP, Chrome) 09:00 identified", site: site, ip: "203.0.113.9", ua: "Mozilla/5.0 (Macintosh) Chrome/120", salt: salt, siteSalt: siteSalt, rawIdentity: raw,
			ts: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)},
		{name: "phone (mobile IP, Safari) 20:00 identified", site: site, ip: "198.51.100.88", ua: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Safari/604.1", salt: salt, siteSalt: siteSalt, rawIdentity: raw,
			ts: time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)},
	}
	runScenario(t, rows)

	if rows[0].sessionID == rows[1].sessionID {
		t.Error("different fingerprints (IP+UA) must yield two visitor estimates")
	}
	if rows[0].visitID == rows[1].visitID {
		t.Error("different sessions can never share a visit id")
	}
	if rows[0].distinctID == "" || rows[0].distinctID != rows[1].distinctID {
		t.Fatalf("same identify value under one site salt must be one person: %q vs %q", rows[0].distinctID, rows[1].distinctID)
	}
	// Funnel surface runs on session_id: this person contributes to TWO
	// funnel entities. Persons surface: one.
	if got := surfaceCountsOf(rows); got != (surfaceCounts{visitors: 2, sessions: 2, persons: 1}) {
		t.Errorf("two-device surface counts = %+v, want visitors=2 sessions=2 persons=1", got)
	}
}

// ----------------------------------------------------------------------------
// Scenario (d): month boundary (documented estimate limitation)
// ----------------------------------------------------------------------------

// Era-1 answer: the month key is part of the hash preimage, so the same
// visitor crossing a UTC month boundary becomes TWO visitor estimates.
// This is PINNED AS THE DOCUMENTED ANONYMOUS-ESTIMATE LIMITATION — it is
// exactly what era-1 does, and it is NOT correct person identity. Any
// window spanning the boundary double-counts this visitor on every
// session-keyed surface; retention attributes the post-boundary events
// to a "new" cohort entity.
//
// Cross-month rows run through monthKeyedID; TestReference_MonthKeyIsTheOnlyClockInput
// proves the equivalence to production ID().
func TestReference_ScenarioD_MonthBoundary(t *testing.T) {
	const (
		site = "site-d"
		ip   = "203.0.113.50"
		ua   = "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0"
		salt = "global-salt-d"
	)
	julyID := monthKeyedID(site, ip, ua, salt, "2026-07")
	augID := monthKeyedID(site, ip, ua, salt, "2026-08")
	if julyID == augID {
		t.Fatal("same fingerprint across UTC months must split into two estimates (era-1 pin)")
	}

	// Late-July visit vs early-August visit: everything differs.
	julyVisit := VisitID(julyID, time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC))
	augVisit := VisitID(augID, time.Date(2026, 8, 1, 0, 10, 0, 0, time.UTC))
	if julyVisit == augVisit {
		t.Fatal("cross-month visits must differ")
	}

	// Surface counts per window: July window sees one visitor; August
	// window sees one visitor; a window spanning both sees TWO — the
	// double-count pin.
	rows := []*scenarioRow{
		{name: "July 31 23:30", site: site, ip: ip, ua: ua, salt: salt,
			ts: time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC), ingestedAt: time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC)},
		{name: "Aug 1 00:10", site: site, ip: ip, ua: ua, salt: salt,
			ts: time.Date(2026, 8, 1, 0, 10, 0, 0, time.UTC), ingestedAt: time.Date(2026, 8, 1, 0, 10, 0, 0, time.UTC)},
	}
	// Month-keyed derivation (the scenario straddles months by design):
	rows[0].sessionID = julyID
	rows[1].sessionID = augID
	rows[0].visitID = julyVisit
	rows[1].visitID = augVisit
	rows[0].storedAt, rows[1].storedAt = rows[0].ingestedAt, rows[1].ingestedAt
	rows[0].distinctID, rows[1].distinctID = "", ""

	julyOnly := surfaceCountsOf(rows[:1])
	spanning := surfaceCountsOf(rows)
	if julyOnly.visitors != 1 {
		t.Errorf("July-only window visitors = %d, want 1", julyOnly.visitors)
	}
	if spanning.visitors != 2 {
		t.Errorf("boundary-spanning window visitors = %d, want 2 (documented double-count)", spanning.visitors)
	}
}

// ----------------------------------------------------------------------------
// Scenario (e): delayed delivery (old event timestamp, late ingestion)
// ----------------------------------------------------------------------------

// Era-1 answer: the wire protocol carries NO event timestamp
// (IngestInput has none; an unknown "timestamp" key is folded into
// properties), and prepareEvent buckets the visit on the server clock.
// A delayed event lands in the INGESTION-hour visit and the INGESTION
// month's session estimate, and its stored timestamp is the ingestion
// time — time-series, funnel-ordering, and retention bucketing all see
// the late arrival as having happened when the server received it.
//
// The identity model (ADR D8) REQUIRES event-time bucketing; this table
// pins what era-1 does instead so the eventual change is conscious.
func TestReference_ScenarioE_DelayedDelivery(t *testing.T) {
	const (
		site = "site-e"
		ip   = "203.0.113.99"
		ua   = "Mozilla/5.0 (Macintosh) Chrome/121"
		salt = "global-salt-e"
	)
	sess := monthKeyedID(site, ip, ua, salt, "2026-07")

	onTime := time.Date(2026, 7, 10, 9, 10, 0, 0, time.UTC)     // event at 09:10, delivered 09:10
	eventAt := time.Date(2026, 7, 10, 9, 50, 0, 0, time.UTC)    // event happened 09:50 ...
	delivered := time.Date(2026, 7, 10, 11, 40, 0, 0, time.UTC) // ... ingested 11:40

	truthVisit := VisitID(sess, eventAt)     // where the event BELONGS
	landingVisit := VisitID(sess, delivered) // where era-1 PUTS it

	if truthVisit == landingVisit {
		t.Fatal("09:50 event delivered 11:40 must land outside its true hour bucket (era-1 pin)")
	}
	if landingVisit != VisitID(sess, delivered) {
		t.Fatal("era-1 landing bucket must be keyed on ingestion time")
	}
	// A genuinely-on-time 11:40 event and the delayed 09:50 event become
	// indistinguishable: same visit, same stored timestamp.
	if VisitID(sess, delivered) == VisitID(sess, onTime) {
		t.Fatal("sanity: 09:10 on-time event shares no bucket with 11:40")
	}

	rows := []*scenarioRow{
		{name: "on-time 09:10", site: site, ip: ip, ua: ua, salt: salt,
			ts: onTime, ingestedAt: onTime},
		{name: "delayed 09:50 delivered 11:40", site: site, ip: ip, ua: ua, salt: salt,
			ts: eventAt, ingestedAt: delivered},
	}
	// Direct era-1 derivation (do not use runScenario: its VisitID keying
	// IS the behavior under test here).
	for _, r := range rows {
		r.sessionID = sess
		r.visitID = VisitID(sess, r.ingestedAt)
		r.storedAt = r.ingestedAt
		r.distinctID = ""
	}
	if got := surfaceCountsOf(rows); got != (surfaceCounts{visitors: 1, sessions: 2, persons: 0}) {
		t.Errorf("delayed-delivery surface counts = %+v, want visitors=1 sessions=2 persons=0 (truth: sessions=1)", got)
	}
	// Stored-timestamp pin: the delayed row's stored time is the delivery
	// time, so event-time-ordered surfaces sequence it after 11:00.
	if !rows[1].storedAt.Equal(delivered) {
		t.Errorf("era-1 stored timestamp = %v, want ingestion time %v", rows[1].storedAt, delivered)
	}
}

// ----------------------------------------------------------------------------
// O04: funnel / retention expected-answer tables (entity semantics)
// ----------------------------------------------------------------------------

// Programme workstream O04 ("funnel + retention semantics") extends this
// oracle from identity derivation to the query surfaces. The tables below
// pin, per O03 scenario and per entity mode, the ENTITY PARTITION (which
// events the query groups together) and HAND-COMPUTED funnel/retention
// counts. The counts are literals written by hand here — they are the
// spec, not the output of any implementation.
//
// The executable binding lives in internal/query: its storage-free walk
// tests and its Nucleus-gated integration tests re-derive the SAME
// scenario ids through these same public functions (session.ID /
// monthKeyedID, session.VisitID, identity.HashDistinctID) and assert the
// SAME literals against the real Funnel/Retention code. A mismatch on
// either side means the implementation is wrong or the era is being
// bumped — the O03 era rule applies unchanged.
//
// Entity modes (O03 ADR vocabulary, used verbatim):
//
//   visitor-estimate  events grouped on session_id (the monthly
//                     fingerprint estimate — current/default behavior)
//   visit             events grouped on visit_id (the clock-hour bucket)
//   person            events grouped on distinct_id, IDENTIFIED events
//                     only (distinct_id '' excluded — anonymous events
//                     belong to no person in era-1)
//
// Funnel semantics pinned by these tables (full statement in
// internal/query/funnel.go):
//   - ordering: (timestamp, event_id) ascending — timestamp is the STORED
//     column, which era-1 sets to INGESTION time (ADR D8: no event-time
//     on the wire; see the late-delivery pin below)
//   - progression: greedy earliest chain; each step consumes a distinct
//     event strictly after the previous step's event in that total order
//   - conversion window W: last step within W of the FIRST step's event,
//     boundary ts == e0+W is IN, ts > e0+W is OUT; W<=0 = unbounded
//   - exclusions: the first exclusion-matching event strictly after e0
//     terminates progression (steps kept, nothing further)

// o04Row is one seeded analytics event with the identity inputs the
// ingest path would have seen. storedTS is the timestamp era-1 stores
// (= ingestion time). identity inputs derive session/visit; rawIdentity
// derives the person id.
type o04Row struct {
	name        string
	eventType   string
	pathname    string
	storedTS    time.Time
	rawIdentity string
	eventID     string

	sessionID  string
	visitID    string
	distinctID string
}

// o04Derive derives the era-1 ids for a scenario's rows under one fixed
// fingerprint triple + site salt, exactly as ingest would: session.ID
// (month = server clock, pinned to the current month), VisitID keyed on
// the STORED (ingestion) timestamp, distinct_id = per-site-salt HMAC.
func o04Derive(t *testing.T, site, ip, ua, salt, siteSalt string, rows []*o04Row) {
	t.Helper()
	month := currentMonthKey()
	for _, r := range rows {
		r.sessionID = monthKeyedID(site, ip, ua, salt, month)
		r.visitID = VisitID(r.sessionID, r.storedTS)
		r.distinctID = distinctIDOf(r.rawIdentity, siteSalt)
	}
	if month != currentMonthKey() {
		t.Fatalf("UTC month rolled mid-scenario (%s); rerun the test", month)
	}
}

// o04Partition groups rows by the selected entity mode's id and drops
// anonymous rows in person mode (the persons-surface rule).
func o04Partition(rows []*o04Row, mode string) map[string][]*o04Row {
	groups := make(map[string][]*o04Row)
	for _, r := range rows {
		var key string
		switch mode {
		case "visit":
			key = r.visitID
		case "person":
			if r.distinctID == "" {
				continue
			}
			key = r.distinctID
		default: // visitor-estimate
			key = r.sessionID
		}
		groups[key] = append(groups[key], r)
	}
	return groups
}

func o04PartitionSizes(rows []*o04Row, mode string) map[int]int {
	sizes := make(map[int]int)
	for _, g := range o04Partition(rows, mode) {
		sizes[len(g)]++
	}
	return sizes
}

// Scenario A (anon -> login -> logout, one browser), funnel tables.
// Events: pageview 10:00 anon, signup 10:05 identified, purchase 11:30
// identified, pageview 11:45 anon (post-logout). Same O03 scenario-a
// fingerprint.
//
// HAND-COMPUTED (the spec):
//
//	funnel [pageview /, purchase]
//	  visitor-estimate: [1,1]  one entity; pageview 10:00 -> purchase 11:30
//	  visit:            [2,0]  10h visit has the pageview, 11h visit has
//	                           the purchase — the sequence splits across
//	                           clock-hour visits
//	  person:           [0,0]  purchase is identified, but the step-1
//	                           pageview is anonymous — no chain exists
//	funnel [signup, purchase]
//	  visitor-estimate: [1,1]
//	  visit:            [1,0]  signup 10h, purchase 11h — split
//	  person:           [1,1]  one person, signup 10:05 -> purchase 11:30
func TestReference_O04_ScenarioA_FunnelEntityTables(t *testing.T) {
	const (
		site     = "site-a"
		ip       = "203.0.113.7"
		ua       = "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36"
		salt     = "global-salt-a"
		siteSalt = "site-salt-a"
	)
	rows := []*o04Row{
		{name: "pageview 10:00 anon", eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "a-1"},
		{name: "signup 10:05 identified", eventType: "signup", storedTS: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC), rawIdentity: "user-1", eventID: "a-2"},
		{name: "purchase 11:30 identified", eventType: "purchase", storedTS: time.Date(2026, 7, 10, 11, 30, 0, 0, time.UTC), rawIdentity: "user-1", eventID: "a-3"},
		{name: "pageview 11:45 anon", eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 11, 45, 0, 0, time.UTC), eventID: "a-4"},
	}
	o04Derive(t, site, ip, ua, salt, siteSalt, rows)

	// Partition proof: one estimate, two visits (10h, 11h), one person
	// carrying exactly the two identified rows.
	if got := o04PartitionSizes(rows, "visitor-estimate"); !reflectDeepEqualIntMap(got, map[int]int{4: 1}) {
		t.Errorf("visitor-estimate partition = %v, want one entity with all 4 events", got)
	}
	if got := o04PartitionSizes(rows, "visit"); !reflectDeepEqualIntMap(got, map[int]int{2: 2}) {
		t.Errorf("visit partition = %v, want two entities with 2 events each", got)
	}
	if got := o04PartitionSizes(rows, "person"); !reflectDeepEqualIntMap(got, map[int]int{2: 1}) {
		t.Errorf("person partition = %v, want one person with the 2 identified events", got)
	}

	// The count literals above are the spec; internal/query binds them
	// (funnel_semantics_test.go + funnel_retention_nucleus_test.go).
	wantFunnel := map[string][2][2]int{
		// mode -> {funnel[pageview,purchase], funnel[signup,purchase]}
		"visitor-estimate": {{1, 1}, {1, 1}},
		"visit":            {{2, 0}, {1, 0}},
		"person":           {{0, 0}, {1, 1}},
	}
	for mode, tables := range wantFunnel {
		if len(o04Partition(rows, mode)) == 0 && (tables[0][0] != 0 || tables[1][0] != 0) {
			t.Errorf("%s: partition is empty but step-0 count literal is nonzero", mode)
		}
	}
	// Step-0 counts can never exceed the entity count per mode (the
	// literals stay honest against the partition even without running
	// the walk here).
	for mode, tables := range wantFunnel {
		n := len(o04Partition(rows, mode))
		for _, table := range tables {
			if table[0] > n {
				t.Errorf("%s: step-0 literal %d exceeds entity count %d", mode, table[0], n)
			}
		}
	}
}

// Scenario B (two people behind one NAT), funnel tables. Distinct UAs
// split the estimate; IDENTICAL UAs merge (the pinned D2 limitation).
//
// HAND-COMPUTED (funnel [pageview /, purchase]; person A does both,
// person B only the pageview):
//
//	distinct-UA NAT:
//	  visitor-estimate: [2,1]   visit: [2,1]   person: [0,0] (no identified events)
//	same-UA NAT (A does both, C only the pageview):
//	  visitor-estimate: [1,1]   visit: [1,1]   person: [0,0]
//	  — the [1,1] is the ESTIMATE answer; two real people did this (truth
//	  [2,1]). Pinned as the documented limitation, not as people counts.
func TestReference_O04_ScenarioB_FunnelEntityTables(t *testing.T) {
	const (
		site = "site-b"
		ip   = "198.51.100.25"
		salt = "global-salt-b"
	)
	chrome := "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/121 Safari/537.36"
	firefox := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"

	distinct := []*o04Row{
		{eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "b-1"},
		{eventType: "purchase", storedTS: time.Date(2026, 7, 10, 10, 10, 0, 0, time.UTC), eventID: "b-2"},
	}
	o04Derive(t, site, ip, chrome, salt, "", distinct)
	bOnly := []*o04Row{
		{eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 10, 2, 0, 0, time.UTC), eventID: "b-3"},
	}
	o04Derive(t, site, ip, firefox, salt, "", bOnly)

	all := append(append([]*o04Row{}, distinct...), bOnly...)
	if got := o04PartitionSizes(all, "visitor-estimate"); !reflectDeepEqualIntMap(got, map[int]int{2: 1, 1: 1}) {
		t.Errorf("distinct-UA partition = %v, want entities of 2 and 1 events", got)
	}
	if got := o04PartitionSizes(all, "visit"); !reflectDeepEqualIntMap(got, map[int]int{2: 1, 1: 1}) {
		t.Errorf("distinct-UA visit partition = %v, want 2+1", got)
	}
	if got := len(o04Partition(all, "person")); got != 0 {
		t.Errorf("person partition = %d entities, want 0 (no identified events)", got)
	}

	sameA := []*o04Row{
		{eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "b-4"},
		{eventType: "purchase", storedTS: time.Date(2026, 7, 10, 10, 10, 0, 0, time.UTC), eventID: "b-5"},
	}
	o04Derive(t, site, ip, chrome, salt, "", sameA)
	sameC := []*o04Row{
		{eventType: "pageview", pathname: "/", storedTS: time.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC), eventID: "b-6"},
	}
	o04Derive(t, site, ip, chrome, salt, "", sameC)
	merged := append(append([]*o04Row{}, sameA...), sameC...)
	if got := len(o04Partition(merged, "visitor-estimate")); got != 1 {
		t.Errorf("same-UA estimate partition = %d, want 1 (the merged-estimate pin)", got)
	}
	if got := len(o04Partition(merged, "visit")); got != 1 {
		t.Errorf("same-UA visit partition = %d, want 1 (same clock hour)", got)
	}
}

// Scenario C (one person, two devices), funnel + retention tables. The
// laptop signs up 09:00; the phone purchases 20:00; both identified as
// the same raw user under the same site salt.
//
// HAND-COMPUTED (funnel [signup, purchase]):
//
//	visitor-estimate: [1,0]  the laptop entity matches signup only; the
//	                        purchase lives on the phone's entity, so the
//	                        person never converts — split across two
//	                        estimates
//	visit:            [1,0]  same split
//	person:           [1,1]  one person, signup (laptop) -> purchase
//	                        (phone): the cross-device conversion
//
// HAND-COMPUTED retention (period = 1 day; range Jul 10 00:00 -> Jul 12
// 00:00, both days complete, two grid columns Jul 10 + Jul 11):
//
//	visitor-estimate: cohort Jul 10 (laptop) size 1 [100, 0];
//	                  cohort Jul 11 (phone)  size 1 [100]
//	visit:            same shape — [100, 0] and [100]
//	person:           cohort Jul 10 size 1 [100, 100]  — the day-2 return
//	                                                 IS attributed
//	(the estimate/visit grids can never show the return: the returning
//	 device is a different estimate and a different visit)
//
// Incomplete-period pin (range ending Jul 11 12:00, phone purchase
// delivered 09:00 that morning): the Jul 11 column is not complete, so
// the person cohort Jul 10 reports [100] with one incomplete period
// excluded, and the phone's Jul 11 estimate cohort reports no periods
// at all.
func TestReference_O04_ScenarioC_CrossDeviceTables(t *testing.T) {
	const (
		site     = "site-c"
		salt     = "global-salt-c"
		siteSalt = "site-salt-c"
		raw      = "user-1"
	)
	laptop := []*o04Row{
		{eventType: "signup", storedTS: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), rawIdentity: raw, eventID: "c-1"},
	}
	o04Derive(t, site, "203.0.113.9", "Mozilla/5.0 (Macintosh) Chrome/120", salt, siteSalt, laptop)
	phone := []*o04Row{
		{eventType: "purchase", storedTS: time.Date(2026, 7, 11, 20, 0, 0, 0, time.UTC), rawIdentity: raw, eventID: "c-2"},
	}
	o04Derive(t, site, "198.51.100.88", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Safari/604.1", salt, siteSalt, phone)

	all := append(append([]*o04Row{}, laptop...), phone...)
	// Partition: two estimates, two visits, ONE person with both events.
	if got := len(o04Partition(all, "visitor-estimate")); got != 2 {
		t.Errorf("estimate partition = %d, want 2", got)
	}
	if got := len(o04Partition(all, "visit")); got != 2 {
		t.Errorf("visit partition = %d, want 2", got)
	}
	if got := o04PartitionSizes(all, "person"); !reflectDeepEqualIntMap(got, map[int]int{2: 1}) {
		t.Errorf("person partition = %v, want ONE person holding both devices' events", got)
	}
}

// Timestamp-tie pin. One entity, two events at the SAME stored timestamp
// with event ids ordered opposite to insertion ("ev-002" inserted first,
// "ev-001" second). The pinned total order is (timestamp, event_id)
// ascending, so signup ("ev-001") sequences BEFORE purchase ("ev-002")
// regardless of ingestion order, and the funnel [signup, purchase]
// converts. Flipping the tie-break breaks the conversion.
func TestReference_O04_TimestampTieBreak(t *testing.T) {
	const (
		site = "site-tie"
		ip   = "203.0.113.61"
		ua   = "Mozilla/5.0 Chrome/121"
		salt = "global-salt-tie"
	)
	rows := []*o04Row{
		{name: "purchase, inserted FIRST", eventType: "purchase", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "ev-002"},
		{name: "signup, inserted SECOND", eventType: "signup", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "ev-001"},
	}
	o04Derive(t, site, ip, ua, salt, "", rows)
	// Same entity in every session-keyed mode (same fingerprint, same hour).
	for _, mode := range []string{"visitor-estimate", "visit"} {
		if got := len(o04Partition(rows, mode)); got != 1 {
			t.Errorf("%s partition = %d entities, want 1", mode, got)
		}
	}
	// HAND-COMPUTED: funnel [signup, purchase] = [1,1] under (ts, event_id)
	// ascending; [1,0] under any flipped tie-break. Bound in
	// internal/query/funnel_semantics_test.go.
	if rows[0].eventID <= rows[1].eventID {
		t.Fatal("fixture must have the later-id event inserted first for the tie pin to mean anything")
	}
}

// Late-arrival pin (the D8 truth). True order: B 09:00 then A 09:05.
// Ingestion: A stored 09:59, B stored 10:00 (B delivered late). The
// queries order by the STORED timestamp (= ingestion time, era-1), so the
// funnel [B, A] sees A before B and does NOT convert: [1,0]. Under event
// time it WOULD convert ([1,1]) — that divergence is the O03-implementation
// dependency (wire event-time, ADR D8), recorded in AUDIT_OPEN.md.
func TestReference_O04_LateArrivalOrdersByIngestionTime(t *testing.T) {
	const (
		site = "site-late"
		ip   = "203.0.113.62"
		ua   = "Mozilla/5.0 Chrome/121"
		salt = "global-salt-late"
	)
	rows := []*o04Row{
		{name: "A, true 09:05, ingested 09:59", eventType: "A", storedTS: time.Date(2026, 7, 10, 9, 59, 0, 0, time.UTC), eventID: "l-1"},
		{name: "B, true 09:00, ingested 10:00", eventType: "B", storedTS: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), eventID: "l-2"},
	}
	o04Derive(t, site, ip, ua, salt, "", rows)
	if !rows[0].storedTS.Before(rows[1].storedTS) {
		t.Fatal("fixture must store the late-delivered B after A")
	}
	// HAND-COMPUTED: funnel [B, A] = [1,0] on stored order (A precedes B,
	// so B's match leaves no A after it). Event-time truth would be [1,1].
}

// reflectDeepEqualIntMap is a tiny map[int]int equality helper (kept
// local so the oracle file needs no new imports beyond its existing set).
func reflectDeepEqualIntMap(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------
// Salt persistence and rotation (era rules)
// ----------------------------------------------------------------------------

// Era-1 answers pinned here:
//
//  1. FIXED salt -> all derivations are pure functions of their inputs;
//     nothing process-local enters them, so restart stability with a
//     fixed salt is total (the only state is the env var itself).
//  2. CHANGING the global salt -> every session_id and visit_id changes
//     (new era), but distinct_id on a site-backed install does NOT
//     (its HMAC key is the per-site salt from the sites table, not the
//     global salt).
//  3. FALLBACK branch (unknown site / unwired SiteService): the global
//     salt IS the identity salt, so a rotation re-keys persons too.
//  4. UNSET salt -> the server mints a random per-process salt
//     (cmd/observe/main.go), so every restart silently starts a new era
//     for sessions and visits. Pinned as fact, with the ADR's
//     recommendation against running this way.
func TestReference_SaltEras(t *testing.T) {
	const (
		site     = "site-salt"
		ip       = "203.0.113.120"
		ua       = "Mozilla/5.0 Chrome/122"
		siteSalt = "per-site-salt-1"
		raw      = "user-1"
	)
	month := currentMonthKey()

	// 1. Fixed salt: deterministic across independent calls ("restarts").
	if monthKeyedID(site, ip, ua, "fixed-salt", month) != monthKeyedID(site, ip, ua, "fixed-salt", month) {
		t.Fatal("fixed salt must derive stable session ids")
	}
	if got1, got2 := ID(site, ip, ua, "fixed-salt"), ID(site, ip, ua, "fixed-salt"); got1 != got2 {
		t.Fatalf("production ID with fixed salt unstable: %s != %s", got1, got2)
	}

	// 2. Global-salt rotation: sessions and visits re-key ...
	s1 := monthKeyedID(site, ip, ua, "salt-era-1", month)
	s2 := monthKeyedID(site, ip, ua, "salt-era-2", month)
	if s1 == s2 {
		t.Fatal("rotating the global salt must change session ids")
	}
	ts := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	if VisitID(s1, ts) == VisitID(s2, ts) {
		t.Fatal("rotating the global salt must change visit ids (via the session id)")
	}
	// ... but the persons entity on a site-backed install does not.
	if distinctIDOf(raw, siteSalt) != distinctIDOf(raw, siteSalt) ||
		distinctIDOf(raw, siteSalt) != identity.HashDistinctID(raw, siteSalt) {
		t.Fatal("distinct_id derivation must be deterministic under the per-site salt")
	}
	if distinctIDOf(raw, siteSalt) == "" {
		t.Fatal("distinct_id must not be empty for a non-empty raw value")
	}
	// The global salt never touches the per-site HMAC: era change leaves it identical.
	if identity.HashDistinctID(raw, siteSalt) != identity.HashDistinctID(raw, siteSalt) {
		t.Fatal("per-site-salt HMAC must not depend on the global salt")
	}

	// 3. Fallback branch: when no per-site salt exists, the global salt
	//    keys the persons HMAC (internal/ingest/handler.go falls back to
	//    the global salt), so rotation re-keys persons there.
	if identity.HashDistinctID(raw, "salt-era-1") == identity.HashDistinctID(raw, "salt-era-2") {
		t.Fatal("fallback-branch distinct_id MUST change under global-salt rotation (era-1 pin)")
	}

	// 4. Distinct-salt independence of the two derivations: the global
	//    salt and the per-site salt produce different person digests for
	//    the same raw value (different keys), which is why session
	//    estimates and persons never collide by construction.
	if identity.HashDistinctID(raw, "salt-era-1") == identity.HashDistinctID(raw, siteSalt) {
		t.Fatal("different salts must key different person digests")
	}
}
