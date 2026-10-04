package query

import (
	"context"
)

// Cohort filtering for the heavy read paths (funnel, funnel breakdown,
// retention). The stats routes expand a cohort into a SQL
// `distinct_id IN (...)` clause (StatsInput.resolveFilters); these paths
// stream raw events instead, so the same resolved membership is applied as
// a Go-side set test on each streamed row's distinct_id.
//
// Same machinery, same limits: the membership comes from the same
// CohortResolver (cohorts.Service.MembersForFilter), so the
// cohorts.MaxFilterMembers cap (422), the not-found / foreign-site refusal
// (404) and the fail-closed mapping (cohortResolveError: 503, or the
// guard's own 429/504) are identical to the stats routes. The resolution
// runs INSIDE the query admission slot the caller already holds
// (beginQuery), so a cohort-filtered funnel is one admitted, time-budgeted
// query, not an admitted query plus an unguarded cohort evaluation.

// memberSet is a resolved cohort membership: a nil set means "no cohort
// filter"; a non-nil (possibly empty) set admits only its ids.
type memberSet map[string]struct{}

// has reports whether an event with this distinct_id passes the filter.
// An event with no distinct_id never belongs to a cohort.
func (m memberSet) has(distinctID string) bool {
	if distinctID == "" {
		return false
	}
	_, ok := m[distinctID]
	return ok
}

// resolveCohortSet resolves cohortID to a membership set under ctx (the
// caller's already-admitted, budget-bounded context). An empty cohortID is
// no filter (nil set, nil error). A missing resolver, a missing cohort or
// an empty cohort yield an empty non-nil set - match nothing, never
// unfiltered. Failures are mapped (cohortResolveError) and never fall back
// to unfiltered.
func (s *StatsService) resolveCohortSet(ctx context.Context, siteID, cohortID string) (memberSet, error) {
	if cohortID == "" {
		return nil, nil
	}
	ids, err := s.ResolveCohort(ctx, siteID, cohortID)
	if err != nil {
		return nil, cohortResolveFailure(ctx, siteID, cohortID, err)
	}
	set := make(memberSet, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// resolveCohortAdmitted is resolveCohortSet for callers that do NOT already
// hold an admission slot (the stats-route resolveFilters): it takes one
// for the duration of the resolution so cohort evaluation is subject to the
// same concurrency and time budgets as the heavy paths. With no guard and
// no timeout configured (a bare StatsService in tests) it resolves directly.
func (s *StatsService) resolveCohortAdmitted(ctx context.Context, siteID, cohortID string) ([]string, error) {
	if s.guard == nil && s.budgets.Timeout <= 0 {
		return s.ResolveCohort(ctx, siteID, cohortID)
	}
	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()
	ids, err := s.ResolveCohort(qctx, siteID, cohortID)
	if err != nil {
		return nil, s.scanDeadlineError(ctx, err)
	}
	return ids, nil
}
