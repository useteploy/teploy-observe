package main

// Persons API (C2 Wave 4) — read-only aggregate over events.distinct_id.
// Routes live here so the integration into main.go is a single
// RegisterPersonsRoutes call (matches the boards / metrics / cohorts
// convention).

import (
	"context"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/persons"
)

// RegisterPersonsRoutes wires the persons endpoints onto the given router.
// JWT-only — there is no destructive endpoint here (read aggregates only),
// so editor / admin role enforcement is unnecessary.
func RegisterPersonsRoutes(r *neutron.Router, jwtMW neutron.Middleware, svc *persons.Service) {
	api := r.Group("/api/v1/persons", jwtMW)

	neutron.Get(api, "", listPersonsHandler(svc),
		neutron.WithTags("persons"),
		neutron.WithSummary("List identified users (aggregate of events.distinct_id)"),
	)
	neutron.Get(api, "/{distinct_id}", personDetailHandler(svc),
		neutron.WithTags("persons"),
		neutron.WithSummary("Person detail with last 100 events"),
	)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type listPersonsInput struct {
	SiteID           string `query:"site_id"`
	From             string `query:"from"`
	To               string `query:"to"`
	Limit            int    `query:"limit"`
	Offset           int    `query:"offset"`
	IncludeAnonymous bool   `query:"include_anonymous"`
}

// listPersonsResult bundles the page + total so the UI can render
// pagination without a second round-trip.
type listPersonsResult struct {
	Persons []persons.Person `json:"persons"`
	Total   int64            `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

func listPersonsHandler(svc *persons.Service) neutron.HandlerFunc[listPersonsInput, listPersonsResult] {
	return func(ctx context.Context, in listPersonsInput) (listPersonsResult, error) {
		if in.SiteID == "" {
			return listPersonsResult{}, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(in.From, in.To)
		fromMs, toMs := from.UnixMilli(), to.UnixMilli()

		rows, err := svc.ListPersons(ctx, in.SiteID, fromMs, toMs, in.Limit, in.Offset, in.IncludeAnonymous)
		if err != nil {
			return listPersonsResult{}, err
		}
		total, _ := svc.CountPersons(ctx, in.SiteID, fromMs, toMs, in.IncludeAnonymous)
		if rows == nil {
			rows = []persons.Person{}
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		return listPersonsResult{
			Persons: rows, Total: total,
			Limit: limit, Offset: in.Offset,
		}, nil
	}
}

type personDetailInput struct {
	DistinctID string `path:"distinct_id"`
	SiteID     string `query:"site_id"`
}

func personDetailHandler(svc *persons.Service) neutron.HandlerFunc[personDetailInput, persons.PersonDetail] {
	return func(ctx context.Context, in personDetailInput) (persons.PersonDetail, error) {
		if in.SiteID == "" {
			return persons.PersonDetail{}, neutron.ErrBadRequest("site_id required")
		}
		if strings.TrimSpace(in.DistinctID) == "" {
			return persons.PersonDetail{}, neutron.ErrBadRequest("distinct_id required")
		}
		return svc.PersonDetail(ctx, in.SiteID, in.DistinctID)
	}
}
