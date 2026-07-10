package cohorts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// connect is the shared "skip if nucleus down" boilerplate.
func connect(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return ctx, db, func() {
		db.Close()
		cancel()
	}
}

// plantEvent inserts one events row with the given attributes.
// Caller is responsible for unique event_id / site_id.
func plantEvent(t *testing.T, ctx context.Context, db *nucleus.Client,
	siteID, distinctID, sessionID, eventType, country, browser, pathname string, tsMs int64) {
	t.Helper()
	_, err := db.SQL().Exec(ctx,
		`INSERT INTO events (
			event_id, tenant_id, site_id, session_id, visit_id,
			timestamp, event_type, url, pathname, title, referrer,
			utm_source, utm_medium, utm_campaign,
			country, browser, os, device, language,
			screen_width, screen_height,
			properties, distinct_id, release_tag
		) VALUES ($1, 'default', $2, $3, $3, $4, $5,
			$6, $7, '', '',
			'', '', '', $8, $9, '', '', '',
			0, 0, 'null', $10, '')`,
		cohortsRandID(), siteID, sessionID,
		strconv.FormatInt(tsMs, 10), eventType,
		"https://x"+pathname, pathname,
		country, browser,
		distinctID,
	)
	if err != nil {
		t.Fatalf("plant event: %v", err)
	}
}

// TestEvaluateCohort_EventRule pins the contract: an event-presence
// rule returns exactly the distinct_ids that fired the named event
// at least min_count times in the window.
func TestEvaluateCohort_EventRule(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	site := "test_cohort_event_" + cohortsToken()
	now := time.Now().UTC().UnixMilli()
	// One day ago — well inside the default 30d window.
	tsRecent := now - 86_400_000

	// userA fires "purchase" twice.
	plantEvent(t, ctx, db, site, "userA", "sa1", "purchase", "US", "Chrome", "/", tsRecent)
	plantEvent(t, ctx, db, site, "userA", "sa1", "purchase", "US", "Chrome", "/", tsRecent+1000)
	// userB fires "purchase" once.
	plantEvent(t, ctx, db, site, "userB", "sb1", "purchase", "DE", "Safari", "/", tsRecent+2000)
	// userC fires only "pageview" — must NOT match.
	plantEvent(t, ctx, db, site, "userC", "sc1", "pageview", "FR", "Firefox", "/", tsRecent+3000)
	// anonymous purchase — must be excluded.
	plantEvent(t, ctx, db, site, "", "anon1", "purchase", "FR", "Firefox", "/", tsRecent+4000)

	svc := NewService(db)

	// min_count=1: userA + userB
	ids, err := svc.EvaluateCohort(ctx, site, Definition{
		Op: "and",
		Rules: []Rule{
			{Type: "event", Name: "purchase", Window: "30d", MinCount: 1},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCohort: %v", err)
	}
	sort.Strings(ids)
	if got, want := joined(ids), "userA,userB"; got != want {
		t.Errorf("event min=1: got %q, want %q", got, want)
	}

	// min_count=2: userA only
	ids2, err := svc.EvaluateCohort(ctx, site, Definition{
		Op: "and",
		Rules: []Rule{
			{Type: "event", Name: "purchase", Window: "30d", MinCount: 2},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCohort min=2: %v", err)
	}
	if got, want := joined(ids2), "userA"; got != want {
		t.Errorf("event min=2: got %q, want %q", got, want)
	}
}

// TestEvaluateCohort_PropertyRule pins property-based filtering on an
// indexed events column.
func TestEvaluateCohort_PropertyRule(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	site := "test_cohort_prop_" + cohortsToken()
	now := time.Now().UTC().UnixMilli() - 3600_000

	plantEvent(t, ctx, db, site, "userUS1", "s1", "pageview", "US", "Chrome", "/", now)
	plantEvent(t, ctx, db, site, "userUS2", "s2", "pageview", "US", "Safari", "/", now+1000)
	plantEvent(t, ctx, db, site, "userDE", "s3", "pageview", "DE", "Chrome", "/", now+2000)

	svc := NewService(db)
	ids, err := svc.EvaluateCohort(ctx, site, Definition{
		Op: "and",
		Rules: []Rule{
			{Type: "property", Key: "country", Operator: "=", Value: "US"},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCohort property: %v", err)
	}
	sort.Strings(ids)
	if got, want := joined(ids), "userUS1,userUS2"; got != want {
		t.Errorf("property country=US: got %q, want %q", got, want)
	}
}

// TestEvaluateCohort_AndCombination pins AND-intersection across an
// event rule and a property rule.
func TestEvaluateCohort_AndCombination(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	site := "test_cohort_and_" + cohortsToken()
	ts := time.Now().UTC().UnixMilli() - 3600_000

	// userA: US + purchase  → matches
	plantEvent(t, ctx, db, site, "userA", "s1", "purchase", "US", "Chrome", "/", ts)
	plantEvent(t, ctx, db, site, "userA", "s1", "pageview", "US", "Chrome", "/", ts+100)
	// userB: US + no purchase → fails event rule
	plantEvent(t, ctx, db, site, "userB", "s2", "pageview", "US", "Safari", "/", ts+1000)
	// userC: DE + purchase → fails property rule
	plantEvent(t, ctx, db, site, "userC", "s3", "purchase", "DE", "Chrome", "/", ts+2000)

	svc := NewService(db)
	ids, err := svc.EvaluateCohort(ctx, site, Definition{
		Op: "and",
		Rules: []Rule{
			{Type: "event", Name: "purchase", Window: "30d", MinCount: 1},
			{Type: "property", Key: "country", Operator: "=", Value: "US"},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCohort and: %v", err)
	}
	if got, want := joined(ids), "userA"; got != want {
		t.Errorf("AND: got %q, want %q", got, want)
	}
}

// TestEvaluateCohort_Empty pins that a definition with zero rules
// returns zero members (not "everyone") — defensive default for the
// UI's empty-builder state.
func TestEvaluateCohort_Empty(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := NewService(db)
	ids, err := svc.EvaluateCohort(ctx, "anything", Definition{Op: "and"})
	if err != nil {
		t.Fatalf("EvaluateCohort empty: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("empty rule len = %d, want 0", len(ids))
	}
}

// TestCohortCRUD pins the create / get / list / update / refresh /
// delete lifecycle. Validates that read-time dedup picks the latest
// updated_at row (finding #10 family) and that delete tombstones the
// row out of subsequent reads.
func TestCohortCRUD(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := NewService(db)

	site := "test_cohort_crud_" + cohortsToken()
	ts := time.Now().UTC().UnixMilli() - 3600_000
	plantEvent(t, ctx, db, site, "userA", "s1", "purchase", "US", "Chrome", "/", ts)

	def := Definition{
		Op: "and",
		Rules: []Rule{
			{Type: "event", Name: "purchase", Window: "30d", MinCount: 1},
		},
	}
	c, err := svc.Create(ctx, site, "Buyers", "people who bought", def)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.CohortID == "" {
		t.Fatalf("CohortID empty")
	}
	if c.MemberCount != 1 {
		t.Errorf("MemberCount = %d, want 1", c.MemberCount)
	}

	// Get round-trip.
	got, err := svc.Get(ctx, site, c.CohortID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatalf("Get returned nil")
	}
	if got.Name != "Buyers" {
		t.Errorf("name = %q, want Buyers", got.Name)
	}

	// List should contain it.
	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, x := range list {
		if x.CohortID == c.CohortID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created cohort not in List output")
	}

	// Plant another matching event then Refresh.
	plantEvent(t, ctx, db, site, "userB", "s2", "purchase", "US", "Safari", "/", ts+1000)
	refreshed, err := svc.Refresh(ctx, site, c.CohortID)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.MemberCount != 2 {
		t.Errorf("after refresh MemberCount = %d, want 2", refreshed.MemberCount)
	}

	// Delete (tombstone) → Get returns nil.
	if err := svc.Delete(ctx, site, c.CohortID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	gone, _ := svc.Get(ctx, site, c.CohortID)
	if gone != nil {
		t.Errorf("after delete Get returned %+v, want nil", gone)
	}
}

// TestParseWindow pins the time-window parser.
func TestParseWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":      30 * 24 * time.Hour,
		"7d":       7 * 24 * time.Hour,
		"24h":      24 * time.Hour,
		"":         30 * 24 * time.Hour,
		"garbage":  30 * 24 * time.Hour,
		"15m":      15 * time.Minute,
		"-5d":      30 * 24 * time.Hour,
	}
	for in, want := range cases {
		if got := parseWindow(in); got != want {
			t.Errorf("parseWindow(%q) = %v, want %v", in, got, want)
		}
	}
}

func joined(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

func cohortsToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cohortsRandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
