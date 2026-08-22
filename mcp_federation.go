package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
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

// mcpSessionHeader is the MCP Streamable HTTP transport's session-id
// header — set by a backend server's "initialize" response to open a
// session, and echoed by a client (this gateway, on the handshake-fallback
// retry) on every subsequent call that session covers. See
// mcpBackendCall's own doc comment for when this gateway ever sends one.
const mcpSessionHeader = "Mcp-Session-Id"

// Per-request budgets a federated call to a single backend server is
// allowed: the WHOLE per-server operation — a bare attempt, and, only when
// that signals a session is required, one initialize + one retry + a
// best-effort session close — must fit inside this window, not each
// individual HTTP request separately (mcpBackendCall's own doc comment).
// tools/list's fan-out runs one of these per allowed server, concurrently,
// so the shorter budget bounds a single client request's worst case to
// toolsListBackendTimeout regardless of how many servers are configured;
// tools/call talks to exactly one server and gets the longer budget a
// slower tool invocation (a web search, a browser action) may need.
const (
	toolsListBackendTimeout = 20 * time.Second
	toolsCallBackendTimeout = 120 * time.Second
	// mcpSessionCloseTimeout bounds the best-effort DELETE that ends a
	// handshake-fallback session (mcpBackendCall). It runs on its own
	// context.Background()-derived deadline, deliberately NOT the parent
	// call's own (possibly already near-exhausted) toolsList/
	// toolsCallBackendTimeout budget: cleanup must never race, or get
	// starved by, the real call it is cleaning up after.
	mcpSessionCloseTimeout = 5 * time.Second
)

// mcpBackendResponseMaxBytes caps a single backend MCP server's HTTP
// response body doBackendJSONRPC will read into memory — previously
// maxRequestBytes (10MiB, routes_unified.go), a budget sized for a
// CLIENT's own chat/embeddings request body, not a backend's JSON-RPC
// reply (security audit finding 1c, 2026-08-22). 4MiB matches this
// codebase's other "generous but bounded" read caps for a non-primary-
// request-body read (cache.go's maxAccountingTeeBytes is also 4MiB) —
// comfortably above any tools/list schema listing or tools/call result
// this gateway has actually seen live (typically low KB), while bounding
// mcpFederatedFanoutConcurrency backends' worst-case concurrent memory to
// 4 × 4MiB = 16MiB, against the unbounded ~110MiB (11 production servers
// × up to 10MiB each, all fired at once) the security audit measured
// before this fix.
const mcpBackendResponseMaxBytes = 4 << 20

// jsonrpcRequest is one JSON-RPC 2.0 request/notification, decoded from
// the client's POST body and re-encoded (with a synthetic id) for this
// gateway's own outbound calls to a backend MCP server (mcpBackendCall).
// ID is nil for a notification (no "id" key in the wire form) — the
// signal notifications/* dispatch checks for a response-shaped request.
// Only a single JSON-RPC object is supported, never a batch array: MCP's
// own Streamable HTTP transport is the target wire format here, and
// nothing in this round's brief calls for JSON-RPC batch support.
//
// JSONRPC itself is decoded but never validated against the literal
// string "2.0" — deliberately permissive: real MCP clients this gateway
// has actually seen (and the mock backends this repo's own tests drive)
// never send anything else, rejecting a request purely for carrying a
// different (or absent) "jsonrpc" field would reject a client this
// gateway can otherwise serve correctly for a purity check no consumer
// has ever needed, and the field is not read for any routing or safety
// decision this file makes.
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
// body, an unauthenticated/forbidden caller, a rate limit, a disallowed
// HTTP method — is handled before this function is ever reached, via the
// gateway's own writeOAIError/writeLimitViolation envelope, matching
// every other route in this package).
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

// isLegacySSETransportURL reports whether rawURL's path ends in "/sse" —
// the MCP prior HTTP+SSE transport's own URL convention (superseded by
// Streamable HTTP), where the CLIENT — not an intermediary — must drive a
// persistent SSE session: a GET establishing the event stream, POSTs
// correlated against a server-assigned endpoint the stream itself
// announces. mcpBackendCall's simple "POST a JSON-RPC object, read one
// response" model cannot speak that transport at all; there is no
// handshake or retry that fixes this, unlike the session-required case
// MF3 handles. Verified live against production traffic (review round 2,
// 2026-08-21): a real server on this transport (URL ending "/sse")
// answers a Streamable-HTTP-shaped POST with HTTP 400 "Missing sessionId"
// — a different failure MODE than the session-required 4xx/JSON-RPC-error
// case mcpBackendCall's handshake retry targets, and one no retry of any
// kind resolves. Such a server is excluded from federation's tools/list
// fan-out and tools/call resolution (allowedMCPServerNames) before ever
// reaching mcpBackendCall — it stays fully reachable via the per-server
// proxy (/mcp/{name}/...), where the real client drives the transport
// end to end, exactly as it always has.
//
// A rawURL that fails to parse returns false (not excluded) rather than
// panicking or erroring: every configured target URL already passed
// validateTargetURLs at construction (mcp_a2a.go), so this branch is
// unreachable in practice, and "don't exclude" is the safe default for an
// unreachable branch — the same convention sanitizeBaseURL applies to its
// own unparsable-URL case (admin.go).
func isLegacySSETransportURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Path, "/sse")
}

// allowedMCPServerNames returns the sorted names of every MCP server BOTH
// grp.allowsMCP permits AND that speaks a transport federation can
// actually use (isLegacySSETransportURL excludes the rest) — the SAME
// per-server authorization check handleTargetProxy enforces (mcp_a2a.go),
// applied here across the whole catalog: tools/list aggregation only ever
// fans out to these names, and tools/call prefix resolution
// (resolveFederatedTool) only ever searches them — so a group restricted
// to N servers can see or invoke only those N servers' tools through
// POST /mcp too, exactly as it can only reach them through POST
// /mcp/{name} directly, and a legacy-SSE-transport server is invisible to
// federation regardless of group access (it is not a federation-eligible
// server for ANY caller, not an authorization decision).
func allowedMCPServerNames(cfg *Config, grp *group) []string {
	names := make([]string, 0, len(cfg.MCPServers))
	for name, tc := range cfg.MCPServers {
		if grp.allowsMCP(name) && !isLegacySSETransportURL(tc.URL) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// handleMCPFederated implements POST /mcp (federatedMCPPath): the
// federated JSON-RPC endpoint this file's own doc comment explains the
// need for. ServeHTTP (llmgateway.go) answers any OTHER method on this
// exact path with 405 (Allow: POST) before this function is ever called.
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
// persisted across separate CLIENT requests (mcpBackendCall's own doc
// comment covers the one-call, per-backend-server session a handshake
// fallback DOES open and close, entirely inside a single incoming
// request), matching how the per-server proxy today carries no
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
// other metered route — with per-target attribution added separately
// inside mcpFederatedToolsList/mcpFederatedToolsCall (their own doc
// comments cover exactly what "attributed" means for each).
//
// The weight charged to checkAndCount is 1 for every method EXCEPT
// tools/list (security audit finding 1b, 2026-08-22): a tools/list call
// fans out to every server allowedMCPServerNames(grp) returns — up to the
// whole MCP catalog, concurrently (mcpFederatedToolsList's own doc
// comment) — so one incoming client request can cost this gateway many
// real outbound backend calls. Charging it as a single request against
// the caller's requests-per-minute/requests-per-day budget undercounts
// that real cost; weighting it by len(names) instead makes the budget
// reflect what the request actually does. This is computed and charged
// exactly once, right here, before dispatch — never doubled by anything
// mcpFederatedToolsList itself does (its own countTargetRequests call is a
// SEPARATE scope kind, "mcp"/target-id, entirely disjoint from the
// user/group/total scopes checkAndCountWeighted evaluates here; see
// countTargetRequests' own doc comment, limits.go). tools/call, by
// contrast, always resolves to exactly ONE backend (resolveFederatedTool),
// so it keeps the plain weight of 1 — checkAndCountWeighted(scopes, 1) is
// byte-for-byte checkAndCount's own existing behavior.
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

	var toolsListNames []string
	weight := int64(1)
	if req.Method == "tools/list" {
		toolsListNames = allowedMCPServerNames(g.cfg, grp)
		if n := int64(len(toolsListNames)); n > weight {
			weight = n
		}
	}

	if violation := g.limiter.checkAndCountWeighted(scopes, weight); violation != nil {
		writeLimitViolation(w, violation)
		return
	}

	switch {
	case req.Method == "initialize":
		g.mcpFederatedInitialize(w, req)
	case req.Method == "ping":
		writeJSONRPCResult(w, req.ID, map[string]any{})
	case req.Method == "tools/list":
		g.mcpFederatedToolsList(w, r, req, toolsListNames)
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

// defaultMCPProtocolVersion is reported when the caller's own "initialize"
// request omits protocolVersion, or names one outside
// knownMCPProtocolVersions. This is a fallback default this plugin picks
// for its own local response, not a verified compatibility claim about any
// downstream MCP server's supported version — see
// mcpFederatedInitialize's own doc comment for why the caller's own value
// is preferred whenever it names a recognized one.
const defaultMCPProtocolVersion = "2025-11-25"

// knownMCPProtocolVersions is the small, explicit allowlist of protocol
// revisions mcpFederatedInitialize will ever echo back verbatim — every
// dated revision this plugin's own reading of the MCP spec's version
// history recognizes at the time this constant was last reviewed.
// Echoing an ARBITRARY caller-supplied string back as this gateway's own
// declared protocolVersion would let a malformed or malicious client make
// this response claim compatibility with a revision this plugin has never
// actually been evaluated against; the allowlist keeps the "echo the
// caller's choice" behavior (mcpFederatedInitialize's own doc comment)
// bounded to values this plugin actually knows about, falling back to
// defaultMCPProtocolVersion for anything else — including a genuinely
// newer, valid revision this constant simply predates; that is a
// known, accepted staleness risk of hardcoding a version list at all,
// not a correctness bug.
var knownMCPProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

// mcpFederatedInitialize answers "initialize" locally — no backend server
// is ever contacted for it (handleMCPFederated's own doc comment).
// protocolVersion echoes the caller's own requested value when it names a
// revision in knownMCPProtocolVersions: version negotiation is normally
// the caller's choice to make, but echoing an arbitrary unrecognized
// string back as this gateway's own declared capability would overclaim
// compatibility this plugin cannot back up (knownMCPProtocolVersions' own
// doc comment) — falling back to defaultMCPProtocolVersion in that case,
// and whenever the caller omits protocolVersion entirely.
// capabilities.tools is present (empty object, no sub-fields) to declare
// tool-calling support without overclaiming listChanged notifications
// this gateway never sends.
func (g *Gateway) mcpFederatedInitialize(w http.ResponseWriter, req jsonrpcRequest) {
	var params mcpInitializeParams
	_ = json.Unmarshal(req.Params, &params) // best-effort; empty/malformed params falls back to defaultMCPProtocolVersion below

	protocolVersion := defaultMCPProtocolVersion
	if knownMCPProtocolVersions[params.ProtocolVersion] {
		protocolVersion = params.ProtocolVersion
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

// doBackendJSONRPC issues one JSON-RPC 2.0 POST to targetURL — an MCP
// server's own configured base URL, the same single endpoint
// handleTargetProxy reverse-proxies every method to — carrying sessionID
// as the Mcp-Session-Id request header when non-empty. It is the one place
// that actually builds and sends an HTTP request for this file; every
// other function in the handshake-fallback machinery (mcpBackendCall,
// mcpBackendHandshake, mcpBackendCloseSession's own DELETE aside) goes
// through this.
//
// Return values: resp is nil whenever err is non-nil. status is the
// backend's real HTTP status when one was received at all (0 for a
// request-build or network failure that never got a response), reported
// even alongside a non-nil err — mcpBackendCall's own handshake-fallback
// decision (a JSON-RPC error OR an HTTP 4xx) needs the status specifically
// in the case where a non-2xx response has no valid JSON-RPC body at all
// (err is then the "upstream returned HTTP %d" error below, not a
// parse/validity error). respSessionID is the backend's own
// Mcp-Session-Id RESPONSE header — read on every call, not only
// "initialize", so mcpBackendHandshake needs no special-cased second
// code path for the one call that actually cares about it.
//
// A non-2xx status is always an error (MF2, review round 2, 2026-08-21):
// the earlier version of this file had no such check at all, which let a
// non-JSON-RPC-shaped error page (a plain-text 500, an nginx 502 HTML
// page) fall through to parseBackendJSONRPC and fail there anyway, but
// only by accident of that body not happening to be valid JSON — a body
// that WAS valid JSON but not a real JSON-RPC response (see
// parseBackendJSONRPC's own validity check) would have silently produced
// an all-zero-fields response otherwise.
func (g *Gateway) doBackendJSONRPC(ctx context.Context, targetURL, method string, params any, sessionID string) (resp *jsonrpcResponse, status int, respSessionID string, err error) {
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return nil, 0, "", err
	}
	reqBody, err := json.Marshal(jsonrpcRequest{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`"federated"`), Method: method, Params: paramsRaw})
	if err != nil {
		return nil, 0, "", err
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
		return nil, 0, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set(mcpSessionHeader, sessionID)
	}

	httpResp, err := g.targetClient.Do(httpReq)
	if err != nil {
		return nil, 0, "", err
	}
	defer httpResp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	status = httpResp.StatusCode
	respSessionID = httpResp.Header.Get(mcpSessionHeader)

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, mcpBackendResponseMaxBytes))
	if err != nil {
		return nil, status, respSessionID, err
	}
	if status < 200 || status >= 300 {
		return nil, status, respSessionID, fmt.Errorf("upstream returned HTTP %d", status)
	}

	parsed, err := parseBackendJSONRPC(httpResp.Header.Get("Content-Type"), respBody)
	if err != nil {
		return nil, status, respSessionID, err
	}
	return parsed, status, respSessionID, nil
}

// mcpBackendHandshake issues a fresh, session-less "initialize" call
// against targetURL — minimal clientInfo, this gateway's own default
// protocol version — for mcpBackendCall's handshake-fallback retry.
// sessionID is "" when the backend's response carries no Mcp-Session-Id
// header of its own (a legitimate, spec-permitted case for a server that
// turns out to be stateless-tolerant after all); err is non-nil only when
// the initialize call itself failed outright (network error, non-2xx, a
// JSON-RPC error response to initialize itself) — there is no retry of a
// failed handshake.
func (g *Gateway) mcpBackendHandshake(ctx context.Context, targetURL string) (sessionID string, err error) {
	initParams := map[string]any{
		"protocolVersion": defaultMCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "traefik-llmgateway", "version": pluginVersion},
	}
	resp, _, respSessionID, err := g.doBackendJSONRPC(ctx, targetURL, "initialize", initParams, "")
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("initialize: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	return respSessionID, nil
}

// mcpBackendCloseSession best-effort DELETEs targetURL with sessionID
// attached (the MCP Streamable HTTP transport's own session-end
// convention) — failure is logged, never surfaced: this runs after the
// caller's own real response has already been read and returned, so
// nothing downstream is waiting on it, and a backend that does not support
// session termination at all (silently ignoring the DELETE, or answering
// 404/405 for it) must never turn into a federation-visible error over
// pure cleanup.
func (g *Gateway) mcpBackendCloseSession(ctx context.Context, targetURL, sessionID string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, targetURL, nil) //nolint:gosec // same operator-configured target URL as doBackendJSONRPC
	if err != nil {
		return
	}
	req.Header.Set(mcpSessionHeader, sessionID)

	resp, err := g.targetClient.Do(req)
	if err != nil {
		g.logf("federated mcp: best-effort session close failed for %q: %v", targetURL, err)
		return
	}
	_ = resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure
}

// mcpBackendCall issues a JSON-RPC 2.0 request for method to targetURL —
// bare first, matching the per-request "no persisted CLIENT session"
// design (handleMCPFederated's own doc comment) — and falls back to a
// ONE-SHOT initialize/retry/close handshake ONLY when the bare attempt
// signals the server actually requires a session: a JSON-RPC-level error
// response, or an HTTP 4xx status.
//
// This ruling is backed by a live probe against all 11 production servers
// behind this gateway (review round 2, 2026-08-21): 8 answer a bare,
// session-less call outright (brave-search, captcha-solver, google-maps,
// lightpanda, time, weather, web-search, wikipedia); readitall and fetch
// both reject the bare call — readitall: "method tools/list is invalid
// during..."; fetch: -32602 with or without params — and need exactly
// this handshake to work; a third class (a legacy HTTP+SSE-transport
// server, verified as playwright, HTTP 400 "Missing sessionId") is
// excluded from federation entirely before ever reaching this function
// (isLegacySSETransportURL, allowedMCPServerNames), since no handshake
// fixes a transport this function was never built to speak.
//
// A network failure, timeout, or 5xx never triggers the fallback:
// retrying those would spend a second timeout budget against a server
// that was never going to answer either way, bare or not — only a
// response that specifically signals "you're missing a session" is worth
// a second attempt. ctx governs the WHOLE operation (bare attempt,
// optional handshake, optional retry, best-effort close all share it,
// except the close itself — see mcpSessionCloseTimeout's own doc
// comment); callers derive it from context.WithTimeout(r.Context(), ...)
// with toolsListBackendTimeout or toolsCallBackendTimeout (MF1, review
// round 2, 2026-08-21).
func (g *Gateway) mcpBackendCall(ctx context.Context, targetURL, method string, params any) (*jsonrpcResponse, error) {
	resp, status, _, err := g.doBackendJSONRPC(ctx, targetURL, method, params, "")
	if err == nil && resp.Error == nil {
		return resp, nil
	}

	retryEligible := (err == nil && resp.Error != nil) || (status >= 400 && status < 500)
	if !retryEligible {
		return nil, err
	}

	sessionID, initErr := g.mcpBackendHandshake(ctx, targetURL)
	if initErr != nil {
		// The handshake itself failed: surface the bare attempt's own
		// outcome when it has one (a JSON-RPC error response is more
		// informative to a caller than a generic "handshake failed"),
		// falling back to initErr only when the bare attempt produced no
		// usable response at all (its own err was non-nil — the HTTP-4xx-
		// with-no-JSON-RPC-body case).
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	if sessionID != "" {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), mcpSessionCloseTimeout)
			defer cancel()
			g.mcpBackendCloseSession(closeCtx, targetURL, sessionID)
		}()
	}

	retryResp, _, _, retryErr := g.doBackendJSONRPC(ctx, targetURL, method, params, sessionID)
	if retryErr != nil {
		return nil, retryErr
	}
	return retryResp, nil
}

// parseBackendJSONRPC decodes body as a backend MCP server's JSON-RPC
// response, accepting either shape the MCP Streamable HTTP transport
// allows a POST response to take: a bare JSON object (any other
// Content-Type is treated as this default case), or an SSE stream
// (Content-Type: text/event-stream) of one or more "data:" lines — the
// LAST such line is parsed (lastSSEDataLine's own doc comment covers what
// this does and does not correlate).
//
// A body that parses as valid JSON but carries NEITHER "result" NOR
// "error" is also rejected (MF2, review round 2, 2026-08-21): a backend
// answering with some other JSON shape entirely (a plain `{}`, a
// non-compliant `{"status":"ok"}`) would otherwise decode into a
// jsonrpcResponse with every field at its zero value and LOOK like a
// legitimate, empty-but-successful JSON-RPC response — silently producing
// a client-visible `{"jsonrpc":"2.0","id":1}` with neither a result to
// read nor an error to report. Every caller of this function treats the
// returned error identically to a network failure (fails the call,
// mcpFederatedToolsList skips the server rather than crediting it with an
// empty success), so this is caught here once rather than at every call
// site.
func parseBackendJSONRPC(contentType string, body []byte) (*jsonrpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		body = lastSSEDataLine(body)
	}
	var out jsonrpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if out.Result == nil && out.Error == nil {
		return nil, fmt.Errorf("invalid JSON-RPC response: neither result nor error present")
	}
	return &out, nil
}

// lastSSEDataLine returns the payload of the last "data:" line in an SSE
// body — body itself, unchanged, if it contains no such line at all (so
// the caller's own json.Unmarshal fails informatively on whatever body
// actually was, rather than this function silently swallowing a
// malformed stream into an empty byte slice).
//
// This does NOT implement per-request id correlation across multiple
// concurrent SSE-delivered messages the way a real MCP client's
// Streamable-HTTP transport layer would (matching a specific response to
// the request that triggered it by id, ignoring unrelated
// server-initiated notifications interleaved on the same stream) — it
// takes the textually LAST "data:" line, full stop. This is a deliberate,
// documented limitation, acceptable for mcpBackendCall's own one-request-
// in-flight-at-a-time usage of this stream (it never has two outstanding
// calls sharing one SSE response to correlate between): a backend that
// interleaves an unrelated notification AFTER its real response on the
// same stream would be misread by this function. No production server
// probed for this round exhibited that behavior.
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

// mcpFederatedFanoutConcurrency bounds how many of names' backend servers
// mcpFederatedToolsList's own fan-out below contacts at once — a
// buffered-channel semaphore, the same bounded-concurrency shape
// limiter.spawnTokens already uses for its own goroutine fan-out
// (limits.go), applied here to gate each goroutine's actual outbound HTTP
// work rather than its launch. Before this bound (security audit finding
// 1a, 2026-08-22), one incoming tools/list request spawned one goroutine
// per allowed server with NO cap at all: with the 11 production servers,
// one client call could hold up to 11 concurrent connections and up to
// 11× mcpBackendResponseMaxBytes of backend response bodies in memory at
// once, entirely outside this gateway's own admission control. 4 keeps
// every server eventually contacted — a goroutine blocks on the
// semaphore, it is never dropped or skipped — while capping the worst
// case to 4 concurrent backends' cost, not the whole catalog's.
const mcpFederatedFanoutConcurrency = 4

// mcpFederatedToolsList implements the federated "tools/list": it fans
// out one mcpBackendCall per server in names (handleMCPFederated's own
// allowedMCPServerNames(grp) call, made once there so the SAME set that
// was weighed into checkAndCountWeighted is the set actually contacted),
// at most mcpFederatedFanoutConcurrency at a time — each under its own
// toolsListBackendTimeout budget (context.WithTimeout off r.Context(),
// MF1) — then merges every reachable server's own tools into one result,
// each tool's name prefixed "<serverName>_<toolName>", the exact
// underscore-separator convention the old agentgateway this plugin
// replaces used, and the one pugbot's mcpclient and agentkit have already
// persisted into their own tool-id databases (e.g.
// "brave-search_brave_web_search" — verified live). A server that errors,
// times out, panics, or returns an unparsable/invalid result is logged
// and skipped, not surfaced as a whole-call failure — UNLESS every single
// attempted server failed, in which case an empty tools list would
// misleadingly look like "this caller's group has no MCP access" — see
// the loud-failure branch below.
//
// countTargetRequests (limits.go) attributes one request to EVERY server
// ATTEMPTED here, in ONE batched call after wg.Wait() — "attempted", not
// "reached" or "succeeded": a server this gateway dialed and got a
// response (or a timeout, a connection refusal, or a panic) from still had
// a real request sent to it and a real slot of this gateway's outbound
// capacity spent on it, which is what these counters exist to track. A
// single federated tools/list call can move several targets' own
// counters, not just one — unlike handleTargetProxy's always-exactly-
// one-target shape (mcp_a2a.go).
func (g *Gateway) mcpFederatedToolsList(w http.ResponseWriter, r *http.Request, req jsonrpcRequest, names []string) {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		merged = make([]mcpTool, 0, len(names))
		failed []string
	)
	sem := make(chan struct{}, mcpFederatedFanoutConcurrency)
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			// Unrecovered-panic guard (security audit finding 2,
			// 2026-08-22): this goroutine runs off the request's own
			// goroutine, so a panic here has no ServeHTTP caller to
			// unwind into and would crash the whole shared Traefik
			// process — the same reasoning limiter.spawn (limits.go) and
			// modelRegistry.refreshProvider/captureModelMetadata
			// (registry.go) already apply to their own off-request
			// goroutines. A panicking backend must degrade to "that
			// server failed", exactly like an HTTP error from it, never
			// a process-wide crash.
			defer func() {
				if rec := recover(); rec != nil {
					g.logf("federated tools/list: server %q panicked: %v", name, rec)
					mu.Lock()
					failed = append(failed, name)
					mu.Unlock()
				}
			}()

			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(r.Context(), toolsListBackendTimeout)
			defer cancel()
			targetURL := g.cfg.MCPServers[name].URL
			resp, err := g.mcpBackendCall(ctx, targetURL, "tools/list", struct{}{})

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

	if len(names) > 0 {
		attempted := make([]limitScope, len(names))
		for i, name := range names {
			attempted[i] = limitScope{kind: targetKindMCP, id: name}
		}
		g.limiter.countTargetRequests(attempted)
	}

	if len(failed) > 0 {
		sort.Strings(failed)
		g.logf("federated tools/list: %d of %d allowed MCP server(s) failed and were skipped: %v", len(failed), len(names), failed)
	}

	// Every attempted server failed: an empty tools list here would read
	// as "this caller's group has legitimate no-access", indistinguishable
	// from the case where names itself was empty (no allowed servers at
	// all — that IS a correct empty result, left alone below). A total
	// outage is a real infrastructure problem the caller needs to see as
	// one, not a silently degraded empty success (MF3, review round 2,
	// 2026-08-21).
	if len(names) > 0 && len(failed) == len(names) {
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInternalError, "no MCP server reachable")
		return
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
// configured server the caller's group simply cannot reach, or one that
// speaks a transport federation excludes entirely, since allowedNames
// (allowedMCPServerNames) already excludes both; this is what makes
// tools/call authz- and transport-aware without a second, separate check.
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
// server (resolveFederatedTool), forwards the call there — under its own
// toolsCallBackendTimeout budget (context.WithTimeout off r.Context(),
// MF1) — with the server prefix stripped, and relays its result or error
// back to the client under the CLIENT's own id — never the synthetic one
// mcpBackendCall's outbound call used. An unresolvable prefix is a
// JSON-RPC invalid-params error, not an HTTP 404: the request reached a
// real method on a real route, it just named a tool nothing federation
// could route (an unconfigured server, one this caller's group cannot
// reach, or one on a transport federation excludes entirely).
//
// countTargetRequest (limits.go) attributes exactly one request to the
// single resolved server, once the call was actually attempted against
// it — the same "attempted, not necessarily succeeded" accounting
// mcpFederatedToolsList's own doc comment explains for its own fan-out.
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

	ctx, cancel := context.WithTimeout(r.Context(), toolsCallBackendTimeout)
	defer cancel()
	targetURL := g.cfg.MCPServers[serverName].URL
	resp, err := g.mcpBackendCall(ctx, targetURL, "tools/call", mcpToolCallParams{Name: toolName, Arguments: params.Arguments})
	g.limiter.countTargetRequest(targetKindMCP, serverName)
	if err != nil {
		g.logf("federated tools/call: server %q: %v", serverName, err)
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInternalError, "upstream error")
		return
	}

	// Defensive final guard (MF2, review round 2, 2026-08-21): mcpBackendCall
	// can only return a nil-err resp whose Result/Error are both nil if some
	// future code path bypasses doBackendJSONRPC's own parseBackendJSONRPC
	// validity check (which already rejects this shape as an error) — this
	// is unreachable today, kept as the explicit guard the review round
	// asked for rather than trusting that invariant silently.
	if resp.Result == nil && resp.Error == nil {
		g.logf("federated tools/call: server %q returned a JSON-RPC response with neither result nor error", serverName)
		writeJSONRPCErrorResponse(w, req.ID, jsonrpcInternalError, "invalid upstream response")
		return
	}

	writeJSONRPCEnvelope(w, jsonrpcResponse{ID: req.ID, Result: resp.Result, Error: resp.Error})
}
