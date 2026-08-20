package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxPassthroughBytes caps a client passthrough request body: 32MiB,
// generous enough for a native multimodal payload (audio, image, or a
// large document) that the unified routes' 10MiB maxRequestBytes does not
// need to accommodate, while still bounding memory against an oversized
// or malicious body.
const maxPassthroughBytes = 32 << 20

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
// "/{providerName}/{rest...}". ok is false for an empty or "/"-only path,
// which carries no candidate provider name at all — the caller falls
// through to its next-route/404 handling in that case. passthroughRoute
// makes no claim the returned providerName is actually configured; the
// caller checks that against g.adapters before treating the request as
// passthrough.
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
// providerTypeOpenAI/Anthropic/Gemini). A body that fails to decode, or
// that carries none of the recognized usage shapes, returns a zero usage
// — the caller's account call is a no-op for zero usage, not a wrong
// charge. The model id falls back to "unknown/"+providerName when the
// response carries no "model" field of its own, which is always true for
// Gemini's native response.
func extractPassthroughUsage(typeName, providerName string, body []byte) (usage, string) {
	var payload passthroughUsagePayload
	_ = json.Unmarshal(body, &payload) // best-effort; zero usage/empty model on decode failure

	model := payload.Model
	if model == "" {
		model = "unknown/" + providerName
	}

	switch typeName {
	case providerTypeOpenAI:
		return usage{prompt: payload.Usage.PromptTokens, completion: payload.Usage.CompletionTokens}, model
	case providerTypeAnthropic:
		return usage{prompt: payload.Usage.InputTokens, completion: payload.Usage.OutputTokens}, model
	case providerTypeGemini:
		return usage{prompt: payload.UsageMetadata.PromptTokenCount, completion: payload.UsageMetadata.CandidatesTokenCount}, model
	default:
		return usage{}, model
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
// For a Gemini provider, rest is appended to base() exactly as the client
// sent it: there is no model extraction or URL rewriting here, so a
// Gemini passthrough client must address it with Gemini's own native URL
// structure, including its "/v1beta/models/{model}:generateContent"
// paths — injectAuth still sets the same x-goog-api-key header it sets
// for every other Gemini request.
func (g *Gateway) handlePassthrough(w http.ResponseWriter, r *http.Request, u *user, grp *group, providerName, rest string) {
	if r.Header.Get("Upgrade") != "" {
		writeOAIError(w, http.StatusNotImplemented, "invalid_request_error", "websocket/upgrade passthrough not supported")
		return
	}

	if !grp.allowsProvider(providerName) {
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "provider access denied")
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
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bodyReader) //nolint:gosec // path/query passthrough onto an operator-fixed host; see comment above
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
	copyHeadersExcept(upstreamReq.Header, r.Header, hopByHopHeaders, gatewayCredentialHeaders)
	adapter.injectAuth(upstreamReq)

	resp, err := adapter.httpClient().Do(upstreamReq) //nolint:gosec // same upstreamReq built above; host is operator-fixed, see its construction comment
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

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if readErr != nil {
			g.errorf("passthrough: read response body (provider %q): %v", providerName, readErr)
			return
		}
		if _, err := fw.Write(body); err != nil {
			g.errorf("passthrough: write response body (provider %q): %v", providerName, err)
			return
		}
		respUsage, model := extractPassthroughUsage(adapter.typeName(), providerName, body)
		cost := unifiedCostMicros(providerName+"/"+model, model, respUsage, g.cfg.Pricing)
		g.limiter.account(scopes, respUsage, cost)
		return
	}

	// Streaming or non-JSON: usage is unknowable from here, so only the
	// request itself gets accounted — already done by checkAndCount above.
	if _, err := io.Copy(fw, resp.Body); err != nil {
		g.errorf("passthrough: stream response body (provider %q): %v", providerName, err)
	}
}
