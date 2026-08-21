package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// federatedMCPPath is the bare "/mcp" route this file implements —
// distinct from the per-server "/mcp/{name}/..." target proxy
// (targetRoute/handleTargetProxy, mcp_a2a.go), which rejects a bare "/mcp"
// or "/mcp/" outright (targetRoute's own doc comment: "carries no target
// name to route to"). Two real consumers (pugbot's mcpclient, agentkit)
// were built against the earlier, pre-plugin gateway's single aggregated
// MCP endpoint and reach every configured MCP server through ONE URL —
// this route restores that shape on top of the plugin's per-server
// config, rather than forcing those consumers into a lossy
// single-server-at-a-time workaround.
const federatedMCPPath = "/mcp"

// JSON-RPC 2.0 envelope fields and standard error codes (jsonrpc.org's
// spec §5.1) this file's own responses use. MCP's wire format is JSON-RPC
// 2.0 verbatim — no plugin-specific envelope on top.
const (
	jsonrpcVersion        = "2.0"
	jsonrpcParseError     = -32700
	jsonrpcInvalidParams  = -32602
	jsonrpcMethodNotFound = -32601
	jsonrpcInternalError  = -32603
)

// jsonrpcRequest is one JSON-RPC 2.0 request/notification, decoded from
// the client's POST body and re-encoded (with a synthetic id) for this
// gateway's own outbound calls to a backend MCP server (mcpBackendCall).
// ID is nil for a notification (no "id" key in the wire form) — the
// signal notifications/* dispatch checks for a response-shaped request.
// Only a single JSON-RPC object is supported, never a batch array: MCP's
// own Streamable HTTP transport is the target wire format here, and
// nothing in this round's brief calls for JSON-RPC batch support.
type jsonrpcRequest struct {
	Params  json.RawMessage `json:"params,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// jsonrpcError is a JSON-RPC 2.0 error object.
type jsonrpcError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// jsonrpcResponse is one JSON-RPC 2.0 response, used both to decode a
// backend MCP server's reply (mcpBackendCall) and to encode this
// gateway's own reply to the client — Result is left as json.RawMessage
// deliberately (never `any`) so a backend's raw result can be relayed to
// the client verbatim (mcpFederatedToolsCall) without a decode-then-
// re-encode round trip, and so building a fresh result (mcpFederatedInitialize,
// mcpFederatedToolsList) is one explicit json.Marshal away rather than
// implicit. ID has no "omitempty": JSON-RPC 2.0 requires an "id" key on
// every non-notification response, explicitly null when the request's own
// id could not even be determined (a parse error) — omitting the key
// entirely in that case would be spec-incorrect, not just unusual.
type jsonrpcResponse struct {
	Error   *jsonrpcError   `json:"error,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// writeJSONRPCEnvelope writes resp as the response body, matching
// setAdminJSONHeaders' own "declared Content-Type, nothing else asserted"
// minimalism — a JSON-RPC error is still carried at HTTP 200 (JSON-RPC
// errors are a payload-level concept, not a transport-level one; every
// genuinely transport-level failure this route can hit — an unreadable
// body, an unauthenticated/forbidden caller, a rate limit — is handled
// before this function is ever reached, via the gateway's own
// writeOAIError/writeLimitViolation envelope, matching every other route
// in this package).
func writeJSONRPCEnvelope(w http.ResponseWriter, resp jsonrpcResponse) {
	resp.JSONRPC = jsonrpcVersion
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // headers already committed; nothing useful to do on encode failure
}

// writeJSONRPCResult marshals result and writes it as a successful
// JSON-RPC response under id. A marshal failure (result is not
// JSON-marshalable — never true for any value this file actually passes,
// but defensively handled rather than panicking) reports itself as a
// JSON-RPC internal error instead.
func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		writeJSONRPCErrorResponse(w, id, jsonrpcInternalError, "failed to encode result")
		return
	}
	writeJSONRPCEnvelope(w, jsonrpcResponse{ID: id, Result: resultBytes})
}

// writeJSONRPCErrorResponse writes a JSON-RPC error response under id.
func writeJSONRPCErrorResponse(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSONRPCEnvelope(w, jsonrpcResponse{ID: id, Error: &jsonrpcError{Code: code, Message: message}})
}

// allowedMCPServerNames returns the sorted names of every MCP server
// grp.allowsMCP permits — the SAME per-server authorization check
// handleTargetProxy enforces (mcp_a2a.go), applied here across the whole
// catalog: tools/list aggregation only ever fans out to these names,
// and tools/call prefix resolution (resolveFederatedTool) only ever
// searches them — so a group restricted to N servers can see or invoke
// only those N servers' tools through POST /mcp too, exactly as it can
// only reach them through POST /mcp/{name} directly.
func allowedMCPServerNames(cfg *Config, grp *group) []string {
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		if grp.allowsMCP(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// handleMCPFederated implements POST /mcp (federatedMCPPath): the
// federated JSON-RPC endpoint this file's own doc comment explains the
// need for.
//
// Unlike handleTargetProxy, this is not a byte-level reverse proxy — it
// cannot be: aggregating N servers' tools/list results into one response,
// or routing one tools/call to the single right server, both require
// actually parsing the JSON-RPC envelope, which a blind proxy never does.
// The gateway terminates the client's JSON-RPC exchange itself: it
// answers "initialize" and "ping" locally (mcpFederatedInitialize; ping
// needs no backend at all) and, for "tools/list"/"tools/call", issues its
// OWN outbound JSON-RPC call(s) to the relevant backend server(s) as part
// of handling THIS one incoming request — no session state is ever
// persisted across separate client requests (mcpBackendCall's own doc
// comment), matching how the per-server proxy today carries no
// gateway-side session state of its own either (it is a pure byte pipe;
// whatever MCP session semantics exist are negotiated directly between
// the client and that one backend, via headers proxyUpstream forwards
// unmodified — see handleTargetProxy, mcp_a2a.go). A response can never be
// a raw byte relay here, even for a single-target call (tools/call): every
// outbound backend call carries its own synthetic id (mcpBackendCall), so
// the client's own id must always be decoded and re-attached on the way
// back out — this is the concrete reason "relay the response" cannot mean
// what it means for handleTargetProxy.
//
// checkAndCount runs once per incoming request, before method dispatch —
// covering the caller's own user/group/total scopes exactly like every
// other metered route — with per-target attribution (countTargetRequest)
// added separately, per backend server actually contacted, inside
// mcpFederatedToolsList/mcpFederatedToolsCall: a single tools/list call
// may contact several servers, so "one request in, one target scope
// incremented" does not hold here the way it does for handleTargetProxy.
func (g *Gateway) handleMCPFederated(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}

	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// A malformed envelope carries no reliable id at all — JSON-RPC
		// 2.0 §5.1 requires "id":null for exactly this case, not an
		// omitted key (jsonrpcResponse.ID's own doc comment).
		writeJSONRPCErrorResponse(w, json.RawMessage("null"), jsonrpcParseError, "parse error")
		return
	}

	scopes := withTotalScope(buildLimitScopes(u, grp))
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		writeLimitViolation(w, violation)
		return
	}

	switch {
	case req.Method == "initialize":
		g.mcpFederatedInitialize(w, req)
	case req.Method == "ping":
		writeJSONRPCResult(w, req.ID, map[string]any{})
	case req.Method == "tools/list":
		g.mcpFederatedToolsList(w, r, req, grp)
	case req.Method == "tools/call":
		g.mcpFederatedToolsCall(w, r, req, grp)
	case strings.HasPrefix(req.Method, "notifications/"):
		// A JSON-RPC notification carries no id and gets no response body
		// by definition; MCP's Streamable HTTP transport answers a
		// notification POST with 202 Accepted and nothing else.
		w.WriteHeader(http.StatusAccepted)
	default:
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcMethodNotFound, "method not found: "+req.Method)
	}
}

// mcpInitializeParams is the one field of MCP's "initialize" request
// params this gateway reads.
type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// defaultMCPProtocolVersion is reported only when the caller's own
// "initialize" request omits protocolVersion entirely. This is a
// fallback default this plugin picks for its own local response, not a
// verified compatibility claim about any downstream MCP server's
// supported version — see mcpFederatedInitialize's own doc comment for
// why the caller's own value is preferred whenever it sends one.
const defaultMCPProtocolVersion = "2025-06-18"

// mcpFederatedInitialize answers "initialize" locally — no backend server
// is ever contacted for it (handleMCPFederated's own doc comment).
// protocolVersion echoes the caller's own requested value when present:
// version negotiation is the caller's choice to make, and echoing it back
// avoids this plugin hardcoding a claim about which dated MCP protocol
// revision it actually tracks. capabilities.tools is present (empty
// object, no sub-fields) to declare tool-calling support without
// overclaiming listChanged notifications this gateway never sends.
func (g *Gateway) mcpFederatedInitialize(w http.ResponseWriter, req jsonrpcRequest) {
	var params mcpInitializeParams
	_ = json.Unmarshal(req.Params, &params) // best-effort; empty/malformed params falls back to defaultMCPProtocolVersion below

	protocolVersion := params.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = defaultMCPProtocolVersion
	}

	writeJSONRPCResult(w, req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "traefik-llmgateway",
			"version": pluginVersion,
		},
	})
}

// mcpTool is one tool entry in an MCP "tools/list" result, either as read
// back from a backend server's own response or as re-emitted (name
// prefixed) in this gateway's federated aggregate.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// mcpToolsListResult is the "result" object of an MCP "tools/list"
// response, both a backend's own and this gateway's federated one.
type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

// mcpBackendCall issues one JSON-RPC 2.0 request to targetURL — an MCP
// server's own configured base URL, the same single endpoint
// handleTargetProxy reverse-proxies every method to — and returns its
// parsed response. It carries a synthetic id ("federated"), never the
// client's own: tools/list may issue this call several times over for one
// incoming client request (once per allowed server), so reusing the
// client's id across N independent outbound calls would be meaningless,
// and the id on the way back is discarded by every caller of this
// function either way — only Result/Error is read.
//
// No outbound "initialize" handshake precedes this call, and none of its
// own session/cookie state is cached across separate mcpBackendCall
// invocations — deliberately: per-request, throwaway upstream calls are
// what "keep upstream sessions per-request" (handleMCPFederated's own doc
// comment) means in practice here, matching the per-server proxy's own
// complete absence of gateway-side session tracking. A backend MCP server
// that hard-requires a prior initialize handshake before honoring
// tools/list or tools/call will reject this call; that is a real,
// documented trade-off of this design (see README's federation section),
// not an oversight.
//
// A backend may answer either a bare JSON object (Content-Type:
// application/json) or an MCP Streamable HTTP SSE stream (Content-Type:
// text/event-stream) carrying one or more "data:" events — both are
// accepted (parseBackendJSONRPC).
func (g *Gateway) mcpBackendCall(ctx context.Context, targetURL, method string, params any) (*jsonrpcResponse, error) {
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(jsonrpcRequest{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`"federated"`), Method: method, Params: paramsRaw})
	if err != nil {
		return nil, err
	}

	// gosec G704 (SSRF via taint analysis): targetURL is an
	// operator-configured MCP server base (Config.MCPServers[name].URL,
	// validated at construction by validateTargetURLs) — no client-
	// supplied path segment or query ever reaches this call, unlike
	// proxyUpstream's own upstreamURL (routes_passthrough.go), which
	// deliberately does forward client-supplied path material and
	// documents that same taint-analysis false positive at its own call
	// site.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBody)) //nolint:gosec // operator-configured target URL, no client-supplied path/query
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := g.targetClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBytes))
	if err != nil {
		return nil, err
	}
	return parseBackendJSONRPC(resp.Header.Get("Content-Type"), respBody)
}

// parseBackendJSONRPC decodes body as a backend MCP server's JSON-RPC
// response, accepting either shape the MCP Streamable HTTP transport
// allows a POST response to take: a bare JSON object (any other
// Content-Type is treated as this default case), or an SSE stream
// (Content-Type: text/event-stream) of one or more "data:" lines — the
// LAST such line is parsed, matching a minimal backend that emits exactly
// one data event carrying its final response.
func parseBackendJSONRPC(contentType string, body []byte) (*jsonrpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		body = lastSSEDataLine(body)
	}
	var out jsonrpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// lastSSEDataLine returns the payload of the last "data:" line in an SSE
// body — body itself, unchanged, if it contains no such line at all (so
// the caller's own json.Unmarshal fails informatively on whatever body
// actually was, rather than this function silently swallowing a
// malformed stream into an empty byte slice).
func lastSSEDataLine(body []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			last = data
		} else if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(data)
		}
	}
	if last == nil {
		return body
	}
	return last
}

// mcpFederatedToolsList implements the federated "tools/list": it fans
// out one mcpBackendCall per server allowedMCPServerNames(grp) permits,
// concurrently, then merges every reachable server's own tools into one
// result — each tool's name prefixed "<serverName>_<toolName>", the exact
// underscore-separator convention the old agentgateway this plugin
// replaces used, and the one pugbot's mcpclient and agentkit have already
// persisted into their own tool-id databases (e.g.
// "brave-search_brave_web_search" — verified live). A server that errors,
// times out, or returns an unparsable result is logged and skipped, not
// surfaced as a whole-call failure: a federated aggregate degrading to
// "every OTHER server's tools" when one upstream is down is the useful
// behavior an aggregator should have, matching how a client would want a
// federated view to behave in practice.
//
// countTargetRequest attributes one request to EVERY server actually
// contacted here (handleMCPFederated's own doc comment) — a single
// federated tools/list call can move several targets' own counters, not
// just one.
func (g *Gateway) mcpFederatedToolsList(w http.ResponseWriter, r *http.Request, req jsonrpcRequest, grp *group) {
	names := allowedMCPServerNames(g.cfg, grp)

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		merged = make([]mcpTool, 0, len(names))
		failed []string
	)
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			targetURL := g.cfg.MCPServers[name].URL
			resp, err := g.mcpBackendCall(r.Context(), targetURL, "tools/list", struct{}{})
			g.limiter.countTargetRequest(targetKindMCP, name)

			mu.Lock()
			defer mu.Unlock()
			if err != nil || resp.Error != nil {
				failed = append(failed, name)
				return
			}
			var result mcpToolsListResult
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				failed = append(failed, name)
				return
			}
			for _, tool := range result.Tools {
				merged = append(merged, mcpTool{Name: name + "_" + tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
			}
		}(name)
	}
	wg.Wait()

	if len(failed) > 0 {
		sort.Strings(failed)
		g.logf("federated tools/list: %d of %d allowed MCP server(s) failed and were skipped: %v", len(failed), len(names), failed)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })

	writeJSONRPCResult(w, req.ID, mcpToolsListResult{Tools: merged})
}

// mcpToolCallParams is one "tools/call" request's params: name is the
// caller-facing, server-prefixed tool id federation itself minted in
// tools/list ("<serverName>_<toolName>").
type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// resolveFederatedTool finds which of allowedNames is fullName's own
// server prefix, matched by LONGEST "<serverName>_" prefix — disambiguates
// two configured, allowed servers whose names are themselves prefixes of
// one another (e.g. both "foo" and "foo_bar" configured: a tool id
// "foo_bar_lookup" must resolve to server "foo_bar"'s tool "lookup", not
// server "foo"'s tool "bar_lookup"). ok is false when no allowed server's
// prefix matches at all — including a tool that belongs to a real,
// configured server the caller's group simply cannot reach, since
// allowedNames (allowedMCPServerNames) already excludes it; this is what
// makes tools/call authz-aware without a second, separate access check.
func resolveFederatedTool(fullName string, allowedNames []string) (serverName, toolName string, ok bool) {
	best := ""
	for _, name := range allowedNames {
		if strings.HasPrefix(fullName, name+"_") && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, strings.TrimPrefix(fullName, best+"_"), true
}

// mcpFederatedToolsCall resolves req's "name" param to one allowed MCP
// server (resolveFederatedTool), forwards the call there with the server
// prefix stripped, and relays its result or error back to the client
// under the CLIENT's own id — never the synthetic one mcpBackendCall's
// outbound call used. An unresolvable prefix is a JSON-RPC invalid-params
// error, not an HTTP 404: the request reached a real method on a real
// route, it just named a tool nothing federation could route (an
// unconfigured server, or one this caller's group cannot reach).
func (g *Gateway) mcpFederatedToolsCall(w http.ResponseWriter, r *http.Request, req jsonrpcRequest, grp *group) {
	var params mcpToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInvalidParams, "invalid params")
		return
	}

	serverName, toolName, ok := resolveFederatedTool(params.Name, allowedMCPServerNames(g.cfg, grp))
	if !ok {
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInvalidParams, "unknown tool: "+params.Name)
		return
	}

	targetURL := g.cfg.MCPServers[serverName].URL
	resp, err := g.mcpBackendCall(r.Context(), targetURL, "tools/call", mcpToolCallParams{Name: toolName, Arguments: params.Arguments})
	g.limiter.countTargetRequest(targetKindMCP, serverName)
	if err != nil {
		g.logf("federated tools/call: server %q: %v", serverName, err)
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInternalError, "upstream error")
		return
	}
	writeJSONRPCEnvelope(w, jsonrpcResponse{ID: req.ID, Result: resp.Result, Error: resp.Error})
}
