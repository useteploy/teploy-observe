package main

// Error-tracking depth routes: issue merge / unmerge and assignment
// (migration 058). One RegisterIssueAdminRoutes call from main.go.
//
// Authorization: all three are mutations, so they sit behind the JWT
// middleware AND requireEditor (viewers get 403 before any handler
// code). The site boundary is enforced in the service: both issues must
// exist in the request's site_id, and a foreign id is a plain 404.
// The audit trail comes from auditMiddleware (every non-GET /api/v1
// call: actor, issues.merge.create / issues.unmerge.create /
// issues.assignee.update, target path, result).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/neutron-build/neutron/go/neutron"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
)

// issueAdminService is the slice of *errors.IssueService these routes use.
type issueAdminService interface {
	MergeIssues(ctx context.Context, siteID, sourceID, targetID, actor string) error
	UnmergeIssue(ctx context.Context, siteID, sourceID, actor string) error
	AssignIssue(ctx context.Context, siteID, issueID, assignee, actor string) error
}

// routeRegistrar is the part of *neutron.Router (and http.ServeMux) used.
type routeRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

const maxIssueAdminBody = 4 << 10

// RegisterIssueAdminRoutes mounts the merge/unmerge/assignee routes.
// actor resolves the acting username from the request (may return "").
func RegisterIssueAdminRoutes(
	r routeRegistrar,
	jwtMW, requireEditor neutron.Middleware,
	svc issueAdminService,
	actor func(*http.Request) string,
) {
	wrap := func(h http.HandlerFunc) http.Handler { return jwtMW(requireEditor(h)) }
	r.Handle("POST /api/v1/issues/{issue_id}/merge", wrap(issueMergeHandler(svc, actor)))
	r.Handle("POST /api/v1/issues/{issue_id}/unmerge", wrap(issueUnmergeHandler(svc, actor)))
	r.Handle("PUT /api/v1/issues/{issue_id}/assignee", wrap(issueAssigneeHandler(svc, actor)))
}

type issueAdminBody struct {
	SiteID   string `json:"site_id"`
	TargetID string `json:"target_id"`
	Assignee string `json:"assignee"`
}

func decodeIssueAdmin(w http.ResponseWriter, r *http.Request) (issueAdminBody, string, bool) {
	var body issueAdminBody
	r.Body = http.MaxBytesReader(w, r.Body, maxIssueAdminBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		neutron.WriteError(w, r, neutron.ErrBadRequest("invalid JSON body"))
		return body, "", false
	}
	id := r.PathValue("issue_id")
	if id == "" || body.SiteID == "" {
		neutron.WriteError(w, r, neutron.ErrBadRequest("issue_id and site_id required"))
		return body, "", false
	}
	return body, id, true
}

func writeIssueAdminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, obserrors.ErrIssueNotFound):
		neutron.WriteError(w, r, neutron.ErrNotFound("issue not found"))
	case errors.Is(err, obserrors.ErrMergeSelf), errors.Is(err, obserrors.ErrInvalidAssignee):
		neutron.WriteError(w, r, neutron.ErrBadRequest(err.Error()))
	case errors.Is(err, obserrors.ErrMergeCycle), errors.Is(err, obserrors.ErrMergeDepth),
		errors.Is(err, obserrors.ErrAlreadyMerged), errors.Is(err, obserrors.ErrNotMerged):
		neutron.WriteError(w, r, neutron.ErrConflict(err.Error()))
	default:
		neutron.WriteError(w, r, neutron.ErrInternal("issue update failed"))
	}
}

func issueMergeHandler(svc issueAdminService, actor func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, id, ok := decodeIssueAdmin(w, r)
		if !ok {
			return
		}
		if body.TargetID == "" {
			neutron.WriteError(w, r, neutron.ErrBadRequest("target_id required"))
			return
		}
		if err := svc.MergeIssues(r.Context(), body.SiteID, id, body.TargetID, actor(r)); err != nil {
			writeIssueAdminError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func issueUnmergeHandler(svc issueAdminService, actor func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, id, ok := decodeIssueAdmin(w, r)
		if !ok {
			return
		}
		if err := svc.UnmergeIssue(r.Context(), body.SiteID, id, actor(r)); err != nil {
			writeIssueAdminError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func issueAssigneeHandler(svc issueAdminService, actor func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, id, ok := decodeIssueAdmin(w, r)
		if !ok {
			return
		}
		if err := svc.AssignIssue(r.Context(), body.SiteID, id, body.Assignee, actor(r)); err != nil {
			writeIssueAdminError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
