package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/surveys"
)

func allSitesKnown(context.Context, string) bool { return true }

func TestSurveyRespondErrorsAreSanitized(t *testing.T) {
	// A store-shaped error (wrapped DB text) must never reach the caller.
	rec := httptest.NewRecorder()
	writeSurveyRespondError(rec, fmt.Errorf("submit response: dedupe lookup: %w", errors.New("nucleus: relation survey_responses at 10.0.0.5:5432 password=hunter2")))
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError || !strings.Contains(body, "could not record response") {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	for _, leak := range []string{"nucleus", "10.0.0.5", "hunter2", "dedupe", "survey_responses"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks %q: %s", leak, body)
		}
	}
	// Known validation/gate errors keep their fixed message with a 400.
	rec = httptest.NewRecorder()
	writeSurveyRespondError(rec, fmt.Errorf("wrapped: %w", &surveys.PublicError{Msg: "survey is not active"}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "survey is not active") {
		t.Fatalf("public error: %d %s", rec.Code, rec.Body.String())
	}
}

// End to end through the handler: validation failures are 400 with the fixed
// text; these run before any store access so a bare service suffices.
func TestSurveyRespondHandlerValidation(t *testing.T) {
	h := surveyRespondHandler(&surveys.SurveyService{})
	body := `{"survey_id":"sv","site_id":"s","user_id":"` + strings.Repeat("u", 200) + `","answers":{}}`
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 400 || out["ok"] != false || !strings.Contains(fmt.Sprint(out["error"]), "user_id too long") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSurveySiteLimitSkipsUnknownSites(t *testing.T) {
	rl := ingest.NewRateLimiter(1, time.Hour, 1)
	var reached int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reached, 1)
		w.WriteHeader(200)
	})
	known := func(_ context.Context, s string) bool { return s == "real" }
	h := surveySiteLimitMW(rl, known)(next)
	do := func(site string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"site_id":"`+site+`"}`)))
		return rec.Code
	}
	for i := 0; i < 200; i++ {
		if c := do(fmt.Sprintf("junk-%d", i)); c != 404 {
			t.Fatalf("unknown site: %d", c)
		}
	}
	if reached != 0 {
		t.Fatal("unknown site reached the handler")
	}
	// None of the 200 junk ids may have consumed a bucket: the real site
	// still has its full burst (1) and is then limited.
	if do("real") != 200 {
		t.Fatal("real site refused")
	}
	if do("real") != 429 {
		t.Fatal("real site not limited")
	}
}

func TestSurveyActiveUnknownSiteEmpty(t *testing.T) {
	rl := ingest.NewRateLimiter(1, time.Hour, 1)
	h := surveysActiveHandler(nil, rl, func(context.Context, string) bool { return false })
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", fmt.Sprintf("/api/v1/surveys/active?site_id=nope-%d", i), nil))
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("unknown site: %d %q", rec.Code, rec.Body.String())
		}
	}
}

func TestSiteKnownCacheTTLAndCap(t *testing.T) {
	var calls int32
	now := time.Unix(1000, 0)
	c := newSiteKnownCacheSized(func(_ context.Context, s string) bool {
		atomic.AddInt32(&calls, 1)
		return s == "real"
	}, 10*time.Second, 3, func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if !c.known(ctx, "real") || c.known(ctx, "fake") {
			t.Fatal("wrong answer")
		}
	}
	if calls != 2 {
		t.Fatalf("lookups = %d, want 2 (cached)", calls)
	}
	now = now.Add(11 * time.Second)
	c.known(ctx, "real")
	if calls != 3 {
		t.Fatalf("expired entry not re-looked-up: %d", calls)
	}
	for i := 0; i < 20; i++ {
		c.known(ctx, fmt.Sprintf("junk-%d", i))
	}
	if len(c.m) > 3 {
		t.Fatalf("cache exceeded cap: %d", len(c.m))
	}
}
