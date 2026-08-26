package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy-observe/internal/audit"
)

// ── fakes ────────────────────────────────────────────────────────────────

// memVerifier is an in-memory Verifier so the protocol suite runs without a
// database. TokenStore's own behaviour is covered in tokens_test.go.
type memVerifier struct{ tokens map[string]Token }

func (m *memVerifier) Verify(_ context.Context, plaintext string) (Token, bool) {
	t, ok := m.tokens[plaintext]
	if !ok || t.Revoked() {
		return Token{}, false
	}
	return t, true
}

type recordingAudit struct {
	mu     sync.Mutex
	events []audit.AuditEvent
}

func (r *recordingAudit) Record(_ context.Context, ev audit.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingAudit) all() []audit.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.AuditEvent(nil), r.events...)
}

// fakeBackend records which service calls the tools actually made. A tool that
// was refused must leave NO trace here — the point of every boundary test below
// is that the refusal happens before the backend, not after.
type fakeBackend struct {
	calls []string
	// generated is what GenerateSQL hands back, standing in for the LLM.
	generated string
}

func (f *fakeBackend) rec(s string) (string, error) {
	f.calls = append(f.calls, s)
	return "ok:" + s, nil
}

func (f *fakeBackend) ListTables(context.Context) ([]string, error) {
	f.calls = append(f.calls, "list_tables")
	return AllowedTables(), nil
}
func (f *fakeBackend) Query(_ context.Context, sql string) (string, error) {
	return f.rec("query " + sql)
}
func (f *fakeBackend) Explain(_ context.Context, sql string) (string, error) {
	return f.rec("explain " + sql)
}
func (f *fakeBackend) GenerateSQL(_ context.Context, question, card string) (string, error) {
	f.calls = append(f.calls, "generate "+question)
	if !strings.Contains(card, "stats_daily") {
		return "", fmt.Errorf("schema card was not built from the allowlist")
	}
	return f.generated, nil
}
func (f *fakeBackend) ActiveIncidents(_ context.Context, site string) (string, error) {
	return f.rec("incidents_active " + site)
}
func (f *fakeBackend) IncidentsInRange(_ context.Context, site string, from, to int64) (string, error) {
	return f.rec(fmt.Sprintf("incidents_range %s %d %d", site, from, to))
}
func (f *fakeBackend) LiveStats(_ context.Context, site string, minutes int) (string, error) {
	return f.rec(fmt.Sprintf("live %s %d", site, minutes))
}
func (f *fakeBackend) ListFlags(_ context.Context, site string) (string, error) {
	return f.rec("flags " + site)
}

// mutatingTool stands in for the second-pass mutation set (create/close
// incident, evaluate flag) so the role gate that will carry them is exercised
// now rather than the first time one is added.
func mutatingTool(b *fakeBackend) Tool {
	return Tool{
		Name:        "observe_test_mutate",
		Description: "test-only mutating tool",
		InputSchema: schema(nil, nil),
		Destructive: true,
		Run: func(context.Context, map[string]interface{}) (string, error) {
			return b.rec("mutate")
		},
	}
}

func testServer(t *testing.T) (*httptest.Server, string, string, *fakeBackend, *recordingAudit) {
	t.Helper()
	b := &fakeBackend{generated: "SELECT ts_bucket, pageviews FROM stats_daily"}
	v := &memVerifier{tokens: map[string]Token{
		TokenPrefix + "editor": {ID: "ed01", Name: "editor-token", Role: RoleEditor},
		TokenPrefix + "viewer": {ID: "vw01", Name: "viewer-token", Role: RoleViewer},
		TokenPrefix + "gone":   {ID: "rv01", Name: "revoked-token", Role: RoleEditor, RevokedAt: 1},
	}}
	rec := &recordingAudit{}
	h := NewHandler(v, append(Tools(b), mutatingTool(b)), "test", rec)
	return httptest.NewServer(h), TokenPrefix + "editor", TokenPrefix + "viewer", b, rec
}

func rpc(t *testing.T, url, token, method string, params interface{}) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d for %s", resp.StatusCode, method)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func callTool(t *testing.T, url, token, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	out := rpc(t, url, token, "tools/call", map[string]interface{}{"name": name, "arguments": args})
	res, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result in %v", out)
	}
	return res
}

func resultText(t *testing.T, res map[string]interface{}) string {
	t.Helper()
	content, ok := res["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("no content in %v", res)
	}
	return content[0].(map[string]interface{})["text"].(string)
}

// ── protocol (ported from Dash) ──────────────────────────────────────────

func TestUnauthorized(t *testing.T) {
	srv, _, _, _, rec := testServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header")
	}
	events := rec.all()
	if len(events) != 1 || events[0].Action != "mcp.auth" || events[0].Result != audit.ResultDenied {
		t.Fatalf("a rejected credential must be audited, got %+v", events)
	}
}

// A revoked token must be refused exactly as a forged one is. Without the
// Revoked() check in Verify/match the record still hashes correctly and would
// authenticate.
func TestRevokedTokenRefused(t *testing.T) {
	srv, _, _, b, _ := testServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+TokenPrefix+"gone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("revoked token got %d, want 401", resp.StatusCode)
	}
	if len(b.calls) != 0 {
		t.Fatalf("revoked token reached the backend: %v", b.calls)
	}
}

func TestGetMethodNotAllowed(t *testing.T) {
	srv, _, _, _, _ := testServer(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("expected 405 on GET, got %d", resp.StatusCode)
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	srv, editor, viewer, _, _ := testServer(t)
	defer srv.Close()

	out := rpc(t, srv.URL, editor, "initialize", map[string]interface{}{"protocolVersion": "2025-06-18"})
	result := out["result"].(map[string]interface{})
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol echo failed: %v", result)
	}
	if info := result["serverInfo"].(map[string]interface{}); info["name"] != "teploy-observe" {
		t.Fatalf("serverInfo = %v", info)
	}

	full := rpc(t, srv.URL, editor, "tools/list", nil)["result"].(map[string]interface{})["tools"].([]interface{})
	ro := rpc(t, srv.URL, viewer, "tools/list", nil)["result"].(map[string]interface{})["tools"].([]interface{})
	if len(ro) >= len(full) {
		t.Fatalf("viewer list (%d) should be smaller than editor list (%d)", len(ro), len(full))
	}
	for _, tl := range ro {
		ann := tl.(map[string]interface{})["annotations"].(map[string]interface{})
		if ann["readOnlyHint"] != true {
			t.Fatalf("read-only token saw mutating tool: %v", tl)
		}
	}
}

// The shipped v1 surface, pinned. A tool added without a decision about what it
// can reach breaks this test.
func TestShippedToolSet(t *testing.T) {
	want := []string{
		"observe_ask", "observe_explain", "observe_list_flags", "observe_list_incidents",
		"observe_live_stats", "observe_query", "observe_tables",
	}
	got := ToolNames(Tools(&fakeBackend{}))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool set = %v, want %v", got, want)
	}
	for _, tl := range Tools(&fakeBackend{}) {
		if !tl.ReadOnly {
			t.Fatalf("v1 ships read-only tools only; %s is not", tl.Name)
		}
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	srv, editor, _, _, _ := testServer(t)
	defer srv.Close()

	res := callTool(t, srv.URL, editor, "nope", nil)
	if res["isError"] != true {
		t.Fatal("unknown tool should be an isError result")
	}

	out := rpc(t, srv.URL, editor, "wat/method", nil)
	if out["error"] == nil {
		t.Fatal("unknown method should be a protocol error")
	}
}

func TestNotificationAccepted(t *testing.T) {
	srv, editor, _, _, _ := testServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+editor)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("expected 202 for notification, got %d", resp.StatusCode)
	}
}

func TestMissingArgs(t *testing.T) {
	srv, editor, _, _, _ := testServer(t)
	defer srv.Close()

	res := callTool(t, srv.URL, editor, "observe_query", map[string]interface{}{})
	if res["isError"] != true {
		t.Fatal("missing sql arg should be an error result")
	}
	if !strings.Contains(resultText(t, res), "sql") {
		t.Fatalf("error should name the missing arg: %s", resultText(t, res))
	}
}

func rawPost(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRejectsMissingOrWrongJSONRPCVersion(t *testing.T) {
	srv, editor, _, _, _ := testServer(t)
	defer srv.Close()

	cases := []string{
		`{"id":1,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2","id":1,"method":"ping"}`,
	}
	for _, body := range cases {
		resp := rawPost(t, srv.URL, editor, body)
		var out rpcResponse
		err := json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("body %q: invalid JSON response: %v", body, err)
		}
		if out.Error == nil || out.Error.Code != -32600 {
			t.Errorf("body %q: error = %+v, want code -32600", body, out.Error)
		}
	}
}

func TestRejectsTrailingJSONValue(t *testing.T) {
	srv, editor, _, _, _ := testServer(t)
	defer srv.Close()

	resp := rawPost(t, srv.URL, editor, `{"jsonrpc":"2.0","id":1,"method":"ping"}{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if out.Error == nil || out.Error.Code != -32700 {
		t.Errorf("error = %+v, want code -32700 (parse error)", out.Error)
	}
}

// ── RBAC ─────────────────────────────────────────────────────────────────

func TestReadOnlyTokenCannotMutate(t *testing.T) {
	srv, editor, viewer, b, rec := testServer(t)
	defer srv.Close()

	// The editor token can.
	res := callTool(t, srv.URL, editor, "observe_test_mutate", nil)
	if res["isError"] == true {
		t.Fatalf("editor mutation errored: %v", res)
	}
	if len(b.calls) != 1 || b.calls[0] != "mutate" {
		t.Fatalf("backend not called: %v", b.calls)
	}

	// The viewer token cannot, and does not reach the backend.
	res = callTool(t, srv.URL, viewer, "observe_test_mutate", nil)
	if res["isError"] != true {
		t.Fatalf("viewer mutation should be refused: %v", res)
	}
	if len(b.calls) != 1 {
		t.Fatalf("refused mutation still reached the backend: %v", b.calls)
	}
	if !strings.Contains(resultText(t, res), "read-only") {
		t.Fatalf("refusal should say why: %s", resultText(t, res))
	}

	denied := 0
	for _, ev := range rec.all() {
		if ev.Action == "mcp.observe_test_mutate" && ev.Result == audit.ResultDenied {
			denied++
			if ev.Actor != "vw01" {
				t.Fatalf("denial attributed to %q, want the viewer token id", ev.Actor)
			}
		}
	}
	if denied != 1 {
		t.Fatalf("a denied mutation must be audited exactly once, got %d", denied)
	}

	// Read tools still work for a viewer token.
	res = callTool(t, srv.URL, viewer, "observe_list_flags", nil)
	if res["isError"] == true {
		t.Fatalf("read tool failed for viewer token: %v", res)
	}
}

// ── the data boundary, over the wire ─────────────────────────────────────

// A person-level table must be refused through observe_query, and refused
// BEFORE the explorer service is called — a refusal that filtered results after
// the read would still have read them.
func TestQueryToolRefusesDisallowedTable(t *testing.T) {
	srv, editor, _, b, rec := testServer(t)
	defer srv.Close()

	for _, sql := range []string{
		"SELECT distinct_id FROM events",
		"SELECT session_id FROM sessions",
		"SELECT cohort_id FROM cohorts",
		"SELECT session_id FROM replay_sessions",
		"SELECT x FROM click_heatmaps",
		"SELECT prompt FROM llm_traces",
		"SELECT password_hash FROM users",
		"SELECT table_name FROM information_schema.tables",
		// nested, so the check cannot be a first-table-only check
		"WITH t AS (SELECT distinct_id FROM events) SELECT distinct_id FROM t",
		"SELECT ts_bucket FROM stats_daily WHERE site_id IN (SELECT site_id FROM api_keys)",
		// and the withheld columns of allowed tables
		"SELECT session_salt FROM sites",
		"SELECT ping_token FROM cron_monitors",
		"SELECT targeting FROM feature_flags",
	} {
		res := callTool(t, srv.URL, editor, "observe_query", map[string]interface{}{"sql": sql})
		if res["isError"] != true {
			t.Errorf("%q was NOT refused", sql)
		}
	}
	if len(b.calls) != 0 {
		t.Fatalf("a refused query reached the backend: %v", b.calls)
	}

	// An allowed query proves the refusals above are the allowlist talking and
	// not the tool being broken.
	res := callTool(t, srv.URL, editor, "observe_query",
		map[string]interface{}{"sql": "SELECT ts_bucket, sum(pageviews) AS views FROM stats_daily GROUP BY ts_bucket ORDER BY ts_bucket"})
	if res["isError"] == true {
		t.Fatalf("an allowlisted query was refused: %s", resultText(t, res))
	}
	if len(b.calls) != 1 {
		t.Fatalf("allowed query did not reach the backend: %v", b.calls)
	}

	for _, ev := range rec.all() {
		if ev.Action != "mcp.observe_query" {
			continue
		}
		if !strings.Contains(ev.Metadata, "sql") {
			t.Fatalf("audit metadata must carry what was asked: %s", ev.Metadata)
		}
	}
}

// The same boundary, through the model. This is the smuggling path the scope
// doc worries about: a question phrased so the assistant drafts SQL against a
// person-level table. The generated statement goes through the SAME gate, so it
// is refused and never executed.
func TestAskToolGatesGeneratedSQL(t *testing.T) {
	srv, editor, _, b, _ := testServer(t)
	defer srv.Close()

	b.generated = "SELECT distinct_id, count(*) AS n FROM events GROUP BY distinct_id"
	res := callTool(t, srv.URL, editor, "observe_ask", map[string]interface{}{"question": "who visits most"})
	if res["isError"] != true {
		t.Fatalf("generated person-level SQL was returned: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "data boundary") {
		t.Fatalf("refusal should name the boundary: %s", resultText(t, res))
	}

	// Identical path, allowlisted table: the SQL comes back. The only variable
	// between this and the case above is the table name, which is what makes
	// the gate — not the tool — the thing doing the refusing.
	b.generated = "SELECT ts_bucket, pageviews FROM stats_daily ORDER BY ts_bucket"
	res = callTool(t, srv.URL, editor, "observe_ask", map[string]interface{}{"question": "traffic by day"})
	if res["isError"] == true {
		t.Fatalf("allowlisted generated SQL was refused: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "stats_daily") {
		t.Fatalf("observe_ask should return the SQL: %s", resultText(t, res))
	}
	_ = b
}

// ── audit ────────────────────────────────────────────────────────────────

// Every call, of every kind, lands in the trail. Without the record() calls in
// callTool this fails on the first tool.
func TestEveryCallIsAudited(t *testing.T) {
	srv, editor, _, _, rec := testServer(t)
	defer srv.Close()

	callTool(t, srv.URL, editor, "observe_tables", nil)
	callTool(t, srv.URL, editor, "observe_list_flags", map[string]interface{}{"site_id": "shop"})
	callTool(t, srv.URL, editor, "observe_query", map[string]interface{}{"sql": "SELECT distinct_id FROM events"})
	callTool(t, srv.URL, editor, "observe_live_stats", nil)
	callTool(t, srv.URL, editor, "does_not_exist", nil)

	events := rec.all()
	if len(events) != 5 {
		t.Fatalf("expected 5 audit events, got %d: %+v", len(events), events)
	}
	wantActions := []string{
		"mcp.observe_tables", "mcp.observe_list_flags", "mcp.observe_query",
		"mcp.observe_live_stats", "mcp.does_not_exist",
	}
	for i, ev := range events {
		if ev.Action != wantActions[i] {
			t.Errorf("event %d action = %q, want %q", i, ev.Action, wantActions[i])
		}
		if ev.Actor != "ed01" {
			t.Errorf("event %d actor = %q, want the token id", i, ev.Actor)
		}
		if ev.ActorType != audit.ActorAgent {
			t.Errorf("event %d actor_type = %q, want agent", i, ev.ActorType)
		}
		if ev.Target != "editor-token" {
			t.Errorf("event %d target = %q, want the token name", i, ev.Target)
		}
		if !strings.Contains(ev.Metadata, `"token_id":"ed01"`) {
			t.Errorf("event %d metadata missing token id: %s", i, ev.Metadata)
		}
	}
	// The refused query is recorded as a failure carrying the SQL that was
	// asked, so the trail can reconstruct the attempt.
	q := events[2]
	if q.Result != audit.ResultFailure {
		t.Errorf("refused query result = %q, want failure", q.Result)
	}
	if !strings.Contains(q.Metadata, "FROM events") {
		t.Errorf("refused query metadata should carry the SQL: %s", q.Metadata)
	}
	// site_id rides along so the trail is filterable by site.
	if events[1].SiteID != "shop" {
		t.Errorf("site_id not recorded: %+v", events[1])
	}
}

func TestAuditArgumentsAreBounded(t *testing.T) {
	srv, editor, _, _, rec := testServer(t)
	defer srv.Close()

	long := "SELECT ts_bucket FROM stats_daily WHERE pathname = '" + strings.Repeat("x", 8000) + "'"
	callTool(t, srv.URL, editor, "observe_query", map[string]interface{}{"sql": long})
	ev := rec.all()[0]
	if len(ev.Metadata) > 3*maxAuditArg {
		t.Fatalf("audit metadata is unbounded: %d bytes", len(ev.Metadata))
	}
	if !strings.Contains(ev.Metadata, "truncated") {
		t.Fatalf("a long argument should be marked truncated: %s", ev.Metadata[:200])
	}
}
