package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/audit"
)

// fakeAuditStore records List filters and captures written events.
type fakeAuditStore struct {
	lastFilter audit.Filter
	listResult []audit.AuditEvent
	listErr    error
	recorded   []audit.AuditEvent
	recordErr  error
}

func (f *fakeAuditStore) List(_ context.Context, flt audit.Filter) ([]audit.AuditEvent, error) {
	f.lastFilter = flt
	return f.listResult, f.listErr
}

func (f *fakeAuditStore) Record(_ context.Context, ev audit.AuditEvent) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, ev)
	return nil
}

func TestAuditListHandler_ParsesFilters(t *testing.T) {
	store := &fakeAuditStore{listResult: []audit.AuditEvent{{AuditID: "a", Action: "auth.login"}}}
	h := auditListHandler(store)

	req := httptest.NewRequest("GET", "/api/v1/audit?site_id=s1&actor=alice&action=auth.login&result=failure&from=1000&to=2000&limit=25", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	f := store.lastFilter
	if f.SiteID != "s1" || f.Actor != "alice" || f.Action != "auth.login" || f.Result != "failure" {
		t.Errorf("string filters not parsed: %+v", f)
	}
	if f.From != 1000 || f.To != 2000 || f.Limit != 25 {
		t.Errorf("numeric filters not parsed: %+v", f)
	}

	var got []audit.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].AuditID != "a" {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestAuditListHandler_EmptyIsJSONArray(t *testing.T) {
	// nil result must serialize as [] not null.
	store := &fakeAuditStore{listResult: nil}
	w := httptest.NewRecorder()
	auditListHandler(store)(w, httptest.NewRequest("GET", "/api/v1/audit", nil))
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("empty audit list should be [], got %q", w.Body.String())
	}
}

func TestAuditRecordHandler_StampsServerSide(t *testing.T) {
	store := &fakeAuditStore{}
	h := auditRecordHandler(store)

	body := `{"site_id":"s1","actor":"cli-agent","actor_type":"agent","action":"deploy.run","target":"web","result":"success","metadata":{"host":"box1"}}`
	req := httptest.NewRequest("POST", "/api/v1/audit", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("User-Agent", "teploy-cli/1.0")
	// A producer must not be able to forge source IP via X-Forwarded-For.
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	if len(store.recorded) != 1 {
		t.Fatalf("expected 1 recorded event, got %d", len(store.recorded))
	}
	ev := store.recorded[0]
	if ev.Action != "deploy.run" || ev.Actor != "cli-agent" || ev.ActorType != "agent" {
		t.Errorf("payload fields not carried: %+v", ev)
	}
	if ev.SourceIP != "203.0.113.9" {
		t.Errorf("source IP should be the direct peer, not XFF: %q", ev.SourceIP)
	}
	if ev.UserAgent != "teploy-cli/1.0" {
		t.Errorf("user agent not stamped: %q", ev.UserAgent)
	}
	if ev.Metadata != `{"host":"box1"}` {
		t.Errorf("metadata not marshaled: %q", ev.Metadata)
	}
}

func TestAuditRecordHandler_BadJSON(t *testing.T) {
	store := &fakeAuditStore{}
	w := httptest.NewRecorder()
	auditRecordHandler(store)(w, httptest.NewRequest("POST", "/api/v1/audit", strings.NewReader("{not json")))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
	if len(store.recorded) != 0 {
		t.Errorf("nothing should be recorded on bad JSON")
	}
}
