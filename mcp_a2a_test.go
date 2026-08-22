package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit tests: targetRoute ---

func TestTargetRoute(t *testing.T) {
	tests := []struct {
		path     string
		prefix   string
		wantName string
		wantRest string
		wantOK   bool
	}{
		{"/mcp/myserver/tools/list", "mcp", "myserver", "tools/list", true},
		{"/mcp/myserver", "mcp", "myserver", "", true},
		{"/mcp/myserver/", "mcp", "myserver", "", true},
		{"/mcp", "mcp", "", "", false},
		{"/mcp/", "mcp", "", "", false},
		{"/a2a/agent1/.well-known/agent-card.json", "a2a", "agent1", ".well-known/agent-card.json", true},
		{"/a2a/agent1", "a2a", "agent1", "", true},
		{"/notmcp/foo", "mcp", "", "", false},
		{"/", "mcp", "", "", false},
		{"", "mcp", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.prefix+tt.path, func(t *testing.T) {
			gotName, gotRest, gotOK := targetRoute(tt.path, tt.prefix)
			if gotOK != tt.wantOK || gotName != tt.wantName || gotRest != tt.wantRest {
				t.Errorf("targetRoute(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.path, tt.prefix, gotName, gotRest, gotOK, tt.wantName, tt.wantRest, tt.wantOK)
			}
		})
	}
}

// --- registry filtering: GET /v1/mcp/servers, GET /v1/agents ---

func TestHandleMCPServers_GroupFiltered_SortedByName(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"charlie": {URL: "http://mcp-charlie.internal"},
		"alpha":   {URL: "http://mcp-alpha.internal"},
		"bravo":   {URL: "http://mcp-bravo.internal"},
	}
	cfg.Groups = map[string]*GroupConfig{"limited": {MCPServers: []string{"alpha", "charlie"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "limited", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/servers", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Servers []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Servers) != 2 {
		t.Fatalf("servers = %+v, want 2 entries (alpha, charlie; bravo denied by group)", got.Servers)
	}
	if got.Servers[0].Name != "alpha" || got.Servers[0].URL != "/mcp/alpha" {
		t.Errorf("servers[0] = %+v, want name=alpha url=/mcp/alpha", got.Servers[0])
	}
	if got.Servers[1].Name != "charlie" || got.Servers[1].URL != "/mcp/charlie" {
		t.Errorf("servers[1] = %+v, want name=charlie url=/mcp/charlie", got.Servers[1])
	}
}

func TestHandleAgents_GroupFiltered_SortedByName_CardPath(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Agents = map[string]*AgentConfig{
		"zeta":  {URL: "http://agent-zeta.internal"},
		"delta": {URL: "http://agent-delta.internal", Card: "/custom/card.json"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Agents []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
			Card string `json:"card"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("agents = %+v, want 2 entries", got.Agents)
	}
	// sorted: delta before zeta
	if got.Agents[0].Name != "delta" || got.Agents[0].Card != "/a2a/delta/custom/card.json" {
		t.Errorf("agents[0] = %+v, want name=delta card=/a2a/delta/custom/card.json", got.Agents[0])
	}
	if got.Agents[1].Name != "zeta" || got.Agents[1].Card != "/a2a/zeta/.well-known/agent-card.json" {
		t.Errorf("agents[1] = %+v, want name=zeta card=/a2a/zeta/.well-known/agent-card.json (default cardPath)", got.Agents[1])
	}
}

func TestHandleMCPServers_Unauthenticated_Returns401(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, path := range []string{"/v1/mcp/servers", "/v1/agents"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- target proxy: strips gateway auth, injects nothing ---

func TestHandleTargetProxy_StripsGatewayAuth_NoUpstreamInjection(t *testing.T) {
	var gotAuth, gotAPIKeyHeader, gotConnection string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		gotConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mcp-ok"))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/tools/list", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Connection", "keep-alive")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "mcp-ok" {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}
	if gotAuth != "" {
		t.Errorf("upstream saw Authorization %q, want stripped and nothing injected (in-cluster target)", gotAuth)
	}
	if gotAPIKeyHeader != "" {
		t.Errorf("upstream saw x-api-key %q, want stripped", gotAPIKeyHeader)
	}
	if gotConnection != "" {
		t.Errorf("upstream saw hop-by-hop Connection header %q, want stripped", gotConnection)
	}
}

// TestHandleTargetProxy_ClientAPIKeyHeaderStripped_NoUpstreamInjection
// proves the client's gateway credential, presented via x-api-key rather
// than Authorization, never reaches an MCP target. handleTargetProxy
// injects no credential at all (unlike a provider adapter's injectAuth),
// so if gatewayCredentialHeaders' X-Api-Key entry were ever dropped from
// the strip set, the client's own key would pass straight through
// copyHeadersExcept to the upstream target (I1: the sibling test above
// authenticates via Authorization and never sends x-api-key at all, so it
// never exercised this strip entry).
func TestHandleTargetProxy_ClientAPIKeyHeaderStripped_NoUpstreamInjection(t *testing.T) {
	var gotAPIKeyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mcp-ok"))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "gateway-key-must-not-leak"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/tools/list", nil)
	// Gateway auth presented via x-api-key only (no Authorization header) —
	// presentedKey falls back to x-api-key when Authorization is absent.
	req.Header.Set("x-api-key", "gateway-key-must-not-leak")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "mcp-ok" {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}
	if gotAPIKeyHeader != "" {
		t.Errorf("upstream saw x-api-key = %q, want empty (MCP target proxy injects nothing; the client's gateway credential must be stripped)", gotAPIKeyHeader)
	}
}

// TestHandleTargetProxy_DangerousHeadersStripped is the MCP/A2A-target
// half of the security+performance audit's (2026-08-22) additive header
// strip — proxyUpstream applies dangerousClientHeaders/
// dangerousClientHeaderPrefixes on BOTH proxy paths, so an MCP target
// must never see a client-spoofed identity/forwarding header, exactly
// like a native passthrough provider.
func TestHandleTargetProxy_DangerousHeadersStripped(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/tools/list", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Forwarded", "for=203.0.113.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	req.Header.Set("X-Auth-Request-Email", "spoofed@example.com")
	req.Header.Set("X-Remote-User", "spoofed-admin")
	req.Header.Set("X-Remote-Groups", "admin")
	req.Header.Set("Cookie", "session=stolen")
	req.Header.Set("X-Client-Custom", "keep-me")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, hdr := range []string{"Forwarded", "X-Forwarded-For", "X-Auth-Request-Email", "X-Remote-User", "X-Remote-Groups", "Cookie"} {
		if v := got.Get(hdr); v != "" {
			t.Errorf("upstream MCP target saw %s = %q, want stripped", hdr, v)
		}
	}
	if got.Get("X-Client-Custom") != "keep-me" {
		t.Error("upstream did not see ordinary header X-Client-Custom — strip must not be a full allowlist inversion")
	}
}

// --- SSE-safe streaming through the target proxy ---

func TestHandleTargetProxy_SSE_FlushesIncrementally(t *testing.T) {
	continueCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = w.Write([]byte("data: chunk-1\n\n"))
		fl.Flush()
		<-continueCh
		_, _ = w.Write([]byte("data: chunk-2\n\n"))
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/stream", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	sw := newSyncFlushWriter()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(sw, req)
		close(done)
	}()

	select {
	case <-sw.writeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first chunk to reach the client")
	}
	_, writesAfterFirst, _, bodyAfterFirst := sw.snapshot()
	if writesAfterFirst != 1 {
		t.Fatalf("writes after first chunk = %d, want exactly 1 (chunks must not coalesce)", writesAfterFirst)
	}
	if !strings.Contains(bodyAfterFirst, "chunk-1") || strings.Contains(bodyAfterFirst, "chunk-2") {
		t.Fatalf("body after first chunk = %q, want chunk-1 only", bodyAfterFirst)
	}

	close(continueCh)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ServeHTTP to finish")
	}

	status, writes, flushes, body := sw.snapshot()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if writes < 2 {
		t.Fatalf("want >=2 writes (one per upstream chunk), got %d", writes)
	}
	if flushes < writes {
		t.Fatalf("want a flush per write (incremental delivery), got %d writes and %d flushes", writes, flushes)
	}
	if !strings.Contains(body, "chunk-1") || !strings.Contains(body, "chunk-2") {
		t.Errorf("body missing streamed chunks, got %q", body)
	}
}

// --- authz and validation checks ---

func TestHandleTargetProxy_UnknownName_Returns404(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	cfg.Agents = map[string]*AgentConfig{"agent1": {URL: "http://agent1.internal"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, path := range []string{"/mcp/nosuchserver/tools/list", "/a2a/nosuchagent/tasks/send"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleTargetProxy_Unauthenticated_Returns401(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Even a name that does not exist must be rejected as 401 before 404 —
	// auth runs before the unknown-target check.
	req := httptest.NewRequest(http.MethodGet, "/mcp/nosuchserver/tools/list", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTargetProxy_GroupDenies_Returns403(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"alpha": {URL: "http://mcp-alpha.internal"},
		"beta":  {URL: "http://mcp-beta.internal"},
	}
	cfg.Groups = map[string]*GroupConfig{"limited": {MCPServers: []string{"alpha"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "limited", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/beta/tools/list", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTargetProxy_TraversalPath_Returns400(t *testing.T) {
	upstreamCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, path := range []string{
		"/mcp/alpha/../secret",
		"/mcp/alpha/..%2f..%2fsecret",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a rejected traversal path")
	}
}

func TestHandleTargetProxy_UpgradeHeader_Returns501(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/realtime", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
}

// --- empty rest proxies to target root ---

func TestHandleTargetProxy_EmptyRest_ProxiesToRoot(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/" {
		t.Errorf("upstream path = %q, want %q (empty rest proxies to target root)", gotPath, "/")
	}
}

// --- dead upstream maps to 502, same as native passthrough ---

func TestHandleTargetProxy_DeadUpstream_Returns502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: deadURL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/tools/list", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
}

// --- a2a proxies distinctly from mcp ---

func TestHandleTargetProxy_A2A_ProxiesToAgent(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a2a-ok"))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Agents = map[string]*AgentConfig{"agent1": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/a2a/agent1/tasks/send", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "a2a-ok" {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}
	if gotPath != "/tasks/send" {
		t.Errorf("upstream path = %q, want /tasks/send", gotPath)
	}
}

// --- rate limiting: rejected-before-upstream requests still count, and a
// breached limit is enforced before the upstream is ever called ---

// TestHandleTargetProxy_RateLimited_Returns429WithRetryAfter proves the
// target proxy enforces the caller's group requestsPerMinute limit: the
// first request within the window succeeds, the second is refused with
// 429 and a Retry-After header, and the upstream is only ever called
// once.
func TestHandleTargetProxy_RateLimited_Returns429WithRetryAfter(t *testing.T) {
	upstreamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"limited": {Limits: &LimitsConfig{RequestsPerMinute: 1}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "limited", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/x", nil)
		req.Header.Set("Authorization", "Bearer sk-alice")
		return req
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("want a Retry-After header on the 429 response")
	}

	if upstreamCalls != 1 {
		t.Errorf("upstream calls = %d, want exactly 1 (the rejected second request must never reach the upstream)", upstreamCalls)
	}
}

// --- accounting: a target request counts only against request-rate
// counters, never token/cost, even for a JSON response shaped like a
// usage-bearing one ---

// TestHandleTargetProxy_JSONResponse_OnlyRequestCounterMoves proves the
// target proxy never tees or parses a response for usage accounting
// (unlike native passthrough): a JSON body carrying usage-shaped fields
// leaves the caller's token/cost counters untouched, while the
// request-rate counter checkAndCount always increments still moves.
func TestHandleTargetProxy_JSONResponse_OnlyRequestCounterMoves(t *testing.T) {
	const respBody = `{"model":"gpt-native-x","usage":{"prompt_tokens":7,"completion_tokens":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: srv.URL}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/alpha/tools/list", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 0 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 0 — target proxy must never account response usage", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 0 — target proxy must never account response usage", tokOut, ok)
	}
	cost, ok := gw.limiter.getCounter("user", "alice", metricCost, windowDay, time.Now())
	if !ok || cost != 0 {
		t.Errorf("user cost/day counter = %d (ok=%v), want 0 — target proxy must never account response cost", cost, ok)
	}
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1 — the request itself is still accounted", reqCount, ok)
	}

	// withTotalScope (handleTargetProxy, mcp_a2a.go) must have appended
	// the synthetic total scope too (v0.2 data-layer task): counted like
	// any other scope, still never token/cost-accounted here.
	totalReq, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, time.Now())
	if !ok || totalReq != 1 {
		t.Errorf("total request/min counter = %d (ok=%v), want 1", totalReq, ok)
	}

	// Feature A (v0.22): handleTargetProxy/proxyUpstream never wrap the
	// request context with an attemptRecorder — an MCP/A2A target proxy
	// attempt against the "openai" MCP server's own upstream must never be
	// mistaken for provider traffic, even though a provider of the same
	// name happens to be configured too.
	provAttempts, _ := gw.limiter.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, time.Now())
	if provAttempts != 0 {
		t.Errorf("provider attempts/day = %d, want 0 — the MCP target proxy must never record provider accounting", provAttempts)
	}
}

// --- config validation at construction ---

func TestNewGateway_TargetURLValidation(t *testing.T) {
	tests := []struct {
		mcpServers map[string]*TargetConfig
		agents     map[string]*AgentConfig
		name       string
		wantErr    bool
	}{
		{
			name:       "valid mcp server url",
			mcpServers: map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal:8080"}},
			wantErr:    false,
		},
		{
			name:    "valid https agent url",
			agents:  map[string]*AgentConfig{"agent1": {URL: "https://agent1.internal"}},
			wantErr: false,
		},
		{
			name:       "malformed mcp url (control character)",
			mcpServers: map[string]*TargetConfig{"alpha": {URL: "http://exa\x7fmple.com"}},
			wantErr:    true,
		},
		{
			name:       "disallowed scheme for mcp server",
			mcpServers: map[string]*TargetConfig{"alpha": {URL: "ftp://mcp-alpha.internal"}},
			wantErr:    true,
		},
		{
			name:    "disallowed scheme for agent",
			agents:  map[string]*AgentConfig{"agent1": {URL: "ws://agent1.internal"}},
			wantErr: true,
		},
		{
			name:    "malformed agent url (invalid port)",
			agents:  map[string]*AgentConfig{"agent1": {URL: "http://agent1.internal:notaport"}},
			wantErr: true,
		},
		{
			name:       "nil mcpServers config value is a constructor error, not a panic",
			mcpServers: map[string]*TargetConfig{"alpha": nil},
			wantErr:    true,
		},
		{
			name:    "nil agents config value is a constructor error, not a panic",
			agents:  map[string]*AgentConfig{"agent1": nil},
			wantErr: true,
		},
		{
			name:       "mcpServers name reserved (v1) is a constructor error",
			mcpServers: map[string]*TargetConfig{"v1": {URL: "http://mcp-alpha.internal:8080"}},
			wantErr:    true,
		},
		{
			name:       "mcpServers name reserved (mcp) is a constructor error",
			mcpServers: map[string]*TargetConfig{"mcp": {URL: "http://mcp-alpha.internal:8080"}},
			wantErr:    true,
		},
		{
			name:    "agents name reserved (a2a) is a constructor error",
			agents:  map[string]*AgentConfig{"a2a": {URL: "https://agent1.internal"}},
			wantErr: true,
		},
		{
			name:       "mcpServers name with invalid character is a constructor error",
			mcpServers: map[string]*TargetConfig{"has a space": {URL: "http://mcp-alpha.internal:8080"}},
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
			cfg.MCPServers = tt.mcpServers
			cfg.Agents = tt.agents
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if h != nil {
					t.Fatal("want nil handler on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

// --- Feature B (v0.21): per-target request accounting ---

func TestTargetScopeKind(t *testing.T) {
	cases := []struct {
		routingKind string
		want        string
	}{
		{targetKindMCP, "mcp"},
		{targetKindAgent, scopeKindAgent},
	}
	for _, tc := range cases {
		t.Run(tc.routingKind, func(t *testing.T) {
			if got := targetScopeKind(tc.routingKind); got != tc.want {
				t.Errorf("targetScopeKind(%q) = %q, want %q", tc.routingKind, got, tc.want)
			}
		})
	}
}

// TestHandleTargetProxy_PerTargetCounters_AttributedAndCollisionSafe proves
// handleTargetProxy attributes a proxied request to name's own per-target
// scope (Feature B, v0.21) at every window countTargetRequest tracks
// (min/hour/day/month), for both an MCP server and an A2A agent target,
// AND that this never collides with a same-named user counter: both cases
// deliberately name the target "alice" — identical to the calling user's
// own name — so a bug that dropped kind from the counter key (windowKey,
// limits.go) would show up as the user's own counters silently absorbing
// the target's, or vice versa.
func TestHandleTargetProxy_PerTargetCounters_AttributedAndCollisionSafe(t *testing.T) {
	cases := []struct {
		wantScopeKind string
		routingKind   string
		targetName    string
		path          string
	}{
		{wantScopeKind: targetKindMCP, routingKind: targetKindMCP, targetName: "alice", path: "/mcp/alice/tools/list"},
		{wantScopeKind: scopeKindAgent, routingKind: targetKindAgent, targetName: "alice", path: "/a2a/alice/tasks/send"},
	}
	for _, tc := range cases {
		t.Run(tc.routingKind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
			if tc.routingKind == targetKindMCP {
				cfg.MCPServers = map[string]*TargetConfig{tc.targetName: {URL: srv.URL}}
			} else {
				cfg.Agents = map[string]*AgentConfig{tc.targetName: {URL: srv.URL}}
			}
			cfg.Groups = map[string]*GroupConfig{"default": {}}
			cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw, ok := h.(*Gateway)
			if !ok {
				t.Fatal("handler is not *Gateway")
			}

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}

			now := time.Now()
			for _, window := range []string{windowMin, windowHour, windowDay, windowMonth} {
				got, gotOK := gw.limiter.getCounter(tc.wantScopeKind, tc.targetName, metricReq, window, now)
				if !gotOK || got != 1 {
					t.Errorf("%s/%s req:%s counter = %d (ok=%v), want 1", tc.wantScopeKind, tc.targetName, window, got, gotOK)
				}
			}

			// Collision guard: the caller's own USER scope (kind="user")
			// must stay independent of the TARGET scope above despite
			// sharing the identical id string "alice" — proving kind, not
			// id alone, is what makes a counter key unique.
			userReqMin, minOK := gw.limiter.getCounter("user", "alice", metricReq, windowMin, now)
			if !minOK || userReqMin != 1 {
				t.Errorf("user/alice req:min counter = %d (ok=%v), want 1 (its own scope, unaffected by the target scope of the same id)", userReqMin, minOK)
			}
			userReqMonth, monthOK := gw.limiter.getCounter("user", "alice", metricReq, windowMonth, now)
			if !monthOK || userReqMonth != 0 {
				t.Errorf("user/alice req:month counter = %d (ok=%v), want 0 (checkAndCount never writes a month window for user/group scopes; nonzero here would mean the target scope's month write leaked into it)", userReqMonth, monthOK)
			}
		})
	}
}
