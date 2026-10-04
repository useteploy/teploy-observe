package errors

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Read-side helpers for merge/assignment (see merge.go).

const maxScopeIDs = 50

func capIDs(ids []string) []string {
	if len(ids) > maxScopeIDs-1 {
		ids = ids[:maxScopeIDs-1]
	}
	return ids
}

// issueIDClause renders `issue_id IN ($n, ...)` for ids (an empty list
// matches nothing), with parameters numbered from start.
func issueIDClause(start int, ids []string) (string, []any) {
	if len(ids) == 0 {
		return "1 = 0", nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", start+i)
		args[i] = id
	}
	return "issue_id IN (" + strings.Join(ph, ", ") + ")", args
}

func (s *IssueService) siteMerges(ctx context.Context, siteID string) map[string]string {
	if s.merge == nil {
		return nil
	}
	return s.merge.siteMerges(ctx, siteID)
}

func (s *IssueService) siteAssignments(ctx context.Context, siteID string) map[string]Assignment {
	if s.merge == nil {
		return nil
	}
	m, err := s.merge.store.assignments(ctx, siteID)
	if err != nil {
		slog.Warn("errors: assignment lookup failed", "site", siteID, "err", err)
		return nil
	}
	return m
}

// attributeMerged redirects an ingest-time issue id to its merge target
// and bumps the target (last_seen, regression reopen) the way ResolveIssue
// did for the source. Unmerged issues pass through untouched.
func (s *IssueService) attributeMerged(ctx context.Context, siteID, issueID string, ts int64) string {
	target := s.ResolveMerged(ctx, siteID, issueID)
	if target == issueID {
		return issueID
	}
	count := s.eventCount(ctx, siteID, s.issueScope(ctx, siteID, target)) + 1
	_ = s.bumpIssue(ctx, target, siteID, ts, count)
	return target
}

// issueScope is [issue, merged sources...] (capped), the id set whose
// events count towards the issue.
func (s *IssueService) issueScope(ctx context.Context, siteID, issueID string) []string {
	return append([]string{issueID}, capIDs(sourcesOf(s.siteMerges(ctx, siteID), issueID))...)
}
