package dogfood

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShouldSkipExcludesIngestPaths(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"/v1/traces", true, "OTLP standard path"},
		{"/api/v1/v1/traces", true, "API-key OTLP path"},
		{"/api/v1/logs", true, "log ingest"},
		{"/api/v1/logs/stream", true, "log ingest subpath"},
		{"/api/v1/errors", true, "error ingest"},
		{"/api/v1/events", true, "event ingest"},
		{"/api/v1/events/batch", true, "event batch ingest subpath"},
		{"/api/v1/replays", true, "replay ingest"},
		{"/healthz", true, "health probe"},
		{"/assets/main-abc.js", true, "static asset"},
		{"/api/v1/sites", false, "regular API call should be traced"},
		{"/api/v1/dashboards/abc/panels", false, "platform API should be traced"},
		{"/login", false, "login page should be traced"},
		{"/api/v1/auth/login", false, "auth/login itself should be traced (we can revisit if it floods)"},
		{"", false, "empty path is not in skip list"},
	}
	for _, c := range cases {
		got := shouldSkip(c.path)
		if got != c.want {
			t.Errorf("shouldSkip(%q) = %v, want %v (%s)", c.path, got, c.want, c.why)
		}
	}
}

func TestStatusRecorderCapturesExplicitWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	sr.WriteHeader(404)
	if sr.status != 404 {
		t.Errorf("status = %d, want 404", sr.status)
	}
	if rec.Code != 404 {
		t.Errorf("underlying recorder Code = %d, want 404", rec.Code)
	}
}

func TestStatusRecorderIgnoresSecondWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	sr.WriteHeader(500)
	sr.WriteHeader(200) // should be ignored — first write wins
	if sr.status != 500 {
		t.Errorf("status = %d, want 500 (first WriteHeader wins)", sr.status)
	}
}

func TestStatusRecorderDefaultsTo200OnPlainWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	if _, err := sr.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sr.status != 200 {
		t.Errorf("status = %d, want 200 (no explicit WriteHeader)", sr.status)
	}
	if !sr.wrote {
		t.Errorf("wrote flag should be set after Write")
	}
}

func TestNilSelfMiddlewarePassThrough(t *testing.T) {
	var s *Self
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(204)
	})

	// Both methods should return next unchanged when Self is nil-ish.
	s.TraceMiddleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !called {
		t.Errorf("nil Self TraceMiddleware should pass through to next")
	}

	called = false
	s.RecoverMiddleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !called {
		t.Errorf("nil Self RecoverMiddleware should pass through to next")
	}
}

func TestEmptySelfMiddlewarePassThrough(t *testing.T) {
	s := &Self{} // Client is nil
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	s.TraceMiddleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !called {
		t.Errorf("empty Self TraceMiddleware should pass through")
	}

	called = false
	s.RecoverMiddleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !called {
		t.Errorf("empty Self RecoverMiddleware should pass through")
	}
}

func TestEmptySelfLoggerReturnsDefault(t *testing.T) {
	var s *Self
	if s.Logger() == nil {
		t.Errorf("nil Self Logger should return slog.Default(), not nil")
	}
}

func TestEmptySelfCloseIsNoop(t *testing.T) {
	var s *Self
	if err := s.Close(); err != nil {
		t.Errorf("nil Self Close should be no-op, got %v", err)
	}
	s2 := &Self{}
	if err := s2.Close(); err != nil {
		t.Errorf("empty Self Close should be no-op, got %v", err)
	}
}
