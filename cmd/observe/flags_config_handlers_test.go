package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/flags"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

type stubConfigSource struct {
	gotSite string
	err     error
}

func (s *stubConfigSource) Config(_ context.Context, siteID string) (*flags.ConfigBundle, error) {
	s.gotSite = siteID
	if s.err != nil {
		return nil, s.err
	}
	b := flags.BuildConfig(siteID, []flags.FeatureFlag{{FlagKey: "k", FlagType: "boolean", Enabled: true, RolloutPct: 100}}, time.Unix(0, 0))
	return &b, nil
}

func passthrough(next http.Handler) http.Handler { return next }

// The real API key middleware rejects a request with no key before the
// handler (and therefore the config) is reached.
func TestFlagConfigRequiresAPIKey(t *testing.T) {
	src := &stubConfigSource{}
	h := flagConfigRoute(auth.APIKeyAuthMiddleware(nil), passthrough, src)
	for _, hdr := range []map[string]string{nil, {"X-API-Key": ""}, {"X-API-Key": "   "}, {"Authorization": "Bearer x"}} {
		req := httptest.NewRequest("GET", "/api/v1/flags/config?site_id=s1", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("headers %v: status %d, want 401", hdr, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"flags"`) {
			t.Fatal("config leaked without a key")
		}
	}
	if src.gotSite != "" {
		t.Fatal("config source read without authentication")
	}
}

func TestFlagConfigHandlerScopesToKeySite(t *testing.T) {
	src := &stubConfigSource{}
	h := flagConfigHandler(src)
	withSite := func(url, site string) *http.Request {
		r := httptest.NewRequest("GET", url, nil)
		return r.WithContext(ingest.WithSiteID(r.Context(), site))
	}

	// No site in context (middleware bypassed): fail closed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/flags/config?site_id=s1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no site: %d", rec.Code)
	}

	// site_id query for another site is refused, and nothing is read.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSite("/api/v1/flags/config?site_id=other", "s1"))
	if rec.Code != http.StatusForbidden || src.gotSite != "" {
		t.Fatalf("cross-site: %d site=%q", rec.Code, src.gotSite)
	}

	// Matching (or omitted) site_id is served for the key's site.
	for _, u := range []string{"/api/v1/flags/config", "/api/v1/flags/config?site_id=s1"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, withSite(u, "s1"))
		if rec.Code != 200 || src.gotSite != "s1" {
			t.Fatalf("%s: %d site=%q", u, rec.Code, src.gotSite)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"bucketing"`) || !strings.Contains(body, `"key":"k"`) {
			t.Fatalf("body: %s", body)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
			t.Fatalf("cache-control %q", cc)
		}
	}

	// ETag revalidation.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSite("/api/v1/flags/config", "s1"))
	etag := rec.Header().Get("ETag")
	req := withSite("/api/v1/flags/config", "s1")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("304 expected, got %d", rec.Code)
	}

	// Store failure is a retryable 503, not an empty config.
	src.err = errors.New("down")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSite("/api/v1/flags/config", "s1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage: %d", rec.Code)
	}
}
