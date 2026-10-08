package errors

// Issue merge/unmerge and assignment (migration 058).
//
// Semantics:
//   - A merge points a SOURCE issue at a TARGET. Reads of the source
//     resolve to the target (GetIssue), the source disappears from
//     listings, the target's counts/events include the source's, and NEW
//     events whose fingerprint maps to the source are attributed to the
//     target at ingest. Unmerge restores all of it; no history is
//     rewritten (old events keep their original issue_id).
//   - Authorization boundary: every operation names a site and both
//     issues must exist IN THAT SITE; a foreign or unknown id is
//     ErrIssueNotFound (never distinguishable from a missing one).
//   - Integrity: no self-merge, no cycles, chains bounded by
//     maxMergeDepth, a source can have only one active merge.
//
// Storage is behind mergeStore so the rules are unit-testable without
// Nucleus; sqlMergeStore is the live implementation (UNVERIFIED against
// live Nucleus in this environment).

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

const (
	maxMergeDepth         = 5
	maxAssignmentsPerSite = 10000
	maxAssigneeLen        = 128
	mergeCacheTTL         = 15 * time.Second
	mergeCacheMaxSites    = 1024
)

var (
	ErrIssueNotFound   = errors.New("issue not found")
	ErrMergeSelf       = errors.New("cannot merge an issue into itself")
	ErrMergeCycle      = errors.New("merge would create a cycle")
	ErrMergeDepth      = errors.New("merge chain too deep")
	ErrAlreadyMerged   = errors.New("issue is already merged; unmerge it first")
	ErrNotMerged       = errors.New("issue is not merged")
	ErrInvalidAssignee = errors.New("invalid assignee")
	// ErrMergeUnavailable: the service was built without merge state.
	ErrMergeUnavailable = errors.New("issue merge is not available")
)

// Assignment is the current assignee of an issue.
type Assignment struct {
	Assignee   string `json:"assignee"`
	AssignedAt int64  `json:"assigned_at"`
	AssignedBy string `json:"assigned_by"`
}

type mergeStore interface {
	issueExists(ctx context.Context, siteID, issueID string) (bool, error)
	// activeMerges returns source -> target for active merges of a site.
	activeMerges(ctx context.Context, siteID string) (map[string]string, error)
	putMerge(ctx context.Context, siteID, source, target string, active bool, by string) error
	assignments(ctx context.Context, siteID string) (map[string]Assignment, error)
	putAssignment(ctx context.Context, siteID, issueID, assignee, by string) error
}

type mergeCacheEntry struct {
	merges  map[string]string
	fetched time.Time
}

// mergeState is the IssueService's merge cache (single-process posture:
// in-process invalidation on write, TTL for anything else).
type mergeState struct {
	store    mergeStore
	mu       sync.Mutex
	cache    map[string]mergeCacheEntry
	revision uint64
	now      func() time.Time
	// writeLocks serialise merge/unmerge validate-then-write per site
	// (striped by site hash): two concurrent merges (A->B, B->A) each pass
	// cycle validation against the pre-write state and would otherwise both
	// commit, creating a cycle.
	writeLocks [64]sync.Mutex
}

func (m *mergeState) lockSite(siteID string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(siteID))
	mu := &m.writeLocks[h.Sum32()%uint32(len(m.writeLocks))]
	mu.Lock()
	return mu.Unlock
}

func newMergeState(store mergeStore) *mergeState {
	return &mergeState{store: store, cache: map[string]mergeCacheEntry{}, now: time.Now}
}

func (m *mergeState) invalidate(siteID string) {
	m.mu.Lock()
	delete(m.cache, siteID)
	m.revision++
	m.mu.Unlock()
}

// siteMerges returns the cached active-merge map. Read failure degrades
// to "no merges" (logged): ingest and reads must not fail because the
// merge table is unreadable.
func (m *mergeState) siteMerges(ctx context.Context, siteID string) map[string]string {
	m.mu.Lock()
	if e, ok := m.cache[siteID]; ok && m.now().Sub(e.fetched) < mergeCacheTTL {
		m.mu.Unlock()
		return e.merges
	}
	revision, fetched := m.revision, m.now()
	m.mu.Unlock()
	merges, err := m.store.activeMerges(ctx, siteID)
	if err != nil {
		slog.Warn("errors: merge lookup failed; treating as no merges", "site", siteID, "err", err)
		return nil
	}
	m.mu.Lock()
	if len(m.cache) >= mergeCacheMaxSites {
		m.cache = map[string]mergeCacheEntry{}
	}
	if m.revision == revision {
		m.cache[siteID] = mergeCacheEntry{merges: merges, fetched: fetched}
	}
	m.mu.Unlock()
	return merges
}

// resolveChain follows source -> target links to the final target.
// depth reports the hops; ok=false on a cycle or a chain over the limit
// (callers fall back to the unresolved id).
func resolveChain(merges map[string]string, id string) (final string, depth int, ok bool) {
	seen := map[string]bool{id: true}
	cur := id
	for {
		next, merged := merges[cur]
		if !merged {
			return cur, depth, true
		}
		if seen[next] || depth >= maxMergeDepth {
			return id, depth, false
		}
		seen[next] = true
		cur = next
		depth++
	}
}

// sourcesOf returns every issue id that resolves (transitively) to target.
func sourcesOf(merges map[string]string, target string) []string {
	var out []string
	for src := range merges {
		if src == target {
			continue
		}
		if final, _, ok := resolveChain(merges, src); ok && final == target {
			out = append(out, src)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveMerged maps an issue id to its merge target (itself when not
// merged). Hot path at ingest: cached.
func (s *IssueService) ResolveMerged(ctx context.Context, siteID, issueID string) string {
	if s.merge == nil {
		return issueID
	}
	final, _, ok := resolveChain(s.merge.siteMerges(ctx, siteID), issueID)
	if !ok {
		return issueID
	}
	return final
}

// MergeIssues merges source into target within one site.
func (s *IssueService) MergeIssues(ctx context.Context, siteID, sourceID, targetID, actor string) error {
	if sourceID == "" || targetID == "" || siteID == "" {
		return ErrIssueNotFound
	}
	if sourceID == targetID {
		return ErrMergeSelf
	}
	if s.merge == nil {
		return ErrMergeUnavailable
	}
	defer s.merge.lockSite(siteID)()
	// Both issues must exist in THIS site (IDOR boundary).
	for _, id := range []string{sourceID, targetID} {
		ok, err := s.merge.store.issueExists(ctx, siteID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIssueNotFound
		}
	}
	// Fresh read: the cache must not hide a concurrent merge from validation.
	s.merge.invalidate(siteID)
	merges, err := s.merge.store.activeMerges(ctx, siteID)
	if err != nil {
		return err
	}
	if _, merged := merges[sourceID]; merged {
		return ErrAlreadyMerged
	}
	finalTarget, _, ok := resolveChain(merges, targetID)
	if !ok {
		return ErrMergeDepth
	}
	if finalTarget == sourceID {
		return ErrMergeCycle
	}
	// Depth of the resulting chain: sources that already point at
	// source gain one hop.
	probe := make(map[string]string, len(merges)+1)
	for k, v := range merges {
		probe[k] = v
	}
	probe[sourceID] = finalTarget
	for src := range probe {
		if _, _, ok := resolveChain(probe, src); !ok {
			return ErrMergeDepth
		}
	}
	if err := s.merge.store.putMerge(ctx, siteID, sourceID, finalTarget, true, actor); err != nil {
		return err
	}
	s.merge.invalidate(siteID)
	return nil
}

// UnmergeIssue restores a merged source.
func (s *IssueService) UnmergeIssue(ctx context.Context, siteID, sourceID, actor string) error {
	if s.merge == nil {
		return ErrMergeUnavailable
	}
	defer s.merge.lockSite(siteID)()
	ok, err := s.merge.store.issueExists(ctx, siteID, sourceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueNotFound
	}
	merges, err := s.merge.store.activeMerges(ctx, siteID)
	if err != nil {
		return err
	}
	target, merged := merges[sourceID]
	if !merged {
		return ErrNotMerged
	}
	if err := s.merge.store.putMerge(ctx, siteID, sourceID, target, false, actor); err != nil {
		return err
	}
	s.merge.invalidate(siteID)
	return nil
}

// ValidateAssignee bounds the free-text assignee (” clears).
func ValidateAssignee(a string) error {
	if len(a) > maxAssigneeLen {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidAssignee, maxAssigneeLen)
	}
	for _, r := range a {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control characters", ErrInvalidAssignee)
		}
	}
	return nil
}

// AssignIssue sets (or, with ”, clears) an issue's assignee. An issue
// that is merged away is assigned via its target.
func (s *IssueService) AssignIssue(ctx context.Context, siteID, issueID, assignee, actor string) error {
	assignee = strings.TrimSpace(assignee)
	if err := ValidateAssignee(assignee); err != nil {
		return err
	}
	if s.merge == nil {
		return ErrMergeUnavailable
	}
	ok, err := s.merge.store.issueExists(ctx, siteID, issueID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueNotFound
	}
	issueID = s.ResolveMerged(ctx, siteID, issueID)
	return s.merge.store.putAssignment(ctx, siteID, issueID, assignee, actor)
}

// ---- live SQL store ------------------------------------------------------

type sqlMergeStore struct{ db *nucleus.Client }

func (s sqlMergeStore) issueExists(ctx context.Context, siteID, issueID string) (bool, error) {
	rows, err := nucleus.Query[issueCountRow](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS n FROM issues WHERE site_id = $1 AND issue_id = $2`, siteID, issueID)
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].N > 0, nil
}

func (s sqlMergeStore) activeMerges(ctx context.Context, siteID string) (map[string]string, error) {
	// One statement/row stream includes all accepted mappings, including legacy
	// sites over 10,000. No pagination snapshot or arbitrary row cap is needed.
	rows, err := s.db.Pool().Query(ctx,
		`SELECT tenant_id, source_issue_id, target_issue_id FROM (
			SELECT tenant_id, site_id, source_issue_id,
			       argMax(target_issue_id, version) AS target_issue_id,
			       argMax(active, version) AS active
			FROM issue_merges WHERE site_id = $1
			GROUP BY tenant_id, site_id, source_issue_id) AS m
		 WHERE active = 'true'
		 ORDER BY tenant_id ASC, source_issue_id ASC`, pgx.QueryExecModeSimpleProtocol, siteID)
	if err != nil {
		return nil, err
	}
	return readMergeMappings(rows)
}

// A failed scan/stream never publishes a partially populated cache entry.
func readMergeMappings(rows pgx.Rows) (map[string]string, error) {
	defer rows.Close()
	out := make(map[string]string)
	tenants := make(map[string]string)
	for rows.Next() {
		var tenant, source, target string
		if err := rows.Scan(&tenant, &source, &target); err != nil {
			return nil, err
		}
		// The application addresses issues by (site, issue). Refuse ambiguous
		// legacy tenant collisions instead of choosing a map overwrite winner.
		if prior, ok := tenants[source]; ok && prior != tenant {
			return nil, fmt.Errorf("merge source has ambiguous tenant identity")
		}
		tenants[source], out[source] = tenant, target
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type maxVersionRow struct {
	V int64 `db:"v"`
}

func (s sqlMergeStore) putMerge(ctx context.Context, siteID, source, target string, active bool, by string) error {
	rows, err := nucleus.Query[maxVersionRow](ctx, s.db.SQL(),
		`SELECT COALESCE(MAX(version), 0) AS v FROM issue_merges WHERE site_id = $1 AND source_issue_id = $2`,
		siteID, source)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	ver := now
	if len(rows) > 0 && rows[0].V >= ver {
		ver = rows[0].V + 1
	}
	flag := "false"
	if active {
		flag = "true"
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO issue_merges (tenant_id, site_id, source_issue_id, target_issue_id, active, merged_at, merged_by, version)
		 VALUES ('default', $1, $2, $3, $4, CAST($5 AS BIGINT), $6, CAST($7 AS BIGINT))`,
		siteID, source, target, flag, dbutil.IntParam(now), by, dbutil.IntParam(ver))
	return err
}

func (s sqlMergeStore) assignments(ctx context.Context, siteID string) (map[string]Assignment, error) {
	type row struct {
		IssueID    string `db:"issue_id"`
		Assignee   string `db:"assignee"`
		AssignedAt int64  `db:"assigned_at"`
		AssignedBy string `db:"assigned_by"`
	}
	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		`SELECT issue_id, assignee, assigned_at, assigned_by FROM (
			SELECT tenant_id, site_id, issue_id,
			       argMax(assignee, version) AS assignee,
			       argMax(assigned_at, version) AS assigned_at,
			       argMax(assigned_by, version) AS assigned_by
			FROM issue_assignments WHERE site_id = $1
			GROUP BY tenant_id, site_id, issue_id) AS a
		 WHERE assignee <> ''
		 LIMIT `+fmt.Sprint(maxAssignmentsPerSite), siteID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Assignment, len(rows))
	for _, r := range rows {
		out[r.IssueID] = Assignment{Assignee: r.Assignee, AssignedAt: r.AssignedAt, AssignedBy: r.AssignedBy}
	}
	return out, nil
}

func (s sqlMergeStore) putAssignment(ctx context.Context, siteID, issueID, assignee, by string) error {
	rows, err := nucleus.Query[maxVersionRow](ctx, s.db.SQL(),
		`SELECT COALESCE(MAX(version), 0) AS v FROM issue_assignments WHERE site_id = $1 AND issue_id = $2`,
		siteID, issueID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	ver := now
	if len(rows) > 0 && rows[0].V >= ver {
		ver = rows[0].V + 1
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO issue_assignments (tenant_id, site_id, issue_id, assignee, assigned_at, assigned_by, version)
		 VALUES ('default', $1, $2, $3, CAST($4 AS BIGINT), $5, CAST($6 AS BIGINT))`,
		siteID, issueID, assignee, dbutil.IntParam(now), by, dbutil.IntParam(ver))
	return err
}
