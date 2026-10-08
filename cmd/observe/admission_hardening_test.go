package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBoundedRequestBodyBeforeBinder(t *testing.T) {
	for _, size := range []int{8192, 8193} {
		for _, chunked := range []bool{false, true} {
			called := false
			h := loginBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
			r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(strings.Repeat("x", size)))
			r.Header.Set("Content-Type", "application/json")
			if chunked {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusNoContent
			if size > 8192 {
				want = http.StatusRequestEntityTooLarge
			}
			if w.Code != want || called != (size <= 8192) {
				t.Fatalf("size=%d chunked=%v code=%d called=%v", size, chunked, w.Code, called)
			}
		}
	}
	for _, ct := range []string{"multipart/form-data; boundary=x", "text/plain", ""} {
		r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader("{}"))
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		loginBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unsupported content reached binder") })).ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s code=%d", ct, w.Code)
		}
	}
}

func TestReadinessEveryFailureCombination(t *testing.T) {
	for mask := 0; mask < 128; mask++ {
		es := func(bit int) error {
			if mask&(1<<bit) != 0 {
				return errors.New("fixture failure")
			}
			return nil
		}
		got := readinessFailures(mask&1 != 0, mask&2 != 0, es(2), es(3), es(4), es(5), mask&64 != 0)
		want := 0
		for i := 0; i < 7; i++ {
			if mask&(1<<i) != 0 {
				want++
			}
		}
		if len(got) != want {
			t.Fatalf("mask=%d failures=%v expected count=%d", mask, got, want)
		}
	}
}

func TestTelemetryAuditSkipsDoNotHideAdministration(t *testing.T) {
	for _, p := range []string{"/api/v1/errors", "/api/v1/logs", "/api/v1/logs/batch", "/api/v1/replays", "/api/v1/llm/ingest", "/api/v1/experiments/expose", "/api/v1/experiments/convert"} {
		if auditableRequest("POST", p) {
			t.Errorf("telemetry audited %s", p)
		}
	}
	for _, p := range []string{"/api/v1/errors/issue/resolve", "/api/v1/log-pipelines", "/api/v1/experiments", "/api/v1/replays/delete"} {
		if !auditableRequest("POST", p) {
			t.Errorf("administration not audited %s", p)
		}
	}
}
