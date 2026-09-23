package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- unit: resolveFederatedTool ---

// TestResolveFederatedTool covers good/bad/edge cases: an unambiguous
// match, the longest-prefix disambiguation between two configured server
// names where one is a prefix of the other, and an unresolvable prefix
// (unconfigured server, or one simply not in the allowed set — the same
// thing from this function's point of view).
func TestResolveFederatedTool(t *testing.T) {
	cases := []struct {
		name           string
		fullName       string
		wantServerName string
		wantToolName   string
		allowedNames   []string
		wantOK         bool
	}{
		{
			name:           "unambiguous single server match",
			fullName:       "brave-search_brave_web_search",
			allowedNames:   []string{"brave-search"},
			wantServerName: "brave-search",
			wantToolName:   "brave_web_search",
			wantOK:         true,
		},
		{
			name:           "longest prefix wins when one server name prefixes another",
			fullName:       "foo_bar_lookup",
			allowedNames:   []string{"foo", "foo_bar"},
			wantServerName: "foo_bar",
			wantToolName:   "lookup",
			wantOK:         true,
		},
		{
			name:         "unknown prefix (no configured server matches)",
			fullName:     "nosuchserver_tool",
			allowedNames: []string{"alpha", "beta"},
			wantOK:       false,
		},
		{
			name:         "prefix belongs to a real server not in the allowed set (authz-excluded)",
			fullName:     "beta_tool",
			allowedNames: []string{"alpha"}, // beta exists in config elsewhere but is not allowed here
			wantOK:       false,
		},
		{
			name:         "empty allowed set",
			fullName:     "alpha_tool",
			allowedNames: nil,
			wantOK:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverName, toolName, ok := resolveFederatedTool(tc.fullName, tc.allowedNames)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if serverName != tc.wantServerName || toolName != tc.wantToolName {
				t.Errorf("resolveFederatedTool(%q, %v) = (%q, %q), want (%q, %q)",
					tc.fullName, tc.allowedNames, serverName, toolName, tc.wantServerName, tc.wantToolName)
			}
		})
	}
}

// --- unit: parseBackendJSONRPC (JSON and SSE-shaped backend responses) ---

func TestParseBackendJSONRPC(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantResult  string
		wantErr     bool
	}{
		{
			name:        "plain JSON object",
			contentType: "application/json",
			body:        `{"jsonrpc":"2.0","id":"federated","result":{"ok":true}}`,
			wantResult:  `{"ok":true}`,
		},
		{
			name:        "SSE stream, single data event",
			contentType: "text/event-stream",
			body:        "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"federated\",\"result\":{\"ok\":true}}\n\n",
			wantResult:  `{"ok":true}`,
		},
		{
			name:        "SSE stream, last of multiple data events wins",
			contentType: "text/event-stream",
			body:        "data: {\"jsonrpc\":\"2.0\",\"id\":\"federated\",\"result\":{\"stage\":1}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"federated\",\"result\":{\"stage\":2}}\n\n",
			wantResult:  `{"stage":2}`,
		},
		{
			// F6, review round 3, 2026-09: a single event's own "data:"
			// field can legitimately span several physical lines (this
			// gateway's own sseWriter.writeData emits exactly this shape
			// for a multi-line payload) — the SSE wire format joins them
			// with "\n" into one logical payload, not two separate
			// events. Split right after the "result" key's colon, where
			// JSON permits whitespace (including a newline) before the
			// value.
			name:        "SSE event with one data field split across two data: lines, joined with newline",
			contentType: "text/event-stream",
			body:        "data: {\"jsonrpc\":\"2.0\",\"id\":\"federated\",\"result\":\ndata: {\"ok\":true}}\n\n",
			wantResult:  `{"ok":true}`,
		},
		{
			name:        "malformed JSON",
			contentType: "application/json",
			body:        `not json`,
			wantErr:     true,
		},
		{
			name:        "SSE stream with no data: line at all falls through to the raw body",
			contentType: "text/event-stream",
			body:        "event: ping\n\n",
			wantErr:     true, // "event: ping" is not valid JSON on its own
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBackendJSONRPC(tc.contentType, []byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got.Result) != tc.wantResult {
				t.Errorf("Result = %s, want %s", got.Result, tc.wantResult)
			}
		})
	}
}

// --- end-to-end: POST /mcp ---

// mcpToolsToRaw converts the test-fixture []mcpTool shape into the
// []map[string]json.RawMessage wire shape mcpToolsListResult.Tools
// actually decodes/encodes as (F2, review round 3, 2026-09) — every mock
// server in this file builds its own canned tools/list response through
// this, so a test still writes the compact mcpTool{Name: ...} literal
// while the bytes on the wire (and so what mcpFederatedToolsList's own
// F2 field-passthrough sees) match a real backend's shape.
func mcpToolsToRaw(t *testing.T, tools []mcpTool) []map[string]json.RawMessage {
	t.Helper()
	if tools == nil {
		return nil
	}
	out := make([]map[string]json.RawMessage, len(tools))
	for i, tool := range tools {
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal mcpTool: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal mcpTool: %v", err)
		}
		out[i] = m
	}
	return out
}

// mustToolName decodes tool's own "name" field, failing the test if it is
// missing or not a JSON string — used everywhere a test needs to compare
// mcpFederatedToolsList's own merged output (now []map[string]json.RawMessage,
// F2) against an expected name string.
func mustToolName(t *testing.T, tool map[string]json.RawMessage) string {
	t.Helper()
	var name string
	if err := json.Unmarshal(tool["name"], &name); err != nil {
		t.Fatalf("tool has no valid \"name\" field: %+v (%v)", tool, err)
	}
	return name
}

// TestPrefixMCPTool_SkipsInvalidName is the P3 regression test (review
// round 4): json.Unmarshal("null", &toolName) succeeds and leaves
// toolName as its zero value (""), identical to an explicit "name":"" —
// without the fix, either one mints and lists a bare "<server>_" tool
// with no real name behind it, rather than being skipped like the
// already-handled missing-field and non-string-name cases.
func TestPrefixMCPTool_SkipsInvalidName(t *testing.T) {
	cases := []struct {
		tool map[string]json.RawMessage
		name string
	}{
		{name: "missing name field", tool: map[string]json.RawMessage{"x": json.RawMessage(`1`)}},
		{name: "null name", tool: map[string]json.RawMessage{"name": json.RawMessage(`null`)}},
		{name: "empty string name", tool: map[string]json.RawMessage{"name": json.RawMessage(`""`)}},
		{name: "non-string name", tool: map[string]json.RawMessage{"name": json.RawMessage(`42`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, fullName, ok := prefixMCPTool("srv", tc.tool)
			if ok || out != nil || fullName != "" {
				t.Errorf("prefixMCPTool(%+v) = (%+v, %q, %v), want (nil, \"\", false)", tc.tool, out, fullName, ok)
			}
		})
	}
	// Good case, alongside the bad ones above (table style): a real name
	// still prefixes normally.
	t.Run("valid name", func(t *testing.T) {
		out, fullName, ok := prefixMCPTool("srv", map[string]json.RawMessage{"name": json.RawMessage(`"search"`)})
		if !ok || fullName != "srv_search" {
			t.Fatalf("prefixMCPTool = (%+v, %q, %v), want ok=true, fullName=\"srv_search\"", out, fullName, ok)
		}
		var gotName string
		if err := json.Unmarshal(out["name"], &gotName); err != nil || gotName != "srv_search" {
			t.Errorf("out[\"name\"] = %s, want \"srv_search\"", out["name"])
		}
	})
}

// mockJSONRPCServer is a minimal MCP-shaped backend for federation tests:
// it answers "tools/list" with a fixed tool set and "tools/call" by
// echoing back the resolved (prefix-stripped) name/arguments it was
// actually called with, so a test can assert routing stripped the prefix
// correctly. called counts every request this server received, so a test
// can assert a server the caller's group must NOT reach was never
// contacted at all.
type mockJSONRPCServer struct {
	srv    *httptest.Server
	tools  []mcpTool
	called atomic.Int32
}

func newMockJSONRPCServer(t *testing.T, tools []mcpTool) *mockJSONRPCServer {
	t.Helper()
	m := &mockJSONRPCServer{tools: tools}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.called.Add(1)
		var req jsonrpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("mock server: decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			result, _ := json.Marshal(mcpToolsListResult{Tools: mcpToolsToRaw(t, m.tools)})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		case "tools/call":
			var params mcpToolCallParams
			_ = json.Unmarshal(req.Params, &params)
			echo, _ := json.Marshal(map[string]any{"calledName": params.Name, "calledArguments": json.RawMessage(params.Arguments)})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: echo})
		default:
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: jsonrpcMethodNotFound, Message: "method not found"}})
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// sessionRequiredMockServer simulates a backend that rejects a bare
// (session-less) call with a JSON-RPC error but honors a proper
// initialize -> session -> retry handshake — modeling readitall/fetch's
// real, live-probed behavior (mcpBackendCall's own doc comment). It
// records the exact sequence of calls it received (method plus whichever
// session header, if any, accompanied it) so a test can assert the
// handshake shape precisely: bare attempt, initialize, retried real call
// carrying the session header, best-effort DELETE.
type sessionRequiredMockServer struct {
	srv       *httptest.Server
	sessionID string
	tools     []mcpTool
	calls     []string
	mu        sync.Mutex
	failInit  bool
}

func newSessionRequiredMockServer(t *testing.T, tools []mcpTool) *sessionRequiredMockServer {
	t.Helper()
	m := &sessionRequiredMockServer{tools: tools, sessionID: "sess-1"}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession := r.Header.Get(mcpSessionHeader)

		if r.Method == http.MethodDelete {
			m.mu.Lock()
			m.calls = append(m.calls, "DELETE:session="+gotSession)
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var req jsonrpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("session-required mock: decode request: %v", err)
		}
		m.mu.Lock()
		m.calls = append(m.calls, req.Method+":session="+gotSession)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if req.Method == "initialize" {
			if m.failInit {
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "initialize refused"}})
				return
			}
			w.Header().Set(mcpSessionHeader, m.sessionID)
			result, _ := json.Marshal(map[string]any{"protocolVersion": defaultMCPProtocolVersion, "capabilities": map[string]any{}})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
			return
		}

		if gotSession != m.sessionID {
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "session required: call initialize first"}})
			return
		}
		switch req.Method {
		case "tools/list":
			result, _ := json.Marshal(mcpToolsListResult{Tools: mcpToolsToRaw(t, m.tools)})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		case "tools/call":
			var params mcpToolCallParams
			_ = json.Unmarshal(req.Params, &params)
			echo, _ := json.Marshal(map[string]any{"calledName": params.Name})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: echo})
		default:
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: jsonrpcMethodNotFound, Message: "method not found"}})
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *sessionRequiredMockServer) callSequence() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// newFederationTestConfig builds a Config with two MCP servers ("alpha",
// "beta"), a group whose mcpServers glob restricts it to alphaAllowed
// only when alphaOnly is true (otherwise unrestricted), and one inline
// user "alice" in that group.
func newFederationTestConfig(alphaURL, betaURL string, alphaOnly bool) *Config {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"alpha": {URL: alphaURL},
		"beta":  {URL: betaURL},
	}
	grpCfg := &GroupConfig{}
	if alphaOnly {
		grpCfg.MCPServers = []string{"alpha"}
	}
	cfg.Groups = map[string]*GroupConfig{"default": grpCfg}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	return cfg
}

func newFederatedRequest(t *testing.T, apiKey string, req jsonrpcRequest) *http.Request {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, federatedMCPPath, bytes.NewReader(body))
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return httpReq
}

func TestHandleMCPFederated_Unauthenticated_Returns401(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMCPFederated_MalformedBody_ReturnsJSONRPCParseError(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, federatedMCPPath, bytes.NewReader([]byte("not json")))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC errors are payload-level, not transport-level), body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcParseError {
		t.Errorf("error = %+v, want code %d (parse error)", got.Error, jsonrpcParseError)
	}
	if string(got.ID) != "null" {
		t.Errorf("id = %s, want literal null (JSON-RPC 2.0 §5.1: id must be null, not omitted, when it cannot be determined)", got.ID)
	}
}

func TestHandleMCPFederated_UnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "resources/list", ID: json.RawMessage("7")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcMethodNotFound {
		t.Errorf("error = %+v, want code %d (method not found)", got.Error, jsonrpcMethodNotFound)
	}
	if string(got.ID) != "7" {
		t.Errorf("id = %s, want the client's own id 7 echoed back", got.ID)
	}
}

func TestHandleMCPFederated_Notification_Returns202NoBody(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "notifications/initialized"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty (a notification gets no response body)", rec.Body.String())
	}
}

func TestHandleMCPFederated_Initialize_EchoesProtocolVersionLocally(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	params, _ := json.Marshal(map[string]any{"protocolVersion": "2024-11-05"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "initialize", ID: json.RawMessage("1"), Params: params,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil", got.Error)
	}
	var result struct {
		Capabilities struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want the caller's own requested version echoed back", result.ProtocolVersion)
	}
	if result.Capabilities.Tools == nil {
		t.Error("capabilities.tools must be present (tool-calling support declared)")
	}
}

func TestHandleMCPFederated_Initialize_DefaultsWhenCallerOmitsVersion(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "initialize", ID: json.RawMessage("1")}))
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProtocolVersion != defaultMCPProtocolVersion {
		t.Errorf("protocolVersion = %q, want the fallback default %q", result.ProtocolVersion, defaultMCPProtocolVersion)
	}
}

func TestHandleMCPFederated_Ping_AnsweredLocally(t *testing.T) {
	alpha := newMockJSONRPCServer(t, nil)
	cfg := newFederationTestConfig(alpha.srv.URL, "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if alpha.called.Load() != 0 {
		t.Error("ping must never contact any backend server")
	}
}

// TestHandleMCPFederated_ToolsList_AggregatesPrefixesAndRespectsGroupAccess
// is the core aggregation test: two mock MCP servers ("alpha", "beta"),
// each with their own distinctly-named tool. A group unrestricted to
// mcpServers sees both, prefixed and merged, sorted by name; a group
// restricted to "alpha" only sees alpha's own tool, beta is never even
// contacted, and per-target counters land only on the server(s) actually
// reached.
func TestHandleMCPFederated_ToolsList_AggregatesPrefixesAndRespectsGroupAccess(t *testing.T) {
	cases := []struct {
		name          string
		wantToolNames []string
		wantBetaCalls int32
		alphaOnly     bool
	}{
		{
			name:          "unrestricted group sees both servers' tools, merged and sorted",
			alphaOnly:     false,
			wantToolNames: []string{"alpha_lookup", "beta_search"},
			wantBetaCalls: 1,
		},
		{
			name:          "group restricted to alpha never contacts beta at all",
			alphaOnly:     true,
			wantToolNames: []string{"alpha_lookup"},
			wantBetaCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup", Description: "alpha's tool"}})
			beta := newMockJSONRPCServer(t, []mcpTool{{Name: "search", Description: "beta's tool"}})

			cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, tc.alphaOnly)
			h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw, ok := h.(*Gateway)
			if !ok {
				t.Fatal("handler is not *Gateway")
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}

			var got jsonrpcResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Error != nil {
				t.Fatalf("error = %+v, want nil", got.Error)
			}
			var result mcpToolsListResult
			if err := json.Unmarshal(got.Result, &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}

			gotNames := make([]string, len(result.Tools))
			for i, tool := range result.Tools {
				gotNames[i] = mustToolName(t, tool)
			}
			if len(gotNames) != len(tc.wantToolNames) {
				t.Fatalf("tools = %v, want %v", gotNames, tc.wantToolNames)
			}
			for i, want := range tc.wantToolNames {
				if gotNames[i] != want {
					t.Errorf("tools[%d] = %q, want %q", i, gotNames[i], want)
				}
			}

			if beta.called.Load() != tc.wantBetaCalls {
				t.Errorf("beta server called %d times, want %d", beta.called.Load(), tc.wantBetaCalls)
			}

			// Per-target counter attribution (Feature B machinery reused
			// by Feature C): alpha is always contacted in both cases.
			now := time.Now()
			alphaReq, ok := gw.limiter.getCounter(targetKindMCP, "alpha", metricReq, windowDay, now)
			if !ok || alphaReq != 1 {
				t.Errorf("mcp/alpha req:day counter = %d (ok=%v), want 1", alphaReq, ok)
			}
			betaReq, ok := gw.limiter.getCounter(targetKindMCP, "beta", metricReq, windowDay, now)
			if !ok || betaReq != int64(tc.wantBetaCalls) {
				t.Errorf("mcp/beta req:day counter = %d (ok=%v), want %d", betaReq, ok, tc.wantBetaCalls)
			}
		})
	}
}

// TestHandleMCPFederated_ToolsList_UnreachableServerDegradesNotFails proves
// one failing/unreachable allowed server never fails the whole aggregate —
// the other server's tools still come back.
func TestHandleMCPFederated_ToolsList_UnreachableServerDegradesNotFails(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
	// "beta" points at a URL nothing listens on.
	cfg := newFederationTestConfig(alpha.srv.URL, "http://127.0.0.1:1", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil (a down server degrades the aggregate, it does not fail the call)", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_lookup" {
		t.Errorf("tools = %+v, want exactly alpha_lookup (beta unreachable, skipped)", result.Tools)
	}
}

// TestHandleMCPFederated_ToolsList_FollowsNextCursor is the F3 regression
// test (review round 3, 2026-09): a paginated backend's tools all arrive
// in the federated aggregate, not just the first page, and the cursor
// each successive request carries is exactly the previous page's own
// nextCursor.
func TestHandleMCPFederated_ToolsList_FollowsNextCursor(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var params mcpToolsListParams
		_ = json.Unmarshal(req.Params, &params)
		calls = append(calls, "cursor="+params.Cursor)

		var page mcpToolsListResult
		switch params.Cursor {
		case "":
			page = mcpToolsListResult{Tools: mcpToolsToRaw(t, []mcpTool{{Name: "one"}}), NextCursor: "page2"}
		case "page2":
			page = mcpToolsListResult{Tools: mcpToolsToRaw(t, []mcpTool{{Name: "two"}})} // no NextCursor: last page
		default:
			t.Fatalf("unexpected cursor %q", params.Cursor)
		}
		result, _ := json.Marshal(page)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true) // alphaOnly
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	gotNames := make([]string, len(result.Tools))
	for i, tool := range result.Tools {
		gotNames[i] = mustToolName(t, tool)
	}
	wantNames := []string{"alpha_one", "alpha_two"}
	if len(gotNames) != len(wantNames) || gotNames[0] != wantNames[0] || gotNames[1] != wantNames[1] {
		t.Fatalf("tools = %v, want %v (both pages merged)", gotNames, wantNames)
	}
	if len(calls) != 2 || calls[0] != "cursor=" || calls[1] != "cursor=page2" {
		t.Errorf("calls = %v, want [\"cursor=\" \"cursor=page2\"] (first page bare, second page echoing the first page's own nextCursor)", calls)
	}
}

// TestHandleMCPFederated_ToolsList_LaterPageFailure_KeepsEarlierPages is
// the F3 partial-keep regression test (review round 4): page 1 succeeds
// and sets a NextCursor, page 2 fails with a JSON-RPC error. Before this
// fix, ANY page failure discarded the whole server's contribution,
// including the tools from pages already fetched successfully; now only
// page 0 (the very first page) failing does that — a later page failing
// keeps what came before and logs a warning naming what was lost.
func TestHandleMCPFederated_ToolsList_LaterPageFailure_KeepsEarlierPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var params mcpToolsListParams
		_ = json.Unmarshal(req.Params, &params)
		w.Header().Set("Content-Type", "application/json")
		switch params.Cursor {
		case "":
			result, _ := json.Marshal(mcpToolsListResult{Tools: mcpToolsToRaw(t, []mcpTool{{Name: "one"}}), NextCursor: "page2"})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		case "page2":
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "backend broke mid-pagination"}})
		default:
			t.Fatalf("unexpected cursor %q", params.Cursor)
		}
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true) // alphaOnly
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var rec *httptest.ResponseRecorder
	logOutput := captureStderr(t, func() {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil — page 1's tool must still be served, not turned into a total failure", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_one" {
		t.Fatalf("tools = %+v, want exactly [alpha_one] (page 1's tool kept despite page 2 failing)", result.Tools)
	}
	if !strings.Contains(logOutput, "page 1 failed") || !strings.Contains(logOutput, "keeping 1 tool") {
		t.Errorf("log output = %q, want a warning naming the failed page and how many tools were kept", logOutput)
	}
}

// TestHandleMCPFederated_ToolsList_NextCursorLoop_BoundedByPageCap proves
// a backend that ALWAYS sets nextCursor (an unbounded pagination loop, by
// bug or by design) cannot hold the fan-out hostage forever — F3's own
// mcpToolsListMaxPagesPerServer cap stops it, degrading to a partial tool
// list rather than exhausting the fan-out's shared timeout budget one
// page at a time. Also covers F3's page-cap warning (review round 4): a
// truncation this silent otherwise gives an operator no signal a
// legitimately larger catalog got cut off.
func TestHandleMCPFederated_ToolsList_NextCursorLoop_BoundedByPageCap(t *testing.T) {
	var pageCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pageCount.Add(1)
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		page := mcpToolsListResult{
			Tools:      mcpToolsToRaw(t, []mcpTool{{Name: fmt.Sprintf("tool%d", n)}}),
			NextCursor: fmt.Sprintf("next%d", n), // never empty: an infinite pagination loop
		}
		result, _ := json.Marshal(page)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var rec *httptest.ResponseRecorder
	logOutput := captureStderr(t, func() {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logOutput, "hit the") || !strings.Contains(logOutput, "page cap") {
		t.Errorf("log output = %q, want a warning naming the page-cap truncation", logOutput)
	}

	if got := pageCount.Load(); got != int32(mcpToolsListMaxPagesPerServer) {
		t.Errorf("pages fetched = %d, want exactly mcpToolsListMaxPagesPerServer (%d)", got, mcpToolsListMaxPagesPerServer)
	}
}

// TestHandleMCPFederated_ToolsList_CollisionDropsLaterDeterministically is
// the F4 regression test (review round 3, 2026-09), extended for P2
// (review round 4): two configured, allowed servers whose names collide
// under the "<server>_<tool>" prefix convention (server "a" has tool
// "b_c"; server "a_b" has tool "c"; both mint "a_b_c") must not both
// appear in the merged list — one wins, deterministically, and the drop
// is logged. The survivor must be server "a_b", not "a": resolveFederatedTool
// resolves "a_b_c" against allowed names ["a","a_b"] by LONGEST matching
// prefix, which is "a_b" — the OLD alphabetical tiebreak instead kept "a"
// (it sorts first), which would advertise a tool description tools/call
// could never actually reach, since every call for "a_b_c" is routed to
// "a_b" regardless of which one tools/list lists. Each server's tool
// carries its own description so the assertion below proves the LISTED
// description is the ROUTED server's own, not just that a name survived.
func TestHandleMCPFederated_ToolsList_CollisionDropsLaterDeterministically(t *testing.T) {
	a := newMockJSONRPCServer(t, []mcpTool{{Name: "b_c", Description: "FROM_A"}})
	ab := newMockJSONRPCServer(t, []mcpTool{{Name: "c", Description: "FROM_A_B"}})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"a":   {URL: a.srv.URL},
		"a_b": {URL: ab.srv.URL},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	// Exactly one "a_b_c" survives — never two entries sharing one name,
	// which some client-side LLM tool APIs reject outright, and never
	// zero (the collision must resolve to a deterministic winner, not
	// drop both).
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "a_b_c" {
		t.Fatalf("tools = %+v, want exactly one entry named a_b_c", result.Tools)
	}
	// P2: the listed description must belong to server "a_b" (FROM_A_B) —
	// the one resolveFederatedTool actually routes "a_b_c" calls to —
	// never server "a"'s (FROM_A), which the old alphabetical tiebreak
	// would have kept despite tools/call never reaching it under that
	// name.
	var desc string
	if err := json.Unmarshal(result.Tools[0]["description"], &desc); err != nil {
		t.Fatalf("tool has no valid \"description\" field: %+v (%v)", result.Tools[0], err)
	}
	if desc != "FROM_A_B" {
		t.Errorf("description = %q, want %q (the routed server a_b's own, not a's)", desc, "FROM_A_B")
	}
	if routedServer, _, ok := resolveFederatedTool("a_b_c", []string{"a", "a_b"}); !ok || routedServer != "a_b" {
		t.Fatalf("sanity check failed: resolveFederatedTool(%q) = (%q, ok=%v), want (\"a_b\", true) — the test's own premise is wrong if this fails", "a_b_c", routedServer, ok)
	}
}

// TestHandleMCPFederated_ToolsCall_RoutesToCorrectServer_StripsPrefix
// proves tools/call resolves the server prefix, strips it before
// forwarding, contacts ONLY the resolved server (beta must never be
// called for an alpha-prefixed tool), relays the backend's result, and
// re-attaches the CLIENT's own id (never mcpBackendCall's synthetic one).
func TestHandleMCPFederated_ToolsCall_RoutesToCorrectServer_StripsPrefix(t *testing.T) {
	alpha := newMockJSONRPCServer(t, nil)
	beta := newMockJSONRPCServer(t, nil)
	cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup", Arguments: json.RawMessage(`{"q":"traefik"}`)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage(`"client-id-42"`), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil", got.Error)
	}
	if string(got.ID) != `"client-id-42"` {
		t.Errorf("id = %s, want the client's own id echoed back, not mcpBackendCall's synthetic one", got.ID)
	}

	var echoed struct {
		CalledName      string          `json:"calledName"`
		CalledArguments json.RawMessage `json:"calledArguments"`
	}
	if err := json.Unmarshal(got.Result, &echoed); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if echoed.CalledName != "lookup" {
		t.Errorf("backend saw tool name %q, want the prefix stripped down to %q", echoed.CalledName, "lookup")
	}
	if string(echoed.CalledArguments) != `{"q":"traefik"}` {
		t.Errorf("backend saw arguments %s, want the client's own arguments forwarded verbatim", echoed.CalledArguments)
	}

	if alpha.called.Load() != 1 {
		t.Errorf("alpha called %d times, want exactly 1", alpha.called.Load())
	}
	if beta.called.Load() != 0 {
		t.Error("beta must never be contacted for an alpha-prefixed tool call")
	}

	betaReq, ok := gw.limiter.getCounter(targetKindMCP, "beta", metricReq, windowDay, time.Now())
	if !ok || betaReq != 0 {
		t.Errorf("mcp/beta req:day counter = %d (ok=%v), want 0 (never contacted)", betaReq, ok)
	}
	alphaReq, ok := gw.limiter.getCounter(targetKindMCP, "alpha", metricReq, windowDay, time.Now())
	if !ok || alphaReq != 1 {
		t.Errorf("mcp/alpha req:day counter = %d (ok=%v), want 1 (the resolved target)", alphaReq, ok)
	}
}

// TestHandleMCPFederated_ToolsCall_ForwardsMeta is the F11 regression test
// (review round 3, 2026-09): a client's own "_meta" object on tools/call —
// progressToken included — reaches the resolved backend verbatim,
// alongside the usual prefix-stripped name and forwarded arguments.
func TestHandleMCPFederated_ToolsCall_ForwardsMeta(t *testing.T) {
	var gotMeta json.RawMessage
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var params mcpToolCallParams
		_ = json.Unmarshal(req.Params, &params)
		gotMeta = params.Meta
		w.Header().Set("Content-Type", "application/json")
		result, _ := json.Marshal(map[string]any{"ok": true})
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
	}))
	defer alpha.Close()

	cfg := newFederationTestConfig(alpha.URL, "http://beta.invalid", true) // alphaOnly
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{
		Name: "alpha_lookup",
		Meta: json.RawMessage(`{"progressToken":"tok-1"}`),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if string(gotMeta) != `{"progressToken":"tok-1"}` {
		t.Errorf("backend saw _meta = %s, want the client's own _meta forwarded verbatim", gotMeta)
	}
}

// TestHandleMCPFederated_ToolsCall_BackendError_RelayedToClient proves a
// backend's own JSON-RPC error (e.g. a tool execution failure) is relayed
// to the client verbatim, under the client's own id — not swallowed or
// replaced with a generic gateway error.
func TestHandleMCPFederated_ToolsCall_BackendError_RelayedToClient(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "tool execution failed"}})
	}))
	defer alpha.Close()

	cfg := newFederationTestConfig(alpha.URL, "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("5"), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != -32000 || got.Error.Message != "tool execution failed" {
		t.Errorf("error = %+v, want the backend's own error relayed verbatim", got.Error)
	}
	if string(got.ID) != "5" {
		t.Errorf("id = %s, want the client's own id 5", got.ID)
	}
}

// TestHandleMCPFederated_ToolsCall_UnreachableServer_ReturnsInternalError
// proves a resolved-but-unreachable backend server is a JSON-RPC internal
// error, not a raw connection failure surfaced to the client.
func TestHandleMCPFederated_ToolsCall_UnreachableServer_ReturnsInternalError(t *testing.T) {
	cfg := newFederationTestConfig("http://127.0.0.1:1", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("6"), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError {
		t.Errorf("error = %+v, want code %d (internal error)", got.Error, jsonrpcInternalError)
	}
}

// TestHandleMCPFederated_ToolsCall_UnknownPrefix_ReturnsJSONRPCError proves
// an unresolvable tool name is a JSON-RPC invalid-params error (still HTTP
// 200), not an HTTP 404 — the request reached a real route and a real
// method, it just named a tool nothing could route.
func TestHandleMCPFederated_ToolsCall_UnknownPrefix_ReturnsJSONRPCError(t *testing.T) {
	cases := []struct {
		name       string
		toolName   string
		restricted bool
	}{
		{name: "no configured server matches the prefix at all", toolName: "nosuchserver_tool"},
		{name: "prefix belongs to a real but group-restricted-away server", toolName: "beta_tool", restricted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alpha := newMockJSONRPCServer(t, nil)
			beta := newMockJSONRPCServer(t, nil)
			cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, tc.restricted)
			h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			callParams, _ := json.Marshal(mcpToolCallParams{Name: tc.toolName})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
				JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("9"), Params: callParams,
			}))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (JSON-RPC error, not an HTTP error), body=%s", rec.Code, rec.Body.String())
			}
			var got jsonrpcResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Error == nil || got.Error.Code != jsonrpcInvalidParams {
				t.Errorf("error = %+v, want code %d (invalid params)", got.Error, jsonrpcInvalidParams)
			}
			if alpha.called.Load() != 0 || beta.called.Load() != 0 {
				t.Error("no backend server must ever be contacted for an unresolvable tool name")
			}
		})
	}
}

func TestHandleMCPFederated_RateLimited_Returns429(t *testing.T) {
	alpha := newMockJSONRPCServer(t, nil)
	cfg := newFederationTestConfig(alpha.srv.URL, "http://beta.invalid", false)
	cfg.Groups["default"].Limits = &LimitsConfig{RequestsPerMinute: 1}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("1")}))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "ping", ID: json.RawMessage("2")}))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body=%s", rec2.Code, rec2.Body.String())
	}
}

// --- MF1: per-backend-call timeouts ---

func TestBackendTimeoutConstants(t *testing.T) {
	if toolsListBackendTimeout != 20*time.Second {
		t.Errorf("toolsListBackendTimeout = %v, want 20s", toolsListBackendTimeout)
	}
	if toolsCallBackendTimeout != 120*time.Second {
		t.Errorf("toolsCallBackendTimeout = %v, want 120s", toolsCallBackendTimeout)
	}
}

// TestHandleMCPFederated_ToolsList_SlowBackend_BoundedByRequestContext
// proves the per-server context.WithTimeout(r.Context(), toolsListBackendTimeout)
// (MF1) actually derives its deadline from the INCOMING request's own
// context, not a freestanding timer: context.WithTimeout always resolves
// to the EARLIER of its parent's existing deadline and its own duration,
// so giving the incoming request a context with a much shorter deadline
// than toolsListBackendTimeout (20s) and confirming the whole call
// returns in well under a second — against a backend that hangs
// indefinitely — proves the wiring without ever waiting anywhere near the
// real 20s budget in this test suite.
func TestHandleMCPFederated_ToolsList_SlowBackend_BoundedByRequestContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) }) // must unblock the handler BEFORE srv.Close (which waits for it) — registered after, so it runs first (t.Cleanup is LIFO)

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true) // alphaOnly: only the hanging server is reachable
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")})
	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(shortCtx)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want well under toolsListBackendTimeout (20s) — the per-server timeout must derive from r.Context(), not ignore it", elapsed)
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError {
		t.Errorf("error = %+v, want internal error (the only allowed server timed out, degrading to 'no MCP server reachable')", got.Error)
	}
}

// TestHandleMCPFederated_ToolsCall_SlowBackend_BoundedByRequestContext
// mirrors the tools/list timeout test above for tools/call's own,
// separate context.WithTimeout(r.Context(), toolsCallBackendTimeout) call
// site.
func TestHandleMCPFederated_ToolsCall_SlowBackend_BoundedByRequestContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams})
	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(shortCtx)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want well under toolsCallBackendTimeout (120s) — the per-call timeout must derive from r.Context(), not ignore it", elapsed)
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError {
		t.Errorf("error = %+v, want internal error (the resolved server timed out)", got.Error)
	}
	// G4, feat/target-health review round 2: tools/call always actually
	// dials its one resolved server (no semaphore to wait behind), so
	// this shared-budget expiry must record a failure, not nothing.
	if state := gw.targetHealth.stateOf(targetKindMCP, "alpha"); state != targetHealthUnhealthy {
		t.Errorf("alpha state = %q, want unhealthy (dialed and never answered within the request's own budget)", state)
	}
}

// --- MF2: non-2xx status and empty-envelope guards ---

// TestHandleMCPFederated_ToolsCall_BackendNon2xxStatus_IsError proves a
// non-2xx upstream response is always an error, even when its body
// happens to be a validly-shaped JSON-RPC success object — a 5xx (or any
// non-2xx) status is a transport-level failure the body's own content can
// never override.
func TestHandleMCPFederated_ToolsCall_BackendNon2xxStatus_IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"federated","result":{}}`))
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError {
		t.Errorf("error = %+v, want internal error — a 5xx status must never be rescued by a well-shaped body", got.Error)
	}
}

// TestHandleMCPFederated_ToolsCall_BackendEmptyEnvelope_ConvertsToInternalError
// is the reviewer's named silent-failure scenario (MF2): a backend answers
// 200 with valid JSON that is NOT a real JSON-RPC response (neither
// "result" nor "error") — without the guard, this decodes into an
// all-zero-fields jsonrpcResponse and the client would silently receive
// {"jsonrpc":"2.0","id":1} with no indication anything went wrong.
func TestHandleMCPFederated_ToolsCall_BackendEmptyEnvelope_ConvertsToInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"federated","status":"ok"}`))
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil {
		t.Fatal("want a JSON-RPC error, not a silent empty success — this is the reviewer's exact silent-failure scenario")
	}
	if got.Error.Code != jsonrpcInternalError {
		t.Errorf("error code = %d, want %d", got.Error.Code, jsonrpcInternalError)
	}
	if len(got.Result) != 0 {
		t.Errorf(`result = %s, want empty — must never emit {"jsonrpc":"2.0","id":1} silently`, got.Result)
	}
}

// --- MF3: bare-first-with-fallback handshake retry ---

// TestMcpBackendCall_HandshakeFallbackTrigger proves the fallback fires
// ONLY on a JSON-RPC-level error or an HTTP 4xx status from the bare
// attempt — never on a 5xx — table-driven over exactly the two trigger
// shapes and the one non-trigger shape (a network failure is covered
// separately below, since it needs a different mock shape entirely: no
// server to answer at all).
// TestMcpBackendCall_HandshakeFallbackTrigger covers mcpSessionRequiredSignal's
// narrowed rule (F1, review round 3, 2026-09) end to end through
// mcpBackendCall: only a narrow session-missing signal may trigger the
// handshake retry, never an arbitrary tools/call error and never a 429 —
// the exact over-eager-retry bug F1 fixes, where a non-idempotent
// tools/call could be silently double-executed. method defaults to
// "tools/list" when empty, matching the broader "any JSON-RPC error is a
// session signal" rule that method gets (mcpSessionRequiredSignal's own
// doc comment); the "tool called once" cases below explicitly exercise
// "tools/call" to prove the narrower rule that method gets instead.
func TestMcpBackendCall_HandshakeFallbackTrigger(t *testing.T) {
	cases := []struct {
		bareHandler   http.HandlerFunc
		name          string
		method        string
		wantHandshake bool
	}{
		{
			name: "bare JSON-RPC error triggers the handshake (tools/list: any error is a session signal)",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				var req jsonrpcRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "session required"}})
			},
			wantHandshake: true,
		},
		{
			name: "HTTP 404 with no JSON-RPC body triggers the handshake",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantHandshake: true,
		},
		{
			name: "HTTP 403 with no JSON-RPC body does NOT trigger the handshake (F1: narrowed from any 4xx to just 400/404)",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantHandshake: false,
		},
		{
			name: "HTTP 429 never triggers the handshake (F1: rate-limited, retrying only adds load)",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantHandshake: false,
		},
		{
			name: "HTTP 500 never triggers the handshake",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantHandshake: false,
		},
		{
			// The F1 scenario itself: a tools/call answered with an
			// ordinary, unrelated JSON-RPC error (a real tool-level
			// failure, e.g. bad arguments) must be called exactly ONCE —
			// retrying it could silently double-execute a non-idempotent
			// tool's side effect.
			name:   "tools/call: arbitrary JSON-RPC error does not trigger the handshake (tool called once)",
			method: "tools/call",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				var req jsonrpcRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32602, Message: "invalid arguments: missing required field \"query\""}})
			},
			wantHandshake: false,
		},
		{
			// The narrow allow-case for tools/call: the SAME JSON-RPC
			// error shape, but its own message clearly names a session
			// problem, still gets the handshake.
			name:   "tools/call: JSON-RPC error naming a session problem still triggers the handshake",
			method: "tools/call",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				var req jsonrpcRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "Session required: call initialize first"}})
			},
			wantHandshake: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = "tools/list"
			}
			var callCount atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if callCount.Add(1) == 1 {
					tc.bareHandler(w, r)
					return
				}
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				body, _ := io.ReadAll(r.Body)
				var req jsonrpcRequest
				_ = json.Unmarshal(body, &req)
				w.Header().Set("Content-Type", "application/json")
				result, _ := json.Marshal(map[string]any{})
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
			}))
			defer srv.Close()

			cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
			h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw, ok := h.(*Gateway)
			if !ok {
				t.Fatal("handler is not *Gateway")
			}

			_, _ = gw.mcpBackendCall(context.Background(), srv.URL, method, struct{}{}, mcpBackendResponseMaxBytes)

			gotCalls := callCount.Load()
			if tc.wantHandshake && gotCalls < 2 {
				t.Errorf("calls = %d, want >=2 (bare attempt + initialize handshake)", gotCalls)
			}
			if !tc.wantHandshake && gotCalls != 1 {
				t.Errorf("calls = %d, want exactly 1 (bare attempt only, no handshake retry)", gotCalls)
			}
		})
	}
}

// TestMcpBackendCall_NetworkFailure_NeverAttemptsHandshake proves a
// connection-level failure (nothing listening at all) never triggers the
// handshake fallback either — the same "not retry-eligible" rule as a
// 5xx, verified against a genuinely different failure shape (doBackendJSONRPC
// never even gets an HTTP status back).
func TestMcpBackendCall_NetworkFailure_NeverAttemptsHandshake(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	_, err = gw.mcpBackendCall(context.Background(), "http://127.0.0.1:1", "tools/list", struct{}{}, mcpBackendResponseMaxBytes)
	if err == nil {
		t.Fatal("want an error (nothing listening on 127.0.0.1:1)")
	}
}

// --- security review round 2, important finding 4: dual response caps ---

// TestMcpBackendCall_ResponseExceedsCap_ReturnsDistinctTooLargeError is a
// fast, small-scale unit test of doBackendJSONRPC's own maxBytes
// mechanism, using an artificially tiny cap so the test needs no
// multi-megabyte payload: a backend response over the cap must be
// reported by wrapping errMCPResponseTooLarge — distinguishable via
// errors.Is from an ordinary network/parse failure — never silently
// truncated into a confusing parse error.
func TestMcpBackendCall_ResponseExceedsCap_ReturnsDistinctTooLargeError(t *testing.T) {
	const tinyCap = 64
	big := strings.Repeat("x", tinyCap*4) // well past tinyCap once JSON-encoded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		result, _ := json.Marshal(map[string]string{"data": big})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	_, err = gw.mcpBackendCall(context.Background(), srv.URL, "tools/list", struct{}{}, tinyCap)
	if err == nil {
		t.Fatal("want an error for a response exceeding the cap")
	}
	if !errors.Is(err, errMCPResponseTooLarge) {
		t.Errorf("err = %v, want it to wrap %v", err, errMCPResponseTooLarge)
	}
}

// TestMcpBackendCall_ResponseAtOrUnderCap_Succeeds proves the cap+1 read
// technique does not false-positive: a response exactly AT the cap must
// still succeed, not be mistaken for oversized.
func TestMcpBackendCall_ResponseAtOrUnderCap_Succeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		result, _ := json.Marshal(map[string]bool{"ok": true})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	// A generous cap comfortably above this tiny response — must succeed
	// cleanly, not be flagged as too-large.
	resp, err := gw.mcpBackendCall(context.Background(), srv.URL, "tools/list", struct{}{}, 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("resp.Error = %+v, want nil", resp.Error)
	}
}

// TestHandleMCPFederated_ToolsCall_ResponseBetween4And10MiB_Succeeds is
// the end-to-end regression for important finding 4: a real tool result
// between mcpBackendResponseMaxBytes (4MiB, the fan-out-only cap) and
// mcpBackendCallResponseMaxBytes (10MiB, tools/call's own cap) — legal
// under the pre-finding-1c budget, and a realistic size for an image or
// extracted-document tool result — must succeed on the tools/call path,
// proving that path was never shrunk to the fan-out's smaller cap.
func TestHandleMCPFederated_ToolsCall_ResponseBetween4And10MiB_Succeeds(t *testing.T) {
	const payloadBytes = 6 << 20 // 6MiB: over the 4MiB fan-out cap, under the 10MiB tools/call cap
	big := strings.Repeat("y", payloadBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/call" {
			result, _ := json.Marshal(map[string]string{"data": big})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: json.RawMessage(`{}`)})
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"),
		Params: json.RawMessage(`{"name":"alpha_lookup"}`),
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil (a 6MiB result must succeed on the tools/call path)", got.Error)
	}
}

// TestHandleMCPFederated_ToolsList_HandshakeFallback_SucceedsAfterSessionRetry
// is MF3's core proof: a server that rejects a bare tools/list call
// (readitall/fetch's real, live-probed behavior — sessionRequiredMockServer's
// own doc comment) still ends up contributing its tools to the federated
// aggregate, via exactly the documented bare -> initialize -> retry ->
// close sequence.
func TestHandleMCPFederated_ToolsList_HandshakeFallback_SucceedsAfterSessionRetry(t *testing.T) {
	strict := newSessionRequiredMockServer(t, []mcpTool{{Name: "search"}})
	cfg := newFederationTestConfig(strict.srv.URL, "http://beta.invalid", true) // alphaOnly: only "alpha" (the strict server) is reachable
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil (the handshake fallback should have recovered)", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_search" {
		t.Fatalf("tools = %+v, want exactly alpha_search", result.Tools)
	}

	seq := strict.callSequence()
	// F5, review round 3, 2026-09: notifications/initialized is now sent
	// between the handshake and the retry, one more call than before.
	if len(seq) != 5 {
		t.Fatalf("call sequence = %v, want exactly 5 calls (bare attempt, initialize, notifications/initialized, retried real call, best-effort close — the DELETE runs synchronously inside mcpBackendCall's own defer, before it returns)", seq)
	}
	if seq[0] != "tools/list:session=" {
		t.Errorf("call[0] = %q, want a bare tools/list attempt with no session header", seq[0])
	}
	if seq[1] != "initialize:session=" {
		t.Errorf("call[1] = %q, want an initialize handshake with no session header", seq[1])
	}
	if seq[2] != "notifications/initialized:session=sess-1" {
		t.Errorf("call[2] = %q, want the lifecycle's own notifications/initialized, carrying the session header the handshake returned", seq[2])
	}
	if seq[3] != "tools/list:session=sess-1" {
		t.Errorf("call[3] = %q, want the retried tools/list carrying the session header the handshake returned", seq[3])
	}
	if seq[4] != "DELETE:session=sess-1" {
		t.Errorf("call[4] = %q, want the best-effort session close carrying the same session header", seq[4])
	}
}

// TestMcpBackendCall_SendsNotificationsInitialized_AfterHandshake is the
// F5 regression test (review round 3, 2026-09), pinning the notification's
// own exact wire shape directly: no "id" field at all (it is a JSON-RPC
// notification, not a request awaiting a reply), method
// "notifications/initialized", and the session header carrying the
// handshake's own Mcp-Session-Id. A backend that answers it with an
// error (as this one deliberately does, mirroring a real server that
// does not implement this endpoint) must never surface as a failure of
// the call this handshake serves — fire-and-forget, as documented.
func TestMcpBackendCall_SendsNotificationsInitialized_AfterHandshake(t *testing.T) {
	var (
		mu                 sync.Mutex
		sawNotification    bool
		notificationHasID  bool
		notificationSessID string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			w.Header().Set(mcpSessionHeader, "sess-f5")
			result, _ := json.Marshal(map[string]any{})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		case "notifications/initialized":
			mu.Lock()
			sawNotification = true
			_, notificationHasID = raw["id"]
			notificationSessID = r.Header.Get(mcpSessionHeader)
			mu.Unlock()
			// Deliberately answer with an error, to prove this response
			// is never inspected by the caller.
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: jsonrpcMethodNotFound, Message: "not implemented"}})
		default:
			if r.Header.Get(mcpSessionHeader) == "sess-f5" {
				// The retried real call, now carrying the session the
				// handshake returned: succeeds.
				result, _ := json.Marshal(mcpToolsListResult{})
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
				return
			}
			// The bare attempt: reject with a session-required signal so
			// the handshake fires.
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "session required"}})
		}
	}))
	defer srv.Close()

	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	if _, err := gw.mcpBackendCall(context.Background(), srv.URL, "tools/list", struct{}{}, mcpBackendResponseMaxBytes); err != nil {
		t.Fatalf("mcpBackendCall: %v, want the retry to succeed despite the notification's own error response", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawNotification {
		t.Fatal("backend never received notifications/initialized")
	}
	if notificationHasID {
		t.Error("notifications/initialized carried an \"id\" field; want none (it is a notification, not a request)")
	}
	if notificationSessID != "sess-f5" {
		t.Errorf("notifications/initialized session header = %q, want %q", notificationSessID, "sess-f5")
	}
}

// TestHandleMCPFederated_ToolsCall_HandshakeFallback_SucceedsAfterSessionRetry
// mirrors the tools/list handshake test above for tools/call's own
// single-target path, additionally asserting the best-effort session
// close (DELETE) actually happened.
func TestHandleMCPFederated_ToolsCall_HandshakeFallback_SucceedsAfterSessionRetry(t *testing.T) {
	strict := newSessionRequiredMockServer(t, nil)
	cfg := newFederationTestConfig(strict.srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_search"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage(`"client-7"`), Params: callParams}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil (the handshake fallback should have recovered)", got.Error)
	}
	if string(got.ID) != `"client-7"` {
		t.Errorf("id = %s, want the client's own id echoed back", got.ID)
	}

	seq := strict.callSequence()
	if !slices.Contains(seq, "tools/call:session=") {
		t.Errorf("call sequence = %v, want a bare tools/call attempt with no session header", seq)
	}
	if !slices.Contains(seq, "initialize:session=") {
		t.Errorf("call sequence = %v, want an initialize handshake", seq)
	}
	if !slices.Contains(seq, "tools/call:session=sess-1") {
		t.Errorf("call sequence = %v, want the retried tools/call carrying the session header", seq)
	}
	if !slices.Contains(seq, "DELETE:session=sess-1") {
		t.Errorf("call sequence = %v, want a best-effort DELETE closing the session", seq)
	}
}

// TestHandleMCPFederated_ToolsList_HandshakeFallback_GivesUpWhenInitializeAlsoFails
// proves a server that rejects the handshake's own initialize call too
// ends up in the failed/skipped list (not a crash, not a hang) — and,
// since it is the only allowed server, the aggregate degrades all the way
// to the "no MCP server reachable" loud failure (MF3), not an empty
// success.
func TestHandleMCPFederated_ToolsList_HandshakeFallback_GivesUpWhenInitializeAlsoFails(t *testing.T) {
	strict := newSessionRequiredMockServer(t, []mcpTool{{Name: "search"}})
	strict.failInit = true
	cfg := newFederationTestConfig(strict.srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError || got.Error.Message != "no MCP server reachable" {
		t.Errorf("error = %+v, want internal error %q", got.Error, "no MCP server reachable")
	}
}

// TestHandleMCPFederated_ToolsList_AllServersFail_ReturnsInternalError
// proves that when every attempted server fails, the response is a loud
// JSON-RPC error, not a silently-empty tools list that would look
// identical to "this caller's group has no MCP access at all".
func TestHandleMCPFederated_ToolsList_AllServersFail_ReturnsInternalError(t *testing.T) {
	cfg := newFederationTestConfig("http://127.0.0.1:1", "http://127.0.0.1:2", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInternalError || got.Error.Message != "no MCP server reachable" {
		t.Errorf("error = %+v, want internal error %q", got.Error, "no MCP server reachable")
	}
}

// --- security audit finding 1a: fan-out concurrency bound ---

// TestHandleMCPFederated_ToolsList_FanoutBoundedConcurrency proves
// mcpFederatedFanoutConcurrency actually caps how many backend servers a
// single tools/list call contacts AT ONCE — not just that it eventually
// contacts all of them. numServers is comfortably above the cap so the
// first wave of goroutines through the semaphore must stall on it: the
// test blocks every mock server's handler until every one of them
// observes exactly mcpFederatedFanoutConcurrency requests in flight
// simultaneously, then releases them all. If the fan-out were unbounded
// (the pre-fix behavior), every server's handler would receive its
// request immediately and this test would deadlock waiting for
// concurrency to reach a ceiling nothing ever enforces — a real
// regression here hangs the test until it times out, it does not
// silently pass.
func TestHandleMCPFederated_ToolsList_FanoutBoundedConcurrency(t *testing.T) {
	const numServers = mcpFederatedFanoutConcurrency + 4

	var (
		current int32
		peak    int32
		release = make(chan struct{})
	)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = make(map[string]*TargetConfig, numServers)
	for i := 0; i < numServers; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&current, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
					break
				}
			}
			<-release
			atomic.AddInt32(&current, -1)

			var req jsonrpcRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			result, _ := json.Marshal(mcpToolsListResult{})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		}))
		t.Cleanup(srv.Close)
		cfg.MCPServers[fmt.Sprintf("srv%d", i)] = &TargetConfig{URL: srv.URL}
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for atomic.LoadInt32(&current) < mcpFederatedFanoutConcurrency {
		select {
		case <-deadline:
			t.Fatal("concurrency never reached mcpFederatedFanoutConcurrency — fan-out may be serialized instead of bounded")
		case <-time.After(time.Millisecond):
		}
	}
	// Give any wrongly-unbounded extra goroutines a chance to also reach
	// the handler before we sample — if the bound were missing, ALL
	// numServers requests would already be in flight by now.
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&current); got > mcpFederatedFanoutConcurrency {
		t.Fatalf("current in-flight backends = %d, want <= %d (fan-out is not bounded)", got, mcpFederatedFanoutConcurrency)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed after releasing all backends")
	}

	if peak != mcpFederatedFanoutConcurrency {
		t.Errorf("peak concurrent backends = %d, want exactly %d", peak, mcpFederatedFanoutConcurrency)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// --- security audit finding 2: unrecovered panic in the fan-out ---

// panicOnHostRoundTripper panics for any request to panicHost — simulating
// an unrecovered interpreter-level panic reached deep inside one backend's
// own call path (mcpBackendCall -> doBackendJSONRPC -> g.targetClient.Do)
// — and delegates every other request to next unchanged. This exercises
// the REAL code path a genuine Yaegi interpreter panic would take,
// through the actual HTTP client the gateway uses, rather than adding any
// test-only hook to production code.
type panicOnHostRoundTripper struct {
	next      http.RoundTripper
	panicHost string
}

func (rt panicOnHostRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == rt.panicHost {
		panic("simulated interpreter-level panic mid-flight")
	}
	return rt.next.RoundTrip(r)
}

// TestHandleMCPFederated_ToolsList_PanickingBackendDegradesNotCrashes is
// the regression for the missing recover() in mcpFederatedToolsList's own
// fan-out goroutine (security audit finding 2, 2026-08-22): that goroutine
// runs off the request's own goroutine, so an unrecovered panic there has
// no ServeHTTP caller to unwind into and would crash the whole shared
// Traefik process, not just fail one backend. Reaching the end of this
// test at all is part of the proof: if the recover() were missing, beta's
// panicking RoundTrip call below would already have crashed the test
// binary — exactly the registry.go precedent this fix follows
// (TestModelRegistry_RefreshProvider_AdapterPanics_RecoversAndReleasesInFlight).
func TestHandleMCPFederated_ToolsList_PanickingBackendDegradesNotCrashes(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
	beta := newMockJSONRPCServer(t, []mcpTool{{Name: "search"}}) // never actually reached; RoundTrip panics first
	cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	betaURL, err := betaHost(beta.srv.URL)
	if err != nil {
		t.Fatalf("parse beta URL: %v", err)
	}
	gw.targetClient.Transport = panicOnHostRoundTripper{panicHost: betaURL, next: http.DefaultTransport}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil (a panicking server degrades the aggregate, it does not fail the call)", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_lookup" {
		t.Errorf("tools = %+v, want exactly alpha_lookup (beta panicked, skipped)", result.Tools)
	}
}

// betaHost extracts the host:port a RoundTripper sees on r.URL.Host for
// requests aimed at rawURL — httptest.Server URLs are already in that
// exact "http://host:port" form, so this just strips the scheme.
func betaHost(rawURL string) (string, error) {
	const prefix = "http://"
	if !strings.HasPrefix(rawURL, prefix) {
		return "", fmt.Errorf("unexpected test server URL shape: %q", rawURL)
	}
	return strings.TrimPrefix(rawURL, prefix), nil
}

// --- security review round 2, critical finding 3: tools/list is NOT weighted ---

// TestHandleMCPFederated_ToolsList_NotWeightedByBackendCount_BudgetBuysNCalls
// is the regression for security review round 2's critical finding 3:
// weighting checkAndCount by the number of allowed MCP servers (security
// audit finding 1b's original fix) was not default-preserving — on a
// config where the MCP allow-list is unrestricted (every group on the
// live fleet), it silently turned a configured requests-per-minute budget
// into budget/serverCount successful tools/list calls, with NO config
// change on the operator's part (11 servers, requests-per-minute: 60 ->
// only 5 calls/minute actually succeeded; examples/kubernetes.yaml's
// shipped requests-per-minute: 10 would 429 the very FIRST call ever
// made). With numServers (11, matching the live fleet) configured and a
// requests-per-minute budget of exactly rpmBudget, all rpmBudget calls
// must succeed and the next one must be rejected — proving the charge is
// exactly 1 per call, regardless of how many backends that call fans out
// to.
func TestHandleMCPFederated_ToolsList_NotWeightedByBackendCount_BudgetBuysNCalls(t *testing.T) {
	const numServers = 11 // matches the live fleet's own MCP server count
	const rpmBudget = 5

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = make(map[string]*TargetConfig, numServers)
	for i := 0; i < numServers; i++ {
		srv := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
		cfg.MCPServers[fmt.Sprintf("srv%d", i)] = &TargetConfig{URL: srv.srv.URL}
	}
	cfg.Groups = map[string]*GroupConfig{"default": {Limits: &LimitsConfig{RequestsPerMinute: rpmBudget}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 1; i <= rpmBudget; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage(fmt.Sprintf("%d", i))}))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d/%d status = %d, want 200 (budget must buy exactly rpmBudget calls, not rpmBudget/numServers), body=%s", i, rpmBudget, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage(`"over"`)}))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("call %d status = %d, want 429 (budget exhausted), body=%s", rpmBudget+1, rec.Code, rec.Body.String())
	}
}

// TestHandleMCPFederated_ToolsList_AmplificationVisibleAtTargetScopeOnly
// proves the fan-out's real per-server cost is still fully visible to an
// operator without silently consuming tenant quota (security review round
// 2, 2026-08-22, critical finding 3's own ruling): after ONE tools/list
// call against 2 servers, the user's own requests-per-minute counter
// reads exactly 1 (never weighted), while EACH server's own "mcp"
// target-scope counter (countTargetRequests, limits.go) reads exactly 1
// — the real amplification, attributed where an operator can see it.
func TestHandleMCPFederated_ToolsList_AmplificationVisibleAtTargetScopeOnly(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
	beta := newMockJSONRPCServer(t, []mcpTool{{Name: "search"}})
	cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	now := time.Now()
	userReq, ok := gw.limiter.getCounter("user", "alice", metricReq, windowDay, now)
	if !ok || userReq != 1 {
		t.Errorf("user req:day counter = %d (ok=%v), want exactly 1 (never weighted by backend count)", userReq, ok)
	}
	alphaReq, ok := gw.limiter.getCounter(targetKindMCP, "alpha", metricReq, windowDay, now)
	if !ok || alphaReq != 1 {
		t.Errorf("mcp/alpha req:day counter = %d (ok=%v), want 1", alphaReq, ok)
	}
	betaReq, ok := gw.limiter.getCounter(targetKindMCP, "beta", metricReq, windowDay, now)
	if !ok || betaReq != 1 {
		t.Errorf("mcp/beta req:day counter = %d (ok=%v), want 1", betaReq, ok)
	}
}

// --- MF3: legacy HTTP+SSE transport exclusion ---

func TestIsLegacySSETransportURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://playwright.internal:8080/sse", true},
		{"http://playwright.internal:8080/sse?foo=bar", true},
		// F9, review round 3, 2026-09: a trailing slash is the identical
		// legacy-SSE endpoint, not a different path shape.
		{"http://playwright.internal:8080/sse/", true},
		{"http://playwright.internal:8080/sse/?foo=bar", true},
		{"http://mcp.internal:8080", false},
		{"http://mcp.internal:8080/mcp", false},
		{"http://mcp.internal:8080/mcp/", false},
		{"http://mcp.internal:8080/sse/extra", false},
		{"://not a valid url", false}, // malformed -> safe default: not excluded
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := isLegacySSETransportURL(tc.url); got != tc.want {
				t.Errorf("isLegacySSETransportURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestAllowedMCPServerNames_ExcludesLegacySSETransport(t *testing.T) {
	cfg := CreateConfig()
	cfg.MCPServers = map[string]*TargetConfig{
		"alpha":      {URL: "http://alpha.internal"},
		"playwright": {URL: "http://playwright.internal/sse"},
		"beta":       {URL: "http://beta.internal"},
	}
	grp := &group{name: "g"} // unrestricted mcpServers: matches everything except the transport exclusion
	got := allowedMCPServerNames(cfg, grp)
	want := []string{"alpha", "beta"}
	if !slices.Equal(got, want) {
		t.Errorf("allowedMCPServerNames = %v, want %v (playwright excluded: legacy HTTP+SSE transport)", got, want)
	}
}

// TestHandleMCPFederated_ToolsList_ExcludesLegacySSEServer_NeverContacted
// proves a legacy-SSE-transport server is not merely omitted from the
// aggregate's RESULT, but never contacted by the fan-out at all.
func TestHandleMCPFederated_ToolsList_ExcludesLegacySSEServer_NeverContacted(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
	legacy := newMockJSONRPCServer(t, []mcpTool{{Name: "browse"}})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"alpha":      {URL: alpha.srv.URL},
		"playwright": {URL: legacy.srv.URL + "/sse"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_lookup" {
		t.Fatalf("tools = %+v, want exactly alpha_lookup (playwright excluded)", result.Tools)
	}
	if legacy.called.Load() != 0 {
		t.Error("the legacy-SSE-transport server must never be contacted by tools/list fan-out")
	}
}

// TestHandleMCPFederated_ToolsCall_LegacySSEServer_UnresolvableLikeUnconfigured
// proves tools/call cannot route to a legacy-SSE-transport server either —
// unresolvable, same as an unconfigured name, and never contacted.
func TestHandleMCPFederated_ToolsCall_LegacySSEServer_UnresolvableLikeUnconfigured(t *testing.T) {
	legacy := newMockJSONRPCServer(t, nil)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"playwright": {URL: legacy.srv.URL + "/sse"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "playwright_click"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams}))
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != jsonrpcInvalidParams {
		t.Errorf("error = %+v, want invalid params (legacy-SSE server must be unresolvable via tools/call, same as unconfigured)", got.Error)
	}
	if legacy.called.Load() != 0 {
		t.Error("the legacy-SSE-transport server must never be contacted")
	}
}

// --- SF6: protocol-version allowlist ---

func TestHandleMCPFederated_Initialize_AllowlistedVersionsEchoedVerbatim(t *testing.T) {
	for version := range knownMCPProtocolVersions {
		t.Run(version, func(t *testing.T) {
			cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
			h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			params, _ := json.Marshal(map[string]any{"protocolVersion": version})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "initialize", ID: json.RawMessage("1"), Params: params}))

			var got jsonrpcResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			var result struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(got.Result, &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if result.ProtocolVersion != version {
				t.Errorf("protocolVersion = %q, want the allowlisted caller-supplied version %q echoed verbatim", result.ProtocolVersion, version)
			}
		})
	}
}

func TestHandleMCPFederated_Initialize_UnrecognizedVersion_FallsBackToDefault(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"protocolVersion": "1999-01-01"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "initialize", ID: json.RawMessage("1"), Params: params}))

	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProtocolVersion != defaultMCPProtocolVersion {
		t.Errorf("protocolVersion = %q, want the fallback default %q for an unrecognized caller-supplied version", result.ProtocolVersion, defaultMCPProtocolVersion)
	}
}

// --- SF8: explicit 405 for a disallowed method on exactly /mcp ---

func TestHandleMCPFederated_DisallowedMethod_Returns405WithAllowHeader(t *testing.T) {
	cfg := newFederationTestConfig("http://alpha.invalid", "http://beta.invalid", false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, federatedMCPPath, nil)
			req.Header.Set("Authorization", "Bearer sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405, body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow header = %q, want %q", got, http.MethodPost)
			}
		})
	}

	t.Run("no auth still 405 (method check precedes auth)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, federatedMCPPath, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405 even without a valid API key, body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestMcpBackendCall_SessionCloseFailure_NeverSurfacedToCaller proves
// mcpBackendCloseSession's own "best-effort... failure logged, never
// surfaced" contract: the server accepts the handshake and the retried
// real call normally, but drops the connection outright on the
// best-effort DELETE — the caller must still see the real call's own
// successful result, not a failure manufactured by cleanup.
func TestMcpBackendCall_SessionCloseFailure_NeverSurfacedToCaller(t *testing.T) {
	const sessionID = "sess-close-fail"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// A network-level failure on close, not merely a non-2xx
			// status: mcpBackendCloseSession never even inspects the
			// response status, only whether Do() itself returned an
			// error — hijacking and dropping the raw connection is what
			// actually exercises that path.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("mock server: ResponseWriter does not support Hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}

		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == "initialize":
			w.Header().Set(mcpSessionHeader, sessionID)
			result, _ := json.Marshal(map[string]any{})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		case req.Method == "tools/list" && r.Header.Get(mcpSessionHeader) == "":
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "session required"}})
		default:
			result, _ := json.Marshal(mcpToolsListResult{Tools: mcpToolsToRaw(t, []mcpTool{{Name: "search"}})})
			_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Result: result})
		}
	}))
	defer srv.Close()

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != nil {
		t.Fatalf("error = %+v, want nil — a failed best-effort session close must never surface as a call failure", got.Error)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 1 || mustToolName(t, result.Tools[0]) != "alpha_search" {
		t.Fatalf("tools = %+v, want alpha_search despite the session-close failure", result.Tools)
	}
}

// TestMcpBackendCloseSession_LogScrubsCredentialFromTargetURL is the P4
// regression test (review round 4): a configured target URL's query
// string is this gateway's own credential channel for an MCP target
// (buildUpstreamTargetURL's doc comment, mcp_a2a.go), and a failed Do()
// against it commonly returns a *url.Error wrapping that URL verbatim.
// Before this fix, mcpBackendCloseSession's best-effort failure log
// printed both the raw targetURL and the raw err, leaking the credential
// straight to stderr; both must now come out scrubbed.
func TestMcpBackendCloseSession_LogScrubsCredentialFromTargetURL(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	// 127.0.0.1:1 is a reserved, always-refused port — Do() fails fast
	// with a real *url.Error wrapping this exact URL, no network flake
	// risk and no server to stand up for a pure "the log scrubs its own
	// error text" assertion.
	const targetURL = "http://127.0.0.1:1/mcp?api-key=SUPERSECRET123"

	logOutput := captureStderr(t, func() {
		gw.mcpBackendCloseSession(context.Background(), targetURL, "sess-1")
	})

	if strings.Contains(logOutput, "SUPERSECRET123") {
		t.Errorf("log output = %q, want the api-key query value scrubbed", logOutput)
	}
	if !strings.Contains(logOutput, "127.0.0.1") {
		t.Errorf("log output = %q, want the target host still present (only credentials must be stripped)", logOutput)
	}
}

// --- feat/target-health: passive recording from federation traffic ---

// TestHandleMCPFederated_ToolsList_TargetHealth_RecordsPerServer proves
// tools/list's own fan-out records a failure for the one server that
// could not be reached, and a healthy observation for every server that
// answered — independent of the aggregate response degrading rather
// than failing (TestHandleMCPFederated_ToolsList_UnreachableServerDegradesNotFails
// already covers the response shape; this proves the target-health side
// effect).
func TestHandleMCPFederated_ToolsList_TargetHealth_RecordsPerServer(t *testing.T) {
	alpha := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
	cfg := newFederationTestConfig(alpha.srv.URL, "http://127.0.0.1:1", false)
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthHealthy {
		t.Errorf("alpha state = %q, want healthy", got)
	}
	if got := gw.targetHealth.stateOf(targetKindMCP, "beta"); got != targetHealthUnhealthy {
		t.Errorf("beta state = %q, want unhealthy (unreachable)", got)
	}
}

// TestHandleMCPFederated_ToolsList_TargetHealth_JSONRPCError_StillHealthy
// proves a server that ANSWERS with its own JSON-RPC-level error is
// still recorded healthy — it responded, which is what target-health
// cares about; only a transport-level failure (err != nil from
// mcpBackendCall) counts against it.
func TestHandleMCPFederated_ToolsList_TargetHealth_JSONRPCError_StillHealthy(t *testing.T) {
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "tools unavailable"}})
	}))
	defer alpha.Close()

	cfg := newFederationTestConfig(alpha.URL, alpha.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthHealthy {
		t.Errorf("alpha state = %q, want healthy — a JSON-RPC-level error still means the server answered", got)
	}
}

// TestHandleMCPFederated_ToolsCall_TargetHealth_RecordsResolvedServerOnly
// proves tools/call records health for the ONE resolved server, never
// the other configured-but-uncontacted one.
func TestHandleMCPFederated_ToolsCall_TargetHealth_RecordsResolvedServerOnly(t *testing.T) {
	alpha := newMockJSONRPCServer(t, nil)
	beta := newMockJSONRPCServer(t, nil)
	cfg := newFederationTestConfig(alpha.srv.URL, beta.srv.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("5"), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthHealthy {
		t.Errorf("alpha state = %q, want healthy", got)
	}
	if got := gw.targetHealth.stateOf(targetKindMCP, "beta"); got != targetHealthUnknown {
		t.Errorf("beta state = %q, want unknown (never contacted)", got)
	}
}

// TestHandleMCPFederated_ToolsCall_TargetHealth_UnreachableServer_RecordsUnhealthy
// proves a resolved-but-unreachable backend records a failure even
// though the client sees a generic JSON-RPC internal error, not a raw
// transport error.
func TestHandleMCPFederated_ToolsCall_TargetHealth_UnreachableServer_RecordsUnhealthy(t *testing.T) {
	cfg := newFederationTestConfig("http://127.0.0.1:1", "http://127.0.0.1:1", false)
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("5"), Params: callParams,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthUnhealthy {
		t.Errorf("alpha state = %q, want unhealthy", got)
	}
}

// --- feat/target-health F2: context-caused fan-out errors record nothing ---
//
// A plain connection-refused server still recording a failure is already
// covered by TestHandleMCPFederated_ToolsList_TargetHealth_RecordsPerServer
// (tools/list) and TestHandleMCPFederated_ToolsCall_TargetHealth_UnreachableServer_RecordsUnhealthy
// (tools/call) above — both must keep passing unchanged by the F2 fix
// below, which only special-cases context.Canceled/context.DeadlineExceeded.

// TestHandleMCPFederated_ToolsList_ClientCanceled_RecordsNothing proves F2:
// a client cancel mid-fan-out — context.Canceled propagating from
// r.Context() into fanoutCtx — records nothing for the servers still in
// flight, mirroring recordTargetProxyHealth's own client-cancel rule
// (target_health.go): a caller hanging up says nothing about the target's
// own health.
func TestHandleMCPFederated_ToolsList_ClientCanceled_RecordsNothing(t *testing.T) {
	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	blocking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		<-release
	})
	alpha := httptest.NewServer(blocking)
	beta := httptest.NewServer(blocking)
	// Registered in this order so t.Cleanup's LIFO order unblocks both
	// handlers (closing release) BEFORE either server's Close is called —
	// httptest.Server.Close blocks until outstanding requests complete, so
	// closing it first would deadlock until the test's own timeout (F6).
	t.Cleanup(alpha.Close)
	t.Cleanup(beta.Close)
	t.Cleanup(func() { close(release) })

	cfg := newFederationTestConfig(alpha.URL, beta.URL, false)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	ctx, cancel := context.WithCancel(context.Background())
	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")}).WithContext(ctx)

	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	<-reached
	<-reached
	cancel()
	<-done

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthUnknown {
		t.Errorf("alpha state = %q, want unknown (a client cancel must record nothing)", got)
	}
	if got := gw.targetHealth.stateOf(targetKindMCP, "beta"); got != targetHealthUnknown {
		t.Errorf("beta state = %q, want unknown (a client cancel must record nothing)", got)
	}
}

// TestHandleMCPFederated_ToolsList_HungDialedServer_RecordsFailure proves
// G4 (feat/target-health review round 2): a server that WAS actually
// dialed and never answers before the fan-out's own shared budget
// expires (toolsListBackendTimeout, or here the incoming request's own
// shorter deadline — see
// TestHandleMCPFederated_ToolsList_SlowBackend_BoundedByRequestContext's
// own doc comment for why that stands in for the real 20s budget) must
// still record a failure, not nothing. F2's original guard
// blanket-suppressed context.DeadlineExceeded too, which meant a
// genuinely hung server read "unknown" forever under federation-only
// traffic whenever targetHealth.enabled defaults false — the
// over-suppression this fix removes.
func TestHandleMCPFederated_ToolsList_HungDialedServer_RecordsFailure(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")})
	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(shortCtx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthUnhealthy {
		t.Errorf("alpha state = %q, want unhealthy (dialed and never answered within the fan-out budget)", got)
	}
}

// TestHandleMCPFederated_ToolsList_NeverDialedSemaphoreStarved_RecordsNothing
// proves G4's other half: a server still parked behind the fan-out
// semaphore when the shared deadline fires — never actually dialed —
// must still record nothing, unlike a server that WAS dialed (the test
// above). One more server than the fan-out semaphore has slots for
// (mcpFederatedFanoutConcurrency) guarantees at least one is never
// dialed; draining exactly mcpFederatedFanoutConcurrency sends on
// reached first proves which servers WERE actually dialed before any
// assertion runs, rather than assuming which specific names win the
// race. snapshot's own "observed" field (target_health.go), not stateOf,
// is what actually distinguishes "recorded a failure, still below
// failureThreshold" from "never recorded at all" — both read state
// targetHealthUnknown (F4), so FailureThreshold: 1 below also makes a
// dialed server's single failure visible as targetHealthUnhealthy
// through stateOf too, for a belt-and-suspenders check.
func TestHandleMCPFederated_ToolsList_NeverDialedSemaphoreStarved_RecordsNothing(t *testing.T) {
	reached := make(chan struct{}, mcpFederatedFanoutConcurrency)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		<-block
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	names := make([]string, 0, mcpFederatedFanoutConcurrency+1)
	mcpServers := make(map[string]*TargetConfig, mcpFederatedFanoutConcurrency+1)
	for i := 0; i < mcpFederatedFanoutConcurrency+1; i++ {
		name := fmt.Sprintf("srv%d", i)
		names = append(names, name)
		mcpServers[name] = &TargetConfig{URL: srv.URL}
	}

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = mcpServers
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage("1")})
	shortCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req = req.WithContext(shortCtx)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	for i := 0; i < mcpFederatedFanoutConcurrency; i++ {
		<-reached
	}
	<-done

	dialed, neverDialed := 0, 0
	for _, name := range names {
		snap := gw.targetHealth.snapshot(targetKindMCP, name)
		if snap.observed {
			dialed++
			if snap.state != targetHealthUnhealthy {
				t.Errorf("mcp/%s state = %q, want unhealthy (dialed and never answered)", name, snap.state)
			}
			continue
		}
		neverDialed++
	}
	if dialed != mcpFederatedFanoutConcurrency {
		t.Errorf("dialed = %d, want %d (exactly one server must never have been dialed)", dialed, mcpFederatedFanoutConcurrency)
	}
	if neverDialed != 1 {
		t.Errorf("neverDialed = %d, want exactly 1", neverDialed)
	}

	// F10, review round 3, 2026-09: countTargetRequests must attribute a
	// request to every DIALED server only — the one server target_health
	// above confirms was never dialed (snap.observed == false) must show
	// ZERO on its per-target request counter, even though it was one of
	// the "allowed" servers this call named. Before the fix, EVERY name
	// in the allowed set moved this counter regardless of whether a
	// goroutine ever won the semaphore for it. getCounter's own "ok"
	// return is not useful here — the in-process fallback store this test
	// runs against (no Redis configured) reports ok=true unconditionally,
	// even for a key that was never incremented (limiter.storeGet's own
	// doc, limits.go) — so the count itself is the only signal that
	// matters.
	now := time.Now()
	for _, name := range names {
		snap := gw.targetHealth.snapshot(targetKindMCP, name)
		got, _ := gw.limiter.getCounter(targetKindMCP, name, metricReq, windowDay, now)
		want := int64(0)
		if snap.observed {
			want = 1
		}
		if got != want {
			t.Errorf("mcp/%s req:day counter = %d, want %d (dialed=%v)", name, got, want, snap.observed)
		}
	}
}

// TestHandleMCPFederated_ToolsCall_ClientCanceled_RecordsNothing mirrors
// TestHandleMCPFederated_ToolsList_ClientCanceled_RecordsNothing for
// tools/call's own, separate context.WithTimeout(r.Context(), ...) call
// site.
func TestHandleMCPFederated_ToolsCall_ClientCanceled_RecordsNothing(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := newFederationTestConfig(srv.URL, "http://beta.invalid", true)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	ctx, cancel := context.WithCancel(context.Background())
	callParams, _ := json.Marshal(mcpToolCallParams{Name: "alpha_lookup"})
	req := newFederatedRequest(t, "sk-alice", jsonrpcRequest{
		JSONRPC: jsonrpcVersion, Method: "tools/call", ID: json.RawMessage("1"), Params: callParams,
	}).WithContext(ctx)

	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	<-reached
	cancel()
	<-done

	if got := gw.targetHealth.stateOf(targetKindMCP, "alpha"); got != targetHealthUnknown {
		t.Errorf("alpha state = %q, want unknown (a client cancel must record nothing)", got)
	}
}
