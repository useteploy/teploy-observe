package query

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// goalSite gives each run its own site id. The events table is append-only
// and shared, so a fixed name would make the second run of the suite count the
// first run's events and fail on numbers that were right when they were
// written.
func goalSite(prefix string) string {
	return prefix + "-" + generateQueryID()[:12]
}

func goalValueDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	return db
}

// insertGoalEvent writes one conversion event carrying props as its JSON
// properties blob. props is a literal so a test can plant a value the cast
// cannot read.
func insertGoalEvent(t *testing.T, db *nucleus.Client, siteID, session, eventType, props string, ts time.Time) {
	t.Helper()
	_, err := db.SQL().Exec(context.Background(),
		`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id,
		                     event_type, timestamp, pathname, properties)
		 VALUES ($1, 'default', $2, $3, $3, $4, $5, '/checkout', $6)`,
		generateQueryID(), siteID, session, eventType,
		fmt.Sprintf("%d", ts.UnixMilli()), props,
	)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

// TestGoalConversionsCarryValue is the whole point of the feature: a goal that
// counts conversions but cannot say what they were worth is the one place
// Observe was behind Plausible and Fathom. Fails before 036 because the
// columns do not exist, and fails before GoalConversions learned to sum them
// because TotalValueMinor is always zero.
func TestGoalConversionsCarryValue(t *testing.T) {
	db := goalValueDB(t)
	svc := NewStatsService(db)
	ctx := context.Background()
	site := goalSite("goalsite-value-fixed")
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)

	// Two sessions, three purchase events: one session bought twice.
	insertGoalEvent(t, db, site, "s1", "purchase", `{}`, now)
	insertGoalEvent(t, db, site, "s1", "purchase", `{}`, now)
	insertGoalEvent(t, db, site, "s2", "purchase", `{}`, now)

	g, err := svc.CreateGoal(ctx, Goal{
		SiteID: site, Name: "Purchase", GoalType: "event", GoalValue: "purchase",
		ValueMinor: 4999, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if g.ValueMinor != 4999 || g.Currency != "USD" {
		t.Fatalf("goal came back without its value: %+v", g)
	}

	got := findConversion(t, svc, site, from, to, g.GoalID)
	if got.Conversions != 2 {
		t.Errorf("conversions = %d, want 2 (distinct sessions)", got.Conversions)
	}
	if got.ConversionEvents != 3 {
		t.Errorf("conversion_events = %d, want 3", got.ConversionEvents)
	}
	// Money is summed over events, not sessions: three purchases at $49.99.
	if got.TotalValueMinor != 3*4999 {
		t.Errorf("total_value_minor = %d, want %d", got.TotalValueMinor, 3*4999)
	}
	if got.Goal.Currency != "USD" {
		t.Errorf("currency = %q, want USD — the amount is meaningless without it", got.Goal.Currency)
	}
}

// TestGoalConversionsPerEventValue covers the second value source: the amount
// rides on the event, so a $12 order and a $400 order are not averaged into a
// fiction. It also pins the two ways the sum must not break — a non-numeric
// property must be skipped rather than abort the query, and the total must be
// exact rather than a float that has drifted.
func TestGoalConversionsPerEventValue(t *testing.T) {
	db := goalValueDB(t)
	svc := NewStatsService(db)
	ctx := context.Background()
	site := goalSite("goalsite-value-event")
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)

	// 0.1 + 0.2 is the canonical float failure; summed as cents it is 30.
	insertGoalEvent(t, db, site, "s1", "purchase", `{"revenue":"0.10"}`, now)
	insertGoalEvent(t, db, site, "s2", "purchase", `{"revenue":"0.20"}`, now)
	insertGoalEvent(t, db, site, "s3", "purchase", `{"revenue":"49.99"}`, now)
	insertGoalEvent(t, db, site, "s4", "purchase", `{"revenue":400}`, now)
	// Neither of these may contribute, and neither may take the query down:
	// Nucleus raises "cannot cast 'n/a' to FLOAT" on an unguarded sum.
	insertGoalEvent(t, db, site, "s5", "purchase", `{"revenue":"n/a"}`, now)
	insertGoalEvent(t, db, site, "s6", "purchase", `{}`, now)

	g, err := svc.CreateGoal(ctx, Goal{
		SiteID: site, Name: "Purchase", GoalType: "event", GoalValue: "purchase",
		Currency: "USD", ValueSource: ValueSourceEvent,
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if g.ValueProperty != DefaultValueProperty {
		t.Fatalf("value_property = %q, want %q", g.ValueProperty, DefaultValueProperty)
	}

	got := findConversion(t, svc, site, from, to, g.GoalID)
	if got.ConversionEvents != 6 {
		t.Errorf("conversion_events = %d, want 6 — the unreadable ones still converted", got.ConversionEvents)
	}
	const want = 10 + 20 + 4999 + 40000
	if got.TotalValueMinor != want {
		t.Errorf("total_value_minor = %d, want %d", got.TotalValueMinor, want)
	}
}

// TestGoalConversionsRespectCurrencyExponent pins that the minor-unit scale is
// read off the goal's currency and not assumed to be 100. A ¥5,000 sale is
// 5000 minor units, not 500,000.
func TestGoalConversionsRespectCurrencyExponent(t *testing.T) {
	db := goalValueDB(t)
	svc := NewStatsService(db)
	ctx := context.Background()
	site := goalSite("goalsite-value-jpy")
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)

	insertGoalEvent(t, db, site, "s1", "purchase", `{"revenue":"5000"}`, now)

	g, err := svc.CreateGoal(ctx, Goal{
		SiteID: site, Name: "Purchase", GoalType: "event", GoalValue: "purchase",
		Currency: "JPY", ValueSource: ValueSourceEvent,
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	got := findConversion(t, svc, site, from, to, g.GoalID)
	if got.TotalValueMinor != 5000 {
		t.Errorf("total_value_minor = %d, want 5000 — JPY has no minor unit", got.TotalValueMinor)
	}
}

// TestUpdateGoalKeepsExactlyTheEdit is what makes the value usable on a goal
// that already exists — every goal on every install predates the money, and
// without an edit path the only way to value one would be to delete it and
// lose its id.
//
// It ran roughly one time in two while UpdateGoal appended a higher version
// and left the collapse to the engine: Nucleus v0.1.8 merges two rows sharing
// an ORDER BY key on the way into the memtable and keeps the older one about
// half the time, so the edit silently reverted. Run it a few times before
// believing a change to UpdateGoal.
func TestUpdateGoalKeepsExactlyTheEdit(t *testing.T) {
	db := goalValueDB(t)
	svc := NewStatsService(db)
	ctx := context.Background()
	site := goalSite("goalsite-value-update")

	g, err := svc.CreateGoal(ctx, Goal{
		SiteID: site, Name: "Signup", GoalType: "event", GoalValue: "signup",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if g.HasValue() {
		t.Fatal("a goal created without money reports a value")
	}

	if _, err := svc.UpdateGoal(ctx, site, g.GoalID, Goal{
		Name: "Signup", GoalType: "event", GoalValue: "signup",
		ValueMinor: 2500, Currency: "GBP",
	}); err != nil {
		t.Fatalf("update goal: %v", err)
	}

	goals, err := svc.ListGoals(ctx, site)
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	var seen int
	var got Goal
	for _, candidate := range goals {
		if candidate.GoalID == g.GoalID {
			seen++
			got = candidate
		}
	}
	if seen != 1 {
		t.Fatalf("goal listed %d times after one edit, want exactly 1", seen)
	}
	if got.ValueMinor != 2500 || got.Currency != "GBP" {
		t.Errorf("edit did not stick: value_minor=%d currency=%q", got.ValueMinor, got.Currency)
	}
	if got.CreatedAt != g.CreatedAt {
		t.Errorf("editing a goal re-dated it: %q -> %q", g.CreatedAt, got.CreatedAt)
	}
}

func findConversion(t *testing.T, svc *StatsService, site string, from, to time.Time, goalID string) GoalConversion {
	t.Helper()
	rows, err := svc.GoalConversions(context.Background(), site, from, to)
	if err != nil {
		t.Fatalf("goal conversions: %v", err)
	}
	for _, r := range rows {
		if r.Goal.GoalID == goalID {
			return r
		}
	}
	t.Fatalf("goal %s missing from conversions (%d rows)", goalID, len(rows))
	return GoalConversion{}
}
