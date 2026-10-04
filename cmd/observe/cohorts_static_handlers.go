package main

// Static cohort routes (C2 depth) and the cohort error mapping shared with
// cohorts_handlers.go. Static cohorts are lists of up to 100k ids, so their
// writes are plain http handlers with a bounded body (readJSONBody /
// MaxBytesReader) rather than typed neutron handlers.
//
//	POST /api/v1/cohorts/static                 {site_id,name,description,ids[]}  create from ids
//	POST /api/v1/cohorts/import?site_id&name    text/csv body                     create from CSV
//	POST /api/v1/cohorts/{id}/members           {site_id,ids[]}                   add members
//	POST /api/v1/cohorts/{id}/members/remove    {site_id,ids[]}                   remove members
//	POST /api/v1/cohorts/{id}/import?site_id    text/csv body                     add members from CSV
//
// All are editor-gated. Every call resolves the cohort through the caller's
// site_id first, so another site's cohort id answers 404.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/guardmap"
)

// staticBodyLimit bounds a static-cohort request body: 100k ids at the
// 256-byte id cap would be 25 MB, but real ids are far shorter; 8 MB covers
// 100k ids of ~80 bytes and anything larger is refused with 413.
const staticBodyLimit = 8 << 20

func registerStaticCohortRoutes(r *neutron.Router, jwtMW, requireEditor neutron.Middleware, svc *cohorts.Service) {
	guard := func(h http.HandlerFunc) http.Handler { return jwtMW(requireEditor(h)) }
	r.Handle("POST /api/v1/cohorts/static", guard(createStaticCohortHandler(svc)))
	r.Handle("POST /api/v1/cohorts/import", guard(createStaticFromCSVHandler(svc)))
	r.Handle("POST /api/v1/cohorts/{cohort_id}/members", guard(addCohortMembersHandler(svc)))
	r.Handle("POST /api/v1/cohorts/{cohort_id}/members/remove", guard(removeCohortMembersHandler(svc)))
	r.Handle("POST /api/v1/cohorts/{cohort_id}/import", guard(importCohortMembersHandler(svc)))
}

// cohortErrorStatus maps a cohort service error to an HTTP status and a
// client-safe message. Anything unrecognised is a 503 with a generic message
// (the raw error is logged by the caller, never echoed).
func cohortErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, cohorts.ErrNotFound):
		return http.StatusNotFound, "cohort not found"
	case errors.Is(err, cohorts.ErrInvalidDefinition),
		errors.Is(err, cohorts.ErrTooLarge),
		errors.Is(err, cohorts.ErrTooManyIDs),
		errors.Is(err, cohorts.ErrNotStatic):
		return http.StatusUnprocessableEntity, err.Error()
	}
	if mapped := guardmap.HTTPError(err); mapped != err {
		var app *neutron.AppError
		if errors.As(mapped, &app) {
			return app.Status, app.Detail
		}
	}
	return http.StatusServiceUnavailable, "cohort operation failed; retry later"
}

// cohortHTTPError is cohortErrorStatus for the typed handlers.
func cohortHTTPError(err error) error {
	if mapped := guardmap.HTTPError(err); mapped != err {
		return mapped // query-admission refusal keeps its own status
	}
	status, msg := cohortErrorStatus(err)
	switch status {
	case http.StatusNotFound:
		return neutron.ErrNotFound(msg)
	case http.StatusUnprocessableEntity:
		return neutron.ErrValidation(msg, []neutron.ValidationError{{Field: "rule", Message: msg}})
	}
	slog.Error("cohort operation failed", "err", err)
	return neutron.ErrServiceUnavailable(msg)
}

func writeCohortError(w http.ResponseWriter, err error) {
	status, msg := cohortErrorStatus(err)
	if status == http.StatusServiceUnavailable {
		slog.Error("cohort operation failed", "err", err)
	}
	writeJSONError(w, status, msg)
}

type staticCohortBody struct {
	SiteID      string   `json:"site_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	IDs         []string `json:"ids"`
}

type memberEditBody struct {
	SiteID string   `json:"site_id"`
	IDs    []string `json:"ids"`
}

type memberEditResult struct {
	Cohort  cohorts.Cohort `json:"cohort"`
	Changed int            `json:"changed"`
}

func createStaticCohortHandler(svc *cohorts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body staticCohortBody
		if !readJSONBody(w, r, staticBodyLimit, &body) {
			return
		}
		if body.SiteID == "" || strings.TrimSpace(body.Name) == "" {
			writeJSONError(w, http.StatusBadRequest, "site_id and name required")
			return
		}
		c, err := svc.CreateStatic(r.Context(), body.SiteID, body.Name, body.Description, body.IDs)
		if err != nil {
			writeCohortError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, c)
	}
}

// csvBodyIDs reads a bounded CSV request body into ids. It writes the error
// response and returns false on failure.
func csvBodyIDs(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, staticBodyLimit)
	ids, err := cohorts.ParseCSVIDs(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeCohortError(w, err)
		}
		return nil, false
	}
	return ids, true
}

func createStaticFromCSVHandler(svc *cohorts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		siteID, name := q.Get("site_id"), strings.TrimSpace(q.Get("name"))
		if siteID == "" || name == "" {
			writeJSONError(w, http.StatusBadRequest, "site_id and name required")
			return
		}
		ids, ok := csvBodyIDs(w, r)
		if !ok {
			return
		}
		c, err := svc.CreateStatic(r.Context(), siteID, name, q.Get("description"), ids)
		if err != nil {
			writeCohortError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, c)
	}
}

func addCohortMembersHandler(svc *cohorts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body memberEditBody
		if !readJSONBody(w, r, staticBodyLimit, &body) {
			return
		}
		if body.SiteID == "" {
			writeJSONError(w, http.StatusBadRequest, "site_id required")
			return
		}
		c, n, err := svc.AddMembers(r.Context(), body.SiteID, r.PathValue("cohort_id"), body.IDs)
		if err != nil {
			writeCohortError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, memberEditResult{Cohort: *c, Changed: n})
	}
}

func removeCohortMembersHandler(svc *cohorts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body memberEditBody
		if !readJSONBody(w, r, staticBodyLimit, &body) {
			return
		}
		if body.SiteID == "" {
			writeJSONError(w, http.StatusBadRequest, "site_id required")
			return
		}
		c, n, err := svc.RemoveMembers(r.Context(), body.SiteID, r.PathValue("cohort_id"), body.IDs)
		if err != nil {
			writeCohortError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, memberEditResult{Cohort: *c, Changed: n})
	}
}

func importCohortMembersHandler(svc *cohorts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		if siteID == "" {
			writeJSONError(w, http.StatusBadRequest, "site_id required")
			return
		}
		ids, ok := csvBodyIDs(w, r)
		if !ok {
			return
		}
		c, n, err := svc.AddMembers(r.Context(), siteID, r.PathValue("cohort_id"), ids)
		if err != nil {
			writeCohortError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, memberEditResult{Cohort: *c, Changed: n})
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
