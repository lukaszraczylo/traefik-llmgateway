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
	"time"
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

// maxModelPeekBytes caps how much of a passthrough request body
// peekPassthroughModel reads to find a top-level "model" field: 64KiB
// (security review fix, 2026-08-22, round 2). This is deliberately far
// smaller than maxPassthroughBytes: a legitimate LLM request always
// carries "model" in its first bytes, so 64KiB is generous headroom
// ahead of it while bounding memory to a small constant regardless of how
// large the rest of the body (a long messages/documents array) is. The
// body is never fully buffered to find this — see peekPassthroughModel's
// own doc comment for the io.MultiReader restoration that lets the
// remainder stream straight to the upstream, unread by this gateway.
//
// OPERATOR CONTRACT: a group with a non-empty Models list (GroupConfig,
// llmgateway.go) enforces model authorization on every native passthrough
// request whose body is inspected (see peekPassthroughModel's own gating)
// — and a request whose "model" field does not appear within this 64KiB
// window is DENIED (403), fail-closed, even if it would otherwise have
// been allowed. This includes a Gemini passthrough request: Gemini's
// model id lives in the URL path, never the JSON body, so a group with a
// restricted Models list and Gemini passthrough traffic will see every
// such request denied under this rule — route Gemini traffic for a
// model-restricted group through the unified /v1/* API instead, where
// modelRegistry.resolve enforces the same authorization against the
// URL-independent "model" field those routes already require.
const maxModelPeekBytes = 64 << 10

// passthroughBinaryContentTypePrefixes lists Content-Type prefixes
// peekPassthroughModel treats as genuinely binary and never reads at all
// (security review fix, 2026-08-22, round 2): multipart uploads
// (parakeet-mlx's transcription passthrough shape) and raw audio/image/
// video/octet-stream payloads carry no JSON "model" field to find, and
// reading them would only cost memory for nothing.
//
// This is a SKIP-list, not an ALLOW-list — the deliberate inversion of
// this gate's first version, which skipped the read unless Content-Type
// contained "application/json". That allow-list shape let a client bypass
// model enforcement outright merely by omitting Content-Type or sending
// an unexpected value (e.g. "text/plain"): the reviewer measured a
// {"model":"EXPENSIVE"} body reaching the upstream with a 200, because
// the Content-Type check alone decided whether the check ran, and
// Content-Type is entirely client-controlled. With a skip-list, every
// Content-Type NOT matching one of these prefixes — including "text/plain"
// and an ABSENT header — is inspected; the attacker no longer controls
// whether the check runs, only (still) what non-binary Content-Type they
// send.
var passthroughBinaryContentTypePrefixes = []string{
	"multipart/",
	"audio/",
	"image/",
	"video/",
	"application/octet-stream",
}

// isPassthroughBinaryContentType reports whether contentType matches one
// of passthroughBinaryContentTypePrefixes, case-insensitively.
func isPassthroughBinaryContentType(contentType string) bool {
	ct := strings.ToLower(contentType)
	for _, prefix := range passthroughBinaryContentTypePrefixes {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

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

// dangerousResponseHeaders lists exact-match upstream RESPONSE headers
// stripped before being relayed to the client (security review finding
// 5, round 3, 2026-08-22) — additive to hopByHopHeaders, which
// proxyUpstream already strips from both directions; this is the
// RESPONSE-side counterpart to dangerousClientHeaders/
// providerCredentialRetargetHeaders above, which already hardened the
// REQUEST side. proxyUpstream copies every OTHER upstream response
// header verbatim to the client — a raw reverse-proxy contract, not a
// translation layer — but three of them let the upstream act on THIS
// gateway's own origin, an origin that also serves the WAN-exposed
// /admin dashboard (admin.go): an upstream provider, or an in-cluster
// MCP/A2A target reached with no credential trust boundary of its own
// (handleTargetProxy's own doc comment, mcp_a2a.go — this strip applies
// there too, since both callers share this one function), could
// otherwise plant a Set-Cookie under the gateway's own domain, or
// rewrite the browser's security policy toward that domain via
// Strict-Transport-Security/Content-Security-Policy, merely by
// returning it in a response this gateway was only ever asked to relay.
//
// Deliberately a DENY-list, not an allowlist (unlike the request-side
// dangerous-header strips, which are also deny-lists, for the same
// reason): passthrough's whole purpose is exposing the provider's raw
// response, including headers this gateway's own code never reads but a
// client SDK does — grepping this repo for what it actually reads off a
// response finds exactly two: Content-Type (every adapter's streaming/
// non-streaming branch) and Retry-After (retry.go's OWN internal retry
// decision on the unified routes, never read from a passthrough
// response). Everything else — provider rate-limit telemetry headers
// like x-ratelimit-remaining-requests/-tokens included — is opaque to
// this gateway but commonly read by an external client's own SDK. An
// allowlist would silently break that transparency for every header this
// package does not already know to name; this narrow deny-list closes
// exactly the origin-integrity holes named above without that cost.
var dangerousResponseHeaders = map[string]bool{
	"Set-Cookie":                true,
	"Strict-Transport-Security": true,
	"Content-Security-Policy":   true,
}

// dangerousResponseHeaderPrefixes strips every Access-Control-* header
// (Access-Control-Allow-Origin, Access-Control-Allow-Credentials, ...)
// an upstream response sets (security review finding 5, round 3,
// 2026-08-22): an upstream provider or in-cluster MCP/A2A target has no
// legitimate reason to dictate THIS gateway's own CORS policy toward
// whatever browser called it — the gateway's own response to the client
// is a separate origin boundary the upstream must never get to speak
// for.
var dangerousResponseHeaderPrefixes = []string{"Access-Control-"}

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

// hasTraversalSegment reports whether rest, once percent-decoded and
// normalized, contains a path segment that resolves to exactly "..".
// rest is still in its escaped form here (see passthroughRoute) — decoding it
// is a validation-only step, never used to build the outgoing upstream
// URL, so a legitimate percent-encoded segment (e.g. "a%2Fb" naming a
// literal "a/b" resource) still reaches the upstream exactly as the
// client sent it. Checking the decoded form, not the escaped one, is what
// catches an encoded traversal attempt like "..%2f..%2fsecret": as a
// single escaped segment it has no literal "/" for a naive split to
// catch, but a permissive upstream server decoding that same "%2f"
// itself would read it as "../../secret" — this check rejects it here
// instead. A rest that fails to url.PathUnescape at all is rejected too:
// a malformed percent-encoding is not a path this gateway can reason
// about safely.
//
// Round 3 (security review, 2026-08-22) closes three further bypasses the
// single-pass decode-and-split above missed, each measured against a
// real upstream convention:
//
//   - Path parameters ("..;/x", the Tomcat/Spring RFC 3986 §3.3
//     convention): a segment's own identity is everything BEFORE its
//     first ";" — an upstream honoring that convention resolves
//     "..;foo=bar" identically to "..". Every segment has its
//     ";"-suffix stripped before the ".." comparison below.
//
//   - Backslash normalization ("..%5c..%5cx", decoding to "..\..\x"):
//     Windows/.NET and other backslash-normalizing upstreams treat "\"
//     as an equivalent path separator. The fully decoded string has
//     every "\" replaced with "/" before splitting, so a segment
//     hidden behind a backslash is caught the same as one behind "/".
//
//   - Double-encoding ("%252e%252e/x"): one url.PathUnescape pass
//     decodes this to "%2e%2e/x" — still containing "%", meaning a
//     SECOND decode pass (which this function deliberately never
//     performs — attempting one only invites indefinite re-encoding)
//     would reveal a hidden "..". Rather than decode again, a rest that
//     still contains "%" after the one legitimate unescape pass is
//     rejected outright — fail closed on ambiguity, matching this
//     function's existing rule for a rest that fails to unescape at
//     all.
//
//     CORRECTED (round 3, 2026-08-22 review): this rule's own comment
//     used to say it "rejects an intentionally double-encoded %25" —
//     that is not quite right and overstated the certainty. "%25" is
//     the CORRECT, single, standards-conforming encoding of a literal
//     "%" character (RFC 3986). After exactly one unescape pass, a
//     legitimately single-encoded literal "%" is byte-for-byte
//     indistinguishable from a genuinely double-encoded sequence (e.g.
//     "%252e") that would reveal a hidden "..%2e" on a second pass —
//     there is no way to tell them apart from the decoded bytes alone.
//     The false-positive cost is real and known: a rest like
//     "v1/100%25done" (a literal "100%done" resource path) decodes once
//     to "v1/100%done", still contains "%", and is rejected here even
//     though the ORIGINAL request carried no traversal attempt at all.
//     Fail-closed on this ambiguity is still the correct call — refusing
//     an occasional legitimate literal "%" is a far smaller cost than
//     admitting a real double-encoded traversal — but the comment should
//     not have implied the rejected input was provably malicious.
//
// Literal ".." and single-encoded "..%2f" keep their existing, already
// correct behavior — both still resolve to a segment of exactly "..".
func hasTraversalSegment(rest string) bool {
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return true
	}
	if strings.Contains(decoded, "%") {
		return true
	}
	normalized := strings.ReplaceAll(decoded, `\`, "/")
	for _, seg := range strings.Split(normalized, "/") {
		if i := strings.IndexByte(seg, ';'); i >= 0 {
			seg = seg[:i]
		}
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
		// CacheCreationInputTokens/CacheReadInputTokens (item 6 fix,
		// 2026-08-22 review — applied "for the anthropic provider
		// generally", not only translate_anthropic.go's own
		// anthropicUsagePayload): Anthropic's native passthrough
		// response reports these two prompt-cache counters separately
		// from InputTokens, and extractPassthroughUsage's own
		// providerTypeAnthropic case folds them in below so a
		// cache-heavy caller's usage is not silently under-billed on
		// this route either.
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
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
		return usage{prompt: payload.Usage.InputTokens + payload.Usage.CacheCreationInputTokens + payload.Usage.CacheReadInputTokens, completion: payload.Usage.OutputTokens}, model, err
	case providerTypeGemini:
		return usage{prompt: payload.UsageMetadata.PromptTokenCount, completion: payload.UsageMetadata.CandidatesTokenCount}, model, err
	default:
		return usage{}, model, err
	}
}

// peekPassthroughModel reads at most maxModelPeekBytes of a passthrough
// request body to find its top-level "model" field, then restores r.Body
// to stream the ORIGINAL, complete body to the upstream unchanged — the
// bytes already read prepended (io.MultiReader) to whatever remains
// unread on the real r.Body, never fully buffered in memory (security
// review fix, 2026-08-22, round 2: the first version buffered the whole
// body, up to 32MiB, into memory before forwarding it).
//
// Callers only invoke this once they have already decided enforcement is
// required (handlePassthrough checks group.hasModelRestriction() first) —
// this function itself has no fail-open/fail-closed opinion; hasModel
// simply reports whether a non-empty top-level "model" STRING was found
// within the peek window. It is skipped, at no cost — no read at all —
// for a Content-Type isPassthroughBinaryContentType recognizes as
// genuinely binary (multipart uploads, audio/image/video, octet-stream):
// none of those carry a JSON "model" field to find. Every OTHER
// Content-Type, including "text/plain" or an absent header, IS read: the
// caller decides what a "not found" result means, not this gate — see
// maxModelPeekBytes' own doc comment for why the caller's answer is
// fail-closed.
//
// The scan (scanTopLevelModel) is a bounded JSON TOKEN walk, not a full
// json.Unmarshal: the peek window is very often a PREFIX of a larger body
// (a long messages/documents array trailing the model field), and
// Unmarshal requires a complete, valid document — it would reject a
// legitimately-truncated-but-found "model" as if it were malformed JSON.
//
// truncated is the round-3 fix (coordinator ruling, 2026-08-22): it is
// true exactly when BOTH (a) the read hit the maxModelPeekBytes cap
// itself — io.ReadFull returned n == len(head) with no error, meaning
// unread bytes genuinely remain on r.Body beyond what was scanned — AND
// (b) scanTopLevelModel could not confirm the top-level JSON object
// actually closed within that window. Only that combination means a
// LATER "model" key could exist past the peek and this function cannot
// rule it out; a body that is merely shorter than the window and
// happens to be malformed (never closes, but nothing more of it exists
// either) is NOT truncated — there is no hidden byte range to distrust,
// and returning found=false there already denies via the caller's
// existing "model could not be determined" branch. See handlePassthrough's
// own doc comment for what a caller does with truncated=true: DENY,
// fail-closed, regardless of what model/hasModel came back, because a
// found model under truncation cannot be trusted — RFC 8259 permits
// duplicate keys and every mainstream JSON parser an upstream might use
// resolves them last-wins, so a second "model" sitting just past
// maxModelPeekBytes would authorize against the first and execute the
// second: `{"model":"cheap", <64KiB of padding>, "model":"expensive"}`.
//
// err is non-nil only for a genuine body-read failure (client disconnect,
// deadline) distinct from merely reaching the end of a body shorter than
// the peek window — the caller maps err to the same 400 runUnified's own
// body-read failure uses (routes_unified.go).
func peekPassthroughModel(r *http.Request) (model string, hasModel, truncated bool, err error) {
	if r.Body == nil || isPassthroughBinaryContentType(r.Header.Get("Content-Type")) {
		return "", false, false, nil
	}

	head := make([]byte, maxModelPeekBytes)
	n, readErr := io.ReadFull(r.Body, head)
	capped := false
	switch readErr { //nolint:errorlint // io.ReadFull returns these sentinels bare, never wrapped — see its own doc comment
	case nil:
		// Read exactly maxModelPeekBytes: the body has at least that much
		// left unread on r.Body, restored below via io.MultiReader — a
		// LATER "model" key beyond this window cannot be ruled out unless
		// scanTopLevelModel confirms the object closed within it.
		capped = true
	case io.EOF, io.ErrUnexpectedEOF:
		// Body was shorter than the peek window (io.EOF: empty; io.
		// ErrUnexpectedEOF: 0 < n < len(head)) — head[:n] IS the whole
		// body; r.Body is now exhausted, so appending it below is a
		// harmless immediate EOF. No bytes exist beyond what was scanned,
		// so "truncated" (in the hidden-duplicate sense) cannot apply.
	default:
		return "", false, false, readErr
	}
	head = head[:n]
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))

	model, hasModel, closed := scanTopLevelModel(head)
	return model, hasModel, capped && !closed, nil
}

// scanTopLevelModel walks head — a bounded, possibly-truncated PREFIX of
// a JSON request body (peekPassthroughModel's own maxModelPeekBytes cap)
// — as a stream of JSON tokens (encoding/json's Decoder.Token, the
// documented mechanism for parsing a document incrementally) and returns
// the string value of its top-level "model" key, without ever requiring
// head to be a complete, valid JSON document on its own: a legitimate
// request's "model" field sits well within the cap even when a trailing
// field (a long messages/documents array) does not, and this walk finds
// it regardless of what truncation or garbage follows. found is false —
// not an error — for anything else: head is not a JSON object at all,
// "model" never appears among its top-level keys before head runs out,
// or "model" is present but its value is not a string.
//
// closed reports whether this walk actually confirmed the top-level
// object's closing '}' within head — round-3 fix (coordinator ruling,
// 2026-08-22): every early-return branch below (a token error, a
// non-string key, a duplicate "model", a nested value that never
// finishes) sets closed=false, and the ONLY way to reach closed=true is
// to fall all the way through the main loop and then successfully
// consume a literal '}' token afterward. dec.More() alone cannot make
// this distinction — its own doc comment says it "reports whether there
// is another element", but internally it treats "the next byte is the
// closing delimiter" and "the underlying reader ran out of bytes mid-
// object" identically (both make its own peek fail and it just returns
// false either way) — so this function does its own explicit
// closing-token check rather than trusting a loop-exited-via-More()
// alone. peekPassthroughModel combines closed with its own knowledge of
// whether the read actually hit the byte cap (there is more of r.Body
// left unread) to decide whether a "found" model can be trusted.
func scanTopLevelModel(head []byte) (model string, found, closed bool) {
	seenModel := false
	dec := json.NewDecoder(bytes.NewReader(head))
	tok, err := dec.Token()
	if err != nil {
		return "", false, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false, false
	}
	for dec.More() {
		keyTok, keyErr := dec.Token()
		if keyErr != nil {
			return model, found, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return model, found, false // malformed: expected an object key
		}
		valTok, valErr := dec.Token()
		if valErr != nil {
			return model, found, false
		}
		if key == "model" {
			// A DUPLICATE top-level "model" fails closed, matching
			// extractMultipartModel's errDuplicateModelField rule
			// (routes_media.go). Returning on the FIRST occurrence would be
			// an authorization bypass: RFC 8259 permits duplicate names and
			// every mainstream parser an upstream might use (Go's
			// encoding/json, Python json.loads, Node JSON.parse, Ruby, PHP,
			// Newtonsoft) resolves them LAST-wins, while this body is
			// forwarded byte-for-byte unmodified — so a client could get
			// {"model":"allowed","model":"expensive"} authorized against the
			// first and executed as the second. Ambiguity is not
			// resolvable here without rewriting the client's bytes, so the
			// only safe answer is to refuse to answer.
			if seenModel {
				return "", false, false
			}
			seenModel = true
			if s, isStr := valTok.(string); isStr && s != "" {
				model = s
				found = true
			}
			continue
		}
		if d, ok := valTok.(json.Delim); ok && (d == '{' || d == '[') {
			if skipErr := skipJSONValue(dec); skipErr != nil {
				return model, found, false
			}
		}
		// Otherwise valTok was a scalar (string/float64/bool/nil) other
		// than "model" — nothing further to do; loop to the next key.
	}
	// dec.More() returned false above: either the object legitimately
	// closed, or the decoder simply ran out of bytes searching for the
	// next token — see this function's own doc comment for why More()
	// cannot tell those apart. Explicitly consume the closing '}' to find
	// out which; only a real, present '}' confirms the object closed
	// within head.
	closeTok, err := dec.Token()
	if err != nil {
		return model, found, false
	}
	if d, ok := closeTok.(json.Delim); !ok || d != '}' {
		return model, found, false
	}
	return model, found, true
}

// skipJSONValue consumes the remainder of a nested JSON array/object
// value whose opening delimiter dec has already emitted, so
// scanTopLevelModel's own dec.More() call — which reports position in
// whatever object/array dec is CURRENTLY inside — correctly reports the
// outer top-level object's state again once this returns, rather than
// still appearing "inside" the value just skipped.
func skipJSONValue(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// allowsPassthroughModel reports whether grp's Models glob authorizes
// model for a native passthrough request already pinned to providerName
// by the URL (security review, 2026-08-22, round 2 — fixes a matcher-
// parity bug: gluing providerName+"/" onto model UNCONDITIONALLY, the
// round-1 version's approach, produces "openai/openai/x" and false-403s a
// legitimate client whose body already carries the prefixed form).
//
// This reuses modelRegistry.splitConfiguredProvider — the SAME rule
// resolve/resolveAgainst apply to a client-given model id (registry.go):
// a leading segment is stripped as a provider prefix ONLY when it names
// an ACTUALLY CONFIGURED provider, never blindly assumed. Three
// candidates are checked, covering this package's two established
// conventions for turning one model reference into every glob form an
// operator might reasonably write:
//   - model, exactly as the client's body sent it (resolveAgainst's own
//     "requestedID" candidate)
//   - bareModel, model's own suffix once any genuinely-configured
//     provider prefix is stripped — equal to model when it carried none
//     (resolveAgainst's own "upstreamModel" candidate)
//   - providerName+"/"+bareModel, the canonical id for the provider THIS
//     route is actually pinned to (listFor's own p+"/"+id synthesis) —
//     lets an operator write their glob against the addressed provider
//     even when the client sends a bare model id
func (g *Gateway) allowsPassthroughModel(grp *group, providerName, model string) bool {
	bareModel := model
	if _, rest, ok := g.registry.splitConfiguredProvider(model); ok {
		bareModel = rest
	}
	return grp.allowsModel(model) || grp.allowsModel(bareModel) || grp.allowsModel(providerName+"/"+bareModel)
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
// when the group restricts models at all (403), path allowlist (403) —
// before capability checks (Upgrade→501, path validity→400) before rate
// limits (429/503) — a caller who cannot use providerName/model/path at
// all learns that first, rather than learning something about how they
// tried to use it. The provider-level Passthrough toggle (ProviderConfig,
// llmgateway.go) is checked earlier still, by ServeHTTP's own route gate
// — a disabled provider never reaches this function at all, reported as
// the ordinary unknown-route 404 instead.
//
// MODEL ENFORCEMENT (security review, 2026-08-22, round 2 — closes a
// round-1 bypass): enforcement runs ONLY for a group with a non-empty
// Models list (grp.hasModelRestriction) — a group that has not opted into
// model restriction has nothing allowsModel could reject, so the request
// body is never even read, exactly as before this feature existed at all.
// For a restricted group, peekPassthroughModel reads (bounded,
// maxModelPeekBytes) for a top-level "model" field and this function
// checks it via allowsPassthroughModel — the round-1 version instead
// gated the READ ITSELF on Content-Type containing "application/json",
// which a client fully controls: sending "text/plain", an unexpected
// value, or no Content-Type at all skipped the check outright and let
// {"model":"EXPENSIVE"} straight through. There is no such escape now: a
// restricted group's every non-genuinely-binary request body is
// inspected (isPassthroughBinaryContentType's skip-list, not an
// allow-list), and one where "model" cannot be found within the peek
// window is DENIED (403) — fail CLOSED, not open, the opposite direction
// from a body with no inspectable model under round 1. This is
// deliberate: it only affects a group that already opted into model
// restriction, and a legitimate LLM request always carries "model" well
// within maxModelPeekBytes. See maxModelPeekBytes' own doc comment for
// the Gemini-passthrough interaction this creates (its model id lives in
// the URL, never the body) and the operator-facing contract.
//
// PADDED-DUPLICATE MODEL (round 3, 2026-08-22, coordinator ruling — closes
// the round-2 fix's own residual gap): round 2 made a duplicate top-level
// "model" WITHIN the peek window fail closed (scanTopLevelModel), but a
// client can still pad the body so a second "model" key sits just PAST
// maxModelPeekBytes: `{"model":"cheap", <64KiB padding>,
// "model":"expensive"}` — the peek reports "cheap" (the only one it saw),
// the body is forwarded byte-for-byte, and the upstream (last-wins on
// every mainstream JSON parser) executes "expensive". Fixed the same way:
// peekPassthroughModel now also reports truncated=true whenever the read
// hit the byte cap AND the scanned window never confirmed the top-level
// object actually closed — meaning a later duplicate cannot be ruled
// out — and this function denies (403, a message distinct from "model
// could not be determined") BEFORE even looking at hasModel/model in that
// case. This only ever applies to a restricted group whose body was
// large enough to hit maxModelPeekBytes in the first place; an
// unrestricted group never calls peekPassthroughModel at all (unchanged
// from round 2), and the unified /v1/* route remains the supported path
// for a legitimately large restricted-group request.
//
// PATH ALLOWLIST (same review): GroupConfig.PassthroughPaths, when
// non-empty, additionally restricts which rest path this group's
// passthrough requests may address (grp.allowsPassthroughPath). Empty
// (the default, matchesGlob's own empty-means-all contract) allows every
// path, exactly as before this field existed — see its own doc comment
// for a path.Match footgun this allow-list inherits (a literal "*"
// pattern does NOT mean "allow everything").
//
// For a Gemini provider, rest is appended to base() exactly as the client
// sent it: there is no model extraction or URL rewriting here, so a
// Gemini passthrough client must address it with Gemini's own native URL
// structure, including its "/v1beta/models/{model}:generateContent"
// paths — injectAuth still sets the same x-goog-api-key header it sets
// for every other Gemini request.
func (g *Gateway) handlePassthrough(w http.ResponseWriter, r *http.Request, u *user, grp *group, providerName, rest string) {
	if !grp.allowsProvider(providerName) {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "provider access denied")
		return
	}

	if grp.hasModelRestriction() {
		model, hasModel, truncated, err := peekPassthroughModel(r)
		if err != nil {
			writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
			return
		}
		// Checked BEFORE hasModel (round-3 coordinator ruling, 2026-08-22):
		// a truncated peek means a "model" found within the window cannot
		// be trusted — a duplicate top-level "model" beyond
		// maxModelPeekBytes may still exist and would win upstream
		// (last-wins parsing) — so this must fail closed regardless of
		// whatever hasModel/model peekPassthroughModel also returned. See
		// peekPassthroughModel's own doc comment for the full mechanism.
		if truncated {
			writeOAIError(w, http.StatusForbidden, "invalid_request_error", "request body exceeds the model-enforcement window; a top-level model field beyond it cannot be safely authorized")
			return
		}
		if !hasModel {
			writeOAIError(w, http.StatusForbidden, "invalid_request_error", "model could not be determined")
			return
		}
		if !g.allowsPassthroughModel(grp, providerName, model) {
			writeOAIError(w, http.StatusForbidden, "invalid_request_error", "model access denied")
			return
		}
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
	// feat: instrument upstream latency — gated on metricsEnabled, same
	// reasoning as runUnified's identical wiring (routes_unified.go). No
	// model here either, for the identical reason the attemptRecorder
	// above passes "": the upstream model lives in the response body,
	// read only after this attempt already resolved (comment above).
	if metricsEnabled(g.cfg) {
		r = r.WithContext(withLatencyRecorder(r.Context(), func(sample latencySample) {
			g.recordLatency(providerName, "", sample)
		}))
	}

	result, ok := g.proxyUpstream(w, r, upstreamURL, adapter.httpClient(), adapter.injectAuth, providerCredentialRetargetHeaders, true, "passthrough (provider "+providerName+")", adapter.requestTimeout())
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

	// respModel: the RESPONSE body's own reported model id (extractPassthroughUsage
	// reads it, when present, from the upstream's reply) — never named
	// "model" here, so it can never be confused with (or shadow) the
	// REQUEST body's "model" field the enforcement block above already
	// consumed via peekPassthroughModel (review fix, 2026-08-22, round 2).
	respUsage, respModel, unmarshalErr := extractPassthroughUsage(adapter.typeName(), providerName, result.tee.buf.Bytes())
	if unmarshalErr != nil {
		g.logf("passthrough: response body did not decode as JSON for usage accounting (provider %q): %v", providerName, unmarshalErr)
	}
	cost := unifiedCostMicros(providerName+"/"+respModel, respModel, respUsage, g.cfg.Pricing)
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
// The RESPONSE side is hardened too (security review finding 5, round 3,
// 2026-08-22): every hop-by-hop header AND dangerousResponseHeaders
// (Set-Cookie, Strict-Transport-Security, Content-Security-Policy) plus
// dangerousResponseHeaderPrefixes (Access-Control-*) are stripped before
// the upstream's response headers reach the client, for BOTH callers —
// an upstream provider or an in-cluster MCP/A2A target must never get to
// plant a cookie or dictate a security/CORS policy on this gateway's own
// origin merely by setting it on a response this function was only ever
// asked to relay. See dangerousResponseHeaders' own doc comment for why
// this is a narrow deny-list, not an allowlist.
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
//
// timeout (feature: request timeout) arms an idle-progress watchdog
// (watchdogBody, timeout.go) around resp.Body: handlePassthrough passes
// its adapter's own requestTimeout(), handleTargetProxy passes
// g.targetTimeout (mcp_a2a.go) — both share the SAME defect this feature
// fixes (client, above, built via newAdapterHTTPClient, previously had no
// timeout at all), so both are covered the same way. The Transport-level
// half (ResponseHeaderTimeout) is already set on client itself and needs
// no separate wiring here.
func (g *Gateway) proxyUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, client *http.Client, injectAuth func(*http.Request), extraStrip map[string]bool, accountJSON bool, logPrefix string, timeout time.Duration) (result proxyResult, ok bool) {
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
	// reqCtx/cancel bound the response body to timeout via watchdogBody
	// (timeout.go), the same idle-progress mechanism upstreamBytes
	// (providers.go) applies to every provider-adapter call — cancel is
	// released either by watchdogBody (once resp.Body exists) or directly
	// below, on a build/send failure that never produced a body to wrap.
	reqCtx, cancel := context.WithCancel(r.Context())
	upstreamReq, err := http.NewRequestWithContext(reqCtx, r.Method, upstreamURL, bodyReader) //nolint:gosec // operator-fixed host, rest is traversal-checked and forwarded escaped; see comment above
	if err != nil {
		cancel()
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

	// start: captured just before the request is actually sent, matching
	// upstreamBytes' own identical capture (providers.go) — see
	// watchdogBody.armLatency's doc comment (timeout.go) for why this,
	// not whatever moment newWatchdogBody itself runs at, is what "just
	// before the upstream request is sent" (task brief) means here.
	start := time.Now()
	resp, err := client.Do(upstreamReq) //nolint:gosec // same upstreamReq built above; operator-fixed host, traversal-checked, see its construction comment
	// Feature A (v0.22): proxyUpstream makes exactly one attempt (no
	// retry.go policy wraps this path), so this fires once per call,
	// whichever way it resolves. Only handlePassthrough's own context ever
	// carries a recorder (attemptRecorderFromContext, providers.go) —
	// handleTargetProxy (mcp_a2a.go), this function's other caller, never
	// wraps r's context this way, so an MCP/A2A target proxy attempt is
	// correctly never accounted as provider traffic. rec is kept in scope
	// (not just checked inline) so the watchdogBody below can reuse it
	// too — a mid-body stall must reach the SAME provider-health
	// accounting a build/send failure already does (coordinator
	// adversarial review, 2026-08-23, finding F4).
	rec := attemptRecorderFromContext(r.Context())
	// latRec: nil for the MCP/A2A target proxy (handleTargetProxy,
	// mcp_a2a.go, never wraps r's context this way — the SAME exclusion
	// rec's own doc comment above already documents for attemptRecorder),
	// non-nil for native passthrough whenever metrics collection is
	// enabled (handlePassthrough's own withLatencyRecorder wiring, above
	// in this file).
	latRec := latencyRecorderFromContext(r.Context())
	if rec != nil {
		rec(resp, err)
	}
	if err != nil {
		cancel()
		if errors.Is(err, context.Canceled) {
			g.logf("%s: client canceled request: %v", logPrefix, err)
			return proxyResult{}, false
		}
		g.errorf("%s: upstream connection error: %v", logPrefix, err)
		writeOAIError(w, http.StatusBadGateway, "server_error", "upstream connection error")
		return proxyResult{}, false
	}
	// logPrefix, not a hardcoded "provider %q": handleTargetProxy's own
	// calls here (mcp_a2a.go) pass "mcp target (name ...)"/"a2a target
	// (name ...)" — an MCP/A2A target is not a provider, and the watchdog
	// error text must not claim it is (finding F9).
	wb := newWatchdogBody(resp.Body, cancel, timeout, logPrefix, rec)
	if latRec != nil {
		wb.armLatency(start, isEventStreamResponse(resp), latRec)
	}
	resp.Body = wb
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	copyHeadersExcept(w.Header(), resp.Header, hopByHopHeaders, dangerousResponseHeaders)
	stripHeaderPrefixes(w.Header(), dangerousResponseHeaderPrefixes)
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
		// errors.Is(err, context.Canceled) here means the CLIENT went away
		// mid-copy — reader wraps resp.Body, which is now a watchdogBody
		// (above): its own Read never lets a timeout error wrap
		// context.Canceled (watchdogBody's own doc comment, timeout.go),
		// so this check still correctly separates a genuine client
		// disconnect from this gateway's own idle-progress watchdog firing
		// — the latter falls through to the errorf below, unchanged.
		if errors.Is(err, context.Canceled) {
			g.logf("%s: client canceled request: %v", logPrefix, err)
			return proxyResult{isJSON: isJSON, tee: tee}, false
		}
		g.errorf("%s: stream response body: %v", logPrefix, err)
		return proxyResult{isJSON: isJSON, tee: tee}, false
	}
	return proxyResult{isJSON: isJSON, tee: tee}, true
}
