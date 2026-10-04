package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
)

type fakeIssueAdmin struct {
	calls []string
	err   error
}

func (f *fakeIssueAdmin) MergeIssues(_ context.Context, site, src, tgt, actor string) error {
	f.calls = append(f.calls, "merge "+site+" "+src+" "+tgt+" "+actor)
	return f.err
}
func (f *fakeIssueAdmin) UnmergeIssue(_ context.Context, site, src, actor string) error {
	f.calls = append(f.calls, "unmerge "+site+" "+src+" "+actor)
	return f.err
}
func (f *fakeIssueAdmin) AssignIssue(_ context.Context, site, id, who, actor string) error {
	f.calls = append(f.calls, "assign "+site+" "+id+" "+who+" "+actor)
	return f.err
}

func issueAdminMux(svc issueAdminService, role string) *http.ServeMux {
	mux := http.NewServeMux()
	jwt := neutron.Middleware(func(next http.Handler) http.Handler { return next })
	editor := neutron.Middleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if role != "editor" && role != "admin" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	RegisterIssueAdminRoutes(mux, jwt, editor, svc, func(*http.Request) string { return "tyler" })
	return mux
}

func doIssueAdmin(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestIssueAdminRoutesViewerForbidden(t *testing.T) {
	f := &fakeIssueAdmin{}
	mux := issueAdminMux(f, "viewer")
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/v1/issues/i1/merge", `{"site_id":"s","target_id":"i2"}`},
		{"POST", "/api/v1/issues/i1/unmerge", `{"site_id":"s"}`},
		{"PUT", "/api/v1/issues/i1/assignee", `{"site_id":"s","assignee":"a"}`},
	} {
		if rec := doIssueAdmin(mux, c.m, c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: viewer got %d", c.m, c.p, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("viewer reached the service: %v", f.calls)
	}
}

func TestIssueAdminRoutesHappyAndValidation(t *testing.T) {
	f := &fakeIssueAdmin{}
	mux := issueAdminMux(f, "editor")
	if rec := doIssueAdmin(mux, "POST", "/api/v1/issues/i1/merge", `{"site_id":"s","target_id":"i2"}`); rec.Code != 204 {
		t.Fatalf("merge %d %s", rec.Code, rec.Body)
	}
	if rec := doIssueAdmin(mux, "POST", "/api/v1/issues/i1/unmerge", `{"site_id":"s"}`); rec.Code != 204 {
		t.Fatalf("unmerge %d", rec.Code)
	}
	if rec := doIssueAdmin(mux, "PUT", "/api/v1/issues/i1/assignee", `{"site_id":"s","assignee":"alice"}`); rec.Code != 204 {
		t.Fatalf("assign %d", rec.Code)
	}
	want := []string{"merge s i1 i2 tyler", "unmerge s i1 tyler", "assign s i1 alice tyler"}
	for i, w := range want {
		if f.calls[i] != w {
			t.Errorf("call %d = %q want %q", i, f.calls[i], w)
		}
	}
	n := len(f.calls)
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/v1/issues/i1/merge", `{"site_id":"s"}`},                                           // no target
		{"POST", "/api/v1/issues/i1/merge", `{"target_id":"i2"}`},                                        // no site
		{"POST", "/api/v1/issues/i1/merge", `not json`},                                                  // bad body
		{"PUT", "/api/v1/issues/i1/assignee", `{"assignee":"a"}`},                                        // no site
		{"POST", "/api/v1/issues/i1/unmerge", `{"site_id":"s","x":"` + strings.Repeat("a", 9000) + `"}`}, // oversized
	} {
		if rec := doIssueAdmin(mux, c.m, c.p, c.b); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s %.20s: got %d want 400", c.m, c.p, c.b, rec.Code)
		}
	}
	if len(f.calls) != n {
		t.Fatalf("invalid requests reached the service: %v", f.calls[n:])
	}
}

func TestIssueAdminRoutesErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{obserrors.ErrIssueNotFound, 404}, // also what a cross-site id yields
		{obserrors.ErrMergeSelf, 400},
		{obserrors.ErrInvalidAssignee, 400},
		{obserrors.ErrMergeCycle, 409},
		{obserrors.ErrMergeDepth, 409},
		{obserrors.ErrAlreadyMerged, 409},
		{obserrors.ErrNotMerged, 409},
		{context.DeadlineExceeded, 500},
	}
	for _, c := range cases {
		mux := issueAdminMux(&fakeIssueAdmin{err: c.err}, "admin")
		rec := doIssueAdmin(mux, "POST", "/api/v1/issues/i1/merge", `{"site_id":"s","target_id":"i2"}`)
		if rec.Code != c.want {
			t.Errorf("%v: got %d want %d", c.err, rec.Code, c.want)
		}
	}
}

// The routes are audited by auditMiddleware (non-GET under /api/v1).
func TestIssueAdminRoutesAreAudited(t *testing.T) {
	for p, want := range map[string]string{
		"POST /api/v1/issues/0123456789abcdef0123456789abcdef/merge":   "issues.merge.create",
		"POST /api/v1/issues/0123456789abcdef0123456789abcdef/unmerge": "issues.unmerge.create",
		"PUT /api/v1/issues/0123456789abcdef0123456789abcdef/assignee": "issues.assignee.update",
	} {
		parts := strings.SplitN(p, " ", 2)
		if !auditableRequest(parts[0], parts[1]) {
			t.Errorf("%s not audited", p)
		}
		if got := deriveAction(parts[0], parts[1]); got != want {
			t.Errorf("%s action = %s want %s", p, got, want)
		}
	}
}
