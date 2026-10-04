package query

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// These tests cover the cohort_id plumbing of the funnel, funnel-breakdown
// and retention paths (cohort_filter.go) without a database: every failure
// mode of cohort resolution must be decided BEFORE the first events read,
// so a nil-db StatsService is enough to prove the request fails closed.

func depthSvc(r CohortResolver) *StatsService {
	return NewStatsService(nil).WithCohortResolver(r)
}

var depthSteps = []FunnelStep{{Type: "event", Value: "a"}, {Type: "event", Value: "b"}}

// consumers runs the same cohort-filtered request through each consumer.
func consumers(svc *StatsService, cohortID string) map[string]error {
	ctx := context.Background()
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	out := map[string]error{}
	_, out["funnel"] = svc.FunnelWithOptions(ctx, "site-a", from, to, depthSteps, FunnelOptions{CohortID: cohortID})
	_, out["breakdown"] = svc.FunnelByBreakdownWithOptions(ctx, "site-a", from, to, depthSteps, "browser", 1, FunnelOptions{CohortID: cohortID})
	_, out["retention"] = svc.RetentionWithOptions(ctx, "site-a", from, to, 1, RetentionOptions{CohortID: cohortID})
	return out
}

func TestCohortConsumersFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"resolver failure is 503", errors.New("store down: secret-dsn"), http.StatusServiceUnavailable},
		{"too large is 422", fmt.Errorf("%w: 99999 members", cohorts.ErrTooLarge), http.StatusUnprocessableEntity},
		{"missing or foreign cohort is 404", cohorts.ErrNotFound, http.StatusNotFound},
		{"wrapped not found is 404", fmt.Errorf("get: %w", cohorts.ErrNotFound), http.StatusNotFound},
		{"unreadable stored rule is 503", fmt.Errorf("invalid cohort rule json: boom"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		svc := depthSvc(func(context.Context, string, string) ([]string, error) { return nil, tc.err })
		for name, err := range consumers(svc, "c1") {
			if err == nil {
				t.Errorf("%s/%s: no error - the cohort filter was dropped", tc.name, name)
				continue
			}
			app := appErr(t, err)
			if app.Status != tc.status {
				t.Errorf("%s/%s: status %d, want %d", tc.name, name, app.Status, tc.status)
			}
			if strings.Contains(app.Detail, "secret-dsn") {
				t.Errorf("%s/%s: raw error leaked: %q", tc.name, name, app.Detail)
			}
		}
	}
}

// The IDOR shape: the resolver is handed the REQUEST's site, so a cohort id
// belonging to another site resolves to ErrNotFound (cohorts.Service.Get is
// site-scoped) and the endpoint answers 404 for every consumer.
func TestCohortConsumersPassRequestSiteToResolver(t *testing.T) {
	var gotSite, gotCohort atomic.Value
	svc := depthSvc(func(_ context.Context, site, cohort string) ([]string, error) {
		gotSite.Store(site)
		gotCohort.Store(cohort)
		return nil, cohorts.ErrNotFound
	})
	for name, err := range consumers(svc, "other-sites-cohort") {
		if appErr(t, err).Status != http.StatusNotFound {
			t.Errorf("%s: want 404, got %v", name, err)
		}
		if gotSite.Load() != "site-a" || gotCohort.Load() != "other-sites-cohort" {
			t.Errorf("%s: resolver saw site=%v cohort=%v", name, gotSite.Load(), gotCohort.Load())
		}
	}
	// And the stats routes (resolveFilters) agree.
	_, err := StatsInput{SiteID: "site-a", CohortID: "other-sites-cohort"}.resolveFilters(context.Background(), svc)
	if appErr(t, err).Status != http.StatusNotFound {
		t.Errorf("resolveFilters: want 404, got %v", err)
	}
}

// Cohort-filtered queries go through the query guard: with the concurrency
// slots exhausted the request is refused (429) before the cohort is even
// resolved, on every consumer.
func TestCohortConsumersAreGuarded(t *testing.T) {
	var resolved atomic.Int32
	svc := depthSvc(func(context.Context, string, string) ([]string, error) {
		resolved.Add(1)
		return []string{"u1"}, nil
	})
	lim := queryguard.NewLimiter(1, 1)
	svc.WithQueryGuard(lim, queryguard.DefaultBudgets())
	release, err := lim.Acquire(context.Background(), "site-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	for name, err := range consumers(svc, "c1") {
		var ref *queryguard.Refusal
		if !errors.As(err, &ref) || ref.Status != http.StatusTooManyRequests {
			t.Errorf("%s: want a 429 refusal, got %v", name, err)
		}
	}
	// The stats-route resolution takes a slot too.
	_, err = StatsInput{SiteID: "site-a", CohortID: "c1"}.resolveFilters(context.Background(), svc)
	if appErr(t, err).Status != http.StatusTooManyRequests {
		t.Errorf("resolveFilters under load: %v", err)
	}
	if n := resolved.Load(); n != 0 {
		t.Errorf("resolver ran %d times while the guard was refusing", n)
	}
}

func TestResolveFiltersTakesAndReleasesSlot(t *testing.T) {
	svc := depthSvc(func(context.Context, string, string) ([]string, error) { return []string{"u1", "u2"}, nil })
	lim := queryguard.NewLimiter(1, 1)
	svc.WithQueryGuard(lim, queryguard.DefaultBudgets())
	for i := 0; i < 3; i++ { // a leaked slot would refuse the second call
		fb, err := StatsInput{SiteID: "site-a", CohortID: "c1"}.resolveFilters(context.Background(), svc)
		if err != nil || !strings.Contains(fb.SQL(), "distinct_id IN") {
			t.Fatalf("call %d: fb=%q err=%v", i, fb.SQL(), err)
		}
	}
	// A failing resolver releases the slot as well.
	svc = depthSvc(func(context.Context, string, string) ([]string, error) { return nil, errors.New("x") })
	svc.WithQueryGuard(lim, queryguard.DefaultBudgets())
	for i := 0; i < 3; i++ {
		_, err := StatsInput{SiteID: "site-a", CohortID: "c1"}.resolveFilters(context.Background(), svc)
		if appErr(t, err).Status != http.StatusServiceUnavailable {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestResolveCohortSet(t *testing.T) {
	ctx := context.Background()
	// No cohort: no filter.
	set, err := depthSvc(nil).resolveCohortSet(ctx, "s", "")
	if set != nil || err != nil {
		t.Fatalf("no cohort: %v %v", set, err)
	}
	// Unwired resolver with a cohort id: match nothing, never unfiltered.
	set, err = depthSvc(nil).resolveCohortSet(ctx, "s", "c")
	if err != nil || set == nil || len(set) != 0 {
		t.Fatalf("unwired: %v %v", set, err)
	}
	// Resolver returning nil ids: empty non-nil set.
	set, err = depthSvc(func(context.Context, string, string) ([]string, error) { return nil, nil }).resolveCohortSet(ctx, "s", "c")
	if err != nil || set == nil || len(set) != 0 {
		t.Fatalf("nil ids: %v %v", set, err)
	}
	set, err = depthSvc(func(context.Context, string, string) ([]string, error) { return []string{"a", "b", "a"}, nil }).resolveCohortSet(ctx, "s", "c")
	if err != nil || len(set) != 2 || !set.has("a") || set.has("z") {
		t.Fatalf("members: %v %v", set, err)
	}
}

func TestMemberSetHas(t *testing.T) {
	var none memberSet
	if none.has("a") || none.has("") {
		t.Error("nil set admits nothing through has")
	}
	m := memberSet{"a": {}, "": {}}
	if !m.has("a") || m.has("b") {
		t.Error("membership")
	}
	if m.has("") {
		t.Error("an event with no distinct_id must never match a cohort")
	}
}

func TestCohortErrorMappingNotFound(t *testing.T) {
	app := appErr(t, cohortResolveError(fmt.Errorf("x: %w", cohorts.ErrNotFound)))
	if app.Status != http.StatusNotFound || strings.Contains(app.Detail, "x:") {
		t.Errorf("%d %q", app.Status, app.Detail)
	}
}
