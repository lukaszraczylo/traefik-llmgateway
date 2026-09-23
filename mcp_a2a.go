package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
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

// scopeKindAgent is the limitScope.kind (and admin-API scope-kind) string
// an A2A agent target's per-target request counters use (Feature B,
// v0.21) — deliberately NOT targetKindAgent ("a2a", this file's own
// URL-routing convention): "agent" is the vocabulary GET /admin/api/targets
// (admin.go) and the webui's MCP & Agents panel use. The split matters for
// collision-safety too: windowKey (limits.go) embeds kind as the counter
// key's own leading segment, so an MCP server and an A2A agent configured
// with the identical name — or either one sharing a name with a user or
// group — can never share a counter regardless of what any of them are
// called; kind, not id alone, is what makes a scope unique.
const scopeKindAgent = "agent"

// targetScopeKind maps handleTargetProxy's routing kind (targetKindMCP or
// targetKindAgent) to the accounting scope kind its per-target request
// counters use: identical for MCP (targetKindMCP is already "mcp"), but
// remapped to scopeKindAgent ("agent") for an A2A target.
func targetScopeKind(kind string) string {
	if kind == targetKindAgent {
		return scopeKindAgent
	}
	return kind
}

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
	g.maybeSweepTargetHealth()
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
	g.maybeSweepTargetHealth()
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
// a target request is counted by checkAndCount below (the caller's own
// user/group/total scopes) and, additionally, attributed to name's own
// per-target scope (limiter.countTargetRequest, Feature B v0.21) — but its
// response is never teed off for token/cost usage extraction, for any of
// those scopes.
//
// Checks run: unknown name (404) before group authorization (403) before
// capability/path checks (Upgrade→501, traversal→400) before rate limits
// (429/503) — matching handlePassthrough's own order (403 → 501 → 400 →
// limit), with the unknown-name check slotted in first since a target
// proxy request can fail that in a way native passthrough never can. A
// request rejected on any check before checkAndCount must never burn the
// caller's or their group's rate-limit quota for a call that was never
// going to reach the upstream.
//
// Once admitted, limiter.countTargetRequest (limits.go) additionally
// attributes the request to name's own per-target scope (Feature B,
// v0.21) — requests only, same as the caller's own user/group counters:
// GET /admin/api/targets (admin.go) reads these back for the webui's MCP &
// Agents panel. This scope carries no limit of its own (no config surface
// added this round) and so can never itself cause a 429/503; it is purely
// additive visibility on top of the user/group/total admission decision
// checkAndCount already made above.
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

	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}
	if hasTraversalSegment(rest) {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid path")
		return
	}

	scopes := withTotalScope(buildLimitScopes(u, grp))
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		g.recordLimitEvent(scopes, kind, violation) // F3 hook 1 (v0.3 dashboard task): kind is already "mcp"/"a2a"
		writeLimitViolation(w, violation)
		return
	}
	// P9 fix (admin dashboard redesign verify round): last-seen only for
	// an admitted request — recordLastSeen's own doc comment (limits.go).
	g.limiter.recordLastSeen(scopes, g.limiter.now())
	// Target x caller (always-on-with-admin, DECISIONS): u.name identifies
	// the caller whose own credential authorized this proxy — see
	// countTargetRequestBy's own doc comment (limits.go) for the id shape
	// and admin-stats gating.
	g.limiter.countTargetRequestBy(u.name, targetScopeKind(kind), name)

	upstreamURL, err := buildUpstreamTargetURL(targetURL, rest, r.URL.RawQuery)
	if err != nil {
		// targetURL is operator-configured and already validated at
		// construction (validateTargetURLs) — unreachable in practice,
		// kept as an explicit guard rather than trusting that invariant
		// silently.
		writeOAIError(w, http.StatusInternalServerError, "invalid_request_error", "invalid target configuration")
		return
	}

	// feat/target-health: passive recording (target_health.go) — latency
	// measured around the whole proxyUpstream call, matching what an
	// active probe of the same target measures. See
	// recordTargetProxyHealth's own doc comment for the exact
	// success/failure rule.
	start := time.Now()
	result, ok := g.proxyUpstream(w, r, upstreamURL, g.targetClient, nil, nil, false, kind+" target (name "+name+")", g.targetTimeout)
	g.recordTargetProxyHealth(kind, name, targetURL, result, ok, time.Since(start))
	g.recordProxyEvent(scopes, name, kind, result) // F3 hook 3 (v0.3 dashboard task)
}

// buildUpstreamTargetURL joins targetURL (an MCP server or A2A agent's own
// configured base, already validated at construction by
// validateTargetURL) with rest (handleTargetProxy's own path remainder,
// already checked for a traversal segment by its caller) using
// url.URL.JoinPath rather than plain string concatenation (F7, review
// round 3, 2026-09): the prior "+ \"/\" + rest" produced a doubled slash
// whenever targetURL's own path already ended in "/" (e.g. configured as
// "http://x/mcp/"), and, worse, a targetURL carrying its own query string
// — the only way TargetConfig/AgentConfig can pass a credential to an
// upstream today, since neither carries a header field — got that query
// glued onto the wrong side of "?" + the request's own RawQuery,
// producing something like "...?token=abc/foo" or two "?" characters and
// corrupting the token federation was relying on. The target's own query
// string is preserved (JoinPath never touches RawQuery) and merged with
// the incoming request's own query (mergeQueryStrings, below) — neither
// silently overwrites the other.
func buildUpstreamTargetURL(targetURL, rest, requestRawQuery string) (string, error) {
	base, err := url.Parse(targetURL)
	if err != nil {
		return "", err
	}
	joined := base
	if rest != "" {
		joined = base.JoinPath(rest)
	}
	joined.RawQuery = mergeQueryStrings(joined.RawQuery, requestRawQuery)
	return joined.String(), nil
}

// mergeQueryStrings combines targetQuery (the configured target URL's own
// query string, returned verbatim when it stands alone) with
// requestQuery (the incoming client request's own), so neither silently
// discards the other (F7). A request query that fails to parse is
// dropped, not treated as a whole-request failure: the request's own
// method/body may still be perfectly valid, and the target's own query —
// a credential, per this file's own TargetConfig/AgentConfig doc
// comments — is the one half of this that must never be lost silently.
func mergeQueryStrings(targetQuery, requestQuery string) string {
	if targetQuery == "" {
		return requestQuery
	}
	if requestQuery == "" {
		return targetQuery
	}
	merged, err := url.ParseQuery(targetQuery)
	if err != nil {
		// targetQuery came from an already-parsed url.URL's own RawQuery,
		// so this is unreachable in practice — falls back to the
		// request's own query alone rather than panicking or silently
		// dropping both.
		return requestQuery
	}
	reqValues, err := url.ParseQuery(requestQuery)
	if err != nil {
		return targetQuery
	}
	for k, vs := range reqValues {
		for _, v := range vs {
			merged.Add(k, v)
		}
	}
	return merged.Encode()
}

// validateTargetURLs checks every configured MCP-server and agent entry at
// construction: the map value must be non-nil, the name must pass
// validateConfigName (the same character-set, dot-only, and reserved-name
// rules as a provider name — an MCP server or agent is routed at
// "/mcp/{name}/*" or "/a2a/{name}/*", so the same shadowing risk and the
// same "." / ".." directory-traversal-segment risk both apply), and the
// target URL must parse (url.Parse) and use an http or https scheme. A nil
// value, invalid name, or malformed/non-HTTP target is rejected once, here,
// at construction — a constructor error, not a nil-pointer panic or a 502
// the first time some caller happens to address it.
func validateTargetURLs(cfg *Config) error {
	for name, tc := range cfg.MCPServers {
		if tc == nil {
			return fmt.Errorf("llmgateway: mcpServers %q: config must not be nil", name)
		}
		if err := validateConfigName("mcpServers", name); err != nil {
			return err
		}
		if err := validateTargetURL(tc.URL); err != nil {
			return fmt.Errorf("llmgateway: mcpServers %q: %w", name, err)
		}
	}
	for name, ac := range cfg.Agents {
		if ac == nil {
			return fmt.Errorf("llmgateway: agents %q: config must not be nil", name)
		}
		if err := validateConfigName("agents", name); err != nil {
			return err
		}
		if err := validateTargetURL(ac.URL); err != nil {
			return fmt.Errorf("llmgateway: agents %q: %w", name, err)
		}
		// F8, review round 3, 2026-09: handleAgents builds the card URL as
		// "/a2a/" + name + cardPath (its own doc comment) with no
		// separator of its own — a Card that does not start with "/"
		// (e.g. "agent.json") concatenates straight onto the agent name
		// instead of starting a new path segment, producing something
		// like "/a2a/myagentagent.json": a 404, or a completely different
		// route, never the card the operator configured. Empty (the
		// default, defaultAgentCardPath applies) is always valid.
		if ac.Card != "" && !strings.HasPrefix(ac.Card, "/") {
			return fmt.Errorf("llmgateway: agents %q: card %q must start with \"/\"", name, ac.Card)
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
