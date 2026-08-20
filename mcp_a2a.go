package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// Target kinds handleTargetProxy and resolveTarget dispatch on, selecting
// which of Config.MCPServers/Config.Agents to consult and which of
// group.allowsMCP/group.allowsAgent authorizes the request.
const (
	targetKindMCP   = "mcp"
	targetKindAgent = "a2a"
)

// defaultAgentCardPath is the well-known A2A agent-card path used for a
// listed agent whose AgentConfig.Card is empty.
const defaultAgentCardPath = "/.well-known/agent-card.json"

// mcpServerListing is one entry of GET /v1/mcp/servers: the configured
// name and the gateway-relative URL a client proxies requests to it
// through.
type mcpServerListing struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// agentListing is one entry of GET /v1/agents: the configured name, the
// gateway-relative proxy URL, and the gateway-relative agent-card URL.
type agentListing struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Card string `json:"card"`
}

// handleMCPServers implements GET /v1/mcp/servers: grp's visible MCP
// servers, filtered by group.allowsMCP and sorted by name for a
// deterministic response.
func (g *Gateway) handleMCPServers(w http.ResponseWriter, grp *group) {
	names := make([]string, 0, len(g.cfg.MCPServers))
	for name := range g.cfg.MCPServers {
		if grp.allowsMCP(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	servers := make([]mcpServerListing, len(names))
	for i, name := range names {
		servers[i] = mcpServerListing{Name: name, URL: "/mcp/" + name}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"servers": servers}) // headers already committed; nothing useful to do on encode failure
}

// handleAgents implements GET /v1/agents: grp's visible A2A agents,
// filtered by group.allowsAgent and sorted by name for a deterministic
// response. Each entry's card URL is the agent's own gateway-relative
// proxy URL plus its configured card path, defaulting to
// defaultAgentCardPath when AgentConfig.Card is empty.
func (g *Gateway) handleAgents(w http.ResponseWriter, grp *group) {
	names := make([]string, 0, len(g.cfg.Agents))
	for name := range g.cfg.Agents {
		if grp.allowsAgent(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	agents := make([]agentListing, len(names))
	for i, name := range names {
		cardPath := g.cfg.Agents[name].Card
		if cardPath == "" {
			cardPath = defaultAgentCardPath
		}
		agents[i] = agentListing{Name: name, URL: "/a2a/" + name, Card: "/a2a/" + name + cardPath}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents}) // headers already committed; nothing useful to do on encode failure
}

// targetRoute splits path into a target name and its rest path for the
// "/{prefix}/{name}/{rest...}" MCP/A2A convention (prefix is
// targetKindMCP or targetKindAgent), reusing passthroughRoute's own
// EscapedPath-based, traversal-safe splitting twice: once to peel prefix
// off path, once more on what is left to peel name off rest. ok is false
// when path's first segment is not exactly prefix, or when no name
// segment follows it — a bare "/mcp" or "/mcp/" carries no target name to
// route to.
func targetRoute(path, prefix string) (name, rest string, ok bool) {
	first, remainder, matched := passthroughRoute(path)
	if !matched || first != prefix {
		return "", "", false
	}
	return passthroughRoute("/" + remainder)
}

// resolveTarget looks up name in the Gateway's MCP-server or agent
// registry, selected by kind (targetKindMCP or targetKindAgent). known is
// false when no such name is configured at all; allowed reports whether
// grp's glob patterns permit it, meaningful only when known is true.
func (g *Gateway) resolveTarget(kind, name string, grp *group) (targetURL string, allowed, known bool) {
	switch kind {
	case targetKindMCP:
		tc, ok := g.cfg.MCPServers[name]
		if !ok {
			return "", false, false
		}
		return tc.URL, grp.allowsMCP(name), true
	case targetKindAgent:
		ac, ok := g.cfg.Agents[name]
		if !ok {
			return "", false, false
		}
		return ac.URL, grp.allowsAgent(name), true
	default:
		return "", false, false
	}
}

// handleTargetProxy implements the MCP/A2A target proxy routes:
// "/mcp/{name}/{rest...}" and "/a2a/{name}/{rest...}" reverse-proxy rest,
// verbatim, to name's configured target URL (Config.MCPServers[name].URL
// or Config.Agents[name].URL, per kind). It shares its proxy mechanics
// with handlePassthrough via proxyUpstream — hop-by-hop header stripping,
// the client's own gateway credential stripped, SSE-safe incremental
// flushing, a dead upstream mapped to 502, a canceled client context
// logged rather than surfaced. Unlike handlePassthrough, no upstream
// credential is injected (proxyUpstream's injectAuth argument is nil):
// MCP servers and A2A agents are in-cluster targets that trust the
// gateway's network position, not a per-provider API key the gateway
// holds on the caller's behalf. Accounting is likewise passthrough-only —
// a target request is counted by checkAndCount below, but its response
// is never teed off for token/cost usage extraction.
//
// Checks run: unknown name (404) before group authorization (403) before
// rate limits (429/503) before the capability/path checks (Upgrade→501,
// traversal→400) that gate the proxy call itself — a caller addressing a
// target that does not exist, or that they may not use, or that their
// group is already over budget on, learns that before any detail of how
// they tried to use it.
func (g *Gateway) handleTargetProxy(w http.ResponseWriter, r *http.Request, u *user, grp *group, kind, name, rest string) {
	targetURL, allowed, known := g.resolveTarget(kind, name, grp)
	if !known {
		writeOAIError(w, http.StatusNotFound, "invalid_request_error", "unknown "+kind+" target")
		return
	}
	if !allowed {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", kind+" access denied")
		return
	}

	scopes := buildLimitScopes(u, grp)
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		writeLimitViolation(w, violation)
		return
	}

	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}
	if hasTraversalSegment(rest) {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid path")
		return
	}

	upstreamURL := targetURL
	if rest != "" {
		upstreamURL += "/" + rest
	}
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	g.proxyUpstream(w, r, upstreamURL, g.targetClient, nil, false, kind+" target (name "+name+")")
}

// validateTargetURLs checks that every configured MCP-server and agent
// target URL parses (url.Parse) and uses an http or https scheme. A
// malformed or non-HTTP target is rejected once, here, at construction —
// a constructor error, not a 502 the first time some caller happens to
// address it.
func validateTargetURLs(cfg *Config) error {
	for name, tc := range cfg.MCPServers {
		if err := validateTargetURL(tc.URL); err != nil {
			return fmt.Errorf("llmgateway: mcpServers %q: %w", name, err)
		}
	}
	for name, ac := range cfg.Agents {
		if err := validateTargetURL(ac.URL); err != nil {
			return fmt.Errorf("llmgateway: agents %q: %w", name, err)
		}
	}
	return nil
}

// validateTargetURL reports an error unless raw parses as a URL with an
// http or https scheme.
func validateTargetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https scheme, got %q", raw, u.Scheme)
	}
	return nil
}
