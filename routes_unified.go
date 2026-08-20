package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
)

// maxRequestBytes caps a client request body the unified routes decode
// into a map[string]any: 10MiB, generous for a chat or embeddings request
// (including an inline base64 image) while bounding memory against an
// oversized or malicious body.
const maxRequestBytes = 10 << 20

// gatewayAliasKey is the request map's internal-convention key carrying
// the client-requested model id through to an adapter (ruling a, ALIAS
// ECHO), distinct from "model" itself, which runUnified rewrites to the
// resolved upstream model id before calling the adapter — an operator's
// model alias must never reach the upstream API, but the client that used
// it must still see it echoed back in the response.
//
// Every adapter deletes this key before marshaling its upstream wire
// body: an openai-type adapter forwards req verbatim, so a leftover
// gatewayAliasKey entry would otherwise reach the real provider as an
// unrecognized request field. anthropic and gemini's adapters, which
// build a brand new response envelope rather than forwarding one
// verbatim, read it (falling back to the upstream model id when absent)
// to set that envelope's "model" field to the client's original id. An
// openai-type adapter's own response is a verbatim passthrough of
// whatever the upstream returned and cannot be rewritten this way — its
// response body carries the upstream's model id, not the alias. This is
// a documented, accepted asymmetry, not a bug.
const gatewayAliasKey = "__alias"

// handleModels implements GET /v1/models: an authenticated caller gets back
// an OpenAI-compatible {"object":"list","data":[...]} envelope of the
// models their group can see, via modelRegistry.listFor.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	u, grp, ok := g.auth.identify(r)
	g.logAuthEvent(ok, authEventUserName(u), r)
	if !ok {
		writeOAIError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"object": "list",
		"data":   g.registry.listFor(grp),
	})
}

// adapterCall abstracts providerAdapter.chatCompletion and .embeddings:
// handleChat and handleEmbeddings share every other step of the pipeline
// below (decode, resolve, enforce limits, account usage, translate
// errors) and differ only in which of these two methods they invoke.
type adapterCall func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)

// handleChat implements POST /v1/chat/completions.
func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	g.runUnified(w, r, u, grp, func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
		return a.chatCompletion(ctx, w, req)
	})
}

// handleEmbeddings implements POST /v1/embeddings.
func (g *Gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	g.runUnified(w, r, u, grp, func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
		return a.embeddings(ctx, w, req)
	})
}

// runUnified is the shared chat/embeddings pipeline: decode the request,
// resolve its model, enforce per-user and per-group limits, invoke the
// adapter via call, then account the resulting usage — even when call
// itself returned an error, so usage captured before a mid-stream failure
// still gets billed — before translating that error into a response. w is
// wrapped in its own statusTrackingWriter so a mid-stream adapter error
// (headers already sent) can be told apart from one that failed before any
// write.
func (g *Gateway) runUnified(w http.ResponseWriter, r *http.Request, u *user, grp *group, call adapterCall) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}

	var req map[string]any
	if err = json.Unmarshal(body, &req); err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	requestedModel, _ := req["model"].(string)
	if requestedModel == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	streaming, _ := req["stream"].(bool)

	adapter, upstreamModel, canonical, err := g.registry.resolve(requestedModel, grp)
	if err != nil {
		writeModelResolveError(sw, err)
		return
	}

	scopes := buildLimitScopes(u, grp)
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		writeLimitViolation(sw, violation)
		return
	}

	req["model"] = upstreamModel

	// cacheable gates every cache step below on spec §2's scope: a
	// non-streaming request, with a working cache (g.cache is nil
	// whenever caching is off or Redis was absent — buildResponseCache,
	// cache.go), for a group that has not opted out (groupCacheEnabled).
	// cacheKey is computed here — after "model" is rewritten to
	// upstreamModel but deliberately BEFORE gatewayAliasKey is injected
	// below — so the hashed request body reflects exactly what goes
	// upstream and never includes gatewayAliasKey, a purely internal
	// echo-back key with no bearing on upstream request equivalence: two
	// different aliases resolving to the same upstream model must share
	// one cache entry.
	cacheable := !streaming && g.cache != nil && groupCacheEnabled(grp)
	var cacheKeyStr string
	if cacheable {
		cacheKeyStr = cacheKey(adapter.name(), upstreamModel, req)
		if cached, hit := g.cache.lookup(cacheKeyStr); hit {
			// A cache hit accounts the request only — checkAndCount
			// above already counted it — never token/cost, and never
			// runs the zero-usage estimate branch below: the client
			// never reached the upstream provider its body size would
			// be estimating against.
			sw.Header().Set("X-Llmgw-Cache", "hit")
			if cached.ContentType != "" {
				sw.Header().Set("Content-Type", cached.ContentType)
			}
			sw.WriteHeader(cached.Status)
			_, _ = sw.Write(cached.Body)
			return
		}
		// Set before call() below writes anything — headers must precede
		// the body a miss is about to produce (spec §2).
		sw.Header().Set("X-Llmgw-Cache", "miss")
	}

	req[gatewayAliasKey] = requestedModel

	// respWriter is sw, wrapped in a capture tee only when this request
	// is cacheable — cacheCaptureWriter buffers everything written so a
	// 200 non-stream response can be stored after call() returns, without
	// any adapter knowing caching exists (routes_unified.go's adapterCall
	// contract is unchanged either way).
	var respWriter http.ResponseWriter = sw
	var capture *cacheCaptureWriter
	if cacheable {
		capture = newCacheCaptureWriter(sw)
		respWriter = capture
	}

	result, callErr := call(adapter, r.Context(), respWriter, req)

	// Usage is accounted before the error branch below runs, not after:
	// every adapter that can fail mid-stream (forwardStream in each of the
	// three provider files) still returns whatever usage it had already
	// captured alongside the error — a usage chunk that arrived just before
	// a dropped connection must still be billed, or a client that aborts
	// right after that frame arrives could repeat the trick to dodge every
	// token/cost budget. A non-streaming failure (providerHTTPError,
	// translateError, or a connection failure before any write) always
	// carries zero usage by contract, so accounting it here is a no-op —
	// account skips every write once both total tokens and cost are zero.
	if callErr == nil && result.total() == 0 {
		if streaming {
			g.logf("unified route: zero usage reported for a streaming response from model %q; accounting the request only", canonical)
		} else {
			result.prompt = int64(math.Ceil(float64(len(body)) / 4))
			result.estimated = true
		}
	}
	g.limiter.account(scopes, result, unifiedCostMicros(canonical, upstreamModel, result, g.cfg.Pricing))
	if result.estimated {
		g.logf("unified route: usage for model %q logged as estimated (%d prompt tokens derived from request body size, not the provider's reported usage)", canonical, result.prompt)
	}

	// A miss stores the response after everything above has already run —
	// accounting must never be skipped or delayed waiting on a cache
	// write. Only a genuine upstream 200 is stored (spec §2's "on 200
	// non-stream, tee and SET"): a non-2xx status never reaches here as
	// capture.status (forwardJSON/translate error paths return
	// *providerHTTPError/*translateError instead of writing through
	// respWriter, so callErr is non-nil and capture.status stays 0), and
	// callErr == nil is checked directly regardless.
	if cacheable && callErr == nil && capture.status == http.StatusOK {
		g.cache.store(cacheKeyStr, capture.status, capture.contentType, capture.buf.Bytes())
	}

	if callErr != nil {
		g.handleAdapterError(sw, callErr, adapter.name())
		return
	}
}

// buildLimitScopes returns the limitScope slice runUnified passes to the
// limiter: a user scope only when u has its own limits configured, then a
// group scope only when grp does (ruling e) — either, both, or neither may
// apply to a given request. The user scope is listed first, so
// checkAndCount reports a user's own violation ahead of their group's when
// both are breached by the same request.
func buildLimitScopes(u *user, grp *group) []limitScope {
	scopes := make([]limitScope, 0, 2)
	if u.limits != nil {
		scopes = append(scopes, limitScope{limits: u.limits, kind: "user", id: u.name})
	}
	if grp.limits != nil {
		scopes = append(scopes, limitScope{limits: grp.limits, kind: "group", id: grp.name})
	}
	return scopes
}

// unifiedCostMicros resolves the price to charge one request's usage
// against: canonical ("provider/model") first, falling back to bare — the
// upstream model id alone — only when canonical has no configured price at
// all, in either overrides or the built-in table. This extends
// costMicros' single-id lookup to the two-id order ruling (d) requires,
// without changing costMicros' own signature: lookupPricing is used purely
// as a side-effect-free existence probe on canonical, and the real
// computation (and its unknown-model warning, when neither id has a price)
// runs through the one costMicros call that is actually charged.
func unifiedCostMicros(canonical, bare string, u usage, overrides map[string]*ModelPricing) int64 {
	if _, ok := lookupPricing(canonical, overrides); ok {
		return costMicros(canonical, u, overrides)
	}
	return costMicros(bare, u, overrides)
}

// writeModelResolveError maps a modelRegistry.resolve error to its HTTP
// envelope: errModelUnknown is a 404 (no configured provider knows this
// model), errModelDenied is a 403 (a real model the caller's group cannot
// use). Any other error is a defensive 500 — resolve's own contract
// promises only these two sentinels, so reaching this branch would be a
// programming error, not a client mistake.
func writeModelResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errModelUnknown):
		writeOAIError(w, http.StatusNotFound, "invalid_request_error", "unknown model")
	case errors.Is(err, errModelDenied):
		writeOAIError(w, http.StatusForbidden, "invalid_request_error", "model access denied")
	default:
		writeOAIError(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

// writeLimitViolation maps a limiter violation to its HTTP envelope
// (ruling b): a storeDown violation — the configured limit store was
// unreachable and failOpen is false — is a 503 server_error, since it is
// not an actual limit breach; any other violation is a 429 rate_limit_error
// carrying v's own message, plus a Retry-After header when the violation
// names a meaningful retry window.
func writeLimitViolation(w http.ResponseWriter, v *limitViolation) {
	if v.storeDown {
		writeOAIError(w, http.StatusServiceUnavailable, "server_error", v.message)
		return
	}
	if v.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(v.retryAfter))
	}
	writeOAIError(w, http.StatusTooManyRequests, "rate_limit_error", v.message)
}

// handleAdapterError translates an adapter's returned error into a
// response, or into nothing at all, per ruling c. sw.wroteHeader is
// checked first, ahead of every other case: an error surfacing after the
// adapter already started writing a response (a mid-stream connection
// drop) must never get a second, conflicting envelope appended, whatever
// kind of error it is.
func (g *Gateway) handleAdapterError(sw *statusTrackingWriter, err error, providerName string) {
	if sw.wroteHeader {
		g.errorf("unified route: adapter error after response started (provider %q): %v", providerName, err)
		return
	}

	// Plain type assertions, not errors.As: providerHTTPError and
	// translateError are both always returned bare from every adapter and
	// translate_*.go call site — never wrapped via fmt.Errorf("%w", ...) —
	// so errors.As's unwrap-chain walk buys nothing here, and yaegi
	// v0.16.1 panics ("errors: *target must be interface or implement
	// error") calling errors.As with an interpreted pointer type as its
	// target, even though that type's Error() method is right there.
	// Verified empirically under real Traefik (Task 15's integration
	// suite); tools/yaegi-check never exercises this call path.
	if perr, ok := err.(*providerHTTPError); ok {
		writeProviderUpstreamError(sw, providerName, perr)
		return
	}

	if terr, ok := err.(*translateError); ok {
		status := http.StatusBadRequest
		if terr.notSupported {
			status = http.StatusNotImplemented
		}
		writeOAIError(sw, status, "invalid_request_error", terr.msg)
		return
	}

	if errors.Is(err, context.Canceled) {
		g.logf("unified route: client canceled request to provider %q: %v", providerName, err)
		return
	}

	g.errorf("unified route: upstream connection error (provider %q): %v", providerName, err)
	writeOAIError(sw, http.StatusBadGateway, "server_error", "upstream connection error")
}

// writeProviderUpstreamError passes perr's status through to the client,
// wrapped in the gateway's own envelope shape rather than perr.body
// verbatim (unified-route callers get this wrapped form; the openai-type
// adapter's own passthrough forwards a non-2xx body unwrapped, since that
// path never reaches this function). perr.body is embedded under
// error.upstream, decoded to a JSON value when it parses as one and left
// as a raw string otherwise, so a caller sees exactly what the provider
// said either way.
func writeProviderUpstreamError(w http.ResponseWriter, providerName string, perr *providerHTTPError) {
	var upstream any = string(perr.body)
	var parsed any
	if json.Unmarshal(perr.body, &parsed) == nil {
		upstream = parsed
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(perr.status)
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"error": map[string]any{
			"message":  providerName + " upstream error",
			"type":     "upstream_error",
			"code":     strconv.Itoa(perr.status),
			"upstream": upstream,
		},
	})
}

// cacheCaptureWriter tees a cacheable request's response into an
// in-memory buffer while writing everything through to the wrapped
// *statusTrackingWriter unchanged — the mechanism runUnified uses to fill
// the response cache (cache.go) on a miss without any adapter, or
// forwardJSON/forwardStream inside one, needing to know caching exists.
//
// It embeds *statusTrackingWriter rather than holding one in a named
// field: every promoted method (Flush included) delegates automatically,
// so only WriteHeader and Write — the two that must also capture — need
// overriding here. This is what "preserve statusTrackingWriter semantics"
// means in practice: Flush passes through for free, and wroteHeader stays
// the single shared bookkeeping field statusTrackingWriter already
// maintains (handleAdapterError inspects it via the original sw pointer,
// not through this wrapper, so a cacheCaptureWriter's WriteHeader/Write
// must delegate to the embedded pointer, never shadow its state).
type cacheCaptureWriter struct {
	*statusTrackingWriter
	contentType string
	buf         bytes.Buffer
	status      int
}

// newCacheCaptureWriter returns a cacheCaptureWriter teeing into sw.
func newCacheCaptureWriter(sw *statusTrackingWriter) *cacheCaptureWriter {
	return &cacheCaptureWriter{statusTrackingWriter: sw}
}

// WriteHeader records status and the Content-Type header already set on
// w.Header() at this point (matching forwardJSON's own ordering: it sets
// Content-Type, then calls WriteHeader), then delegates.
func (w *cacheCaptureWriter) WriteHeader(status int) {
	w.status = status
	w.contentType = w.Header().Get("Content-Type")
	w.statusTrackingWriter.WriteHeader(status)
}

// Write buffers b (unbounded here — the caller, runUnified, only stores
// the buffer for a genuine upstream 200, and the adapter response it
// captures is itself already capped at maxResponseBytes, so this never
// buffers more than that), applies net/http's implicit-200 default when
// no WriteHeader call preceded it, and delegates to the wrapped writer.
func (w *cacheCaptureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_, _ = w.buf.Write(b) // bytes.Buffer.Write never returns an error
	return w.statusTrackingWriter.Write(b)
}
