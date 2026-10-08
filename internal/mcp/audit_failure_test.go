package mcp

import (
	"context"
	"errors"
	"github.com/useteploy/teploy-observe/internal/audit"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingRecorder struct {
	failAt   int
	events   []audit.AuditEvent
	canceled bool
}

func (f *failingRecorder) Record(ctx context.Context, ev audit.AuditEvent) error {
	f.canceled = f.canceled || ctx.Err() != nil
	f.events = append(f.events, ev)
	if len(f.events) == f.failAt {
		return errors.New("synthetic audit outage")
	}
	return nil
}
func TestToolAuditFailureContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAt    int
		runErr    bool
		role      string
		tool      string
		wantCalls int
	}{
		{"intent", 1, false, RoleEditor, "test", 0},
		{"completion", 2, false, RoleEditor, "test", 1},
		{"tool failure completion", 2, true, RoleEditor, "test", 1},
		{"denied", 1, false, RoleViewer, "test", 0},
		{"unknown", 1, false, RoleEditor, "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &failingRecorder{failAt: tc.failAt}
			calls := 0
			h := NewHandler(nil, []Tool{{Name: "test", Run: func(context.Context, map[string]interface{}) (string, error) {
				calls++
				if tc.runErr {
					return "", errors.New("tool failed")
				}
				return "synthetic sensitive output", nil
			}}}, "test", rec)
			req := httptest.NewRequest("POST", "/api/mcp", nil)
			result := h.callTool(req, tc.tool, nil, Token{ID: "id", Role: tc.role})
			if calls != tc.wantCalls || result["isError"] != true || !strings.Contains(directResultText(result), "audit") || strings.Contains(directResultText(result), "sensitive output") {
				t.Fatalf("calls=%d result=%v", calls, result)
			}
		})
	}
}
func TestToolCompletionAuditSurvivesRequestCancellation(t *testing.T) {
	rec := &failingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	h := NewHandler(nil, []Tool{{Name: "test", ReadOnly: true, Run: func(context.Context, map[string]interface{}) (string, error) { cancel(); return "ok", nil }}}, "test", rec)
	req := httptest.NewRequest("POST", "/api/mcp", nil).WithContext(ctx)
	result := h.callTool(req, "test", nil, Token{ID: "id", Role: RoleViewer})
	if result["isError"] == true || rec.canceled || len(rec.events) != 2 || rec.events[1].Result != audit.ResultSuccess {
		t.Fatalf("result=%v events=%v canceled=%v", result, rec.events, rec.canceled)
	}
}
func TestMissingRecorderRefusesExecution(t *testing.T) {
	calls := 0
	h := NewHandler(nil, []Tool{{Name: "test", ReadOnly: true, Run: func(context.Context, map[string]interface{}) (string, error) { calls++; return "ok", nil }}}, "test", nil)
	result := h.callTool(httptest.NewRequest("POST", "/api/mcp", nil), "test", nil, Token{ID: "id"})
	if calls != 0 || result["isError"] != true {
		t.Fatalf("unaudited call: %v", result)
	}
}
func TestSQLPrivacyRefusalsBeforeBackend(t *testing.T) {
	srv, _, viewer, b, _ := testServer(t)
	defer srv.Close()
	for _, name := range []string{"observe_query", "observe_explain"} {
		for _, sql := range []string{"SELECT KV_GET('fixture')", "SELECT KV_SET('fixture','x')", "SELECT session_salt AS session_salt FROM sites", "SELECT s.value FROM sites AS s JOIN metric_points AS m ON s.site_id=m.site_id"} {
			result := callTool(t, srv.URL, viewer, name, map[string]interface{}{"sql": sql})
			if result["isError"] != true {
				t.Fatalf("%s accepted %s", name, sql)
			}
		}
	}
	if len(b.calls) != 0 {
		t.Fatalf("private query reached backend: %v", b.calls)
	}
	result := callTool(t, srv.URL, viewer, "observe_ask", map[string]interface{}{"question": strings.Repeat("x", 4001)})
	if result["isError"] != true || len(b.calls) != 0 {
		t.Fatalf("oversize question reached backend: %v", result)
	}
}

func directResultText(result map[string]interface{}) string {
	return result["content"].([]map[string]string)[0]["text"]
}
