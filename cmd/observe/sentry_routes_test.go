package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Sentry patterns must coexist with the existing /api/v1 routes in one
// mux (a pattern conflict panics at registration, i.e. at boot).
func TestSentryRoutesNoPatternConflict(t *testing.T) {
	mux := http.NewServeMux()
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.Handle("POST /api/v1/events", noop)
	mux.Handle("POST /api/v1/errors", noop)
	mux.Handle("OPTIONS /api/v1/{path...}", noop)
	mux.Handle("GET /api/v1/{path...}", noop)
	mux.Handle("POST /api/v1/infra/report", noop)
	registerSentryRoutes(mux, nil, nil, nil, nil)

	for _, p := range []string{"/api/1/envelope/", "/api/1/envelope", "/api/abc/store/", "/api/abc/store"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", p, nil))
		if rr.Code != 401 { // reached the handler (no key), not the v1 routes
			t.Errorf("%s: %d", p, rr.Code)
		}
	}
}
