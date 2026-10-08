package cohorts

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestR3WindowRefusalPreservesPredicate(t *testing.T) {
	for _, raw := range []string{"0d", "garbage", "106752d", "9223372036854775807d", "9223372036854775807h", "9223372036854775807m"} {
		if _, err := checkedWindow(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if d, err := checkedWindow("30d"); err != nil || d != 30*24*time.Hour {
		t.Fatalf("%v %v", d, err)
	}
	s := NewService(nil)
	s.budgets.MaxWindow = 7 * 24 * time.Hour
	if _, err := s.evalEventRule(context.Background(), "site", Rule{Type: "event", Name: "purchase", Window: "30d"}); err == nil {
		t.Fatal("saved 30d rule silently evaluated as 7d")
	}
}

// Exercises actual Add/Remove control flow with append-only durable rows,
// summary/chunk faults and a regressed clock. This is not a Nucleus oracle.
func TestR3OppositeEditAfterSummaryFaultAndRestart(t *testing.T) {
	for _, initial := range []bool{false, true} {
		c := Cohort{CohortID: "c", SiteID: "s", Name: "members", Rule: staticRule, UpdatedAt: 1000}
		var rows []memberRecord
		if initial {
			rows = []memberRecord{{EntityID: "u", AddedAt: 1000, Version: 1000}}
			c.MemberCount = 1
		}
		failSummary := true
		makeService := func() *Service {
			s := NewService(nil)
			s.clock = func() int64 { return 999 }
			s.summaryRead = func(context.Context, string, string) (*Cohort, error) { copy := c; return &copy, nil }
			s.summaryWrite = func(_ context.Context, next Cohort) error {
				if failSummary {
					return errors.New("summary fault")
				}
				c = next
				return nil
			}
			s.memberRead = func(context.Context, string, string) ([]memberRecord, error) {
				return append([]memberRecord(nil), rows...), nil
			}
			s.memberWrite = func(_ context.Context, _, _ string, next []memberRow, v int64) error {
				for _, r := range next {
					rows = append(rows, memberRecord{EntityID: r.id, AddedAt: r.addedAt, Removed: r.removed, Version: v})
				}
				return nil
			}
			return s
		}
		s := makeService()
		ctx := context.Background()
		var err error
		if initial {
			_, _, err = s.RemoveMembers(ctx, "s", "c", []string{"u"})
		} else {
			_, _, err = s.AddMembers(ctx, "s", "c", []string{"u"})
		}
		if err == nil {
			t.Fatal("fault not injected")
		}
		live := collapseMembers(rows)
		if (len(live) == 1) == initial {
			t.Fatal("member write did not commit before summary fault")
		}
		failSummary = false
		s = makeService() // no process-local watermark survives
		if initial {
			_, _, err = s.AddMembers(ctx, "s", "c", []string{"u"})
		} else {
			_, _, err = s.RemoveMembers(ctx, "s", "c", []string{"u"})
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := len(collapseMembers(rows)); (got == 1) != initial || int64(got) != c.MemberCount {
			t.Fatalf("durable=%v summary=%+v", rows, c)
		}
		// Same operation retry repairs/retains the actual durable count.
		if initial {
			_, _, err = s.AddMembers(ctx, "s", "c", []string{"u"})
		} else {
			_, _, err = s.RemoveMembers(ctx, "s", "c", []string{"u"})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memberVersionAbove(math.MaxInt64, 0); err == nil {
		t.Fatal("watermark wrapped")
	}
}

func TestR3PartialMemberWriteRetry(t *testing.T) {
	c := Cohort{CohortID: "c", SiteID: "s", Name: "members", Rule: staticRule, UpdatedAt: 1000}
	var rows []memberRecord
	s := NewService(nil)
	s.clock = func() int64 { return 900 }
	s.summaryRead = func(context.Context, string, string) (*Cohort, error) { copy := c; return &copy, nil }
	s.summaryWrite = func(_ context.Context, next Cohort) error { c = next; return nil }
	s.memberRead = func(context.Context, string, string) ([]memberRecord, error) { return rows, nil }
	failed := false
	s.memberWrite = func(_ context.Context, _, _ string, next []memberRow, v int64) error {
		for _, r := range next {
			rows = append(rows, memberRecord{EntityID: r.id, AddedAt: r.addedAt, Removed: r.removed, Version: v})
			if !failed {
				failed = true
				return errors.New("partial chunk fault")
			}
		}
		return nil
	}
	ctx := context.Background()
	if _, _, err := s.AddMembers(ctx, "s", "c", []string{"a", "b"}); err == nil {
		t.Fatal("missing fault")
	}
	if c.MemberCount != 0 || len(collapseMembers(rows)) != 1 {
		t.Fatal("bad failure schedule")
	}
	if _, _, err := s.AddMembers(ctx, "s", "c", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range collapseMembers(rows) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"a", "b"}) || c.MemberCount != 2 {
		t.Fatalf("%v %+v", ids, c)
	}
}
