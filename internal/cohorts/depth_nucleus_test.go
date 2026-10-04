package cohorts

// Nucleus-gated integration for the C2 depth work (OR / NOT trees and
// static cohorts). Self-skips without a live Nucleus like every other
// DB-backed test here; it was written WITHOUT access to one and has never
// been run. Expected values are hand-written.

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDepth_TreeOrNot(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := NewService(db)

	site := "test_cohort_tree_" + cohortsToken()
	ts := time.Now().UTC().UnixMilli() - 86_400_000
	plantEvent(t, ctx, db, site, "uUS", "s1", "pageview", "US", "Chrome", "/", ts)
	plantEvent(t, ctx, db, site, "uDE", "s2", "pageview", "DE", "Chrome", "/", ts+1)
	plantEvent(t, ctx, db, site, "uFR", "s3", "pageview", "FR", "Safari", "/", ts+2)
	plantEvent(t, ctx, db, site, "uDE", "s2", "purchase", "DE", "Chrome", "/", ts+3)

	us := Rule{Type: "property", Key: "country", Value: "US"}
	buy := Rule{Type: "event", Name: "purchase"}
	got, err := svc.EvaluateCohort(ctx, site, Definition{Op: "or", Children: []Definition{{Leaf: &us}, {Leaf: &buy}}})
	if err != nil || !reflect.DeepEqual(got, []string{"uDE", "uUS"}) {
		t.Fatalf("or: %v %v", got, err)
	}
	got, err = svc.EvaluateCohort(ctx, site, Definition{Op: "not", Children: []Definition{{Op: "or", Children: []Definition{{Leaf: &us}, {Leaf: &buy}}}}})
	if err != nil || !reflect.DeepEqual(got, []string{"uFR"}) {
		t.Fatalf("not(or): %v %v", got, err)
	}
}

func TestDepth_StaticLifecycleAndIDOR(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := NewService(db)

	siteA := "test_cohort_static_a_" + cohortsToken()
	siteB := "test_cohort_static_b_" + cohortsToken()

	c, err := svc.CreateStatic(ctx, siteA, "vip", "", []string{"u1", "u2", "u2", " u3 "})
	if err != nil || c.MemberCount != 3 {
		t.Fatalf("create: %+v %v", c, err)
	}
	ids, err := svc.MembersForFilter(ctx, siteA, c.CohortID)
	if err != nil || !reflect.DeepEqual(ids, []string{"u1", "u2", "u3"}) {
		t.Fatalf("members: %v %v", ids, err)
	}
	if _, n, err := svc.AddMembers(ctx, siteA, c.CohortID, []string{"u3", "u4"}); err != nil || n != 1 {
		t.Fatalf("add: %d %v", n, err)
	}
	if c2, n, err := svc.RemoveMembers(ctx, siteA, c.CohortID, []string{"u1", "nope"}); err != nil || n != 1 || c2.MemberCount != 3 {
		t.Fatalf("remove: %+v %d %v", c2, n, err)
	}
	if ids, _ := svc.MembersForFilter(ctx, siteA, c.CohortID); !reflect.DeepEqual(ids, []string{"u2", "u3", "u4"}) {
		t.Fatalf("after edits: %v", ids)
	}
	// Re-adding a removed member works (higher version than the tombstone).
	if _, n, err := svc.AddMembers(ctx, siteA, c.CohortID, []string{"u1"}); err != nil || n != 1 {
		t.Fatalf("re-add: %d %v", n, err)
	}

	// IDOR: site B cannot read or edit site A's cohort.
	if _, err := svc.MembersForFilter(ctx, siteB, c.CohortID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if _, _, err := svc.AddMembers(ctx, siteB, c.CohortID, []string{"evil"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign add: %v", err)
	}
	if _, _, err := svc.RemoveMembers(ctx, siteB, c.CohortID, []string{"u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign remove: %v", err)
	}
	if ids, _ := svc.MembersForFilter(ctx, siteA, c.CohortID); !reflect.DeepEqual(ids, []string{"u1", "u2", "u3", "u4"}) {
		t.Fatalf("site A list changed by site B: %v", ids)
	}

	// Member edits on a rule cohort are refused.
	rc, err := svc.Create(ctx, siteA, "rule", "", Definition{Op: "and", Rules: []Rule{{Type: "event", Name: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddMembers(ctx, siteA, rc.CohortID, []string{"u9"}); !errors.Is(err, ErrNotStatic) {
		t.Fatalf("add to rule cohort: %v", err)
	}
}
