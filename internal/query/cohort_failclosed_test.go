package query

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/cohorts"
)

func statsWithResolver(r CohortResolver) *StatsService {
	return (&StatsService{}).WithCohortResolver(r)
}

func appErr(t *testing.T, err error) *neutron.AppError {
	t.Helper()
	var app *neutron.AppError
	if !errors.As(err, &app) {
		t.Fatalf("not an AppError: %v", err)
	}
	return app
}

// A failing resolver must fail the request, never drop the cohort filter.
func TestResolveFiltersFailsClosed(t *testing.T) {
	svc := statsWithResolver(func(ctx context.Context, siteID, cohortID string) ([]string, error) {
		return nil, errors.New("store down: secret-dsn")
	})
	in := StatsInput{SiteID: "s", CohortID: "c1"}
	fb, err := in.resolveFilters(context.Background(), svc)
	if err == nil || fb != nil {
		t.Fatalf("want error and nil builder, got fb=%v err=%v", fb, err)
	}
	app := appErr(t, err)
	if app.Status != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", app.Status)
	}
	if strings.Contains(app.Detail, "secret-dsn") {
		t.Errorf("raw resolver error leaked to client: %q", app.Detail)
	}
}

func TestResolveFiltersTooLargeIs422(t *testing.T) {
	svc := statsWithResolver(func(ctx context.Context, siteID, cohortID string) ([]string, error) {
		return nil, fmt.Errorf("%w: 99999 members", cohorts.ErrTooLarge)
	})
	_, err := StatsInput{SiteID: "s", CohortID: "c1"}.resolveFilters(context.Background(), svc)
	if got := appErr(t, err).Status; got != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422", got)
	}
}

func TestResolveFiltersOK(t *testing.T) {
	svc := statsWithResolver(func(ctx context.Context, siteID, cohortID string) ([]string, error) {
		return []string{"a", "b"}, nil
	})
	fb, err := StatsInput{SiteID: "s", CohortID: "c1"}.resolveFilters(context.Background(), svc)
	if err != nil || fb.SQL() == "" {
		t.Fatalf("fb=%q err=%v", fb.SQL(), err)
	}
	// No cohort: no resolver call, no error.
	fb, err = StatsInput{SiteID: "s"}.resolveFilters(context.Background(), svc)
	if err != nil || fb.SQL() != "" {
		t.Fatalf("no-cohort fb=%q err=%v", fb.SQL(), err)
	}
	// Missing cohort (nil ids) stays match-nothing, not unfiltered.
	svc = statsWithResolver(func(ctx context.Context, siteID, cohortID string) ([]string, error) { return nil, nil })
	fb, _ = StatsInput{SiteID: "s", CohortID: "gone"}.resolveFilters(context.Background(), svc)
	if fb.SQL() != " AND 1 = 0" {
		t.Errorf("missing cohort SQL = %q", fb.SQL())
	}
}
