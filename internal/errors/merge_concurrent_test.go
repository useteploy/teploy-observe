package errors

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Concurrent opposite merges must never both commit (a cycle).
func TestConcurrentOppositeMergesNeverCycle(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 200; round++ {
		a, b := fmt.Sprintf("a%d", round), fmt.Sprintf("b%d", round)
		f := newFakeStore(map[string][]string{"S": {a, b}})
		s := svcWith(f)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.MergeIssues(ctx, "S", a, b, "u") }()
		go func() { defer wg.Done(); _ = s.MergeIssues(ctx, "S", b, a, "u") }()
		wg.Wait()
		if len(f.merges["S"]) != 1 {
			t.Fatalf("round %d: merges=%v, want exactly one", round, f.merges["S"])
		}
	}
}

func TestMergeWithoutStateIsAnErrorNotAPanic(t *testing.T) {
	s := &IssueService{}
	ctx := context.Background()
	if err := s.MergeIssues(ctx, "S", "a", "b", "u"); !errors.Is(err, ErrMergeUnavailable) {
		t.Fatalf("merge: %v", err)
	}
	if err := s.UnmergeIssue(ctx, "S", "a", "u"); !errors.Is(err, ErrMergeUnavailable) {
		t.Fatalf("unmerge: %v", err)
	}
	if err := s.AssignIssue(ctx, "S", "a", "x", "u"); !errors.Is(err, ErrMergeUnavailable) {
		t.Fatalf("assign: %v", err)
	}
}
