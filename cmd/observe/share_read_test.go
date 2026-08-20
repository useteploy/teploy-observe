package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
	"github.com/useteploy/teploy-observe/internal/share"
)

// A share token is the only credential a machine can hold to READ this API: a
// user JWT expires in 24 hours and is tied to a person, and the ingest API keys
// are write-scoped. teploy-ship reads service RED metrics with one to put
// before/after telemetry on a pull request, so the middleware's guarantees —
// GET only, and site_id pinned to the token's own site — are load-bearing
// beyond the public-dashboard case they were written for.
//
// Verified against a REAL engine rather than a fake: this repo has been bitten
// before by a mock that encoded stricter semantics than Nucleus actually has.
func shareTestDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	return db
}

func TestShareTokenReadPath(t *testing.T) {
	db := shareTestDB(t)
	svc := share.NewShareService(db)

	link, err := svc.Create(context.Background(), "site-red")
	if err != nil {
		t.Fatalf("creating share link: %v", err)
	}

	// A JWT middleware stand-in that records whether it was consulted: the
	// share path must not fall through to it, and a request with no token must.
	jwtCalled := false
	jwtMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jwtCalled = true
			w.WriteHeader(http.StatusTeapot)
		})
	}

	var sawSite string
	handler := jwtOrShareMW(jwtMW, svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSite = r.URL.Query().Get("site_id")
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("a valid token reads, pinned to its own site", func(t *testing.T) {
		jwtCalled, sawSite = false, ""
		req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/services?site_id=someone-elses", nil)
		req.Header.Set("X-Share-Token", link.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("share token was refused: %d", rec.Code)
		}
		if jwtCalled {
			t.Error("a share token must not fall through to JWT auth")
		}
		// The client asked for another site and must not get it. This is what
		// makes handing a token to a worker safe.
		if sawSite != "site-red" {
			t.Errorf("site_id was not pinned to the token's site: got %q", sawSite)
		}
	})

	t.Run("a share token cannot write", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/traces/services", nil)
		req.Header.Set("X-Share-Token", link.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a non-GET with a share token must be refused, got %d", rec.Code)
		}
	})

	t.Run("a revoked token stops reading", func(t *testing.T) {
		revocable, err := svc.Create(context.Background(), "site-red")
		if err != nil {
			t.Fatalf("creating second link: %v", err)
		}
		if err := svc.Revoke(context.Background(), revocable.Token); err != nil {
			t.Fatalf("revoking: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/services", nil)
		req.Header.Set("X-Share-Token", revocable.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a revoked token must be refused, got %d — revocation is the only way to take a worker's access away", rec.Code)
		}
	})

	t.Run("no token still requires a session", func(t *testing.T) {
		jwtCalled = false
		req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/services", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !jwtCalled {
			t.Error("a request with no share token must go through JWT auth")
		}
	})
}

// Which trace routes accept a share token is a security decision, and it lives
// inline in main(). Source text is the honest cheap guard for it: the RED
// aggregate is shareable, and everything returning trace PAYLOADS — waterfalls,
// span attributes, search results, which carry request paths, ids and
// user-supplied values — must keep requiring a session.
func TestOnlyAggregateTraceReadsAreShareable(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	if !strings.Contains(text, `traceShared := r.Group("/api/v1/traces", jwtOrShareMW(jwtMW, shareSvc))`) {
		t.Fatal("the RED metrics endpoint is no longer share-readable — teploy-ship's telemetry read depends on it")
	}
	if !strings.Contains(text, `neutron.Get(traceShared, "/services", listServicesHandler(traceQuery)`) {
		t.Error("/traces/services must be registered on the shared group")
	}
	for _, route := range []string{"/search", "/{trace_id}", "/{trace_id}/errors", "/dependencies"} {
		if strings.Contains(text, `neutron.Get(traceShared, "`+route+`"`) {
			t.Errorf("%s returns trace payloads and must not accept a share token", route)
		}
	}
}
