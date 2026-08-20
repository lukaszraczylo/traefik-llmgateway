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
