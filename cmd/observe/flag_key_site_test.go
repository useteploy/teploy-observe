package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/flags"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// An SDK holding a key sends no site, so evaluation has to take it from the key.
func TestFlagEvaluateTakesTheSiteFromTheKey(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	limiter := func() *ingest.RateLimiter { return ingest.NewRateLimiter(1000, time.Minute, 1000) }
	call := func(validate func(context.Context, string) (auth.ValidatedKey, error), key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/flags/evaluate", strings.NewReader(body))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		flagEvaluateHandler(flags.NewFlagService(db), limiter(), validate)(rec, req)
		return rec
	}
	valid := func(_ context.Context, key string) (auth.ValidatedKey, error) {
		if key == "good" {
			return auth.ValidatedKey{SiteID: "flag-key-site"}, nil
		}
		return auth.ValidatedKey{}, errors.New("auth: invalid api key")
	}

	rec := call(valid, "good", `{"flag_key":"missing"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyed, no site: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec := call(valid, "bad", `{"flag_key":"missing"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad key, no site: %d", rec.Code)
	}
	if rec := call(valid, "", `{"flag_key":"missing"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no key, no site: %d", rec.Code)
	}
	if rec := call(valid, "", `{"site_id":"public","flag_key":"missing"}`); rec.Code != http.StatusOK {
		t.Errorf("public path must be unchanged: %d", rec.Code)
	}
	down := func(context.Context, string) (auth.ValidatedKey, error) {
		return auth.ValidatedKey{}, auth.ErrAuthUnavailable
	}
	if rec := call(down, "good", `{"flag_key":"missing"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("store down: %d", rec.Code)
	}
}
