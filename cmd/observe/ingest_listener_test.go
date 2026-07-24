package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sentinel is the handler behind ingestOnly; if a request reaches it, the
// wrapper allowed it through.
func sentinel() (http.Handler, *bool) {
	reached := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), &reached
}

func TestIngestListenerAllowsTelemetryWrites(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"POST", "/api/v1/events"},
		{"POST", "/api/v1/events/batch"},
		{"POST", "/api/v1/errors"},
		{"POST", "/api/v1/logs"},
		{"POST", "/api/v1/logs/batch"},
		{"POST", "/api/v1/replays"},
		{"POST", "/api/v1/feedback"},
		{"POST", "/api/v1/llm/ingest"},
		{"POST", "/api/v1/infra/report"},
		{"POST", "/api/v1/experiments/expose"},
		{"POST", "/api/v1/experiments/convert"},
		{"POST", "/api/v1/flags/evaluate"},
		{"GET", "/api/v1/surveys/active"},
		{"POST", "/api/v1/surveys/respond"},
		{"POST", "/api/v1/sourcemaps/upload"},
		{"GET", "/healthz"},
		// OTLP, both the standard and the group-mounted path.
		{"POST", "/v1/traces"},
		{"POST", "/v1/metrics"},
		{"POST", "/api/v1/v1/traces"},
		// Tracker assets the browser loads.
		{"GET", "/t/observe.js"},
		{"GET", "/t/observe-replay.js"},
		{"GET", "/t/observe-errors.js"},
		{"GET", "/t/observe-feedback.js"},
		{"GET", "/t/pixel.gif"},
		// Cron check-ins.
		{"POST", "/api/v1/checkin/token/abc123"},
		{"GET", "/api/v1/checkin/site/nightly-backup"},
		// CORS preflight.
		{"OPTIONS", "/api/v1/events"},
	}
	for _, tc := range allowed {
		h, reached := sentinel()
		rec := httptest.NewRecorder()
		ingestOnly(h).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if !*reached {
			t.Errorf("%s %s: blocked, want allowed (ingest would break)", tc.method, tc.path)
		}
	}
}

func TestIngestListenerBlocksDashboardAndReads(t *testing.T) {
	blocked := []struct{ method, path string }{
		// The dashboard SPA and its assets — the whole point of the split.
		{"GET", "/"},
		{"GET", "/login"},
		{"GET", "/assets/index.js"},
		{"GET", "/api/docs"},
		// Auth + admin.
		{"POST", "/api/v1/auth/login"},
		{"GET", "/api/v1/auth/setup"},
		{"GET", "/api/v1/sites"},
		{"POST", "/api/v1/sites"},
		{"GET", "/api/v1/sites/abc/keys"},
		{"POST", "/api/v1/sites/abc/keys"},
		{"GET", "/api/v1/platform/users"},
		{"GET", "/api/v1/audit"},
		{"GET", "/api/v1/compliance"},
		// Stored telemetry reads.
		{"GET", "/api/v1/stats/live"},
		{"GET", "/api/v1/export"},
		{"GET", "/api/v1/issues"},
		{"GET", "/api/v1/traces/search"},
		{"GET", "/api/v1/logs/search"},
		{"GET", "/api/v1/replays"},
		{"GET", "/api/v1/llm/stats"},
		{"POST", "/api/v1/query"},
		{"POST", "/api/v1/ai/query"},
		{"GET", "/api/v1/attribution"},
		{"GET", "/api/v1/dashboards"},
		// Same path as an allowed route but the wrong method.
		{"GET", "/api/v1/events"},
		{"DELETE", "/api/v1/events"},
		{"GET", "/v1/traces"},
		// Config/meta disclosure.
		{"GET", "/api/v1/config"},
		{"GET", "/api/v1/meta"},
	}
	for _, tc := range blocked {
		h, reached := sentinel()
		rec := httptest.NewRecorder()
		ingestOnly(h).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if *reached {
			t.Errorf("%s %s: reached the app handler, want blocked (EXPOSED on public listener)", tc.method, tc.path)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// The allowlist runs before http.ServeMux, which cleans paths — so a traversal
// that satisfies an allowed prefix here but resolves to a dashboard route in
// the mux would be a full bypass of the split.
func TestIngestListenerBlocksTraversalBypass(t *testing.T) {
	bypasses := []struct{ method, path string }{
		{"GET", "/t/../api/v1/sites"},
		{"GET", "/t/../../api/v1/stats/live"},
		{"POST", "/v1/../api/v1/query"},
		{"POST", "/api/v1/checkin/../../api/v1/sites"},
		{"GET", "/t/./../login"},
		{"GET", "//api/v1/sites"},
	}
	for _, tc := range bypasses {
		h, reached := sentinel()
		rec := httptest.NewRecorder()
		ingestOnly(h).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if *reached {
			t.Errorf("%s %s: traversal reached the app handler (BYPASS)", tc.method, tc.path)
		}
	}
}

// End-to-end over a real socket: newIngestServer must serve the filtered
// handler, not the raw one.
func TestIngestServerServesFilteredHandlerOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := newIngestServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("app-handler"))
	}))
	go srv.Serve(ln)
	defer srv.Close()

	base := "http://" + ln.Addr().String()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/t/observe.js", http.StatusOK},
		{"POST", "/v1/traces", http.StatusOK},
		{"GET", "/login", http.StatusNotFound},
		{"GET", "/api/v1/sites", http.StatusNotFound},
		{"GET", "/", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, base+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}
