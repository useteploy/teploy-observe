package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

func TestSurveyWidgetServed(t *testing.T) {
	rec := httptest.NewRecorder()
	serveSurveysWidget(rec, httptest.NewRequest("GET", "/t/observe-surveys.js", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/javascript") {
		t.Fatalf("status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "data-site-id") {
		t.Fatal("embedded script is not the widget")
	}
	if !isIngestPath("GET", "/t/observe-surveys.js") || !isIngestPath("POST", "/api/v1/surveys/expose") {
		t.Fatal("widget script and expose must be reachable on the public ingest listener")
	}
}

func TestSurveyActiveRejectsMissingOrHugeSite(t *testing.T) {
	h := surveysActiveHandler(nil, ingest.NewRateLimiter(100, time.Minute, 100))
	for _, u := range []string{"/api/v1/surveys/active", "/api/v1/surveys/active?site_id=" + strings.Repeat("a", maxSurveySiteIDLen+1)} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%.60s: status %d, want 400", u, rec.Code)
		}
	}
}

func TestSurveyActivePerSiteLimit(t *testing.T) {
	// Exhaust the bucket directly so the handler is refused before it
	// would need a service.
	rl := ingest.NewRateLimiter(1, time.Hour, 1)
	rl.Allow("s1", "")
	h := surveysActiveHandler(nil, rl)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/v1/surveys/active?site_id=s1", nil))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestSurveySiteLimitMW(t *testing.T) {
	reached := 0
	var gotBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(200)
	})
	rl := ingest.NewRateLimiter(1, time.Hour, 2)
	h := surveyCORS(surveySiteLimitMW(rl)(next))
	do := func(site string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"site_id":"`+site+`"}`)))
		return rec
	}
	if r := do("a"); r.Code != 200 || r.Header().Get("Access-Control-Allow-Origin") != "*" || gotBody != `{"site_id":"a"}` {
		t.Fatalf("first: %d body=%q", r.Code, gotBody)
	}
	do("a")
	if r := do("a"); r.Code != 429 || r.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("third same-site request: %d, want 429 with CORS header", r.Code)
	}
	if r := do("b"); r.Code != 200 {
		t.Fatalf("other site must be isolated, got %d", r.Code)
	}
	// Oversized body: refused before the handler, not buffered whole.
	before := reached
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"site_id":"c","pad":"`+strings.Repeat("x", publicFormMaxBodyBytes)+`"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge || reached != before {
		t.Fatalf("oversize: status=%d reached=%d", rec.Code, reached-before)
	}
	// Garbage body falls through to the real handler (which owns the 400).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`not json`)))
	if rec.Code != 200 {
		t.Fatalf("garbage body should reach handler, got %d", rec.Code)
	}
	// Absurd site id is refused, never bucketed.
	if r := do(strings.Repeat("z", maxSurveySiteIDLen+1)); r.Code != 429 {
		t.Fatalf("huge site id: %d", r.Code)
	}
}
