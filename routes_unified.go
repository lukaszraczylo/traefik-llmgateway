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

// maxRequestBytes caps a client request body decoded into a
// map[string]any: 10MiB. As of the security review below, the unified
// chat/embeddings routes (runUnified) no longer use this constant
// directly — they use the narrower maxUnifiedRequestBytes instead. This
// constant remains unchanged, at its original value, for every OTHER
// caller that was never in this finding's scope: the media JSON routes'
// own decode (decodeMediaJSONRequest, routes_media.go — shared by
// images.generations and audio.speech), audio.transcriptions' multipart
// cap (readCapped, routes_media.go), and the MCP federation JSON-RPC
// routes (mcp_federation.go). Splitting the constant, rather than
// lowering this shared one, keeps those callers' documented body-size
// behavior (README's "Body limits" section) byte-for-byte unchanged.
const maxRequestBytes = 10 << 20

// maxUnifiedRequestBytes caps a client request body specifically for the
// unified chat/embeddings routes' own decode into map[string]any
// (security review finding 1c, 2026-08-22): 4MiB, down from the general
// maxRequestBytes (10MiB) above.
//
// MEASURED AMPLIFICATION (corrected, round 3, 2026-08-22 review — the
// round-2 doc comment's "~11x" figure was presented as the general case
// and is not): amplification is SHAPE-DEPENDENT, not a fixed multiplier.
// A realistic single-inline-base64-image chat payload (the documented
// case this cap is sized for, below) measures at roughly 1.0x — a large
// base64 string decodes to one large Go string, no material blow-up.
// The adversarial shape — a body packed with many small values instead
// of one large one (e.g. a huge flat array/object of short strings or
// numbers, each becoming its own heap-allocated map entry/interface
// value) — is what actually amplifies: measured on a 4MiB adversarial
// array at 12.3x LIVE heap (~51.5MB resident at the decode's peak) and
// ~45x cumulative allocation (~189MB TotalAlloc, mostly short-lived
// garbage the collector reclaims quickly, not simultaneously resident).
// The bound this cap and acquireBodyAdmission's semaphore actually
// provide, honestly stated: one held decode's LIVE heap peaks at roughly
// 51.5MB in the adversarial case (worst case measured, not a
// theoretical 11x-of-4MiB ~44MiB figure); defaultBodyAdmissionCap's own
// doc comment (llmgateway.go) does the resulting worst-case-concurrent
// arithmetic against that real number, not this one's old estimate.
//
// 1MiB — the audit's own first suggestion — was evaluated and rejected:
// this constant's PRE-finding-1c doc comment said the original 10MiB was
// "generous for a chat or embeddings request (including an inline
// base64 image)", and that is a real, exercised code path, not a
// hypothetical one — translate_anthropic.go's imageContentPart and
// translate_gemini.go's own image handling both accept a
// "data:<type>;base64,<data>" image_url content part inside a chat
// message, and a single moderately-sized photo commonly exceeds 1MiB
// once base64-encoded (roughly +33% over its raw bytes; a compressed
// phone photo alone is often 1-3MB raw). Capping at 1MiB would silently
// break that documented, tested capability for any real-world image —
// and, per the measurement above, that case shows ~1.0x amplification
// regardless, so the ORIGINAL amplification argument for shrinking this
// cap never actually applied to it; 1MiB was rejected for breaking a
// real feature, not because the realistic case was itself dangerous.
// 4MiB keeps headroom for the single-inline-image case the original
// comment named, while still bounding the adversarial shape's LIVE-heap
// footprint to a fixed, known-small amount per held decode — a
// defensible middle ground, not the audit's suggested number, chosen
// because the smaller number would have broken a real feature this
// package ships.
const maxUnifiedRequestBytes = 4 << 20

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
// models their group can see, via modelRegistry.modelsJSON (perf finding
// 3: modelsJSON caches the encoded body per (group, discovery generation)
// so a request between discovery refreshes never re-walks listFor/
// re-marshals — registry.go owns the cache; this call site just writes
// its result).
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	u, grp, ok := g.auth.identify(r)
	g.logAuthEvent(ok, authEventUserName(u), r)
	if !ok {
		writeOAIError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(g.registry.modelsJSON(grp)) // headers already committed; nothing useful to do on write failure
}

// adapterCall abstracts providerAdapter.chatCompletion and .embeddings:
// handleChat and handleEmbeddings share every other step of the pipeline
// below (decode, resolve, enforce limits, account usage, translate
// errors) and differ only in which of these two methods they invoke.
type adapterCall func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)

// handleChat implements POST /v1/chat/completions.
func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	g.runUnified(w, r, u, grp, cacheEndpointChat, func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
		return a.chatCompletion(ctx, w, req)
	})
}

// handleEmbeddings implements POST /v1/embeddings.
func (g *Gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	g.runUnified(w, r, u, grp, cacheEndpointEmbeddings, func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
		return a.embeddings(ctx, w, req)
	})
}

// runUnified is the shared chat/embeddings pipeline: enforce per-user/
// per-group/total REQUEST-RATE limits, claim a body-admission slot to
// decode the request, resolve its model, invoke the adapter via call,
// then account the resulting usage — even when call itself returned an
// error, so usage captured before a mid-stream failure still gets billed
// — before translating that error into a response. w is wrapped in its
// own statusTrackingWriter so a mid-stream adapter error (headers
// already sent) can be told apart from one that failed before any
// write. endpoint is cacheEndpointChat or cacheEndpointEmbeddings — one
// of cacheKey's key-material components (cache.go), so the two routes
// never collide into one cache entry.
//
// ORDERING (security review finding 1a, 2026-08-22): the rate-limit
// check runs BEFORE the body is ever read, not after — the previous
// ordering (decode, resolve model, THEN checkAndCount) meant a caller
// already over their requestsPerMinute paid the full cost of reading and
// json-decoding a body that was always going to be discarded.
// admitRequest needs only u/grp, already available as this function's
// own parameters, so it has no reason to wait for the body. Model
// resolution still runs AFTER decode — it genuinely needs the client's
// requested model id, which only exists once the body is parsed — so
// its own errors (unknown/denied model) are still reported after a
// successful admission+rate-check, exactly as before this fix; only the
// RATE-LIMIT check's position relative to the body moved.
// writeLimitViolation's own response body/headers are unchanged — a
// 429's body is byte-identical to before this finding.
//
// BEHAVIOR CHANGE (documented, not hidden — SHOULD-4, round 3,
// 2026-08-22 review): moving the rate-limit check ahead of decode has
// two real, previously-silent side effects for a caller who is BOTH
// over budget AND sending a request that would otherwise have failed
// decode/model-resolution:
//   - A request that would have gotten 400 (invalid JSON, missing
//     "model") or 404 (unknown model) now gets 429 instead, if the
//     caller was already over budget — the rate-limit check runs first
//     and returns before decode/resolution ever gets a chance to run.
//   - A malformed or unroutable request that would NOT have consumed any
//     rate-limit budget under the old ordering (decode/resolve failed
//     before checkAndCount ran) now DOES consume it, since checkAndCount
//     always runs first regardless of what the body turns out to
//     contain. A tenant with a buggy client that sends malformed JSON
//     repeatedly could previously retry indefinitely for free; it now
//     burns real requestsPerMinute/Day budget on every attempt.
//
// Both are consequences of fixing finding 1a's actual bug (an
// already-over-budget caller paying the read+decode cost) and are
// considered acceptable — arguably improvements, since a client that
// cannot even form a valid request is now itself rate-limited rather
// than getting unlimited free retries — but are called out explicitly
// here since neither is visible from the diff alone.
func (g *Gateway) runUnified(w http.ResponseWriter, r *http.Request, u *user, grp *group, endpoint string, call adapterCall) {
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
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	streaming, _ := req["stream"].(bool)

	adapter, upstreamModel, canonical, err := g.registry.resolve(requestedModel, grp)
	if err != nil {
		writeModelResolveError(sw, err)
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
	// upstream. requestedModel (the client's own, un-rewritten alias
	// string) and endpoint are passed as separate cacheKey arguments,
	// not left in req: a translating adapter bakes the echoed alias
	// straight into the cached response body's own "model" field, so two
	// clients requesting the same upstream model under different alias
	// forms (e.g. "claude-x" vs "anthropic/claude-x") must never collide
	// into one cache entry (cacheKey's own doc comment, cache.go).
	cacheable := !streaming && g.cache != nil && groupCacheEnabled(grp)
	var cacheKeyStr string
	if cacheable {
		cacheKeyStr = cacheKey(adapter.name(), upstreamModel, requestedModel, endpoint, req)
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
		capture = newCacheCaptureWriter(sw, g.cache.maxBodyBytes)
		respWriter = capture
	}

	// Feature A (v0.22): every upstream attempt call() makes — via
	// upstreamJSON's retryPolicy.do, providers.go — reports through to
	// this recorder, closing over the adapter/model this request already
	// resolved to. See attemptRecorder's own doc comment (providers.go).
	ctx := withAttemptRecorder(r.Context(), func(resp *http.Response, attemptErr error) {
		g.limiter.recordProviderAttempt(adapter.name(), upstreamModel, resp, attemptErr)
	})
	result, callErr := call(adapter, ctx, respWriter, req)

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
	// callErr == nil is checked directly regardless. capture.oversize
	// excludes a response cacheCaptureWriter stopped buffering past
	// maxBodyBytes: store's own maxBodyBytes check would reject it too,
	// but only after being handed a silently truncated body — skip the
	// call outright instead of ever constructing a corrupt cache entry.
	if cacheable && callErr == nil && capture.status == http.StatusOK && !capture.oversize {
		g.cache.store(cacheKeyStr, capture.status, capture.contentType, capture.buf.Bytes(), effectiveTTL(g.cache, grp))
	}

	if callErr != nil {
		g.handleAdapterError(sw, callErr, adapter.name())
		return
	}
}

// buildLimitScopes returns the limitScope slice runUnified passes to the
// limiter: a user scope and a group scope, ALWAYS both, unconditionally
// (v0.21 fix — see this function's doc comment history below for the bug
// this closes). The user scope is listed first, so checkAndCount reports a
// user's own violation ahead of their group's when both are breached by the
// same request.
//
// u.limits or grp.limits may be nil — that scope simply carries no limit for
// checkAndCount (limits.go) to enforce, and checkAndCount's own nil-limits
// check skips it during evaluation. It is NOT omitted from the slice: usage
// accounting (checkAndCount's own counting half, and account) is
// unconditional and must never be coupled to whether a limit happens to be
// configured. Production bug (root-caused live, 2026-08-21): the previous
// version of this function omitted a user or group scope entirely whenever
// that entity had no configured Limits, which meant a limit-less user or
// group never accumulated ANY usage counters at all — the admin dashboard's
// per-user rows all read zero while the total kept climbing, until an
// operator worked around it by adding a phantom, deliberately-unreachable
// requestsPerDay limit to every user/group just to make buildLimitScopes
// build a counter scope for them. That workaround is no longer needed:
// accounting and enforcement are now fully decoupled, matching
// buildAdminUsage's own "never omits an entity for having nil limits"
// contract (admin.go) that the dashboard's usage table already promised.
//
// Callers metering actual LLM traffic wrap this result in withTotalScope
// before passing it to checkAndCount/account. handleAdminAPI (admin.go)
// never calls buildLimitScopes at all — its own buildAdminUsage builds
// {kind, id, limits} literals directly from authStore.snapshot's user/group
// listing, for currentUsage's read-only purposes — and, separately, never
// calls checkAndCount either (handleAdminAPI's own doc comment: admin
// traffic must never move req/min-req/day statistics). Fixed a stale
// version of this comment (review round 2, 2026-08-21) that claimed
// handleAdminAPI "calls this directly, without withTotalScope" — it never
// called this function at all, on any version of this file.
func buildLimitScopes(u *user, grp *group) []limitScope {
	return []limitScope{
		{limits: u.limits, kind: "user", id: u.name},
		{limits: grp.limits, kind: "group", id: grp.name},
	}
}

// withTotalScope returns scopes with the synthetic total scope
// (totalScopeKind/totalScopeID, limits.go) appended — the single helper
// every metered route (runUnified above, via admitRequest below;
// handleImagesGenerations/handleAudioSpeech/handleAudioTranscriptions,
// routes_media.go, via the same admitRequest; handlePassthrough,
// routes_passthrough.go; handleTargetProxy, mcp_a2a.go; handleMCPFederated,
// mcp_federation.go) calls around its own buildLimitScopes result, so none
// of them can forget it and none of them duplicate the scope literal.
// Deliberately not folded into buildLimitScopes itself: buildLimitScopes'
// own doc comment now explains why admin traffic needs no special-casing
// here at all — handleAdminAPI never calls either function, so there was
// never a real "admin traffic must not contribute to total" case for this
// split to guard against; the split is kept anyway because
// buildLimitScopes' {user, group} pair and the synthetic total scope are
// conceptually different additions, worth two names.
func withTotalScope(scopes []limitScope) []limitScope {
	return append(scopes, limitScope{kind: totalScopeKind, id: totalScopeID, limits: nil})
}

// admitRequest enforces per-user/per-group/total REQUEST-RATE limits
// (checkAndCount) using only u and grp — never a request body — so every
// caller can, and now does, call this before reading or decoding
// anything (security review finding 1a, 2026-08-22): a caller already
// over budget is refused on the strength of who they are alone, never
// after paying the cost of reading and json-decoding a body that turns
// out to be discarded anyway. scopes is returned alongside ok so the
// caller can reuse the identical scope slice for its own later account
// call, rather than rebuilding (and risking scope-list drift from) a
// second buildLimitScopes/withTotalScope pair. A violation writes its
// response to sw itself (writeLimitViolation, unchanged body/headers from
// before this fix) and returns ok=false.
//
// Shared verbatim by every unified/media route (runUnified above;
// handleImagesGenerations/handleAudioSpeech/handleAudioTranscriptions,
// routes_media.go) — see runUnified's own "BEHAVIOR CHANGE" doc-comment
// section for the two previously-silent side effects this reordering has
// for a caller who is both over budget and sending a malformed/
// unroutable request; they apply identically here.
func (g *Gateway) admitRequest(sw *statusTrackingWriter, u *user, grp *group) (scopes []limitScope, ok bool) {
	scopes = withTotalScope(buildLimitScopes(u, grp))
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		writeLimitViolation(sw, violation)
		return scopes, false
	}
	return scopes, true
}

// bodyAdmissionRetryAfterSeconds is the Retry-After value
// acquireBodyAdmission writes on a 503: a short, fixed window rather than
// a computed one — the semaphore's own occupancy has no natural "when
// will a slot free" answer the way a rate-limit window does (unlike
// writeLimitViolation's retryAfter, which names a real window boundary),
// and the in-flight cap is expected to drain within single-digit seconds
// under normal request latency.
const bodyAdmissionRetryAfterSeconds = 2

// acquireBodyAdmission non-blockingly claims one of g.bodyAdmission's
// slots (security review finding 1b, 2026-08-22) — bounding how many
// unified/media requests may concurrently be INSIDE the read+json-decode
// step (readAndDecodeUnifiedBody, decodeMediaJSONRequest, the readCapped
// call in handleAudioTranscriptions), which is where the measured
// amplification actually happens (an encoding/json decode into
// map[string]any costs up to ~12x the wire body's own bytes in live
// heap for an adversarial shape — see maxUnifiedRequestBytes' own doc
// comment for the measured figures), so an unbounded burst of
// concurrent decodes cannot OOM the whole shared Traefik ingress process
// this plugin runs inside (package doc, llmgateway.go) merely by
// arriving faster than any one of them can be rejected.
//
// SCOPE (round 3, 2026-08-22, coordinator ruling — fixes an availability
// regression the round-2 version shipped): the slot is held ONLY for the
// read+decode step, never for the rest of the request — model
// resolution, the cache lookup, the upstream round trip, or streaming
// the response back to the client. Round 2 acquired the slot at handler
// entry and released it via a single `defer` spanning the WHOLE handler,
// which measurably turned this into a hard ceiling on TOTAL concurrent
// in-flight LLM requests rather than concurrent decodes: an LLM
// completion is I/O-bound for seconds to minutes, so holding a slot for
// that whole span meant the self-tuned default (previously GOMAXPROCS *
// 8) became the gateway's real-world concurrency limit — measured at
// zero-config, 400 concurrent chat requests: 220 succeeded, 180 got 503,
// on a cap a 2-vCPU pod would size at 16. A request parked waiting on an
// upstream response holds no slot now — every caller acquires and
// releases around ONLY its own read+decode call, not around the whole
// handler. The upstream request being built, sent, and its response
// streamed back are unguarded by this semaphore: that path is I/O-bound
// and does not re-amplify memory the way the decode step does, and the
// response side already has its own bound (forwardJSON/forwardStream's
// maxResponseBytes, 32MiB, providers.go — unchanged by this finding).
// Because the held window shrank from "whole request" (seconds-minutes)
// to "one decode" (low milliseconds), the default cap below is sized
// much higher than round 2's — see defaultBodyAdmissionCap's own doc
// comment for the reasoning and the resulting worst-case bound.
//
// Mirrors limiter.spawnTokens' own non-blocking acquire shape
// (limits.go): on exhaustion this NEVER queues or blocks waiting for a
// slot — a blocking acquire would just replace one unbounded-growth
// vector (unbounded concurrent decodes) with another (an unbounded pile
// of goroutines blocked on a channel receive) — it fails immediately
// instead, writing a 503 with a Retry-After header to sw and returning a
// no-op release so the caller's own `defer release()` stays valid
// either way.
//
// release must be deferred by the caller IMMEDIATELY after this
// returns, BEFORE checking ok, and that defer must be scoped narrowly
// around the read+decode step alone (see readAndDecodeUnifiedBody/
// decodeMediaJSONRequest for the pattern) — never around the caller's
// entire handler. Every code path, including this function's own 503
// branch, releases exactly the number of slots it acquired (zero, on
// the 503 path, via the no-op release); a `defer` guarantees this holds
// even on a panic unwinding through the caller, since Go runs deferred
// functions during a panic regardless of where recover() (if any) is
// registered further up the same goroutine's stack.
//
// handlePassthrough (routes_passthrough.go) deliberately does NOT
// acquire a slot at all: it streams the request/response body straight
// through via io.Copy (proxyUpstream) rather than buffering it into a
// map[string]any, so there is no decode-amplification event here for
// this semaphore to bound — the only body-inspection it ever does is
// peekPassthroughModel's own bounded 64KiB read, an intentional
// omission, not an oversight.
func (g *Gateway) acquireBodyAdmission(sw *statusTrackingWriter) (release func(), ok bool) {
	select {
	case g.bodyAdmission <- struct{}{}:
		return func() { <-g.bodyAdmission }, true
	default:
		sw.Header().Set("Retry-After", strconv.Itoa(bodyAdmissionRetryAfterSeconds))
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "server is at capacity; try again shortly")
		return func() {}, false
	}
}

// readAndDecodeUnifiedBody claims a body-admission slot (acquireBodyAdmission),
// reads r's body capped at maxUnifiedRequestBytes, decodes it as a JSON
// object, and releases the slot before returning — on every path: a
// successful decode, a read failure, a decode failure, or admission
// exhaustion itself (security review finding 1b, round 3, 2026-08-22).
// This is deliberately the ENTIRE scope of what the semaphore guards for
// the unified routes: by the time this returns, req is fully decoded and
// nothing further in runUnified re-amplifies memory the way the decode
// itself does, so model resolution, the cache lookup, and the upstream
// round trip all run after the slot is already released. A read or
// decode failure has already written its 400 to sw; ok reports whether
// the caller may proceed.
func (g *Gateway) readAndDecodeUnifiedBody(sw *statusTrackingWriter, r *http.Request) (body []byte, req map[string]any, ok bool) {
	release, admitted := g.acquireBodyAdmission(sw)
	defer release()
	if !admitted {
		return nil, nil, false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxUnifiedRequestBytes))
	if err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return nil, nil, false
	}

	if err = json.Unmarshal(body, &req); err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return nil, nil, false
	}
	return body, req, true
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
// use). A *aliasTargetError (spec §5, v0.2) is also a 404, but with its
// own message naming both the alias and its unresolved target, checked
// first via a plain type assertion — not errors.As, matching this
// package's established yaegi-safe convention for a pointer error type
// (registry.go's aliasTargetError doc comment). Any other error is a
// defensive 500 — resolve's own contract promises only these sentinels/
// types, so reaching this branch would be a programming error, not a
// client mistake.
func writeModelResolveError(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*aliasTargetError); ok {
		writeOAIError(w, http.StatusNotFound, "invalid_request_error", aerr.Error())
		return
	}
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
	// A cacheable request's miss path pre-sets X-Llmgw-Cache: miss before
	// call() runs (runUnified), so headers precede a successful body —
	// but an adapter error means nothing was actually served from, or
	// stored to, the cache. Del is unconditional and harmless when the
	// header was never set (a no-op on an absent key), so this needs no
	// cacheable-specific branch here: every error response — 400, 404,
	// 429, 501, 502 — must never carry a stale cache header from a
	// request that turned out not to succeed.
	sw.Header().Del("X-Llmgw-Cache")

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
// in-memory buffer, capped at maxBodyBytes regardless of how large the
// real response turns out to be, while writing everything through to the
// wrapped *statusTrackingWriter unchanged — the mechanism runUnified uses
// to fill the response cache (cache.go) on a miss without any adapter, or
// forwardJSON/forwardStream inside one, needing to know caching exists.
//
// The cap matters even though the adapter response it captures is itself
// already bounded by maxResponseBytes (32MiB): cache.maxBodyBytes
// defaults to 1MiB and maxes out at 8MiB, both far below that, and
// forwardJSON delivers a non-streaming body as one single Write call —
// without this cap, buffering that one call unconditionally would hold
// up to 32MiB in memory per cacheable request regardless of how small
// maxBodyBytes is actually configured.
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
	contentType  string
	buf          bytes.Buffer
	maxBodyBytes int
	status       int
	// oversize is true once buf has reached maxBodyBytes: Write stops
	// appending to buf from that point on (the real write to the client
	// is never affected), and runUnified skips calling store() entirely
	// rather than handing it a silently truncated body.
	oversize bool
}

// newCacheCaptureWriter returns a cacheCaptureWriter teeing into sw,
// capturing at most maxBodyBytes of the response body.
func newCacheCaptureWriter(sw *statusTrackingWriter, maxBodyBytes int) *cacheCaptureWriter {
	return &cacheCaptureWriter{statusTrackingWriter: sw, maxBodyBytes: maxBodyBytes}
}

// WriteHeader records status and the Content-Type header already set on
// w.Header() at this point (matching forwardJSON's own ordering: it sets
// Content-Type, then calls WriteHeader), then delegates.
func (w *cacheCaptureWriter) WriteHeader(status int) {
	w.status = status
	w.contentType = w.Header().Get("Content-Type")
	w.statusTrackingWriter.WriteHeader(status)
}

// Write captures up to maxBodyBytes total of b into buf — silently
// dropping anything beyond the cap and marking oversize, rather than
// buffering an arbitrarily large response only to reject it in store()
// afterward — applies net/http's implicit-200 default (status AND its
// Content-Type snapshot; WriteHeader's own capture above never ran on
// this path) when no WriteHeader call preceded it, and always delegates
// the full, untruncated b to the wrapped writer: capture is capped, the
// real response to the client never is.
func (w *cacheCaptureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.contentType = w.Header().Get("Content-Type")
	}
	if !w.oversize {
		if remaining := w.maxBodyBytes - w.buf.Len(); remaining <= 0 {
			w.oversize = true
		} else if len(b) > remaining {
			_, _ = w.buf.Write(b[:remaining]) // bytes.Buffer.Write never returns an error
			w.oversize = true
		} else {
			_, _ = w.buf.Write(b)
		}
	}
	return w.statusTrackingWriter.Write(b)
}
