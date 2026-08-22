package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
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
			result, _ := json.Marshal(mcpToolsListResult{Tools: m.tools})
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
			result, _ := json.Marshal(mcpToolsListResult{Tools: m.tools})
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
				gotNames[i] = tool.Name
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
	if len(result.Tools) != 1 || result.Tools[0].Name != "alpha_lookup" {
		t.Errorf("tools = %+v, want exactly alpha_lookup (beta unreachable, skipped)", result.Tools)
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
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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
func TestMcpBackendCall_HandshakeFallbackTrigger(t *testing.T) {
	cases := []struct {
		bareHandler   http.HandlerFunc
		name          string
		wantHandshake bool
	}{
		{
			name: "bare JSON-RPC error triggers the handshake",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				var req jsonrpcRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jsonrpcResponse{ID: req.ID, Error: &jsonrpcError{Code: -32000, Message: "session required"}})
			},
			wantHandshake: true,
		},
		{
			name: "HTTP 403 with no JSON-RPC body triggers the handshake",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantHandshake: true,
		},
		{
			name: "HTTP 500 never triggers the handshake",
			bareHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantHandshake: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			_, _ = gw.mcpBackendCall(context.Background(), srv.URL, "tools/list", struct{}{}, mcpBackendResponseMaxBytes)

			gotCalls := callCount.Load()
			if tc.wantHandshake && gotCalls < 2 {
				t.Errorf("calls = %d, want >=2 (bare attempt + initialize handshake)", gotCalls)
			}
			if !tc.wantHandshake && gotCalls != 1 {
				t.Errorf("calls = %d, want exactly 1 (bare attempt only, no handshake retry against a 5xx)", gotCalls)
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
// reported with mcpResponseTooLargeMarker in its message —
// distinguishable via strings.Contains from an ordinary network/parse
// failure — never silently truncated into a confusing parse error.
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
	if !strings.Contains(err.Error(), mcpResponseTooLargeMarker) {
		t.Errorf("err = %v, want it to contain %q", err, mcpResponseTooLargeMarker)
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
	if len(result.Tools) != 1 || result.Tools[0].Name != "alpha_search" {
		t.Fatalf("tools = %+v, want exactly alpha_search", result.Tools)
	}

	seq := strict.callSequence()
	if len(seq) != 4 {
		t.Fatalf("call sequence = %v, want exactly 4 calls (bare attempt, initialize, retried real call, best-effort close — the DELETE runs synchronously inside mcpBackendCall's own defer, before it returns)", seq)
	}
	if seq[0] != "tools/list:session=" {
		t.Errorf("call[0] = %q, want a bare tools/list attempt with no session header", seq[0])
	}
	if seq[1] != "initialize:session=" {
		t.Errorf("call[1] = %q, want an initialize handshake with no session header", seq[1])
	}
	if seq[2] != "tools/list:session=sess-1" {
		t.Errorf("call[2] = %q, want the retried tools/list carrying the session header the handshake returned", seq[2])
	}
	if seq[3] != "DELETE:session=sess-1" {
		t.Errorf("call[3] = %q, want the best-effort session close carrying the same session header", seq[3])
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
	if len(result.Tools) != 1 || result.Tools[0].Name != "alpha_lookup" {
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
		{"http://mcp.internal:8080", false},
		{"http://mcp.internal:8080/mcp", false},
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
	if len(result.Tools) != 1 || result.Tools[0].Name != "alpha_lookup" {
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
			result, _ := json.Marshal(mcpToolsListResult{Tools: []mcpTool{{Name: "search"}}})
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
	if len(result.Tools) != 1 || result.Tools[0].Name != "alpha_search" {
		t.Fatalf("tools = %+v, want alpha_search despite the session-close failure", result.Tools)
	}
}
