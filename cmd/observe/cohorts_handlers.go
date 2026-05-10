package main

// Cohorts API (C2 Wave 4) — behavioural grouping built on the events
// table. Routes live here so the integration into main.go is a single
// RegisterCohortsRoutes call (matches the boards / metrics / persons
// convention).

import (
	"context"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/cohorts"
)

// RegisterCohortsRoutes wires cohort CRUD + evaluation onto the router.
// Editor + Admin can mutate; Viewer (JWT only) can read + preview.
func RegisterCohortsRoutes(
	r *neutron.Router,
	jwtMW neutron.Middleware,
	requireEditor neutron.Middleware,
	svc *cohorts.Service,
) {
	read := r.Group("/api/v1/cohorts", jwtMW)
	write := read.Group("", requireEditor)

	neutron.Get(read, "", listCohortsHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("List cohorts for a site"),
	)
	neutron.Post(write, "", createCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Create a cohort"),
	)
	// /preview is editor-only because evaluation runs the same SQL as
	// a save and we want the same RBAC posture for both.
	neutron.Post(write, "/preview", previewCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Preview a cohort rule (no save) — returns count + sample"),
	)
	neutron.Get(read, "/{cohort_id}", getCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Get cohort detail"),
	)
	neutron.Get(read, "/{cohort_id}/members", cohortMembersHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Paginated cohort membership"),
	)
	neutron.Put(write, "/{cohort_id}", updateCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Update a cohort"),
	)
	neutron.Delete(write, "/{cohort_id}", deleteCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Delete a cohort"),
	)
	neutron.Post(write, "/{cohort_id}/refresh", refreshCohortHandler(svc),
		neutron.WithTags("cohorts"),
		neutron.WithSummary("Re-evaluate a cohort and update member_count"),
	)
}

// ---------------------------------------------------------------------------
// Inputs / outputs
// ---------------------------------------------------------------------------

type listCohortsInput struct {
	SiteID string `query:"site_id"`
}

type createCohortInput struct {
	SiteID      string             `json:"site_id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Rule        cohorts.Definition `json:"rule"`
}

type updateCohortInput struct {
	CohortID    string             `path:"cohort_id"`
	SiteID      string             `json:"site_id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Rule        cohorts.Definition `json:"rule"`
}

type cohortIDInput struct {
	CohortID string `path:"cohort_id"`
	SiteID   string `query:"site_id"`
}

type cohortMembersInput struct {
	CohortID string `path:"cohort_id"`
	SiteID   string `query:"site_id"`
	Limit    int    `query:"limit"`
	Offset   int    `query:"offset"`
}

type cohortMembersResult struct {
	Members []string `json:"members"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
}

type previewCohortInput struct {
	SiteID string             `json:"site_id"`
	Rule   cohorts.Definition `json:"rule"`
}

// previewResult shows the count + a small sample so the UI can render
// "X people match" without paginating the full list.
type previewResult struct {
	Count  int      `json:"count"`
	Sample []string `json:"sample"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func listCohortsHandler(svc *cohorts.Service) neutron.HandlerFunc[listCohortsInput, []cohorts.Cohort] {
	return func(ctx context.Context, in listCohortsInput) ([]cohorts.Cohort, error) {
		if in.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(svc.List(ctx, in.SiteID))
	}
}

func createCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[createCohortInput, cohorts.Cohort] {
	return func(ctx context.Context, in createCohortInput) (cohorts.Cohort, error) {
		if in.SiteID == "" {
			return cohorts.Cohort{}, neutron.ErrBadRequest("site_id required")
		}
		if strings.TrimSpace(in.Name) == "" {
			return cohorts.Cohort{}, neutron.ErrBadRequest("name required")
		}
		if len(in.Rule.Rules) == 0 {
			return cohorts.Cohort{}, neutron.ErrBadRequest("rule.rules must contain at least one condition")
		}
		c, err := svc.Create(ctx, in.SiteID, in.Name, in.Description, in.Rule)
		if err != nil {
			return cohorts.Cohort{}, err
		}
		return *c, nil
	}
}

func getCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[cohortIDInput, cohorts.Cohort] {
	return func(ctx context.Context, in cohortIDInput) (cohorts.Cohort, error) {
		if in.SiteID == "" {
			return cohorts.Cohort{}, neutron.ErrBadRequest("site_id required")
		}
		c, err := svc.Get(ctx, in.SiteID, in.CohortID)
		if err != nil {
			return cohorts.Cohort{}, err
		}
		if c == nil {
			return cohorts.Cohort{}, neutron.ErrNotFound("cohort not found")
		}
		return *c, nil
	}
}

func cohortMembersHandler(svc *cohorts.Service) neutron.HandlerFunc[cohortMembersInput, cohortMembersResult] {
	return func(ctx context.Context, in cohortMembersInput) (cohortMembersResult, error) {
		if in.SiteID == "" {
			return cohortMembersResult{}, neutron.ErrBadRequest("site_id required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		members, err := svc.Members(ctx, in.SiteID, in.CohortID, limit, in.Offset)
		if err != nil {
			return cohortMembersResult{}, err
		}
		if members == nil {
			members = []string{}
		}
		return cohortMembersResult{Members: members, Limit: limit, Offset: in.Offset}, nil
	}
}

func updateCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[updateCohortInput, cohorts.Cohort] {
	return func(ctx context.Context, in updateCohortInput) (cohorts.Cohort, error) {
		if in.SiteID == "" {
			return cohorts.Cohort{}, neutron.ErrBadRequest("site_id required")
		}
		c, err := svc.Update(ctx, in.SiteID, in.CohortID, in.Name, in.Description, in.Rule)
		if err != nil {
			return cohorts.Cohort{}, err
		}
		return *c, nil
	}
}

func deleteCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[cohortIDInput, neutron.Empty] {
	return func(ctx context.Context, in cohortIDInput) (neutron.Empty, error) {
		if in.SiteID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("site_id required")
		}
		return neutron.Empty{}, svc.Delete(ctx, in.SiteID, in.CohortID)
	}
}

func refreshCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[cohortIDInput, cohorts.Cohort] {
	return func(ctx context.Context, in cohortIDInput) (cohorts.Cohort, error) {
		if in.SiteID == "" {
			return cohorts.Cohort{}, neutron.ErrBadRequest("site_id required")
		}
		c, err := svc.Refresh(ctx, in.SiteID, in.CohortID)
		if err != nil {
			return cohorts.Cohort{}, err
		}
		return *c, nil
	}
}

func previewCohortHandler(svc *cohorts.Service) neutron.HandlerFunc[previewCohortInput, previewResult] {
	return func(ctx context.Context, in previewCohortInput) (previewResult, error) {
		if in.SiteID == "" {
			return previewResult{}, neutron.ErrBadRequest("site_id required")
		}
		ids, err := svc.EvaluateCohort(ctx, in.SiteID, in.Rule)
		if err != nil {
			return previewResult{}, neutron.ErrBadRequest(err.Error())
		}
		// Cap the sample at 10 — the UI just needs a glance, not the
		// whole list. Members API serves the full paginated set.
		sample := ids
		if len(sample) > 10 {
			sample = sample[:10]
		}
		if sample == nil {
			sample = []string{}
		}
		return previewResult{Count: len(ids), Sample: sample}, nil
	}
}
