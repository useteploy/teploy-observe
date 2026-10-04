package errors

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
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

// attributeMerged applies one event to a merge TARGET: bumps it (last_seen,
// regression reopen) and, when the target was resolved, sends the
// regression notification for the target. The hidden source is never
// bumped or notified. Returns the target id.
func (s *IssueService) attributeMerged(ctx context.Context, siteID, target, release string, ts int64) string {
	lc := s.lifecycle()
	t, err := lc.issueByID(ctx, siteID, target)
	count := s.mergedCount(ctx, lc, siteID, target) + 1
	_ = lc.bump(ctx, target, siteID, ts, count)
	if err == nil && t != nil && t.Status == "resolved" {
		s.notify(ctx, IssueEvent{Kind: IssueEventRegression, SiteID: siteID, IssueID: target,
			Title: t.Title, Culprit: t.Culprit, Level: t.Level, Release: release, EventCount: count})
	}
	return target
}

const (
	scopeCountTTL      = time.Minute
	scopeCountMaxItems = 4096
)

type scopeCountEntry struct {
	n       int64
	fetched time.Time
}

// scopeCountCache bounds the COUNT(*) over error_events that attributing an
// event to a merged issue used to run once per ingested event: the exact
// count is re-read at most once per TTL per target and incremented
// in-process in between.
type scopeCountCache struct {
	mu  sync.Mutex
	m   map[string]scopeCountEntry
	now func() time.Time
}

func (s *IssueService) mergedCount(ctx context.Context, lc lifecycleStore, siteID, target string) int64 {
	c := &s.counts
	key := siteID + "\x00" + target
	now := time.Now
	c.mu.Lock()
	if c.now != nil {
		now = c.now
	}
	if e, ok := c.m[key]; ok && now().Sub(e.fetched) < scopeCountTTL {
		e.n++
		c.m[key] = e
		c.mu.Unlock()
		return e.n - 1
	}
	c.mu.Unlock()
	n := lc.scopeCount(ctx, siteID, s.issueScope(ctx, siteID, target))
	c.mu.Lock()
	if c.m == nil || len(c.m) >= scopeCountMaxItems {
		c.m = map[string]scopeCountEntry{}
	}
	// n counts events stored so far; the caller adds the current one.
	c.m[key] = scopeCountEntry{n: n + 1, fetched: now()}
	c.mu.Unlock()
	return n
}

// issueScope is [issue, merged sources...] (capped), the id set whose
// events count towards the issue.
func (s *IssueService) issueScope(ctx context.Context, siteID, issueID string) []string {
	return append([]string{issueID}, capIDs(sourcesOf(s.siteMerges(ctx, siteID), issueID))...)
}
