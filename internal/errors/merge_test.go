package errors

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeMergeStore is an in-memory mergeStore: issues are (site, id) pairs.
type fakeMergeStore struct {
	mu      sync.Mutex
	issues  map[string]bool // site+"/"+id
	merges  map[string]map[string]string
	assigns map[string]map[string]Assignment
	reads   int
}

func newFakeStore(siteIssues map[string][]string) *fakeMergeStore {
	f := &fakeMergeStore{issues: map[string]bool{}, merges: map[string]map[string]string{}, assigns: map[string]map[string]Assignment{}}
	for site, ids := range siteIssues {
		for _, id := range ids {
			f.issues[site+"/"+id] = true
		}
	}
	return f
}

func (f *fakeMergeStore) issueExists(_ context.Context, site, id string) (bool, error) {
	return f.issues[site+"/"+id], nil
}

func (f *fakeMergeStore) activeMerges(_ context.Context, site string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	out := map[string]string{}
	for k, v := range f.merges[site] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeMergeStore) putMerge(_ context.Context, site, src, tgt string, active bool, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.merges[site] == nil {
		f.merges[site] = map[string]string{}
	}
	if active {
		f.merges[site][src] = tgt
	} else {
		delete(f.merges[site], src)
	}
	return nil
}

func (f *fakeMergeStore) assignments(_ context.Context, site string) (map[string]Assignment, error) {
	return f.assigns[site], nil
}

func (f *fakeMergeStore) putAssignment(_ context.Context, site, id, who, by string) error {
	if f.assigns[site] == nil {
		f.assigns[site] = map[string]Assignment{}
	}
	f.assigns[site][id] = Assignment{Assignee: who, AssignedBy: by}
	return nil
}

func svcWith(f *fakeMergeStore) *IssueService {
	return &IssueService{merge: newMergeState(f)}
}

func TestMergeRulesAndResolution(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore(map[string][]string{"A": {"i1", "i2", "i3", "i4", "i5", "i6", "i7"}, "B": {"x1"}})
	s := svcWith(f)

	if err := s.MergeIssues(ctx, "A", "i1", "i1", "u"); !errors.Is(err, ErrMergeSelf) {
		t.Fatalf("self-merge: %v", err)
	}
	if err := s.MergeIssues(ctx, "A", "i1", "i2", "u"); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveMerged(ctx, "A", "i1"); got != "i2" {
		t.Fatalf("resolve = %s", got)
	}
	if got := s.ResolveMerged(ctx, "A", "i2"); got != "i2" {
		t.Fatalf("target must resolve to itself, got %s", got)
	}
	// Cycle: i2 -> i1 while i1 -> i2.
	if err := s.MergeIssues(ctx, "A", "i2", "i1", "u"); !errors.Is(err, ErrMergeCycle) {
		t.Fatalf("cycle: %v", err)
	}
	// Already merged source.
	if err := s.MergeIssues(ctx, "A", "i1", "i3", "u"); !errors.Is(err, ErrAlreadyMerged) {
		t.Fatalf("double merge: %v", err)
	}
	// Merging into a merged target flattens to the final target.
	if err := s.MergeIssues(ctx, "A", "i3", "i1", "u"); err != nil {
		t.Fatal(err)
	}
	if f.merges["A"]["i3"] != "i2" {
		t.Fatalf("not flattened: %v", f.merges["A"])
	}
	srcs := sourcesOf(f.merges["A"], "i2")
	if len(srcs) != 2 {
		t.Fatalf("sources = %v", srcs)
	}
	// Unmerge restores.
	if err := s.UnmergeIssue(ctx, "A", "i1", "u"); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveMerged(ctx, "A", "i1"); got != "i1" {
		t.Fatalf("after unmerge resolve = %s", got)
	}
	if err := s.UnmergeIssue(ctx, "A", "i1", "u"); !errors.Is(err, ErrNotMerged) {
		t.Fatalf("unmerge twice: %v", err)
	}
}

func TestMergeDepthLimit(t *testing.T) {
	ctx := context.Background()
	ids := []string{"n0", "n1", "n2", "n3", "n4", "n5", "n6", "n7"}
	f := newFakeStore(map[string][]string{"A": ids})
	s := svcWith(f)
	// Build a raw chain n0->n1->...->n6 beyond the limit, bypassing the
	// service, to prove resolution and new merges both stay bounded.
	f.merges["A"] = map[string]string{}
	for i := 0; i < 6; i++ {
		f.merges["A"][ids[i]] = ids[i+1]
	}
	if _, _, ok := resolveChain(f.merges["A"], "n0"); ok {
		t.Fatal("over-deep chain must not resolve")
	}
	if got := s.ResolveMerged(ctx, "A", "n0"); got != "n0" {
		t.Fatalf("over-deep chain resolved to %s (must fall back to the id itself)", got)
	}
	if err := s.MergeIssues(ctx, "A", "n7", "n0", "u"); !errors.Is(err, ErrMergeDepth) {
		t.Fatalf("merge into over-deep chain: %v", err)
	}
}

// IDOR: every operation is scoped to the named site. A foreign or unknown
// id is indistinguishable (ErrIssueNotFound) and writes nothing.
func TestMergeAssignCrossSiteIDOR(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore(map[string][]string{"A": {"a1", "a2"}, "B": {"b1", "b2"}})
	s := svcWith(f)

	cases := []struct {
		name string
		err  error
	}{
		{"merge foreign target", s.MergeIssues(ctx, "A", "a1", "b1", "u")},
		{"merge foreign source", s.MergeIssues(ctx, "A", "b1", "a1", "u")},
		{"merge both foreign", s.MergeIssues(ctx, "A", "b1", "b2", "u")},
		{"unmerge foreign", s.UnmergeIssue(ctx, "A", "b1", "u")},
		{"assign foreign", s.AssignIssue(ctx, "A", "b1", "eve", "u")},
		{"merge unknown", s.MergeIssues(ctx, "A", "a1", "nope", "u")},
	}
	for _, c := range cases {
		if !errors.Is(c.err, ErrIssueNotFound) {
			t.Errorf("%s: want ErrIssueNotFound, got %v", c.name, c.err)
		}
	}
	if len(f.merges) != 0 || len(f.assigns) != 0 {
		t.Fatalf("cross-site attempts wrote state: %v %v", f.merges, f.assigns)
	}
	// A merge in site B never affects resolution in site A.
	if err := s.MergeIssues(ctx, "B", "b1", "b2", "u"); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveMerged(ctx, "A", "b1"); got != "b1" {
		t.Fatalf("site A resolved a site B merge: %s", got)
	}
}

func TestAssignIssue(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore(map[string][]string{"A": {"a1", "a2"}})
	s := svcWith(f)
	if err := s.AssignIssue(ctx, "A", "a1", "  alice@example.com ", "bob"); err != nil {
		t.Fatal(err)
	}
	if a := f.assigns["A"]["a1"]; a.Assignee != "alice@example.com" || a.AssignedBy != "bob" {
		t.Fatalf("assignment = %+v", a)
	}
	if err := s.AssignIssue(ctx, "A", "a1", "", "bob"); err != nil { // clear
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Repeat("x", maxAssigneeLen+1), "a\x00b", "a\nb"} {
		if err := s.AssignIssue(ctx, "A", "a1", bad, "bob"); !errors.Is(err, ErrInvalidAssignee) {
			t.Errorf("assignee %q accepted: %v", bad, err)
		}
	}
	// Assigning a merged source lands on its target.
	if err := s.MergeIssues(ctx, "A", "a1", "a2", "u"); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignIssue(ctx, "A", "a1", "carol", "u"); err != nil {
		t.Fatal(err)
	}
	if f.assigns["A"]["a2"].Assignee != "carol" {
		t.Fatalf("assignment not redirected: %v", f.assigns["A"])
	}
}

func TestMergeCacheInvalidatedOnWrite(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore(map[string][]string{"A": {"a1", "a2"}})
	s := svcWith(f)
	if got := s.ResolveMerged(ctx, "A", "a1"); got != "a1" { // primes cache (negative)
		t.Fatal(got)
	}
	if err := s.MergeIssues(ctx, "A", "a1", "a2", "u"); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveMerged(ctx, "A", "a1"); got != "a2" {
		t.Fatalf("stale cache after merge: %s", got)
	}
	before := f.reads
	for i := 0; i < 50; i++ {
		s.ResolveMerged(ctx, "A", "a1")
	}
	if f.reads != before {
		t.Fatalf("hot path re-read the store %d times", f.reads-before)
	}
	if err := s.UnmergeIssue(ctx, "A", "a1", "u"); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveMerged(ctx, "A", "a1"); got != "a1" {
		t.Fatalf("stale cache after unmerge: %s", got)
	}
}

func TestIssueIDClause(t *testing.T) {
	in, args := issueIDClause(2, []string{"a", "b"})
	if in != "issue_id IN ($2, $3)" || len(args) != 2 {
		t.Fatalf("%q %v", in, args)
	}
	if in, _ := issueIDClause(2, nil); in != "1 = 0" {
		t.Fatal(in)
	}
}
