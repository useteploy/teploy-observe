package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/useteploy/teploy-observe/internal/audit"
)

type recordingStore struct{ events []audit.AuditEvent }

func (r *recordingStore) Record(_ context.Context, ev audit.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

func TestAuditableRequest(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/sites", false},           // reads are not audited
		{"POST", "/api/v1/sites", true},            // admin mutation
		{"DELETE", "/api/v1/exports/scheduled/1", true},
		{"POST", "/api/v1/ingest", false},          // telemetry firehose
		{"POST", "/api/v1/auth/login", false},      // recorded directly
		{"POST", "/api/v1/audit", false},           // producer endpoint (no double-record)
		{"POST", "/api/v1/checkin/x", false},       // cron pings
		{"POST", "/api/v1/flags/evaluate", false},  // high-volume eval
		{"POST", "/other", false},                  // outside /api/v1
	}
	for _, c := range cases {
		if got := auditableRequest(c.method, c.path); got != c.want {
			t.Errorf("auditableRequest(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestDeriveAction(t *testing.T) {
	cases := map[string]string{
		"POST /api/v1/sites":                        "sites.create",
		"DELETE /api/v1/exports/scheduled/deadbeef1234abcd00": "exports.scheduled.delete",
		"POST /api/v1/exports/scheduled/42/run":     "exports.scheduled.run.create",
		"PUT /api/v1/ai/config":                     "ai.config.update",
		"DELETE /api/v1/users/12345":                "users.delete",
	}
	for in, want := range cases {
		var method, path string
		_, _ = method, path
		// split "METHOD /path"
		for i := 0; i < len(in); i++ {
			if in[i] == ' ' {
				method, path = in[:i], in[i+1:]
				break
			}
		}
		if got := deriveAction(method, path); got != want {
			t.Errorf("deriveAction(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditResult(t *testing.T) {
	if auditResult(200) != audit.ResultSuccess {
		t.Error("2xx should be success")
	}
	if auditResult(403) != audit.ResultDenied || auditResult(401) != audit.ResultDenied {
		t.Error("401/403 should be denied")
	}
	if auditResult(500) != audit.ResultFailure {
		t.Error("5xx should be failure")
	}
}

// parseActor stub: "goodtoken" → alice, anything else invalid.
func fakeParseActor(tok string) (string, bool) {
	if tok == "goodtoken" {
		return "alice", true
	}
	return "", false
}

func TestAuditMiddleware_RecordsMutationNotReads(t *testing.T) {
	store := &recordingStore{}
	mw := auditMiddleware(store, fakeParseActor)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := mw(next)

	// A read must not be recorded.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/sites", nil))
	if len(store.events) != 0 {
		t.Fatalf("GET should not be audited, got %d events", len(store.events))
	}

	// A mutation must be recorded with derived action + result + UA + the actor
	// parsed from the bearer token (not "system").
	const id = "0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest("DELETE", "/api/v1/sites/"+id, nil)
	req.RemoteAddr = "203.0.113.5:4444"
	req.Header.Set("User-Agent", "admin-console")
	req.Header.Set("Authorization", "Bearer goodtoken")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(store.events) != 1 {
		t.Fatalf("mutation should be audited, got %d", len(store.events))
	}
	ev := store.events[0]
	if ev.Action != "sites.delete" || ev.Target != "/api/v1/sites/"+id || ev.Result != audit.ResultSuccess {
		t.Errorf("unexpected event: %+v", ev)
	}
	if ev.Actor != "alice" || ev.ActorType != audit.ActorUser {
		t.Errorf("actor not attributed from token: actor=%q type=%q", ev.Actor, ev.ActorType)
	}
	if ev.UserAgent != "admin-console" {
		t.Errorf("user agent not captured: %q", ev.UserAgent)
	}
}

func TestAuditMiddleware_RecordsDenied(t *testing.T) {
	store := &recordingStore{}
	// A handler that 403s (e.g. requireAdmin rejecting a non-admin). No token →
	// actor is system, result denied.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	auditMiddleware(store, fakeParseActor)(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/users", nil))
	if len(store.events) != 1 || store.events[0].Result != audit.ResultDenied {
		t.Fatalf("denied mutation should be recorded as denied: %+v", store.events)
	}
	if store.events[0].Actor != "" || store.events[0].ActorType != audit.ActorSystem {
		t.Errorf("no-token attempt should be system: %+v", store.events[0])
	}
}
