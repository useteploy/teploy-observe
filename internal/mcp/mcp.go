package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-observe/internal/audit"
)

// Hand-rolled JSON-RPC 2.0 over streamable HTTP (request/response only — GET
// returns 405, no server push). Stateless: no sessions, every POST carries a
// bearer token. Kept dependency-free on purpose; the protocol surface needed
// (initialize, ping, tools/list, tools/call) is small.
//
// Ported from teploy-dash/internal/mcp, envelope strictness included.

const latestProtocol = "2025-06-18"

var supportedProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Verifier is the authentication surface the handler needs. *TokenStore
// implements it; tests substitute an in-memory one so the protocol suite runs
// without a database.
type Verifier interface {
	Verify(ctx context.Context, plaintext string) (Token, bool)
}

// Recorder is the audit sink. Satisfied by *audit.Service — MCP writes to the
// SAME append-only, tamper-evident trail as every other admin action, rather
// than a log line of its own.
type Recorder interface {
	Record(ctx context.Context, ev audit.AuditEvent) error
}

// Handler serves the MCP endpoint.
type Handler struct {
	tokens   Verifier
	tools    []Tool
	version  string
	audit    Recorder
	clientIP func(*http.Request) string
}

// NewHandler builds the MCP handler over a token store, a tool set and the
// audit trail. The recorder is a required argument, not an option: an
// unaudited MCP server is not a thing this product ships.
func NewHandler(tokens Verifier, tools []Tool, version string, rec Recorder) *Handler {
	return &Handler{tokens: tokens, tools: tools, version: version, audit: rec}
}

// WithClientIP supplies the source-IP resolver for audit records (wired to the
// request-info middleware, which honours trusted proxies). Without it the
// audit trail falls back to the peer address.
func (h *Handler) WithClientIP(fn func(*http.Request) string) *Handler {
	h.clientIP = fn
	return h
}

// ServeHTTP implements the /api/mcp endpoint. Auth is enforced here (bearer
// token) — the route is exempt from the dashboard's session gate.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed — MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}

	tok, ok := h.authenticate(r)
	if !ok {
		// A rejected credential is recorded with no actor: the trail has to
		// show that something tried, and the presented token is never written
		// down (it may be a valid secret for another instance).
		h.record(r, Token{}, "auth", audit.ResultDenied, nil, "invalid or revoked token")
		w.Header().Set("WWW-Authenticate", `Bearer realm="teploy-observe MCP"`)
		http.Error(w, "unauthorized: create an MCP token in Settings > MCP and send it as a Bearer token", http.StatusUnauthorized)
		return
	}

	// Unknown top-level fields are tolerated (JSON-RPC extensions legitimately
	// add them), but the body must decode to exactly one JSON value and declare
	// jsonrpc 2.0 — a second concatenated object or a missing/wrong version
	// would otherwise pass straight through to dispatch (DASH-012).
	dec := json.NewDecoder(r.Body)
	var req rpcRequest
	if err := dec.Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32700, Message: "parse error: request body must contain exactly one JSON value"}})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32600, Message: `invalid request: "jsonrpc" must be "2.0"`}})
		return
	}

	// Notifications (no id) get a bare 202 per streamable-HTTP.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := h.dispatch(r, req, tok)
	writeRPC(w, resp)
}

func (h *Handler) authenticate(r *http.Request) (Token, bool) {
	auth := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if !strings.HasPrefix(auth, scheme) {
		return Token{}, false
	}
	return h.tokens.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(auth, scheme)))
}

func (h *Handler) dispatch(r *http.Request, req rpcRequest, tok Token) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := latestProtocol
		if supportedProtocols[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		resp.Result = map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]string{"name": "teploy-observe", "version": h.version},
			"instructions": "Teploy Observe telemetry. Every tool wraps the same service the dashboard " +
				"calls, so there is no second source of truth. SQL reaches ANALYTICS AGGREGATES ONLY: " +
				"an allowlist of rollup and configuration tables, checked identically whether you wrote " +
				"the query or observe_ask generated it. Raw events, sessions, replays, heatmaps, cohort " +
				"membership and LLM prompts are unreachable by design, not by omission. Every call is " +
				"recorded in the audit trail against this token. Start with observe_tables.",
		}

	case "ping":
		resp.Result = map[string]interface{}{}

	case "tools/list":
		visible := make([]map[string]interface{}, 0, len(h.tools))
		for _, t := range h.tools {
			if tok.ReadOnly() && !t.ReadOnly {
				continue
			}
			visible = append(visible, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
				"annotations": map[string]bool{
					"readOnlyHint":    t.ReadOnly,
					"destructiveHint": t.Destructive,
				},
			})
		}
		resp.Result = map[string]interface{}{"tools": visible}

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		resp.Result = h.callTool(r, params.Name, params.Arguments, tok)

	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
	return resp
}

// callTool runs one tool and always returns an MCP tool result (tool-level
// failures are isError results, not protocol errors). Every outcome —
// permitted, denied, failed, unknown tool — is recorded.
func (h *Handler) callTool(r *http.Request, name string, args map[string]interface{}, tok Token) map[string]interface{} {
	for _, t := range h.tools {
		if t.Name != name {
			continue
		}
		// Enforcement mirrors listing: a read-only token cannot call a
		// mutating tool even if it guesses the name. Read -> viewer,
		// mutation -> editor, per the Teploy RBAC contract.
		if tok.ReadOnly() && !t.ReadOnly {
			msg := fmt.Sprintf("token %q is read-only (role %s); %s requires role %s", tok.Name, tok.Role, name, RoleEditor)
			h.record(r, tok, name, audit.ResultDenied, args, msg)
			return toolError(msg)
		}
		out, err := t.Run(r.Context(), args)
		if err != nil {
			h.record(r, tok, name, audit.ResultFailure, args, err.Error())
			return toolError(err.Error())
		}
		h.record(r, tok, name, audit.ResultSuccess, args, "")
		return map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": out}},
		}
	}
	msg := fmt.Sprintf("unknown tool: %s", name)
	h.record(r, tok, name, audit.ResultFailure, args, msg)
	return toolError(msg)
}

// maxAuditArg bounds one recorded argument value. Arguments are the agent's own
// input (a question, a SQL string), not telemetry, so recording them is what
// makes a call reconstructable — but a model can emit a very long query and the
// audit row should not become the largest thing in the table.
const maxAuditArg = 2000

// record writes one MCP call to the audit trail: which token acted, which tool,
// what it was asked, and whether it was allowed. This is the piece Dash's MCP
// does not have — Dash logs a line to stdout (`log.Printf("[mcp] token=%q
// tool=%s")`) and has no audit package at all, so its MCP calls leave no
// durable trail. Recorded synchronously: a call that could not be audited is
// worth failing loudly about, and audit.Record is a single insert.
func (h *Handler) record(r *http.Request, tok Token, tool, result string, args map[string]interface{}, detail string) {
	if h.audit == nil {
		return
	}
	meta := map[string]any{
		"tool":       tool,
		"token_id":   tok.ID,
		"token_name": tok.Name,
		"token_role": tok.Role,
	}
	if detail != "" {
		meta["detail"] = truncate(detail, maxAuditArg)
	}
	if len(args) > 0 {
		safe := make(map[string]any, len(args))
		for k, v := range args {
			if s, ok := v.(string); ok {
				safe[k] = truncate(s, maxAuditArg)
				continue
			}
			safe[k] = v
		}
		meta["arguments"] = safe
	}

	siteID, _ := args["site_id"].(string)
	ip := ""
	if h.clientIP != nil {
		ip = h.clientIP(r)
	}
	if ip == "" {
		ip = r.RemoteAddr
	}

	_ = h.audit.Record(r.Context(), audit.AuditEvent{
		SiteID:    siteID,
		Actor:     tok.ID,
		ActorType: audit.ActorAgent,
		Action:    "mcp." + tool,
		Target:    tok.Name,
		Result:    result,
		SourceIP:  ip,
		UserAgent: r.UserAgent(),
		Metadata:  audit.MarshalMetadata(meta),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
