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

// handlePassthrough implements the native provider passthrough route:
// "/{providerName}/{rest...}" reverse-proxies rest, verbatim, to
// providerName's configured upstream, with the client's gateway
// credential swapped for the provider's own. It is a raw proxy, not a
// translation layer like the unified routes — a non-2xx upstream status
// is forwarded to the client exactly as the upstream sent it, not wrapped
// in the gateway's own error envelope the way the unified routes wrap a
// providerHTTPError.
//
// Checks run in this order: group authorization (403) before capability
// checks (Upgrade→501, path validity→400) before rate limits (429/503) —
// a caller who cannot use providerName at all learns that first, rather
// than learning something about how they tried to use it.
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

	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}

	if hasTraversalSegment(rest) {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid path")
		return
	}

	scopes := buildLimitScopes(u, grp)
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

	bodyReader := io.LimitReader(r.Body, maxPassthroughBytes)
	// gosec G704 (SSRF via taint analysis) flags upstreamURL as
	// request-derived: it is, by design — this is a reverse proxy, and its
	// whole job is to forward a client-supplied path/query onto the
	// upstream. The host component is never request-derived: adapter.base()
	// is a fixed, operator-configured URL from Config.Providers, and
	// upstreamURL is built as base()+"/"+rest — string concatenation, not a
	// second url.Parse of rest alone — so no client-supplied value can
	// change the scheme or host url.Parse resolves for the final string.
	// rest is additionally traversal-checked above (hasTraversalSegment
	// rejects any ".." segment in its decoded form before this point is
	// reached) and is forwarded here in its original escaped form — see
	// passthroughRoute's EscapedPath()-based split in llmgateway.go — so a
	// client cannot smuggle an encoded "/" (%2f) past this gateway's own
	// routing only to have a permissive upstream reinterpret it as a real
	// separator.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bodyReader) //nolint:gosec // operator-fixed host, rest is traversal-checked and forwarded escaped; see comment above
	if err != nil {
		g.errorf("passthrough: build upstream request (provider %q): %v", providerName, err)
		writeOAIError(w, http.StatusBadGateway, "server_error", "upstream connection error")
		return
	}
	upstreamReq.ContentLength = r.ContentLength
	if r.ContentLength < 0 || r.ContentLength > maxPassthroughBytes {
		// Unknown or over-cap length: let the transport negotiate chunked
		// transfer instead of advertising a Content-Length the capped
		// bodyReader above may not actually deliver.
		upstreamReq.ContentLength = -1
	}
	copyHeadersExcept(upstreamReq.Header, r.Header, hopByHopHeaders, gatewayCredentialHeaders, clientNegotiationHeaders)
	adapter.injectAuth(upstreamReq)

	resp, err := adapter.httpClient().Do(upstreamReq) //nolint:gosec // same upstreamReq built above; operator-fixed host, traversal-checked, see its construction comment
	if err != nil {
		if errors.Is(err, context.Canceled) {
			g.logf("passthrough: client canceled request to provider %q: %v", providerName, err)
			return
		}
		g.errorf("passthrough: upstream connection error (provider %q): %v", providerName, err)
		writeOAIError(w, http.StatusBadGateway, "server_error", "upstream connection error")
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	copyHeadersExcept(w.Header(), resp.Header, hopByHopHeaders)
	w.WriteHeader(resp.StatusCode)
	fw := newFlushWriter(w)

	// Only a non-streaming JSON response gets teed off for best-effort
	// accounting; every other response streams straight through fw with no
	// extra buffering — a large or slow upstream body must never sit
	// waiting for full receipt before the client sees its first byte.
	isJSON := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json")
	var tee *cappedAccountingBuffer
	var reader io.Reader = resp.Body
	if isJSON {
		tee = &cappedAccountingBuffer{}
		reader = io.TeeReader(resp.Body, tee)
	}

	if _, err := io.Copy(fw, reader); err != nil {
		g.errorf("passthrough: stream response body (provider %q): %v", providerName, err)
		return
	}
	if !isJSON {
		// Streaming or non-JSON: usage is unknowable from here, so only the
		// request itself gets accounted — already done by checkAndCount
		// above.
		return
	}
	if tee.truncated {
		g.logf("passthrough: response body exceeded %d bytes; skipping usage accounting (provider %q)", maxAccountingTeeBytes, providerName)
		return
	}

	respUsage, model, unmarshalErr := extractPassthroughUsage(adapter.typeName(), providerName, tee.buf.Bytes())
	if unmarshalErr != nil {
		g.logf("passthrough: response body did not decode as JSON for usage accounting (provider %q): %v", providerName, unmarshalErr)
	}
	cost := unifiedCostMicros(providerName+"/"+model, model, respUsage, g.cfg.Pricing)
	g.limiter.account(scopes, respUsage, cost)
}
