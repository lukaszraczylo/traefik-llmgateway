package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
