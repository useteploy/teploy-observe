package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDemoModeRouteMatrix(t *testing.T) {
	for _, c := range []struct {
		method, path string
		allowed      bool
	}{
		{"POST", "/api/v1/llm/ingest", true}, {"POST", "/api/v1/infra/report", true},
		{"POST", "/v1/traces", true}, {"POST", "/v1/metrics", true}, {"POST", "/v1/logs", true},
		{"POST", "/api/v1/v1/traces", true}, {"POST", "/api/123/envelope/", true},
		{"POST", "/api/123/store", true}, {"POST", "/api/v1/checkin/token/abc", true},
		{"POST", "/api/v1/events", true}, {"POST", "/api/v1/events/batch", true},
		{"POST", "/api/v1/errors", true}, {"POST", "/api/v1/logs/batch", true},
		{"POST", "/api/v1/replays", true}, {"POST", "/api/v1/feedback", true},
		{"POST", "/api/v1/flags/evaluate", true}, {"POST", "/api/v1/persons/properties", true},
		{"POST", "/api/v1/experiments/expose", true}, {"POST", "/api/v1/experiments/convert", true},
		{"POST", "/api/v1/experiments/metric", true}, {"POST", "/api/v1/surveys/expose", true},
		{"POST", "/api/v1/surveys/respond", true}, {"POST", "/api/v1/sourcemaps/upload", true},
		{"POST", "/api/v1/auth/login", true}, {"POST", "/api/v1/auth/stream-ticket", true},
		{"DELETE", "/api/v1/replays/abc", false}, {"POST", "/api/v1/errors/abc/resolve", false},
		{"POST", "/api/v1/logs/retention", false}, {"POST", "/api/v1/auth/setup", false},
		{"POST", "/api/v1/auth/change-password", false}, {"PUT", "/api/v1/auth/login", false},
		{"PATCH", "/api/v1/events", false}, {"POST", "/api/v1/events-other", false},
		{"POST", "/v1/admin", false}, {"POST", "/api/v1/envelope", false},
		{"POST", "/api/v1/checkin/token/abc/other", false},
		{"GET", "/api/v1/sites", true}, {"HEAD", "/api/v1/sites", true}, {"OPTIONS", "/v1/traces", true},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			called := false
			handler := DemoModeMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
			if called != c.allowed {
				t.Fatalf("called=%v want %v (HTTP %d)", called, c.allowed, w.Code)
			}
			if !c.allowed && w.Code != 403 {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}

func TestDemoModeDisabledAllowsWrites(t *testing.T) {
	w := httptest.NewRecorder()
	DemoModeMiddleware(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, httptest.NewRequest("DELETE", "/api/v1/sites/a", nil))
	if w.Code != 204 {
		t.Fatalf("status=%d", w.Code)
	}
}
