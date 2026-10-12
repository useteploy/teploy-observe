package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/replays"
)

const corsTestOrigin = "https://fylun.ai"

// fakeSiteKey stands in for APIKeyAuthMiddleware's success path (which needs a
// live Nucleus): any non-empty key binds the request to site "s1". The missing
// key branch is exercised against the real middleware below.
func fakeSiteKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") == "" {
			neutron.WriteError(w, r, neutron.ErrUnauthorized("missing API key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(ingest.WithSiteID(r.Context(), "s1")))
	})
}

// ingestTestServer assembles the ingest path the way production serves it:
// ingestOnly over the app handler (global middleware included) over the
// /api/v1 group built by ingestMiddleware, with the real replay handler.
func ingestTestServer(t *testing.T, apiKeyMW neutron.Middleware, rl *ingest.RateLimiter) http.Handler {
	t.Helper()
	app := neutron.New(neutron.WithMiddleware(queryRefusalMetadata, securityHeadersMiddleware(true)))
	r := app.Router()
	r.HandleFunc("OPTIONS /api/v1/{path...}", ingestPreflight)
	g := r.Group("/api/v1", ingestMiddleware(apiKeyMW, rl, 10*time.Second)...)
	neutron.Post(g, "/replays", replayIngestHandler(replays.NewReplayService(nil)))
	// A handler-produced 503 (durable-mode refusal shape), on an allowlisted path.
	g.HandleFunc("POST /logs", func(w http.ResponseWriter, r *http.Request) {
		neutron.WriteError(w, r, neutron.ErrServiceUnavailable("ingest queue at high-water"))
	})
	return ingestOnly(app.Handler())
}

func corsPost(path, key, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Origin", corsTestOrigin)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	return req
}

func assertCORS(t *testing.T, name string, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("%s: status = %d, want %d (body %s)", name, rec.Code, wantStatus, rec.Body.String())
	}
	acao := rec.Header().Get("Access-Control-Allow-Origin")
	if acao != corsTestOrigin && acao != "*" {
		t.Fatalf("%s: status %d response has Access-Control-Allow-Origin %q; the browser cannot read it", name, rec.Code, acao)
	}
	// credentials: 'omit' on the client; a credentialed grant would be a
	// widening, and is invalid alongside a wildcard preflight anyway.
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("%s: unexpected Access-Control-Allow-Credentials %q", name, got)
	}
}

// Every response a browser can get from POST /api/v1/replays - success and
// each refusal - carries the same origin grant as the preflight.
func TestReplayIngestEveryResponseCarriesCORS(t *testing.T) {
	h := ingestTestServer(t, fakeSiteKey, ingest.NewRateLimiter(1000, time.Second, 1000))

	pre := httptest.NewRequest(http.MethodOptions, "/api/v1/replays", nil)
	pre.Header.Set("Origin", corsTestOrigin)
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "content-type,x-api-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	assertCORS(t, "preflight", rec, http.StatusNoContent)
	for _, hdr := range []string{"content-type", "x-api-key"} {
		if !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")), hdr) {
			t.Fatalf("preflight does not allow %s", hdr)
		}
	}

	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"success", corsPost("/api/v1/replays", "k", `{}`), http.StatusCreated},
		{"invalid json", corsPost("/api/v1/replays", "k", `{bad`), http.StatusBadRequest},
		{"cross-site body", corsPost("/api/v1/replays", "k", `{"site_id":"other","events":[{}]}`), http.StatusForbidden},
		{"missing key", corsPost("/api/v1/replays", "", `{}`), http.StatusUnauthorized},
		{"handler 503", corsPost("/api/v1/logs", "k", `{}`), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tc.req)
		assertCORS(t, tc.name, rec, tc.status)
	}

	big := corsPost("/api/v1/replays", "k", `{"pad":"`+strings.Repeat("a", 2<<20)+`"}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, big)
	assertCORS(t, "oversize (content-length)", rec, http.StatusRequestEntityTooLarge)

	chunked := corsPost("/api/v1/replays", "k", `{"pad":"`+strings.Repeat("a", 2<<20)+`"}`)
	chunked.ContentLength = -1
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chunked)
	assertCORS(t, "oversize (chunked)", rec, http.StatusRequestEntityTooLarge)
}

func TestReplayIngestRateLimitedResponseCarriesCORS(t *testing.T) {
	h := ingestTestServer(t, fakeSiteKey, ingest.NewRateLimiter(1, time.Minute, 1))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, corsPost("/api/v1/replays", "k", `{}`))
	assertCORS(t, "first", rec, http.StatusCreated)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, corsPost("/api/v1/replays", "k", `{}`))
	assertCORS(t, "rate limited", rec, http.StatusTooManyRequests)
}

// The real key middleware's refusal, not the stand-in's.
func TestReplayIngestRealKeyRefusalCarriesCORS(t *testing.T) {
	authSvc := auth.NewAuthService(nil, "test-secret", slog.Default())
	h := ingestTestServer(t, auth.APIKeyAuthMiddleware(authSvc), ingest.NewRateLimiter(1000, time.Second, 1000))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, corsPost("/api/v1/replays", "", `{}`))
	assertCORS(t, "missing key (real middleware)", rec, http.StatusUnauthorized)
}

// observe.fylun.ai reaches the ingest listener through a cloudflared tunnel
// whose origin keep-alive pool holds idle connections for 90 s (cloudflared's
// default keepAliveTimeout; the tunnel sets no override). With IdleTimeout
// unset, net/http closes idle connections after ReadTimeout (30 s), so the
// proxy regularly sends a request down a connection the server has just
// closed. It does not retry a POST on a reused connection, so the browser gets
// Cloudflare's 502 - which carries no Access-Control-Allow-Origin and so
// surfaces as a CORS failure on a route whose own responses are all correct.
// The origin must outlive the proxy's idle pool.
func TestIngestServerIdleTimeoutOutlivesProxyKeepAlive(t *testing.T) {
	const cloudflaredKeepAlive = 90 * time.Second
	srv := newIngestServer(":0", http.NotFoundHandler())
	idle := srv.IdleTimeout
	if idle == 0 {
		idle = srv.ReadTimeout
	}
	if idle <= cloudflaredKeepAlive {
		t.Fatalf("ingest idle timeout %v must exceed the proxy keep-alive %v", idle, cloudflaredKeepAlive)
	}
}
