package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
)

// messagesPath is the inbound Anthropic Messages API route this gateway
// serves: POST /v1/messages. Distinct from anthropicMessagesPath
// (provider_anthropic.go), which is the SAME literal path string but
// names where an anthropic-type adapter's OWN outbound call lands on its
// configured upstream — two different constants for two different
// directions, kept apart so a future change to one is never mistaken for
// the other.
const messagesPath = "/v1/messages"

// cacheEndpointMessages is cacheKey's endpoint argument (cache.go) for
// this route, mirroring cacheEndpointChat/cacheEndpointEmbeddings
// (routes_unified.go): keeps a /v1/messages request from ever colliding
// into a /v1/chat/completions cache entry even when a request happens to
// canonicalize to an identical provider/upstreamModel/body — the two
// routes answer in different wire shapes (Anthropic vs OpenAI), so a
// cache hit across them would hand a client the wrong envelope entirely.
const cacheEndpointMessages = "messages"

// handleMessages implements POST /v1/messages: the Anthropic Messages API
// shape, inbound. It shares every pipeline step routes_unified.go's
// runUnified already established for /v1/chat/completions and
// /v1/embeddings — body-admission-gated decode, model resolution,
// request-rate limiting, response caching, usage accounting, and
// per-provider attempt recording — by calling the exact same functions
// runUnified calls, rather than reimplementing any of them: g.admitRequest,
// g.readAndDecodeUnifiedBody, g.registry.resolve, withAttemptRecorder,
// g.limiter.account, unifiedCostMicros, cacheKey/groupCacheEnabled/
// effectiveTTL, and cacheCaptureWriter. Only two things are genuinely
// specific to this route and are NOT shared: which wire shape a request/
// response is in (Anthropic, not OpenAI — see callAnthropicMessagesPassthrough/
// callTranslatedMessages below) and which error envelope shape a failure
// gets written in (Anthropic's {"type":"error","error":{...}}, not
// OpenAI's {"error":{...}} — see writeAnthropicError and its callers,
// below).
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	scopes, ok := g.admitRequest(sw, u, grp)
	if !ok {
		return
	}

	body, req, ok := g.readAndDecodeUnifiedBody(sw, r)
	if !ok {
		return
	}

	requestedModel, _ := req["model"].(string)
	if requestedModel == "" {
		writeAnthropicError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	// Streaming is rejected cleanly and explicitly here, before model
	// resolution or any upstream call: this cut of the route ships
	// non-streaming only (brief scope) — a follow-up branch adds the
	// Anthropic SSE event translator (message_start/content_block_delta/
	// message_stop), a genuinely different event model from OpenAI's
	// stream shape this route already knows how to forward. A caller
	// that asked for streaming must see a clear, typed error, never a
	// silently-ignored flag answered non-streamed.
	if streaming, _ := req["stream"].(bool); streaming {
		writeAnthropicError(sw, http.StatusNotImplemented, "invalid_request_error", "streaming is not yet supported on /v1/messages")
		return
	}

	adapter, upstreamModel, canonical, err := g.registry.resolve(requestedModel, grp)
	if err != nil {
		writeAnthropicModelResolveError(sw, err)
		return
	}

	req["model"] = upstreamModel
	delete(req, gatewayAliasKey) // defensive: this route never sets it, but a client-sent "__alias" field must never reach either upstream shape

	cacheable := g.cache != nil && groupCacheEnabled(grp)
	var cacheKeyStr string
	if cacheable {
		cacheKeyStr = cacheKey(adapter.name(), upstreamModel, requestedModel, cacheEndpointMessages, req)
		if cached, hit := g.cache.lookup(cacheKeyStr); hit {
			sw.Header().Set("X-Llmgw-Cache", "hit")
			if cached.ContentType != "" {
				sw.Header().Set("Content-Type", cached.ContentType)
			}
			sw.WriteHeader(cached.Status)
			_, _ = sw.Write(cached.Body)
			return
		}
		sw.Header().Set("X-Llmgw-Cache", "miss")
	}

	var respWriter http.ResponseWriter = sw
	var capture *cacheCaptureWriter
	if cacheable {
		capture = newCacheCaptureWriter(sw, g.cache.maxBodyBytes)
		respWriter = capture
	}

	ctx := withAttemptRecorder(r.Context(), func(resp *http.Response, attemptErr error) {
		g.limiter.recordProviderAttempt(adapter.name(), upstreamModel, resp, attemptErr)
	})

	var result usage
	var callErr error
	if adapter.typeName() == providerTypeAnthropic {
		result, callErr = g.callAnthropicMessagesPassthrough(ctx, respWriter, adapter, req)
	} else {
		result, callErr = g.callTranslatedMessages(ctx, respWriter, adapter, req, requestedModel)
	}

	// Zero-usage fallback, mirroring runUnified's own (routes_unified.go):
	// a non-streaming failure always carries zero usage by contract, so
	// this is a no-op whenever callErr != nil (account below skips every
	// write once both total tokens and cost are zero). Unlike runUnified,
	// there is no streaming branch to skip here — streaming was already
	// rejected above, so every call into this route that reaches here is
	// non-streaming.
	if callErr == nil && result.total() == 0 {
		result.prompt = int64(math.Ceil(float64(len(body)) / 4))
		result.estimated = true
	}
	g.limiter.account(scopes, result, unifiedCostMicros(canonical, upstreamModel, result, g.cfg.Pricing))
	if result.estimated {
		g.logf("messages route: usage for model %q logged as estimated (%d prompt tokens derived from request body size, not the provider's reported usage)", canonical, result.prompt)
	}

	if cacheable && callErr == nil && capture.status == http.StatusOK && !capture.oversize {
		g.cache.store(cacheKeyStr, capture.status, capture.contentType, capture.buf.Bytes(), effectiveTTL(g.cache, grp))
	}

	if callErr != nil {
		g.handleMessagesAdapterError(sw, callErr, adapter.name())
		return
	}
}

// callAnthropicMessagesPassthrough handles a /v1/messages request already
// resolved to an anthropic-type provider: forward req to that provider's
// own Messages endpoint UNTRANSLATED (brief: "pass through, no
// translation of the message body") and copy its response back to w
// exactly as Anthropic sent it. req's only mutation, applied by the
// caller (handleMessages) before this function ever runs, is the "model"
// field rewrite to the resolved upstream model id — the same rewrite
// every route in this package applies before any adapter call
// (runUnified, resolveMediaModel); it is request ROUTING, not message
// translation.
//
// This deliberately bypasses providerAdapter.chatCompletion:
// chatCompletion's contract (providers.go) is OpenAI-shape in, OpenAI-
// shape out for every provider type — anthropicAdapter.chatCompletion
// always calls anthropicRequestFromOpenAI on whatever it is handed, which
// would corrupt an already-Anthropic-shaped body instead of passing it
// through.
//
// The response is written back byte-for-byte: this function reads
// Anthropic's raw JSON only to extract usage (input_tokens/output_tokens,
// via the existing anthropicResponseBody type, translate_anthropic.go —
// reused rather than declared again) for accounting, and never re-
// encodes what reaches the client. One accepted consequence: the
// response's own "model" field carries the upstream model id Anthropic
// actually served, not the client's requested alias — unlike every
// translating adapter's gatewayAliasKey echo (routes_unified.go's own
// doc comment on that mechanism), rewriting it here would mean decoding
// and re-marshaling the full response body, which is itself a
// translation step the brief's "no translation of the message body" rule
// for this branch rules out.
func (g *Gateway) callAnthropicMessagesPassthrough(ctx context.Context, w http.ResponseWriter, adapter providerAdapter, req map[string]any) (usage, error) {
	// Plain type assertion, not an added providerAdapter interface method:
	// this branch only ever runs when the caller already confirmed
	// adapter.typeName() == providerTypeAnthropic, and buildAdapters
	// (providers.go) only ever constructs a *anthropicAdapter for that
	// type, so the assertion is guaranteed to succeed. aa.retry is
	// nil-safe either way (retryPolicy.do's own contract, retry.go), so a
	// failed assertion still degrades safely to one-attempt behavior
	// rather than panicking.
	aa, _ := adapter.(*anthropicAdapter)
	var retry *retryPolicy
	if aa != nil {
		retry = aa.retry
	}

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	adapter.injectAuth(&http.Request{Header: hdr})

	resp, err := upstreamJSON(ctx, adapter.httpClient(), http.MethodPost, adapter.base()+anthropicMessagesPath, hdr, req, retry)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return usage{}, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	var parsed anthropicResponseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return usage{}, fmt.Errorf("%w: decode anthropic response: %w", errUpstream, err)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)

	return usage{prompt: parsed.Usage.InputTokens, completion: parsed.Usage.OutputTokens}, nil
}

// callTranslatedMessages handles a /v1/messages request resolved to a
// non-anthropic-type provider (openai, or gemini — both implement
// providerAdapter.chatCompletion against the SAME OpenAI-shaped
// contract, providers.go, regardless of what their own upstream wire
// format is): translate req from Anthropic's shape to OpenAI's
// (openAIRequestFromAnthropic, translate_anthropic.go), run it through
// the resolved adapter's own chatCompletion exactly as routes_unified.go's
// handleChat does, capturing what it writes into an in-memory recorder
// instead of letting it reach the client directly, then translate that
// captured OpenAI-shaped response back to Anthropic's shape
// (anthropicResponseFromOpenAI) before writing the result to w.
func (g *Gateway) callTranslatedMessages(ctx context.Context, w http.ResponseWriter, adapter providerAdapter, req map[string]any, requestedModel string) (usage, error) {
	openaiReq, err := openAIRequestFromAnthropic(req)
	if err != nil {
		return usage{}, err
	}

	rec := newMessagesResponseRecorder()
	result, err := adapter.chatCompletion(ctx, rec, openaiReq)
	if err != nil {
		return usage{}, err
	}

	out, err := anthropicResponseFromOpenAI(rec.body.Bytes(), requestedModel)
	if err != nil {
		return usage{}, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return usage{}, fmt.Errorf("%w: marshal translated response: %w", errUpstream, err)
	}

	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)

	return result, nil
}

// messagesResponseRecorder is a minimal in-memory http.ResponseWriter
// callTranslatedMessages hands to a resolved adapter's own chatCompletion
// in place of the real client connection. chatCompletion's contract
// (providerAdapter, providers.go) is OpenAI-shape in, OpenAI-shape out
// regardless of provider type, and this route needs to translate that
// output to Anthropic's shape before anything reaches the real client —
// so the adapter's write is captured here and never forwarded; the real
// ResponseWriter only ever sees the translated bytes callTranslatedMessages
// writes afterward. Streaming can never reach this recorder: handleMessages
// rejects "stream": true before either call path runs, so chatCompletion's
// non-streaming forwardJSON is the only path that ever writes to it.
type messagesResponseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newMessagesResponseRecorder() *messagesResponseRecorder {
	return &messagesResponseRecorder{header: http.Header{}}
}

func (r *messagesResponseRecorder) Header() http.Header { return r.header }

func (r *messagesResponseRecorder) WriteHeader(status int) { r.status = status }

func (r *messagesResponseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK // mirrors net/http's implicit-200 default
	}
	return r.body.Write(b)
}

// writeAnthropicError writes an Anthropic Messages API-shaped error
// envelope: {"type":"error","error":{"type":...,"message":...}} — what an
// Anthropic SDK client parses. errType uses the same vocabulary this
// gateway's OpenAI-shaped envelope already uses everywhere else
// (writeOAIError, errors.go: invalid_request_error, authentication_error,
// rate_limit_error, server_error), so an operator reading logs sees one
// consistent taxonomy regardless of which route answered; only the JSON
// shape around it differs, to match what this route's own clients expect.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
}

// writeAnthropicModelResolveError is writeModelResolveError's
// (routes_unified.go) Anthropic-shaped counterpart: the SAME status-code
// decision (aliasTargetError/errModelUnknown/errModelDenied), written
// through writeAnthropicError instead of writeOAIError. Kept as its own
// small function, rather than a routes_unified.go refactor threading an
// envelope-writer parameter through, so this route's addition stays
// isolated to its own file while a sibling branch is concurrently editing
// registry.go/admin.go — the decision logic below is intentionally
// identical to writeModelResolveError's, not independently re-derived.
func writeAnthropicModelResolveError(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*aliasTargetError); ok {
		writeAnthropicError(w, http.StatusNotFound, "invalid_request_error", aerr.Error())
		return
	}
	switch {
	case errors.Is(err, errModelUnknown):
		writeAnthropicError(w, http.StatusNotFound, "invalid_request_error", "unknown model")
	case errors.Is(err, errModelDenied):
		writeAnthropicError(w, http.StatusForbidden, "invalid_request_error", "model access denied")
	default:
		writeAnthropicError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

// handleMessagesAdapterError is handleAdapterError's (routes_unified.go)
// Anthropic-shaped counterpart, covering the identical set of adapter
// error cases with the identical yaegi-safety rationale (plain type
// assertions for providerHTTPError/translateError, never errors.As —
// see providerHTTPError's own doc comment, providers.go) but writing
// through writeAnthropicError/writeAnthropicProviderUpstreamError instead
// of writeOAIError/writeProviderUpstreamError, and its own "messages
// route:" log prefix so a log line always names which route it came from.
func (g *Gateway) handleMessagesAdapterError(sw *statusTrackingWriter, err error, providerName string) {
	sw.Header().Del("X-Llmgw-Cache")

	if sw.wroteHeader {
		g.errorf("messages route: adapter error after response started (provider %q): %v", providerName, err)
		return
	}

	if perr, ok := err.(*providerHTTPError); ok {
		writeAnthropicProviderUpstreamError(sw, providerName, perr)
		return
	}

	if terr, ok := err.(*translateError); ok {
		status := http.StatusBadRequest
		if terr.notSupported {
			status = http.StatusNotImplemented
		}
		writeAnthropicError(sw, status, "invalid_request_error", terr.msg)
		return
	}

	if errors.Is(err, context.Canceled) {
		g.logf("messages route: client canceled request to provider %q: %v", providerName, err)
		return
	}

	g.errorf("messages route: upstream connection error (provider %q): %v", providerName, err)
	writeAnthropicError(sw, http.StatusBadGateway, "server_error", "upstream connection error")
}

// writeAnthropicProviderUpstreamError is writeProviderUpstreamError's
// (routes_unified.go) Anthropic-shaped counterpart: perr's upstream body
// is embedded under error.upstream (decoded to a JSON value when it
// parses as one, left as a raw string otherwise) inside the Anthropic
// {"type":"error","error":{...}} envelope instead of the OpenAI one.
func writeAnthropicProviderUpstreamError(w http.ResponseWriter, providerName string, perr *providerHTTPError) {
	var upstream any = string(perr.body)
	var parsed any
	if json.Unmarshal(perr.body, &parsed) == nil {
		upstream = parsed
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(perr.status)
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"type": "error",
		"error": map[string]any{
			"type":     "upstream_error",
			"message":  providerName + " upstream error",
			"code":     strconv.Itoa(perr.status),
			"upstream": upstream,
		},
	})
}
