package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxPassthroughBytes caps a client passthrough request body: 32MiB,
// generous enough for a native multimodal payload (audio, image, or a
// large document) that the unified routes' 10MiB maxRequestBytes does not
// need to accommodate, while still bounding memory against an oversized
// or malicious body.
const maxPassthroughBytes = 32 << 20

// maxAccountingTeeBytes caps how much of a non-streaming JSON response
// body handlePassthrough tees off for best-effort usage accounting: 4MiB,
// generous for a usage object even inside a very large chat response,
// while the client-facing io.Copy streams every byte of the response —
// of any size — without ever waiting for the full body to buffer first.
const maxAccountingTeeBytes = 4 << 20

// hopByHopHeaders lists the RFC 7230 §6.1 hop-by-hop headers, stripped
// from both the outgoing upstream request and the response copied back
// to the client — they describe this one TCP hop, not the end-to-end
// message, and must never be relayed by an intermediary.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
}

// gatewayCredentialHeaders are the client's own gateway-authentication
// headers — auth.go's presentedKey reads exactly these two. Stripped from
// the outgoing upstream request so a client's gateway API key never
// reaches the upstream provider; adapter.injectAuth sets the provider's
// own credential afterward.
var gatewayCredentialHeaders = map[string]bool{
	"Authorization": true,
	"X-Api-Key":     true,
}

// clientNegotiationHeaders are content-negotiation headers stripped from
// the outgoing upstream request only, never from the response:
// Accept-Encoding, so Go's http.Transport negotiates compression itself
// and transparently decompresses the reply — per its documented contract,
// that automatic decompression only happens when Transport itself added
// the header, not when the caller (this gateway, forwarding the client's
// own Accept-Encoding) set one explicitly. Without this strip, a client
// that sends "Accept-Encoding: gzip" (every mainstream OpenAI SDK and
// curl --compressed does) gets its exact header forwarded verbatim,
// Transport hands back raw gzip bytes, and the accounting JSON parse in
// handlePassthrough silently fails on binary it cannot decode.
var clientNegotiationHeaders = map[string]bool{
	"Accept-Encoding": true,
}

// dangerousClientHeaders lists exact-match client-supplied headers
// stripped from the outgoing request on BOTH proxy paths (native provider
// passthrough and the MCP/A2A target proxy) — additive to
// hopByHopHeaders/gatewayCredentialHeaders/clientNegotiationHeaders,
// never a full allowlist inversion (security+performance audit,
// 2026-08-22). Each of these conveys a caller's identity, or a downstream
// auth-proxy's own asserted identity for THIS gateway's inbound edge, and
// has no legitimate client-to-LLM/MCP use once it reaches an upstream
// provider or an in-cluster MCP server/A2A agent: handleTargetProxy's own
// doc comment (mcp_a2a.go) already documents that a target trusts the
// gateway's network position, not a caller-supplied identity header —
// forwarding one of these would let a tenant impersonate a different
// identity to that target instead of relying on auth.identify, which has
// already established who the caller is.
var dangerousClientHeaders = map[string]bool{
	"Forwarded":       true,
	"X-Remote-User":   true,
	"X-Remote-Groups": true,
	"Cookie":          true,
}

// dangerousClientHeaderPrefixes lists header-name PREFIXES (canonical
// textproto casing — net/http's own canonicalization, matching
// copyHeadersExcept's doc comment) stripped alongside
// dangerousClientHeaders on both proxy paths: every X-Forwarded-*
// (X-Forwarded-For, X-Forwarded-Host, X-Forwarded-User, ...) and X-Auth-*
// (X-Auth-Request-Email, X-Auth-Request-User, ...) header a client sends
// — the convention an in-cluster auth proxy (e.g. oauth2-proxy) uses to
// assert identity to whatever it fronts. A tenant must never spoof that
// convention simply by setting the header on their own request to this
// gateway.
var dangerousClientHeaderPrefixes = []string{"X-Forwarded-", "X-Auth-"}

// providerCredentialRetargetHeaders are additionally stripped from the
// outgoing request on the NATIVE PROVIDER PASSTHROUGH path only (passed as
// proxyUpstream's extraStrip; the MCP/A2A target proxy passes nil, since
// it never carries a provider credential to retarget in the first place):
// a tenant must not be able to redirect the operator's own upstream
// billing/attribution away from what the adapter configured, by simply
// setting these on their own request. anthropic-beta is included even
// though it also carries genuine opt-in feature flags (e.g. prompt
// caching) — an operator who wants to offer those through native
// passthrough sets them on the provider's own adapter/config surface, not
// by trusting an arbitrary client-supplied value straight onto the
// operator's own key (security+performance audit, 2026-08-22).
var providerCredentialRetargetHeaders = map[string]bool{
	"Openai-Organization": true,
	"Openai-Project":      true,
	"Anthropic-Beta":      true,
}

// stripHeaderPrefixes deletes every header in h whose canonical name
// starts with one of prefixes — the prefix-matching half of the
// dangerous-header strip copyHeadersExcept's own exact-match excepts
// cannot express (security+performance audit, 2026-08-22): every
// X-Forwarded-* and X-Auth-* header a client sends, regardless of its
// exact suffix. Deleting the currently-visited (or a not-yet-visited) key
// from a map mid-range is well-defined in Go and safe here.
func stripHeaderPrefixes(h http.Header, prefixes []string) {
	for k := range h {
		for _, prefix := range prefixes {
			if strings.HasPrefix(k, prefix) {
				h.Del(k)
				break
			}
		}
	}
}

// copyHeadersExcept copies every header in src to dst, skipping any key
// present in any of excepts. Header keys from both an *http.Request
// parsed off the wire and an *http.Response parsed by the client's
// Transport are already canonicalized by net/http, so a plain map lookup
// against the canonical names above is exact.
func copyHeadersExcept(dst, src http.Header, excepts ...map[string]bool) {
	for k, vs := range src {
		skip := false
		for _, ex := range excepts {
			if ex[k] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// passthroughRoute splits path into a candidate provider name and the
// remaining rest path, per the native passthrough convention
// "/{providerName}/{rest...}". Callers must pass path as
// r.URL.EscapedPath(), never the decoded r.URL.Path: a decoded path
// collapses a client's percent-encoded "%2f" into a literal "/" before it
// ever reaches this split, silently turning what the client sent as one
// opaque path segment into extra routing segments (and, downstream, into
// a "#" that would truncate the rest of the upstream URL at a fragment
// boundary if the encoded byte were "%23"). ok is false for an empty or
// "/"-only path, which carries no candidate provider name at all — the
// caller falls through to its next-route/404 handling in that case.
// passthroughRoute makes no claim the returned providerName is actually
// configured; the caller checks that against g.adapters before treating
// the request as passthrough.
func passthroughRoute(path string) (providerName, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", false
	}
	providerName, rest, _ = strings.Cut(trimmed, "/")
	if providerName == "" {
		return "", "", false
	}
	return providerName, rest, true
}

// hasTraversalSegment reports whether rest, once percent-decoded, contains
// a path segment exactly equal to "..". rest is still in its escaped form
// here (see passthroughRoute) — decoding it is a validation-only step,
// never used to build the outgoing upstream URL, so a legitimate
// percent-encoded segment (e.g. "a%2Fb" naming a literal "a/b" resource)
// still reaches the upstream exactly as the client sent it. Checking the
// decoded form, not the escaped one, is what catches an encoded traversal
// attempt like "..%2f..%2fsecret": as a single escaped segment it has no
// literal "/" for a naive split to catch, but a permissive upstream
// server decoding that same "%2f" itself would read it as "../../secret"
// — this check rejects it here instead. A rest that fails to
// url.PathUnescape at all is rejected too: a malformed percent-encoding
// is not a path this gateway can reason about safely.
func hasTraversalSegment(rest string) bool {
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return true
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// cappedAccountingBuffer is an io.Writer that accumulates up to
// maxAccountingTeeBytes of a response body, then silently discards the
// rest while still reporting every byte as written. It is the accounting
// side of an io.TeeReader wrapped around a non-streaming JSON response
// body in handlePassthrough: the client-facing io.Copy reads the real
// response (of any size) through the tee, and this buffer only ever
// retains the first maxAccountingTeeBytes of it for a best-effort usage
// parse afterward. truncated records whether the cap was hit, so the
// caller knows a parse would only fail on cut-off JSON and skips it.
type cappedAccountingBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

// Write implements io.Writer. It always reports success for the full
// input length — even once truncated is set and further bytes are being
// discarded — because this type is driven by io.TeeReader, whose Read
// aborts the underlying copy with an error the moment a tee Write returns
// anything short of len(p); silently dropping bytes past the cap must
// never disrupt the real, client-facing copy this buffer is only
// observing.
func (c *cappedAccountingBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if c.truncated {
		return n, nil
	}
	remaining := maxAccountingTeeBytes - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return n, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		c.truncated = true
	}
	c.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never errors
	return n, nil
}

// passthroughUsagePayload captures every shape the three provider types'
// native non-streaming JSON responses report token usage in, plus the
// optional top-level "model" field OpenAI- and Anthropic-shaped responses
// carry (Gemini's native response carries neither "model" nor any of the
// URL-supplied model id).
type passthroughUsagePayload struct {
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	} `json:"usage"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

// extractPassthroughUsage best-effort parses body as one of the three
// provider types' native non-streaming JSON response shapes, returning
// its reported token usage and model id, keyed by typeName (one of
// providerTypeOpenAI/Anthropic/Gemini). The returned error is body's own
// json.Unmarshal error, when decoding failed — the caller logs it rather
// than discarding it, since a body that should have been JSON (its
// Content-Type said so) but did not decode as JSON is worth knowing
// about, not silently swallowing. Usage and model still come back as
// their best-effort zero/fallback values in that case: a zero usage is a
// no-op for the caller's account call, not a wrong charge. The model id
// falls back to "unknown/"+providerName when the response carries no
// "model" field of its own, which is always true for Gemini's native
// response.
func extractPassthroughUsage(typeName, providerName string, body []byte) (usage, string, error) {
	var payload passthroughUsagePayload
	err := json.Unmarshal(body, &payload)

	model := payload.Model
	if model == "" {
		model = "unknown/" + providerName
	}

	switch typeName {
	case providerTypeOpenAI:
		return usage{prompt: payload.Usage.PromptTokens, completion: payload.Usage.CompletionTokens}, model, err
	case providerTypeAnthropic:
		return usage{prompt: payload.Usage.InputTokens, completion: payload.Usage.OutputTokens}, model, err
	case providerTypeGemini:
		return usage{prompt: payload.UsageMetadata.PromptTokenCount, completion: payload.UsageMetadata.CandidatesTokenCount}, model, err
	default:
		return usage{}, model, err
	}
}

// peekPassthroughModel best-effort reads a passthrough request's JSON body
// far enough to extract its top-level "model" field, restoring r.Body
// afterward — a fresh reader over the exact bytes read — so proxyUpstream
// can still forward the request to the upstream unchanged (security+
// performance audit, 2026-08-22). hasModel is false, with no error, for
// every shape handlePassthrough's own doc comment documents as "no
// inspectable model, fall back to provider-only auth": a non-JSON
// Content-Type (Gemini's URL-embedded model; a multipart audio/image
// upload — parakeet-mlx's transcription passthrough is exactly this
// shape, and this check skips reading its body entirely, at no cost), a
// body that fails to decode as JSON, or JSON with no non-empty top-level
// "model" string. err is non-nil only for an actual body-read failure
// (client disconnect, deadline) — the caller maps that to the same 400
// runUnified's own body-read failure uses (routes_unified.go), rather
// than silently treating an unreadable body as "provider-only, proceed".
func peekPassthroughModel(r *http.Request) (model string, hasModel bool, err error) {
	if r.Body == nil || !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return "", false, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxPassthroughBytes))
	if readErr != nil {
		return "", false, readErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Model == "" {
		return "", false, nil
	}
	return payload.Model, true, nil
}

// handlePassthrough implements the native provider passthrough route:
// "/{providerName}/{rest...}" reverse-proxies rest, verbatim, to
// providerName's configured upstream, with the client's gateway
// credential swapped for the provider's own. It is a raw proxy, not a
// translation layer like the unified routes — a non-2xx upstream status
// is forwarded to the client exactly as the upstream sent it, not wrapped
// in the gateway's own error envelope the way the unified routes wrap a
// providerHTTPError.
//
// Checks run in this order: group authorization — provider (403), model
// when the body carries one (403), path allowlist (403) — before
// capability checks (Upgrade→501, path validity→400) before rate limits
// (429/503) — a caller who cannot use providerName/model/path at all
// learns that first, rather than learning something about how they tried
// to use it. The provider-level Passthrough toggle (ProviderConfig,
// llmgateway.go) is checked earlier still, by ServeHTTP's own route gate
// — a disabled provider never reaches this function at all, reported as
// the ordinary unknown-route 404 instead.
//
// MODEL ENFORCEMENT (security+performance audit, 2026-08-22): when the
// request body is JSON and carries a non-empty top-level "model" field
// (peekPassthroughModel), it is checked against grp.allowsModel — both
// the bare form and the "providerName/model" form, the identical
// dual-candidate matcher the unified route's own resolveAgainst applies
// (registry.go) — before proxying, closing the bypass where a tenant
// scoped to one cheap model could otherwise reach the SAME provider's
// entire native API (fine-tuning, files, batches, ...) on the operator's
// key merely by asking natively instead of through /v1/chat/completions.
// A body with no inspectable model (Gemini's URL-embedded model id, a
// multipart upload, a non-JSON body) falls back to provider-only
// authorization, unchanged from before this round — this is a strict
// narrowing of what a passthrough request may address, never a new way
// to allow one a plain grp.allowsProvider check would have refused.
//
// PATH ALLOWLIST (same audit): GroupConfig.PassthroughPaths, when
// non-empty, additionally restricts which rest path this group's
// passthrough requests may address (grp.allowsPassthroughPath). Empty
// (the default, matchesGlob's own empty-means-all contract) allows every
// path, exactly as before this field existed.
//
// For a Gemini provider, rest is appended to base() exactly as the client
// sent it: there is no model extraction or URL rewriting here, so a
// Gemini passthrough client must address it with Gemini's own native URL
// structure, including its "/v1beta/models/{model}:generateContent"
// paths — injectAuth still sets the same x-goog-api-key header it sets
// for every other Gemini request. Gemini's model id lives in that URL
// path, not the JSON body, so peekPassthroughModel never finds one for a
// Gemini request — a Gemini passthrough client is always authorized
// provider-only, exactly as it was before model enforcement existed.
func (g *Gateway) handlePassthrough(w http.ResponseWriter, r *http.Request, u *user, grp *group, providerName, rest string) {
	if !grp.allowsProvider(providerName) {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "provider access denied")
		return
	}

	model, hasModel, err := peekPassthroughModel(r)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	if hasModel && !grp.allowsModel(model) && !grp.allowsModel(providerName+"/"+model) {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "model access denied")
		return
	}

	if !grp.allowsPassthroughPath(rest) {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "path access denied")
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
		writeLimitViolation(w, violation)
		return
	}

	// providerName is only ever passed here by ServeHTTP's dispatch, which
	// already confirmed it is a key of g.adapters before calling in.
	adapter := g.adapters[providerName]

	upstreamURL := adapter.base() + "/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Feature A (v0.22): provider-level only — unlike runUnified/the media
	// routes, the upstream model here lives in the response body
	// (extractPassthroughUsage, below), read only after this attempt
	// already resolved, so there is no model to attribute it to yet. See
	// recordProviderAttempt's own doc comment (limits.go).
	r = r.WithContext(withAttemptRecorder(r.Context(), func(resp *http.Response, attemptErr error) {
		g.limiter.recordProviderAttempt(providerName, "", resp, attemptErr)
	}))

	result, ok := g.proxyUpstream(w, r, upstreamURL, adapter.httpClient(), adapter.injectAuth, providerCredentialRetargetHeaders, true, "passthrough (provider "+providerName+")")
	if !ok || !result.isJSON {
		// A build/connection/copy failure already wrote its own response
		// (or, for a canceled client context, wrote nothing at all — see
		// proxyUpstream); a non-JSON response has nothing more to account
		// than the request itself, already counted by checkAndCount above.
		return
	}
	if result.tee.truncated {
		g.logf("passthrough: response body exceeded %d bytes; skipping usage accounting (provider %q)", maxAccountingTeeBytes, providerName)
		return
	}

	respUsage, model, unmarshalErr := extractPassthroughUsage(adapter.typeName(), providerName, result.tee.buf.Bytes())
	if unmarshalErr != nil {
		g.logf("passthrough: response body did not decode as JSON for usage accounting (provider %q): %v", providerName, unmarshalErr)
	}
	cost := unifiedCostMicros(providerName+"/"+model, model, respUsage, g.cfg.Pricing)
	g.limiter.account(scopes, respUsage, cost)
}

// proxyResult is what proxyUpstream reports back to its caller once it has
// streamed a response to the client: enough for a caller that wants
// best-effort JSON usage accounting (handlePassthrough) to run it, without
// proxyUpstream itself knowing anything about usage or pricing. tee is
// nil unless accountJSON was true and the response's Content-Type was
// application/json.
type proxyResult struct {
	tee    *cappedAccountingBuffer
	isJSON bool
}

// proxyUpstream is the shared reverse-proxy core behind both native
// provider passthrough (handlePassthrough, above) and the MCP/A2A target
// proxy (handleTargetProxy, mcp_a2a.go): it builds an upstream request
// from r's method/body/headers, sends it over client, and streams the
// response back to w incrementally through a flushWriter — so an SSE or
// other chunked upstream body reaches the client as each chunk arrives,
// never buffered until the copy finishes.
//
// Every hop-by-hop header (hopByHopHeaders) and the client's own gateway
// credential (gatewayCredentialHeaders) are stripped from the outgoing
// request, and Accept-Encoding (clientNegotiationHeaders) besides, so
// Transport can negotiate and transparently decompress compression
// itself — plus, additively (security+performance audit, 2026-08-22),
// every client-supplied identity/forwarding header (dangerousClientHeaders,
// dangerousClientHeaderPrefixes: Forwarded, X-Forwarded-*, X-Auth-*,
// X-Remote-User, X-Remote-Groups, Cookie) on BOTH callers, and every
// header in extraStrip on top of that — handlePassthrough passes
// providerCredentialRetargetHeaders (OpenAI-Organization, OpenAI-Project,
// anthropic-beta: a tenant must not retarget the operator's own upstream
// billing/attribution), handleTargetProxy passes nil, since an MCP/A2A
// target never carries a provider credential to retarget in the first
// place. injectAuth, when non-nil, is called on the built request before
// it is sent — handlePassthrough passes its adapter's injectAuth to swap
// the client's key for the provider's own; handleTargetProxy passes nil,
// since an MCP server or A2A agent is an in-cluster target that receives
// no injected credential at all.
//
// A build failure or a dead upstream writes a 502 envelope to w and
// returns ok=false; a canceled client context (errors.Is context.Canceled)
// is logged, not surfaced, and also returns ok=false, writing nothing —
// the client is already gone. A response-copy failure after headers were
// already written also returns ok=false, since nothing further can be
// written to w at that point either way. logPrefix labels every
// logf/errorf line this call emits, so passthrough and target-proxy
// failures stay distinguishable in the log.
//
// accountJSON gates whether a non-streaming application/json response
// gets teed off into result.tee for the caller's own best-effort usage
// parse afterward: handlePassthrough passes true; handleTargetProxy
// passes false, since target-proxy accounting never goes past the
// request-count checkAndCount already ran before calling in, and teeing a
// response nobody will ever read back would only cost memory for nothing.
func (g *Gateway) proxyUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, client *http.Client, injectAuth func(*http.Request), extraStrip map[string]bool, accountJSON bool, logPrefix string) (result proxyResult, ok bool) {
	bodyReader := io.LimitReader(r.Body, maxPassthroughBytes)
	// gosec G704 (SSRF via taint analysis) flags upstreamURL as
	// request-derived: it is, by design — this is a reverse proxy, and its
	// whole job is to forward a client-supplied path/query onto the
	// upstream. The host component is never request-derived: every caller
	// builds upstreamURL as an operator-configured base (a provider's
	// adapter.base(), or an MCP-server/agent TargetConfig.URL validated at
	// construction — see validateTargetURLs) plus "/"+rest — string
	// concatenation, not a second url.Parse of rest alone — so no
	// client-supplied value can change the scheme or host url.Parse
	// resolves for the final string. rest is additionally
	// traversal-checked by every caller before this point is reached
	// (hasTraversalSegment rejects any ".." segment in its decoded form)
	// and is forwarded here in its original escaped form — see
	// passthroughRoute's and targetRoute's EscapedPath()-based splits — so
	// a client cannot smuggle an encoded "/" (%2f) past this gateway's own
	// routing only to have a permissive upstream reinterpret it as a real
	// separator.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bodyReader) //nolint:gosec // operator-fixed host, rest is traversal-checked and forwarded escaped; see comment above
	if err != nil {
		g.errorf("%s: build upstream request: %v", logPrefix, err)
		writeOAIError(w, http.StatusBadGateway, "server_error", "upstream connection error")
		return proxyResult{}, false
	}
	upstreamReq.ContentLength = r.ContentLength
	if r.ContentLength < 0 || r.ContentLength > maxPassthroughBytes {
		// Unknown or over-cap length: let the transport negotiate chunked
		// transfer instead of advertising a Content-Length the capped
		// bodyReader above may not actually deliver.
		upstreamReq.ContentLength = -1
	}
	copyHeadersExcept(upstreamReq.Header, r.Header, hopByHopHeaders, gatewayCredentialHeaders, clientNegotiationHeaders, dangerousClientHeaders, extraStrip)
	stripHeaderPrefixes(upstreamReq.Header, dangerousClientHeaderPrefixes)
	if injectAuth != nil {
		injectAuth(upstreamReq)
	}

	resp, err := client.Do(upstreamReq) //nolint:gosec // same upstreamReq built above; operator-fixed host, traversal-checked, see its construction comment
	// Feature A (v0.22): proxyUpstream makes exactly one attempt (no
	// retry.go policy wraps this path), so this fires once per call,
	// whichever way it resolves. Only handlePassthrough's own context ever
	// carries a recorder (attemptRecorderFromContext, providers.go) —
	// handleTargetProxy (mcp_a2a.go), this function's other caller, never
	// wraps r's context this way, so an MCP/A2A target proxy attempt is
	// correctly never accounted as provider traffic.
	if rec := attemptRecorderFromContext(r.Context()); rec != nil {
		rec(resp, err)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			g.logf("%s: client canceled request: %v", logPrefix, err)
			return proxyResult{}, false
		}
		g.errorf("%s: upstream connection error: %v", logPrefix, err)
		writeOAIError(w, http.StatusBadGateway, "server_error", "upstream connection error")
		return proxyResult{}, false
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	copyHeadersExcept(w.Header(), resp.Header, hopByHopHeaders)
	w.WriteHeader(resp.StatusCode)
	fw := newFlushWriter(w)

	// Only a non-streaming JSON response gets teed off for best-effort
	// accounting, and only when the caller wants that at all (accountJSON);
	// every other response streams straight through fw with no extra
	// buffering — a large or slow upstream body must never sit waiting for
	// full receipt before the client sees its first byte.
	isJSON := accountJSON && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json")
	var tee *cappedAccountingBuffer
	var reader io.Reader = resp.Body
	if isJSON {
		tee = &cappedAccountingBuffer{}
		reader = io.TeeReader(resp.Body, tee)
	}

	if _, err := io.Copy(fw, reader); err != nil {
		g.errorf("%s: stream response body: %v", logPrefix, err)
		return proxyResult{isJSON: isJSON, tee: tee}, false
	}
	return proxyResult{isJSON: isJSON, tee: tee}, true
}
