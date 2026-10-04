package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func TestCohortErrorStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{cohorts.ErrNotFound, http.StatusNotFound},
		{fmt.Errorf("x: %w", cohorts.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("%w: bad", cohorts.ErrInvalidDefinition), http.StatusUnprocessableEntity},
		{fmt.Errorf("%w: big", cohorts.ErrTooLarge), http.StatusUnprocessableEntity},
		{fmt.Errorf("%w: many", cohorts.ErrTooManyIDs), http.StatusUnprocessableEntity},
		{fmt.Errorf("%w: rule", cohorts.ErrNotStatic), http.StatusUnprocessableEntity},
		{errors.New("pq: connection refused dsn=secret"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		status, msg := cohortErrorStatus(tc.err)
		if status != tc.status {
			t.Errorf("%v: status %d want %d", tc.err, status, tc.status)
		}
		if status == http.StatusServiceUnavailable && strings.Contains(msg, "secret") {
			t.Errorf("raw error leaked: %q", msg)
		}
	}
	// Typed handlers surface the same statuses.
	var app *neutron.AppError
	if !errors.As(cohortHTTPError(cohorts.ErrNotFound), &app) || app.Status != 404 {
		t.Errorf("typed not found: %v", app)
	}
	if !errors.As(cohortHTTPError(fmt.Errorf("%w: x", cohorts.ErrInvalidDefinition)), &app) || app.Status != 422 {
		t.Errorf("typed invalid: %v", app)
	}
	if !errors.As(cohortHTTPError(errors.New("boom")), &app) || app.Status != 503 {
		t.Errorf("typed other: %v", app)
	}
	// A query-admission refusal keeps its own status.
	ref := queryguard.NewLimiter(1, 1)
	rel, _ := ref.Acquire(context.Background(), "s")
	defer rel()
	_, err := ref.Acquire(context.Background(), "s")
	if !errors.As(cohortHTTPError(err), &app) || app.Status != 429 {
		t.Errorf("refusal: %v", app)
	}
}

// do runs a handler built with a nil service: every case below must be
// decided before the service is touched.
func do(h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.SetPathValue("cohort_id", "c1")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestStaticHandlersRejectBeforeService(t *testing.T) {
	var svc *cohorts.Service
	cases := []struct {
		name   string
		h      http.HandlerFunc
		target string
		body   string
		status int
	}{
		{"create: invalid json", createStaticCohortHandler(svc), "/", "{", 400},
		{"create: unknown field", createStaticCohortHandler(svc), "/", `{"site_id":"s","name":"n","ids":[],"evil":1}`, 400},
		{"create: no site", createStaticCohortHandler(svc), "/", `{"name":"n","ids":["a"]}`, 400},
		{"create: no name", createStaticCohortHandler(svc), "/", `{"site_id":"s","ids":["a"]}`, 400},
		{"create: trailing doc", createStaticCohortHandler(svc), "/", `{"site_id":"s","name":"n"}{}`, 400},
		{"create: body too large", createStaticCohortHandler(svc), "/", `{"site_id":"s","name":"n","ids":["` + strings.Repeat("a", staticBodyLimit) + `"]}`, 413},
		{"add: no site", addCohortMembersHandler(svc), "/", `{"ids":["a"]}`, 400},
		{"remove: no site", removeCohortMembersHandler(svc), "/", `{"ids":["a"]}`, 400},
		{"import: no site", importCohortMembersHandler(svc), "/", "a\nb\n", 400},
		{"csv-create: no name", createStaticFromCSVHandler(svc), "/?site_id=s", "a\n", 400},
		{"csv-create: no site", createStaticFromCSVHandler(svc), "/?name=n", "a\n", 400},
		{"import: id too long", importCohortMembersHandler(svc), "/?site_id=s", strings.Repeat("x", 300) + "\n", 422},
		{"import: body too large", importCohortMembersHandler(svc), "/?site_id=s", strings.Repeat("a\n", staticBodyLimit/2+1), 413},
	}
	for _, tc := range cases {
		rec := do(tc.h, "POST", tc.target, tc.body)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d want %d (%s)", tc.name, rec.Code, tc.status, strings.TrimSpace(rec.Body.String()))
		}
	}
}

func TestStaticImportOverCapIs422(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= cohorts.MaxStaticMembers; i++ {
		fmt.Fprintf(&b, "u%d\n", i)
	}
	rec := do(importCohortMembersHandler(nil), "POST", "/?site_id=s", b.String())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
