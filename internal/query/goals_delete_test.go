package query

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestDeleteGoalActuallyDeletes pins the fix for a method that was a literal
// `return nil`. It reported success and left the goal in place, so the UI's
// delete appeared to work and the goal came back on the next load — and the
// stated reason ("ReplacingMergeTree: can't delete") was wrong: DELETE removes
// the physical rows, which is what the rollup jobs already depend on.
func TestDeleteGoalActuallyDeletes(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	svc := NewStatsService(db)
	ctx := context.Background()
	site := goalSite("goalsite-delete")
	otherSite := goalSite("goalsite-other")

	g, err := svc.CreateGoal(ctx, Goal{SiteID: site, Name: "Signup", GoalType: "event", GoalValue: "signup"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	keep, err := svc.CreateGoal(ctx, Goal{SiteID: site, Name: "Purchase", GoalType: "event", GoalValue: "purchase"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if err := svc.DeleteGoal(ctx, site, g.GoalID); err != nil {
		t.Fatalf("delete goal: %v", err)
	}

	goals, err := svc.ListGoals(ctx, site)
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	var sawDeleted, sawKept bool
	for _, got := range goals {
		if got.GoalID == g.GoalID {
			sawDeleted = true
		}
		if got.GoalID == keep.GoalID {
			sawKept = true
		}
	}
	if sawDeleted {
		t.Fatalf("DeleteGoal reported success but the goal is still listed")
	}
	if !sawKept {
		t.Fatalf("DeleteGoal removed the wrong goal")
	}

	// Another site's goal with the same id must be untouched — the delete is
	// scoped by site_id precisely so a guessed id cannot reach across.
	other, err := svc.CreateGoal(ctx, Goal{SiteID: otherSite, Name: "Signup", GoalType: "event", GoalValue: "signup"})
	if err != nil {
		t.Fatalf("create other goal: %v", err)
	}
	if err := svc.DeleteGoal(ctx, site, other.GoalID); err != nil {
		t.Fatalf("cross-site delete: %v", err)
	}
	otherGoals, err := svc.ListGoals(ctx, otherSite)
	if err != nil {
		t.Fatalf("list other goals: %v", err)
	}
	var stillThere bool
	for _, got := range otherGoals {
		if got.GoalID == other.GoalID {
			stillThere = true
		}
	}
	if !stillThere {
		t.Fatalf("a delete scoped to one site removed another site's goal")
	}
}
