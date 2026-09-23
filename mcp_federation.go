package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// response body for the tools/list FAN-OUT path only (mcpFederatedToolsList)
// — previously maxRequestBytes (10MiB, routes_unified.go), a budget sized
// for a CLIENT's own chat/embeddings request body, not a backend's
// JSON-RPC reply (security audit finding 1c, 2026-08-22). 4MiB matches
// this codebase's other "generous but bounded" read caps for a non-
// primary-request-body read (cache.go's maxAccountingTeeBytes is also
// 4MiB) — measured at 547KiB for a real tools/list response describing
// 20 fat tools (security review round 2, 2026-08-22), comfortably below
// this cap — while bounding mcpFederatedFanoutConcurrency backends'
// worst-case concurrent memory to a small multiple of 4MiB, against the
// unbounded ~110MiB (11 production servers × up to 10MiB each, all fired
// at once) the security audit measured before this fix. This cap is
// deliberately NOT used for tools/call — see mcpBackendCallResponseMaxBytes,
// whose own doc comment covers why the fan-out memory argument does not
// apply there.
const mcpBackendResponseMaxBytes = 4 << 20

// mcpBackendCallResponseMaxBytes caps a single backend's response for the
// tools/call path (mcpFederatedToolsCall) — kept at maxRequestBytes
// (10MiB), the SAME budget this file used everywhere before finding 1c,
// deliberately NOT shrunk to mcpBackendResponseMaxBytes (security review
// round 2, 2026-08-22, important finding 4): tools/call always resolves
// to exactly ONE backend (resolveFederatedTool), so the fan-out memory-
// amplification argument that justifies the fan-out path's smaller cap
// does not hold here at all — a single tool result can legitimately be
// much larger than a tools/list schema listing (an extracted document, an
// image or other blob a tool returns), and silently truncating one into
// a generic parse failure would be a real, avoidable regression for any
// tool whose legitimate results sit between 4MiB and 10MiB.
const mcpBackendCallResponseMaxBytes = maxRequestBytes

// errMCPResponseTooLarge is the sentinel doBackendJSONRPC wraps when a
// backend response hits its call's maxBytes cap (security review round 2,
// 2026-08-22, important finding 4), letting the tools/call handler report
// "response too large" instead of a generic "upstream error".
//
// errors.Is is the correct match here, and it is Yaegi-safe. An earlier
// revision used a string marker plus strings.Contains out of caution; a
// harness on yaegi v0.16.1 (the version tools/yaegi-check pins, same
// stdlib.Symbols set) interpreting this shape found errors.Is correct
// through shallow, deep, and multi-%w wrapping, and correctly false for
// an unrelated error. The reason is that declaring the VARIABLE in
// interpreted code does not make its VALUE interpreted: errors.New
// returns a compiled *errors.errorString and fmt.Errorf a compiled
// *fmt.wrapError, so the whole chain errors.Is walks is compiled. This
// is the same shape retry.go:isTransient has run in production since
// v0.2.0, and errRequestBuildFailed (providers.go) is already matched
// this way through a multi-%w wrap.
//
// Neither documented Yaegi trap applies: matchesSentinel's TRIP-WIRE
// (limits.go) is about a plugin-declared error TYPE, which errors.New
// does not create, and the comma-ok trap needs a plugin-declared
// interface. A string check was also strictly worse than it looked —
// err.Error() is itself an interpreted-to-compiled method dispatch, so
// it verified nothing extra, and matching on text that partly comes
// from the backend admits false positives a sentinel cannot have.
var errMCPResponseTooLarge = errors.New("mcp backend response too large")

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

// mcpResponseFormat is this route's negotiated response framing, decided
// once per request (mcpNegotiateFormat, called from handleMCPFederated)
// and threaded explicitly as a parameter through every function that can
// end in a call to writeJSONRPCEnvelope — handleMCPFederated itself,
// mcpFederatedInitialize, mcpFederatedToolsList, mcpFederatedToolsCall —
// down to that one chokepoint. Threaded explicitly, not carried by
// wrapping w in a ResponseWriter that rewrites Content-Type and reframes
// bytes after the fact: a wrapper like that has to buffer or intercept
// every Write to retroactively reshape output it already committed,
// exactly the kind of implicit, hard-to-test indirection this package
// avoids elsewhere (see writeJSONRPCEnvelope's own "single chokepoint"
// shape, which this preserves).
type mcpResponseFormat int

const (
	// mcpResponseJSON is today's only framing — a bare JSON document,
	// Content-Type: application/json — and mcpNegotiateFormat's default
	// for every request that does not explicitly ask for SSE.
	mcpResponseJSON mcpResponseFormat = iota
	// mcpResponseSSE frames the identical JSON-RPC envelope as one
	// text/event-stream event: a single "data:" line carrying the
	// compact JSON body, terminated by the blank line the SSE wire
	// format requires (sseWriter.writeData, sse.go, reused as-is here —
	// see writeJSONRPCEnvelope's own doc comment for why).
	mcpResponseSSE
)

// mcpNegotiateFormat decides handleMCPFederated's response framing from
// r's own Accept header — the ONLY signal that ever produces
// mcpResponseSSE. Every other case, including no Accept header at all,
// "application/json" alone, and "*/*", yields mcpResponseJSON: the
// byte-identical-to-today default this route must never change unasked.
//
// "*/*" is a deliberate part of that default, not an oversight: it
// states no SPECIFIC preference for either framing, and a caller whose
// HTTP client stack sets it automatically (a common default for generic
// tooling that never touches this header by hand) would otherwise see
// this route's output silently change shape it never asked to change —
// exactly the regression this negotiation must not cause. Only a caller
// that names the concrete "text/event-stream" token — the real, reported
// client's own "Accept: application/json, text/event-stream" — gets the
// new framing.
func mcpNegotiateFormat(r *http.Request) mcpResponseFormat {
	if mcpAcceptsSSE(r) {
		return mcpResponseSSE
	}
	return mcpResponseJSON
}

// mcpAcceptsSSE reports whether any "Accept" header on r names
// text/event-stream as an acceptable response media type. Accept is a
// comma-separated list of media ranges, each optionally followed by
// ";q=..." or other parameters (RFC 9110 §12.5.1) — this checks every
// element of every "Accept" header LINE present (net/http folds repeated
// header instances into one Header entry under the same key, but a
// client may still send more than one "Accept:" line; Header.Values
// returns each separately, covering both shapes), stripping parameters
// and surrounding whitespace before an EXACT, case-insensitive
// comparison against "text/event-stream" — deliberately never a
// substring or prefix check, so a near-miss media type like
// "application/x-text/event-stream-foo" cannot false-positive the way a
// naive strings.Contains over the whole header would.
func mcpAcceptsSSE(r *http.Request) bool {
	for _, header := range r.Header.Values("Accept") {
		for _, part := range strings.Split(header, ",") {
			mediaType, _, _ := strings.Cut(part, ";")
			if strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// writeJSONRPCEnvelope writes resp as the response body, matching
// setAdminJSONHeaders' own "declared Content-Type, nothing else asserted"
// minimalism for its default JSON framing — a JSON-RPC error is still
// carried at HTTP 200 (JSON-RPC errors are a payload-level concept, not a
// transport-level one; every genuinely transport-level failure this route
// can hit — an unreadable body, an unauthenticated/forbidden caller, a
// rate limit, a disallowed HTTP method — is handled before this function
// is ever reached, via the gateway's own writeOAIError/writeLimitViolation
// envelope, matching every other route in this package).
//
// format (mcpNegotiateFormat's own doc comment) selects between that
// default JSON framing and mcpResponseSSE: one "data:" line carrying resp
// as compact JSON, terminated by the blank line the SSE wire format
// requires. The SSE branch reuses sse.go's newSSEWriter/writeData
// verbatim rather than a second, hand-rolled encoder — writeData already
// writes exactly "data: " + b + "\n\n" as one Write call, which both IS
// the single-shot envelope this route needs and already gets the
// terminator right, the exact class of bug (a missing blank-line
// terminator) this negotiation exists to fix elsewhere, not repeat here.
// Its streaming-oriented extras — writeDone's "[DONE]" sentinel,
// per-event flushing meant for incremental delivery — are simply unused:
// this is one event, not a stream, so nothing here ever calls writeDone,
// and a Flush that is a no-op under Yaegi (newSSEWriter's own doc
// comment) costs this single-shot response nothing, unlike a real
// streamed reply that depends on it for time-to-first-byte.
func writeJSONRPCEnvelope(w http.ResponseWriter, format mcpResponseFormat, resp jsonrpcResponse) {
	resp.JSONRPC = jsonrpcVersion
	if format == mcpResponseSSE {
		sw := newSSEWriter(w) // commits SSE headers unconditionally, before the encode below — same header-then-body order as the JSON branch
		b, err := json.Marshal(resp)
		if err != nil {
			return // headers already committed; nothing useful to do on encode failure
		}
		_ = sw.writeData(b) // headers already committed; nothing useful to do on write failure
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // headers already committed; nothing useful to do on encode failure
}

// writeJSONRPCResult marshals result and writes it as a successful
// JSON-RPC response under id, framed per format (mcpNegotiateFormat). A
// marshal failure (result is not JSON-marshalable — never true for any
// value this file actually passes, but defensively handled rather than
// panicking) reports itself as a JSON-RPC internal error instead, in the
// same negotiated format.
func writeJSONRPCResult(w http.ResponseWriter, format mcpResponseFormat, id json.RawMessage, result any) {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		writeJSONRPCErrorResponse(w, format, id, jsonrpcInternalError, "failed to encode result")
		return
	}
	writeJSONRPCEnvelope(w, format, jsonrpcResponse{ID: id, Result: resultBytes})
}

// writeJSONRPCErrorResponse writes a JSON-RPC error response under id,
// framed per format (mcpNegotiateFormat) — a client that asked for SSE
// gets its error in SSE framing too, never a JSON body it cannot parse.
func writeJSONRPCErrorResponse(w http.ResponseWriter, format mcpResponseFormat, id json.RawMessage, code int, message string) {
	writeJSONRPCEnvelope(w, format, jsonrpcResponse{ID: id, Error: &jsonrpcError{Code: code, Message: message}})
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
//
// The trailing slash is trimmed before the suffix check (F9, review round
// 3, 2026-09): a server configured as "http://host/sse/" is the identical
// legacy-SSE endpoint as "http://host/sse" — trailing slashes are common
// in operator-typed URLs and carry no transport meaning here — but the
// bare HasSuffix check missed it, leaving such a server IN the fan-out
// where every tools/list call wasted a semaphore slot, failed, and marked
// the target unhealthy for a transport this file was never built to
// speak at all.
func isLegacySSETransportURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/sse")
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
// other metered route, charging the plain weight of one request
// regardless of method — with per-target attribution added separately
// inside mcpFederatedToolsList/mcpFederatedToolsCall (their own doc
// comments cover exactly what "attributed" means for each).
//
// tools/list's fan-out is deliberately NOT charged against the caller's
// own requests-per-minute/requests-per-day budget (security review round
// 2, 2026-08-22, critical finding 3 — reverting security audit finding
// 1b's weighted-charge attempt): weighting checkAndCount by the number of
// allowed MCP servers is not default-preserving — on this operator's own
// live fleet, both configured groups have an empty MCP allow-list (every
// server allowed), so EVERY tools/list call would have weighed 11,
// silently turning a configured requests-per-minute: 60 into an effective
// budget of 5 successful tools/list calls per minute, and
// examples/kubernetes.yaml's shipped requests-per-minute: 10 would have
// 429'd the very FIRST tools/list call ever made — with no config change
// on the operator's part. The fan-out's real amplification is still
// fully visible to an operator without silently consuming tenant quota:
// mcpFederatedToolsList's own countTargetRequests call attributes one
// request to EVERY server actually attempted, at the "mcp"/target-id
// scope (limits.go) — a SEPARATE scope kind from the user/group/total
// scopes checkAndCount evaluates here — so the dashboard's targets view
// already shows the true per-server cost. The fan-out's outbound HTTP/
// memory cost is bounded instead by mcpFederatedFanoutConcurrency's
// semaphore (mcpFederatedToolsList's own doc comment), which is the
// actual protection against the amplification finding 1 described.
func (g *Gateway) handleMCPFederated(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}

	// Negotiated once, from the request alone, before the body is even
	// read: every JSON-RPC-shaped response below — including the parse
	// error a malformed body itself produces — must honor it
	// (mcpNegotiateFormat's own doc comment). The transport-level error
	// paths in this function (writeOAIError, writeLimitViolation) are
	// deliberately NOT part of this — see writeJSONRPCEnvelope's own doc
	// comment for why those stay out of scope.
	format := mcpNegotiateFormat(r)

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
		writeJSONRPCErrorResponse(w, format, json.RawMessage("null"), jsonrpcParseError, "parse error")
		return
	}

	scopes := withTotalScope(buildLimitScopes(u, grp))
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		g.recordLimitEvent(scopes, routeMCPFederated, violation) // F3 hook 1 (v0.3 dashboard task)
		writeLimitViolation(w, violation)
		return
	}

	switch {
	case req.Method == "initialize":
		g.mcpFederatedInitialize(w, format, req)
	case req.Method == "ping":
		writeJSONRPCResult(w, format, req.ID, map[string]any{})
	case req.Method == "tools/list":
		g.mcpFederatedToolsList(w, format, r, req, allowedMCPServerNames(g.cfg, grp))
	case req.Method == "tools/call":
		g.mcpFederatedToolsCall(w, format, r, req, grp)
	case strings.HasPrefix(req.Method, "notifications/"):
		// A JSON-RPC notification carries no id and gets no response body
		// by definition; MCP's Streamable HTTP transport answers a
		// notification POST with 202 Accepted and nothing else — format
		// plays no part here, deliberately: there is no body to frame
		// either way, and wrapping an empty body in an SSE "data:" line
		// would fabricate content this response was never meant to carry.
		w.WriteHeader(http.StatusAccepted)
	default:
		writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcMethodNotFound, "method not found: "+req.Method)
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
func (g *Gateway) mcpFederatedInitialize(w http.ResponseWriter, format mcpResponseFormat, req jsonrpcRequest) {
	var params mcpInitializeParams
	_ = json.Unmarshal(req.Params, &params) // best-effort; empty/malformed params falls back to defaultMCPProtocolVersion below

	protocolVersion := defaultMCPProtocolVersion
	if knownMCPProtocolVersions[params.ProtocolVersion] {
		protocolVersion = params.ProtocolVersion
	}

	writeJSONRPCResult(w, format, req.ID, map[string]any{
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

// mcpTool is a convenience literal type this repository's own tests use
// to build a canned backend tools/list response (newMockJSONRPCServer,
// sessionRequiredMockServer) — name/description/inputSchema are the three
// fields every such test fixture actually needs to set. Production code
// no longer round-trips a real tool through this fixed-field shape (F2,
// review round 3, 2026-09) — see mcpToolsListResult's own doc comment for
// why.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// mcpToolsListResult is the "result" object of an MCP "tools/list"
// response, both a backend's own (decoded, in mcpFederatedToolsList's own
// per-server fan-out) and this gateway's federated one (re-encoded from
// the merged tools, mcpFederatedToolsList's own final writeJSONRPCResult
// call).
//
// Tools decodes as []map[string]json.RawMessage, not the fixed mcpTool
// struct above (F2, review round 3, 2026-09): the earlier fixed-field
// struct kept only name/description/inputSchema and silently dropped
// every OTHER field a real tool object carries on the round trip through
// this gateway — title, annotations (destructiveHint, readOnlyHint, ...),
// outputSchema, _meta. A federated client that relies on annotations to
// decide whether a tool needs user confirmation before a call, or on
// outputSchema to validate structuredContent, silently lost that signal.
// Decoding into a raw map per tool and rewriting only "name"
// (prefixMCPTool, below) passes every other field through unchanged,
// byte-for-byte, exactly as the backend sent it. One side effect: a Go
// map's keys marshal back out in SORTED order (encoding/json's own
// documented behavior), not the backend's original field order — a
// cosmetic reordering, never a content change.
//
// NextCursor is the MCP pagination cursor a backend sets when it has more
// tools than fit in one response (F3, review round 3, 2026-09) —
// mcpFederatedToolsList's own per-server loop follows it, bounded by
// mcpToolsListMaxPagesPerServer, until a page comes back with none.
type mcpToolsListResult struct {
	NextCursor string                       `json:"nextCursor,omitempty"`
	Tools      []map[string]json.RawMessage `json:"tools"`
}

// mcpToolsListParams is one "tools/list" request's own params. Cursor is
// omitted (its zero value, "") for a server's first page; a non-empty
// value echoes back exactly the nextCursor that same server's own
// previous page reported (F3) — never a cursor minted by this gateway or
// borrowed from another server, since MCP's cursor is an opaque,
// per-server token.
type mcpToolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// mcpToolsListMaxPagesPerServer bounds how many pages
// mcpFederatedToolsList will follow via nextCursor for any ONE server
// (F3, review round 3, 2026-09) — independent of, and in addition to,
// the fan-out's own shared toolsListBackendTimeout budget (fanoutCtx in
// mcpFederatedToolsList), which already bounds total wall-clock
// regardless of page count. This cap exists so a misbehaving backend
// that always sets nextCursor (an unbounded pagination loop) cannot hold
// this goroutine's semaphore slot for the fan-out's entire timeout
// budget one page at a time. 20 is chosen generously above any real MCP
// server's tool catalog this codebase has seen (the largest production
// tools/list this gateway federates today is a single, unpaginated
// page), so it is never expected to actually trim a legitimate server's
// results — reaching it degrades to a partial tool list for that one
// server plus a logged note, the same "partial, not a hard failure"
// shape the fan-out's own timeout budget already uses.
const mcpToolsListMaxPagesPerServer = 20

// prefixMCPTool returns a shallow copy of tool (F2, review round 3,
// 2026-09) with only its "name" field rewritten to
// "<serverName>_<original name>" — the merge's own established
// server-prefix convention (mcpFederatedToolsList's own doc comment) —
// and every other field passed through unchanged. fullName is that same
// rewritten name, returned separately so a caller building an
// mcpMergedTool for the final sort/dedupe (F4) never has to decode it back
// out of raw. ok is false when tool carries no "name" field at all, "name"
// is not a JSON string, or "name" is null or the empty string (P3, review
// round 4: json.Unmarshal("null", &toolName) succeeds and leaves toolName
// as "", indistinguishable from an explicit "name":"" without this check)
// — none a valid MCP tool object, skipped by the caller like any other
// malformed entry from that server, without failing the whole server's
// contribution.
func prefixMCPTool(serverName string, tool map[string]json.RawMessage) (out map[string]json.RawMessage, fullName string, ok bool) {
	rawName, present := tool["name"]
	if !present {
		return nil, "", false
	}
	var toolName string
	if err := json.Unmarshal(rawName, &toolName); err != nil {
		return nil, "", false
	}
	// P3 (review round 4): "name":null unmarshals into toolName's zero
	// value ("") with no error, same as an explicit "name":"" — either
	// way there is no real tool name to prefix, so this would otherwise
	// mint and list a bare "<serverName>_" tool nothing can ever call by
	// a meaningful name. Treated exactly like the missing-field and
	// non-string cases above: skipped, not emitted.
	if toolName == "" {
		return nil, "", false
	}
	fullName = serverName + "_" + toolName
	prefixedRaw, err := json.Marshal(fullName)
	if err != nil {
		return nil, "", false
	}
	out = make(map[string]json.RawMessage, len(tool))
	for k, v := range tool {
		out[k] = v
	}
	out["name"] = json.RawMessage(prefixedRaw)
	return out, fullName, true
}

// mcpMergedTool pairs one server's already-prefixed tool object (raw map,
// F2) with the two fields mcpFederatedToolsList's own final sort and
// collision check (F4, review round 3, 2026-09) need without re-decoding
// "name" out of raw on every comparison: its own prefixed name, and the
// server it came from — the deterministic tiebreak the collision check
// below uses, so the outcome never depends on which goroutine's fan-out
// happened to finish first.
type mcpMergedTool struct {
	raw    map[string]json.RawMessage
	name   string
	server string
}

// doBackendJSONRPC issues one JSON-RPC 2.0 POST to targetURL — an MCP
// server's own configured base URL, the same single endpoint
// handleTargetProxy reverse-proxies every method to — carrying sessionID
// as the Mcp-Session-Id request header when non-empty, and reading at
// most maxBytes of the response body (mcpBackendResponseMaxBytes for the
// tools/list fan-out, mcpBackendCallResponseMaxBytes for tools/call —
// their own doc comments cover why the two differ; security review round
// 2, 2026-08-22, important finding 4). A response body that hits maxBytes
// is reported by wrapping errMCPResponseTooLarge, not silently truncated
// into whatever partial (and likely invalid) JSON happened to fit.
//
// It is the one place that actually builds and sends an HTTP request for
// this file; every
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
func (g *Gateway) doBackendJSONRPC(ctx context.Context, targetURL, method string, params any, sessionID string, maxBytes int64) (resp *jsonrpcResponse, status int, respSessionID string, err error) {
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

	// Reads one byte past maxBytes so a response that hits the cap is
	// DETECTABLE (len(respBody) > maxBytes) rather than silently truncated
	// and handed to parseBackendJSONRPC to fail confusingly (security
	// review round 2, 2026-08-22, important finding 4).
	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBytes+1))
	if err != nil {
		return nil, status, respSessionID, err
	}
	// Status is checked BEFORE the size cap: a backend that fails with a
	// large HTML error page is diagnosed as "upstream returned HTTP 500",
	// which is actionable, rather than as "too large", which sends the
	// operator hunting a payload-size problem that does not exist. An
	// oversized SUCCESS response still reports the cap, which is the case
	// the cap exists for.
	if status < 200 || status >= 300 {
		return nil, status, respSessionID, fmt.Errorf("upstream returned HTTP %d", status)
	}
	if int64(len(respBody)) > maxBytes {
		return nil, status, respSessionID, fmt.Errorf("%w: exceeds %d bytes", errMCPResponseTooLarge, maxBytes)
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
func (g *Gateway) mcpBackendHandshake(ctx context.Context, targetURL string, maxBytes int64) (sessionID string, err error) {
	initParams := map[string]any{
		"protocolVersion": defaultMCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "traefik-llmgateway", "version": pluginVersion},
	}
	resp, _, respSessionID, err := g.doBackendJSONRPC(ctx, targetURL, "initialize", initParams, "", maxBytes)
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
		// P4 (review round 4): targetURL is this gateway's own credential
		// channel (buildUpstreamTargetURL's doc comment, mcp_a2a.go), and
		// err here is commonly a *url.Error wrapping it verbatim — logged
		// through the same sanitizeBaseURL/sanitizeProviderErr pair every
		// other target-URL-bearing log line in this codebase uses (e.g.
		// adminTargetView.URL, admin.go).
		g.logf("federated mcp: best-effort session close failed for %q: %v", sanitizeBaseURL(targetURL), sanitizeProviderErr(err.Error(), targetURL))
		return
	}
	_ = resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure
}

// mcpBackendSendInitializedNotification best-effort POSTs the MCP
// lifecycle's own "notifications/initialized" to targetURL, carrying
// sessionID (mcpBackendHandshake's own Mcp-Session-Id, when the backend
// set one) as the session header — the client-side half of the
// initialize handshake the MCP spec requires before any OTHER request on
// that session (F5, review round 3, 2026-09). mcpBackendCall's own
// handshake fallback previously sent "initialize" and went straight to
// the retried real call, skipping this notification entirely; a server
// that also gates its OTHER methods on having received it (not just on
// having answered "initialize") would still reject the retry.
//
// A notification carries no "id" (jsonrpcRequest's own zero value already
// omits it, ID being omitempty) and gets no JSON-RPC response body to
// interpret — the MCP Streamable HTTP transport answers a notification
// POST with 202 and nothing else — so there is nothing here to act on
// besides logging a transport-level failure; never surfaced to the
// caller, matching mcpBackendCloseSession's own best-effort convention
// right above. ctx is the same context the handshake and retry share:
// this call must not outlive the operation it is a step of, unlike the
// deliberately-separate mcpSessionCloseTimeout the best-effort DELETE
// runs under afterward (that one runs only once the real response has
// already been read and returned, so it alone needs its own budget).
func (g *Gateway) mcpBackendSendInitializedNotification(ctx context.Context, targetURL, sessionID string) {
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "notifications/initialized"})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body)) //nolint:gosec // same operator-configured target URL as doBackendJSONRPC
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}

	resp, err := g.targetClient.Do(req)
	if err != nil {
		// P4 (review round 4): same targetURL-is-a-credential-channel
		// scrub as mcpBackendCloseSession's identical log line above.
		g.logf("federated mcp: best-effort notifications/initialized failed for %q: %v", sanitizeBaseURL(targetURL), sanitizeProviderErr(err.Error(), targetURL))
		return
	}
	_ = resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure
}

// mcpSessionRequiredSignal decides whether the bare (session-less)
// attempt's own outcome — status, the parsed response when there is one,
// and err — specifically signals "this backend requires a session", the
// ONLY case mcpBackendCall's handshake-fallback retry may fire for (F1,
// review round 3, 2026-09). The PRIOR rule ("any JSON-RPC error OR any
// HTTP 4xx") mistook two different things for a session problem: a
// genuine tool-level failure a backend answers as an ordinary JSON-RPC
// error — retrying THAT silently double-executes whatever side effect a
// non-idempotent tools/call already ran, since the backend already did
// the work before reporting its own error — and a 429 (rate-limited,
// the opposite of what warrants a retry: it adds more load to a backend
// that already asked to be slowed down).
//
// method distinguishes the two call shapes mcpBackendCall is ever used
// for. tools/list's own params are the fixed, always-valid struct{}{}
// (mcpFederatedToolsList) and it has no side effect to double-run, so ANY
// JSON-RPC error there is still treated as session-missing — this is the
// exact behavior verified live against readitall ("method tools/list is
// invalid during...") and fetch (-32602 with or without params), neither
// of which literally names "session" in its own wording (mcpBackendCall's
// own doc comment covers the full live probe). tools/call gets the
// narrow rule instead: a JSON-RPC error must itself mention "session"
// (case-insensitive) to retry — any OTHER JSON-RPC error there is
// presumed to be the tool's own real failure, not a protocol-state
// problem.
//
// HTTP 400 and 404 are the only statuses ever eligible, for either
// method — narrowed from the prior "any 400-499", which is what let a
// 429 through; 401/403/409/422/etc. are real authorization/validation
// outcomes a retry cannot fix and must not paper over.
func mcpSessionRequiredSignal(method string, status int, resp *jsonrpcResponse, err error) bool {
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		return true
	}
	if err != nil || resp == nil || resp.Error == nil {
		return false
	}
	if method == "tools/list" {
		return true
	}
	return strings.Contains(strings.ToLower(resp.Error.Message), "session")
}

// mcpBackendCall issues a JSON-RPC 2.0 request for method to targetURL —
// bare first, matching the per-request "no persisted CLIENT session"
// design (handleMCPFederated's own doc comment) — and falls back to a
// ONE-SHOT initialize/retry/close handshake ONLY when the bare attempt's
// own outcome narrowly signals a missing session (mcpSessionRequiredSignal,
// its own doc comment covers exactly what qualifies and why it was
// narrowed, F1).
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
// round 2, 2026-08-21). maxBytes is doBackendJSONRPC's own response-size
// cap, passed straight through to every doBackendJSONRPC call this
// function makes (bare attempt, handshake, retry) — callers pass
// mcpBackendResponseMaxBytes or mcpBackendCallResponseMaxBytes depending
// on which path they are (security review round 2, 2026-08-22, important
// finding 4).
func (g *Gateway) mcpBackendCall(ctx context.Context, targetURL, method string, params any, maxBytes int64) (*jsonrpcResponse, error) {
	resp, status, _, err := g.doBackendJSONRPC(ctx, targetURL, method, params, "", maxBytes)
	if err == nil && resp.Error == nil {
		return resp, nil
	}

	if !mcpSessionRequiredSignal(method, status, resp, err) {
		// Not a session-required signal: relay the bare attempt's own
		// outcome as-is — a JSON-RPC error response when the backend gave
		// one (err is nil in that case; resp carries it), otherwise the
		// transport-level err — and never retry. Retrying here is exactly
		// what F1 fixes: a tools/call retry against an arbitrary tool-level
		// error could silently double-execute a non-idempotent tool, and a
		// 429 retry only adds load to a backend that is already
		// rate-limiting.
		return resp, err
	}

	sessionID, initErr := g.mcpBackendHandshake(ctx, targetURL, maxBytes)
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
	// F5, review round 3, 2026-09: the MCP lifecycle requires the client
	// to send notifications/initialized once it has the initialize
	// result, before any other request — a step this handshake fallback
	// previously skipped entirely, going straight from "initialize" to
	// the retried real call. Best-effort, matching mcpBackendCloseSession's
	// own convention right below: a notification gets no JSON-RPC
	// response to interpret (a 202 and nothing else), so a failure here
	// is logged, never surfaced — the retry that follows is what actually
	// decides whether the handshake as a whole succeeded.
	g.mcpBackendSendInitializedNotification(ctx, targetURL, sessionID)
	if sessionID != "" {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), mcpSessionCloseTimeout)
			defer cancel()
			g.mcpBackendCloseSession(closeCtx, targetURL, sessionID)
		}()
	}

	retryResp, _, _, retryErr := g.doBackendJSONRPC(ctx, targetURL, method, params, sessionID, maxBytes)
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

// lastSSEDataLine returns the payload of the last COMPLETE SSE event's
// "data:" field in an SSE body — body itself, unchanged, if it contains
// no "data:" line at all (so the caller's own json.Unmarshal fails
// informatively on whatever body actually was, rather than this function
// silently swallowing a malformed stream into an empty byte slice).
//
// An SSE event's own multi-line "data:" field is joined with "\n" per the
// wire format (F6, review round 3, 2026-09; this gateway's own
// sseWriter.writeData, sse.go, emits exactly this shape for a multi-line
// payload) — every "data:" line up to the event's
// terminating blank line ("\n\n") is one logical field, not a separate
// one. A PRIOR revision of this function took only the single, textually
// LAST "data:" line in the whole body, which misread a backend's own
// pretty-printed or line-split JSON payload (each physical line
// individually invalid JSON on its own) as a parse failure, misdiagnosing
// a perfectly well-formed backend as failed.
//
// This does NOT implement per-request id correlation across multiple
// concurrent SSE-delivered EVENTS the way a real MCP client's
// Streamable-HTTP transport layer would (matching a specific response to
// the request that triggered it by id, ignoring unrelated
// server-initiated notifications interleaved on the same stream) — it
// takes the LAST complete event, full stop. This is a deliberate,
// documented limitation, acceptable for mcpBackendCall's own one-request-
// in-flight-at-a-time usage of this stream (it never has two outstanding
// calls sharing one SSE response to correlate between): a backend that
// interleaves an unrelated notification AFTER its real response on the
// same stream would be misread by this function. No production server
// probed for this round exhibited that behavior.
func lastSSEDataLine(body []byte) []byte {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	events := bytes.Split(normalized, []byte("\n\n"))
	for i := len(events) - 1; i >= 0; i-- {
		var dataLines [][]byte
		for _, line := range bytes.Split(events[i], []byte("\n")) {
			line = bytes.TrimRight(line, "\r")
			if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				dataLines = append(dataLines, data)
			} else if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				dataLines = append(dataLines, bytes.TrimSpace(data))
			}
		}
		if dataLines != nil {
			return bytes.Join(dataLines, []byte("\n"))
		}
	}
	return body
}

// mcpFederatedFanoutConcurrency bounds how many of names' backend servers
// mcpFederatedToolsList's own fan-out below contacts at once — a
// buffered-channel semaphore gating each goroutine's actual outbound HTTP
// work rather than its launch. It resembles limiter.spawnTokens
// (limits.go) only in using a buffered channel: spawnTokens is a
// non-blocking select with a `default:` that DROPS the work, whereas this
// one blocks until a slot frees and must never skip a server. Do not
// treat the two as interchangeable. Before this bound (security audit finding
// 1a, 2026-08-22), one incoming tools/list request spawned one goroutine
// per allowed server with NO cap at all: with the 11 production servers,
// one client call could hold up to 11 concurrent connections and up to
// 11× mcpBackendResponseMaxBytes of backend response bodies in memory at
// once, entirely outside this gateway's own admission control. Every
// server is eventually contacted regardless of this bound — a goroutine
// blocks on the semaphore, it is never dropped or skipped. 8 (raised from
// an initial 4, security review round 2, 2026-08-22, important finding
// 3): 4 measurably tripled worst-case wall-clock latency for an
// unrestricted group against all 11 servers (907ms measured at 300ms/
// server, versus ~300ms pre-bound) and, against hung backends, pushed the
// worst case to ceil(11/4)×toolsListBackendTimeout = 60s — well past the
// ~30s timeout many real MCP clients use, holding the ServeHTTP goroutine
// the whole time. 8 nearly halves both: ceil(11/8)=2 batches. The fanoutCtx
// deadline below (not this constant alone) is what actually re-bounds the
// worst case back down near toolsListBackendTimeout regardless of
// concurrency or backend count — see its own doc comment.
const mcpFederatedFanoutConcurrency = 8

// mcpFederatedToolsList implements the federated "tools/list": it fans
// out one per-server fetch (below) per server in names (handleMCPFederated's
// own allowedMCPServerNames(grp) call, made once there so the exact set
// contacted is always the one the client actually asked to reach), at
// most mcpFederatedFanoutConcurrency at a time — every backend sharing
// ONE overall toolsListBackendTimeout deadline (fanoutCtx, below) — then
// merges every reachable server's own tools into one result,
// each tool's name prefixed "<serverName>_<toolName>" (prefixMCPTool; F2's
// own doc comment covers why every OTHER field of the tool object passes
// through unchanged), the exact underscore-separator convention the old
// agentgateway this plugin replaces used, and the one pugbot's mcpclient
// and agentkit have already persisted into their own tool-id databases
// (e.g. "brave-search_brave_web_search" — verified live). Per server, a
// paginated backend is followed via nextCursor up to
// mcpToolsListMaxPagesPerServer pages (F3, review round 3, 2026-09) — all
// still inside that one server's share of fanoutCtx's shared budget, never
// a fresh timeout per page. A server that errors, times out, panics, or
// returns an unparsable/invalid result is logged and skipped, not
// surfaced as a whole-call failure — UNLESS every single attempted server
// failed, in which case an empty tools list would misleadingly look like
// "this caller's group has no MCP access" — see the loud-failure branch
// below.
//
// countTargetRequests (limits.go) attributes one request to every server
// actually DIALED here, in ONE batched call after wg.Wait() — "dialed",
// not "attempted" in the looser sense the prior revision used (F10,
// review round 3, 2026-09): a goroutine that was still waiting on the
// fan-out's own semaphore when fanoutCtx expired never sent a byte to
// its server and must not move that server's own per-target counter —
// only a server this gateway actually opened a connection to (and got a
// response, a timeout, a connection refusal, or a panic from) spent a
// real slot of this gateway's outbound capacity, which is what these
// counters exist to track. A single federated tools/list call can move
// several targets' own counters, not just one — unlike
// handleTargetProxy's always-exactly-one-target shape (mcp_a2a.go).
//
// Two servers can legitimately mint the identical final prefixed name —
// e.g. server "a" has tool "b_c" and server "a_b" has tool "c", both
// producing "a_b_c" (F4, review round 3, 2026-09) — resolveFederatedTool's
// own longest-prefix rule can only ever route such a name to ONE of them,
// so shipping both in tools/list would advertise a tool the client can
// never actually reach as advertised, and some client-side LLM tool APIs
// reject a duplicate name outright. The final merge below detects this by
// sorting on (name, server) — for a stable, deterministic grouping that
// never depends on which goroutine's fan-out happened to finish first —
// then, for every colliding name, keeps the entry whose server is the one
// resolveFederatedTool would ACTUALLY route a tools/call for that name to
// (P2, review round 4: the prior revision instead kept whichever server
// sorted first alphabetically, which is a different server whenever the
// alphabetically-first name is not also the longest-prefix match —
// advertising a schema/description tools/list never lets the caller
// reach, because every call for that name is routed elsewhere). Logs
// exactly which server's tool was dropped and which server tools/call
// actually routes this name to.
func (g *Gateway) mcpFederatedToolsList(w http.ResponseWriter, format mcpResponseFormat, r *http.Request, req jsonrpcRequest, names []string) {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		merged = make([]mcpMergedTool, 0, len(names))
		failed []string
		// dialed is the F10 fix's own accounting list — every name a
		// goroutine actually won the semaphore for (and so is about to
		// call mcpBackendCall for), kept separate from failed: a server
		// starved out by fanoutCtx before ever winning the semaphore
		// belongs in failed (the client-visible degradation log) but NOT
		// in dialed (the per-target request counters below).
		dialed []string
	)
	// fanoutCtx bounds the WHOLE fan-out to toolsListBackendTimeout from
	// when THIS call started — not each individual backend's own start
	// (security review round 2, 2026-08-22, important finding 3). With
	// mcpFederatedFanoutConcurrency backends running at once, a later
	// batch's goroutines only begin once an earlier one's semaphore slot
	// frees; giving each one its own FRESH toolsListBackendTimeout budget
	// (this function's pre-bound design, and its own initial post-
	// semaphore revision) let total wall-clock grow with
	// ceil(len(names)/mcpFederatedFanoutConcurrency) batches. Every
	// backend below shares this ONE already-ticking context instead —
	// context.WithTimeout always resolves to the earlier of a parent's
	// existing deadline and its own duration (proven by
	// TestHandleMCPFederated_ToolsList_SlowBackend_BoundedByRequestContext),
	// so deriving fanoutCtx from r.Context() here preserves that same
	// property for the request's own deadline, on top of this one.
	//
	// The accepted trade: a later batch inherits the REMAINING budget, not
	// a fresh one, so if every backend is slow enough to consume most of
	// toolsListBackendTimeout on its own, servers in later batches are cut
	// off and their tools are missing from the merged list. Measured: 11
	// servers at 11s each returns 8 of 11 tools after 20s, where a per-
	// backend budget returned 11 of 11 after 11s. This is deliberate —
	// the alternative is a worst case of ceil(11/8) x 20s = 60s for a
	// single tools/list — and it degrades to a partial list plus a logged
	// failure line per starved server, never a hung request. Real
	// tools/list calls answer in well under a second (at 300ms/server all
	// 11 return), so this path needs a pathological backend to trigger.
	fanoutCtx, fanoutCancel := context.WithTimeout(r.Context(), toolsListBackendTimeout)
	defer fanoutCancel()

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

			// G4, feat/target-health review round 2: acquiring the
			// semaphore via select, not a bare blocking send, lets a
			// server still parked here when fanoutCtx expires return
			// WITHOUT ever calling mcpBackendCall — it was never dialed,
			// so it has nothing to say about that server's own health
			// (below). Without this, a bare `sem <- struct{}{}` still
			// unblocks once fanoutCtx dies (an earlier holder's own call
			// fails fast against the already-expired context and frees
			// its slot), but by then mcpBackendCall would be called with
			// a context that is already done — indistinguishable from a
			// server that WAS dialed and genuinely timed out, which is
			// exactly the over-suppression this fix removes.
			select {
			case sem <- struct{}{}:
			case <-fanoutCtx.Done():
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
				return
			}
			defer func() { <-sem }()

			// F10: this goroutine won the semaphore, so it is about to
			// actually dial the server — recorded now, unconditionally,
			// regardless of what the paginated fetch below returns, so
			// countTargetRequests below reflects real outbound requests
			// sent, never a server that only ever waited on the
			// semaphore.
			mu.Lock()
			dialed = append(dialed, name)
			mu.Unlock()

			targetURL := g.cfg.MCPServers[name].URL
			probeStart := time.Now()

			// F3: follow nextCursor up to mcpToolsListMaxPagesPerServer
			// pages, all sharing fanoutCtx's one already-ticking budget —
			// never a fresh per-page timeout. transportErr and appFailed
			// are kept separate deliberately: transportErr is a real
			// mcpBackendCall failure (network, non-2xx, the size cap) and
			// is what targetHealth below treats as unhealthy; appFailed
			// is "the backend answered but this fan-out cannot use what
			// it said" (a JSON-RPC error, or a page that fails to
			// unmarshal) — the server is still reachable and healthy, it
			// just has nothing usable to contribute this call, matching
			// the pre-F3 rule that a JSON-RPC-level resp.Error never
			// marks a server unhealthy.
			var (
				pageTools    []map[string]json.RawMessage
				cursor       string
				transportErr error
				appFailed    bool
				// appFailedErr names WHY appFailed was set (a JSON-RPC
				// error object carries no Go error to reuse), so the F3
				// partial-keep log below can say something more useful
				// than "it failed". No wrapped url — a JSON-RPC error
				// message and a json.Unmarshal error never carry
				// targetURL, unlike transportErr, so this never needs the
				// sanitizeProviderErr treatment transportErr gets below.
				appFailedErr error
			)
			page := 0
			for ; page < mcpToolsListMaxPagesPerServer; page++ {
				resp, err := g.mcpBackendCall(fanoutCtx, targetURL, "tools/list", mcpToolsListParams{Cursor: cursor}, mcpBackendResponseMaxBytes)
				if err != nil {
					transportErr = err
					break
				}
				if resp.Error != nil {
					appFailed = true
					appFailedErr = fmt.Errorf("%s (code %d)", resp.Error.Message, resp.Error.Code)
					break
				}
				var result mcpToolsListResult
				if err := json.Unmarshal(resp.Result, &result); err != nil {
					appFailed = true
					appFailedErr = err
					break
				}
				pageTools = append(pageTools, result.Tools...)
				if result.NextCursor == "" {
					cursor = ""
					break
				}
				cursor = result.NextCursor
			}
			// F3 (review round 4): the loop above has no other way to
			// tell "stopped because the server said no more pages"
			// (cursor == "" here) from "stopped because
			// mcpToolsListMaxPagesPerServer ran out while the server's
			// last fetched page still set a NextCursor" — the latter
			// silently truncates a legitimately larger catalog unless
			// logged.
			if page == mcpToolsListMaxPagesPerServer && cursor != "" {
				g.logf("federated tools/list: server %q hit the %d-page cap; results truncated", name, mcpToolsListMaxPagesPerServer)
			}

			// G4: this goroutine WON the select above, so it was actually
			// dialed — only a client hang-up (context.Canceled,
			// propagating from r.Context() into fanoutCtx) says nothing
			// about this server's own health, mirroring
			// recordTargetProxyHealth's identical client-cancel rule
			// (target_health.go). context.DeadlineExceeded here means the
			// shared toolsListBackendTimeout fired while THIS dialed
			// server was still in flight — it did not answer within the
			// fan-out's own budget, which is a real failure worth
			// recording, not a client artifact. Measured across every
			// page this server's own loop actually ran, not just the
			// first: a server that answers its first few pages fine and
			// then times out on a later one is genuinely unhealthy, not
			// a client artifact.
			if transportErr == nil || !errors.Is(transportErr, context.Canceled) {
				g.targetHealth.record(targetKindMCP, name, targetURL, transportErr == nil, transportErr, time.Since(probeStart), targetHealthSourceTraffic)
			}

			mu.Lock()
			defer mu.Unlock()
			// F3 (review round 4): a failure on the very FIRST page (page
			// == 0) leaves nothing to salvage, so the server is still
			// reported failed exactly as before this fix. A failure on a
			// LATER page — this server answered one or more pages fine
			// and only broke partway through pagination — now keeps the
			// tools already fetched instead of discarding the whole
			// server's contribution; a warning names what was lost so the
			// degradation is still visible, matching this fan-out's own
			// "partial, not silent" convention for the page-cap case
			// above.
			if page == 0 && (transportErr != nil || appFailed) {
				failed = append(failed, name)
				return
			}
			if transportErr != nil || appFailed {
				failErr := appFailedErr
				if transportErr != nil {
					// P4: transportErr can be a *url.Error wrapping
					// targetURL verbatim (an operator-configured target
					// URL's query string is this gateway's own credential
					// channel, buildUpstreamTargetURL's doc comment,
					// mcp_a2a.go) — scrubbed the same way every other
					// target-URL-bearing log line in this file is.
					failErr = errors.New(sanitizeProviderErr(transportErr.Error(), targetURL))
				}
				g.logf("federated tools/list: server %q: page %d failed: %v; keeping %d tool(s) already fetched from earlier pages", name, page, failErr, len(pageTools))
			}
			for _, tool := range pageTools {
				prefixed, fullName, ok := prefixMCPTool(name, tool)
				if !ok {
					continue // malformed tool entry from this server; skip just that one, not the whole server
				}
				merged = append(merged, mcpMergedTool{name: fullName, server: name, raw: prefixed})
			}
		}(name)
	}
	wg.Wait()

	if len(dialed) > 0 {
		attempted := make([]limitScope, len(dialed))
		for i, name := range dialed {
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
		writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInternalError, "no MCP server reachable")
		return
	}

	// F4: sort by (name, server) so a collision's outcome is deterministic
	// — never "whichever goroutine happened to append first" — then group
	// every entry sharing a name and pick the survivor below.
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].name != merged[j].name {
			return merged[i].name < merged[j].name
		}
		return merged[i].server < merged[j].server
	})
	deduped := make([]map[string]json.RawMessage, 0, len(merged))
	for i := 0; i < len(merged); {
		j := i + 1
		for j < len(merged) && merged[j].name == merged[i].name {
			j++
		}
		group := merged[i:j]
		survivor := group[0]
		if len(group) > 1 {
			// P2 (review round 4): the survivor must be the server
			// resolveFederatedTool would actually route a tools/call for
			// this name to (longest matching server-name prefix) — the
			// SAME rule, called against the SAME names this fan-out was
			// given, so tools/list can never advertise a schema/
			// description the caller then cannot reach through
			// tools/call. Every candidate server here is by construction
			// one of names (mcpMergedTool.server, set from the fan-out's
			// own per-server loop) and a valid prefix of group[i].name
			// (prefixMCPTool's own "<serverName>_<toolName>" convention),
			// so resolveFederatedTool always resolves to one of them; the
			// !ok branch below is unreachable in practice and only keeps
			// group[0] as the same safe default the rest of this file
			// uses for its own unreachable branches.
			if routedServer, _, ok := resolveFederatedTool(group[0].name, names); ok {
				for _, m := range group {
					if m.server == routedServer {
						survivor = m
						break
					}
				}
			}
			for _, m := range group {
				if m.server == survivor.server {
					continue
				}
				g.logf("federated tools/list: tool %q from server %q dropped: tools/call routes this name to server %q instead", m.name, m.server, survivor.server)
			}
		}
		deduped = append(deduped, survivor.raw)
		i = j
	}

	writeJSONRPCResult(w, format, req.ID, mcpToolsListResult{Tools: deduped})
}

// mcpToolCallParams is one "tools/call" request's params: name is the
// caller-facing, server-prefixed tool id federation itself minted in
// tools/list ("<serverName>_<toolName>"). Meta carries the request's own
// "_meta" object verbatim — MCP's out-of-band per-request extension
// point, whose only field this gateway's own callers have actually used
// is progressToken (F11, review round 3, 2026-09) — forwarded to the
// resolved backend unchanged (mcpFederatedToolsCall) so a caller that
// expects progress correlated by that token at least reaches a backend
// that might honor it. This gateway itself never relays a progress
// notification back to the client (mcpFederatedToolsCall's own doc
// comment covers what it does and does not relay), so a caller depending
// on the notification arriving still needs a direct connection to the
// backend for that half.
type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
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
func (g *Gateway) mcpFederatedToolsCall(w http.ResponseWriter, format mcpResponseFormat, r *http.Request, req jsonrpcRequest, grp *group) {
	var params mcpToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInvalidParams, "invalid params")
		return
	}

	serverName, toolName, ok := resolveFederatedTool(params.Name, allowedMCPServerNames(g.cfg, grp))
	if !ok {
		writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInvalidParams, "unknown tool: "+params.Name)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), toolsCallBackendTimeout)
	defer cancel()
	targetURL := g.cfg.MCPServers[serverName].URL
	// mcpBackendCallResponseMaxBytes (10MiB), not the fan-out's smaller
	// mcpBackendResponseMaxBytes (security review round 2, 2026-08-22,
	// important finding 4): tools/call always resolves to exactly ONE
	// backend, so the fan-out memory-amplification argument does not
	// apply here — see mcpBackendCallResponseMaxBytes' own doc comment.
	// feat/target-health: passive recording (target_health.go) — measured
	// around the whole outbound call, same rule as tools/list's own fan-
	// out above: err != nil is a failure, a JSON-RPC-level resp.Error is
	// not.
	probeStart := time.Now()
	// Meta: params.Meta (F11) — the client's own "_meta" (progressToken
	// included, when set) forwarded verbatim to the resolved backend.
	resp, err := g.mcpBackendCall(ctx, targetURL, "tools/call", mcpToolCallParams{Name: toolName, Arguments: params.Arguments, Meta: params.Meta}, mcpBackendCallResponseMaxBytes)
	// G4: same dialed-server rule as mcpFederatedToolsList's own fan-out,
	// above — ctx here derives from r.Context() too, so a client cancel
	// (context.Canceled) must record nothing. Unlike the fan-out,
	// tools/call has no semaphore to wait behind: the call above is
	// always actually dialed, so context.DeadlineExceeded here means
	// THIS server did not answer within toolsCallBackendTimeout — a real
	// failure worth recording, not a client artifact (F2 over-suppressed
	// this case).
	if err == nil || !errors.Is(err, context.Canceled) {
		g.targetHealth.record(targetKindMCP, serverName, targetURL, err == nil, err, time.Since(probeStart), targetHealthSourceTraffic)
	}
	g.limiter.countTargetRequest(targetKindMCP, serverName)
	if err != nil {
		// P4 (review round 4): err here is commonly a *url.Error wrapping
		// targetURL verbatim, including its query string (this gateway's
		// own credential channel for a target — mcp_a2a.go) — scrubbed
		// the same way as every other target-URL-bearing log line in
		// this file.
		g.logf("federated tools/call: server %q: %v", serverName, sanitizeProviderErr(err.Error(), targetURL))
		// A response that hit mcpBackendCallResponseMaxBytes gets its own
		// specific message, not the generic "upstream error" a truncated-
		// then-unparsable body would otherwise produce (security review
		// round 2, 2026-08-22, important finding 4).
		if errors.Is(err, errMCPResponseTooLarge) {
			writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInternalError, "response too large")
		} else {
			writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInternalError, "upstream error")
		}
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
		writeJSONRPCErrorResponse(w, format, req.ID, jsonrpcInternalError, "invalid upstream response")
		return
	}

	writeJSONRPCEnvelope(w, format, jsonrpcResponse{ID: req.ID, Result: resp.Result, Error: resp.Error})
}
