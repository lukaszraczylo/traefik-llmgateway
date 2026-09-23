package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
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
// routes_messages.go's handleMessages reuses the identical contract for
// its own anthropic-passthrough/translate branching (see its own doc
// comment) — the shape is exactly what runMeteredCall, below, needs
// regardless of which wire shape a route answers in.
type adapterCall func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)

// envelopeWriter abstracts the client-facing error envelope shape so the
// shared metered-request pipeline (runMeteredCall, below) can serve both
// the OpenAI-shaped unified/media routes and the Anthropic-shaped
// /v1/messages route through one implementation (item 7, 2026-08-22
// review — fixing 58 of handleMessages' 73 lines being verbatim
// duplicated from runUnified) rather than duplicating it. writeOAIError
// (errors.go) and writeAnthropicError (routes_messages.go) both already
// match this exact signature.
type envelopeWriter func(w http.ResponseWriter, status int, errType, msg string)

// providerUpstreamErrorWriter abstracts which envelope shape a
// *providerHTTPError gets embedded into (writeProviderUpstreamError,
// below, or routes_messages.go's writeAnthropicProviderUpstreamError) —
// threaded through runMeteredCall/handleAdapterErrorEnvelope alongside
// envelopeWriter for the same reason.
type providerUpstreamErrorWriter func(w http.ResponseWriter, providerName string, perr *providerHTTPError)

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
// decode the request, then hand off to runMeteredCall (below) for
// everything from model resolution through response caching, usage
// accounting, and adapter-error handling. w is wrapped in its own
// statusTrackingWriter so a mid-stream adapter error (headers already
// sent) can be told apart from one that failed before any write.
// endpoint is cacheEndpointChat or cacheEndpointEmbeddings — one of
// cacheKey's key-material components (cache.go), so the two routes never
// collide into one cache entry.
//
// ORDERING (security review finding 1a, 2026-08-22): the rate-limit
// check runs BEFORE the body is ever read, not after — the previous
// ordering (decode, resolve model, THEN checkAndCount) meant a caller
// already over their requestsPerMinute paid the full cost of reading and
// json-decoding a body that was always going to be discarded.
// admitRequestForRoute needs only u/grp, already available as this
// function's own parameters, so it has no reason to wait for the body. Model
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
//
// SHARED CORE (item 7 fix, 2026-08-22 review): routes_messages.go's
// handleMessages needs admitRequestForRoute and the body decode in a
// DIFFERENT relative order than this route does (its own doc comment
// explains why — a streaming request must never touch the rate-limit
// counters at all, which means it has to know "is this streaming" from
// the decoded body before admitRequestForRoute ever runs). That is the
// one genuine ordering difference between the two routes, and it is why
// THIS function still owns admitRequestForRoute/readAndDecodeUnifiedBody
// directly rather than folding
// them into runMeteredCall too — everything after both are done is
// identical regardless of wire shape, and lives in runMeteredCall alone.
func (g *Gateway) runUnified(w http.ResponseWriter, r *http.Request, u *user, grp *group, endpoint string, call adapterCall) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	scopes, ok := g.admitRequestForRoute(sw, u, grp, writeOAIError, eventRouteForEndpoint(endpoint))
	if !ok {
		return
	}

	body, req, ok := g.readAndDecodeUnifiedBody(sw, r, writeOAIError)
	if !ok {
		return
	}

	requestedModel, _ := req["model"].(string)
	if requestedModel == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	g.runMeteredCall(sw, r, scopes, body, req, requestedModel, grp, endpoint, "unified route", writeOAIError, writeProviderUpstreamError, call)
}

// runMeteredCall is the shared implementation from model resolution
// through response caching, usage accounting, and adapter-error handling
// — the part of every metered LLM route (this file's runUnified, above;
// routes_messages.go's handleMessages) that is genuinely identical
// regardless of wire shape (item 7 fix, 2026-08-22 review: this body used
// to be duplicated almost verbatim in routes_messages.go). Callers have
// already run admitRequestForRoute (rate limiting) and decoded the request body
// into req, in whichever relative order their own route needs (see
// runUnified's own doc comment for why that order differs for
// /v1/messages) — this function starts only once both are done.
//
// endpoint is cacheKey's endpoint argument (cacheEndpointChat/
// cacheEndpointEmbeddings/routes_messages.go's cacheEndpointMessages).
// envelope and writeUpstream pick the client-facing error shape. call is
// adapterCall — how the wire-shape-specific request/response translation,
// if any, actually happens; the routes differ ONLY here (call's own
// closure already has whatever it needs — routes_messages.go's own
// requestedModel for its alias echo, in particular) and in which error
// envelope shape they answer in. logPrefix names the route in every log
// line this function writes, so a log reader can always tell which route
// produced a given line without the classification logic existing twice.
//
// feat/failover: this function now tries a LIST of candidate providers
// (g.registry.resolveWithCandidates/g.orderedFailoverCandidates,
// failover.go), not just the one resolve alone would pick, and is the
// ONE hook point both /v1/chat/completions (runUnified, above) and
// /v1/messages (handleMessages, routes_messages.go) share — per the
// brief's own instruction, failover is wired here once rather than
// forked into a second implementation. THE HARD CONSTRAINT this loop is
// built around: call(...) writes directly to respWriter, which tees
// straight through to sw; the very first byte written commits the
// response, so a candidate is only ever retried while sw.wroteHeader is
// still false. Every step that was a one-shot computation before this
// feature (the cache key, the attempt recorder's bound provider name,
// the usage/canonical id billed) now happens fresh INSIDE the loop, once
// per candidate — this is what "rebind the attempt recorder per
// attempt" and "cache under the winning provider's key" actually mean
// in code, not just in the brief's prose.
func (g *Gateway) runMeteredCall(sw *statusTrackingWriter, r *http.Request, scopes []limitScope, body []byte, req map[string]any, requestedModel string, grp *group, endpoint, logPrefix string, envelope envelopeWriter, writeUpstream providerUpstreamErrorWriter, call adapterCall) {
	// F9 (adversarial-review, optional perf fix): a disabled deployment
	// never needs the candidate pool at all — resolvePrimaryOnly skips
	// failoverCandidates' own provider scan entirely, so a config that
	// never opted into failover pays nothing extra for a pool it will
	// never use.
	var primary resolveCandidate
	var extra []resolveCandidate
	var err error
	if g.failover.enabled {
		primary, extra, err = g.registry.resolveWithCandidates(requestedModel, grp)
	} else {
		primary, err = g.registry.resolvePrimaryOnly(requestedModel, grp)
	}
	if err != nil {
		writeModelResolveErrorEnvelope(sw, err, envelope)
		return
	}
	// Sequence matters (each step's own doc comment explains why):
	// health/discovery ordering first, so candidates[0] below is the
	// ACTUAL first-attempted candidate the cost guard compares everyone
	// else against; cost filtering second; the maxAttempts cap last, so
	// it never re-admits a candidate the cost guard already removed.
	candidates := g.orderedFailoverCandidates(primary, extra)
	candidates = g.filterCandidatesByCost(candidates, requestedModel)
	candidates = capFailoverCandidates(candidates, g.failover.maxAttempts)

	// cacheable/streaming gate every cache step below on spec §2's scope,
	// exactly as before this feature: a non-streaming request, with a
	// working cache (g.cache is nil whenever caching is off or Redis was
	// absent — buildResponseCache, cache.go), for a group that has not
	// opted out (groupCacheEnabled). Computed once, outside the loop: it
	// depends only on req["stream"]/grp, neither of which a failover
	// attempt changes.
	streaming, _ := req["stream"].(bool)
	cacheable := !streaming && g.cache != nil && groupCacheEnabled(grp)

	// pristineReq is the client's own decoded request, snapshotted ONCE,
	// BEFORE any candidate's adapter ever touches it, with "model"
	// already rewritten to the upstream id every candidate in this
	// failover chain shares by construction (failoverCandidates,
	// failover.go, only ever adds providers serving the IDENTICAL bare
	// id resolve's primary already resolved to). Every attempt below
	// builds its OWN fresh top-level copy from this snapshot rather than
	// reusing or mutating one shared map (adversarial-review fix, F1):
	// openai-type adapters mutate req in place — chatCompletion forces
	// stream_options.include_usage=true onto it (provider_openai.go) —
	// and reusing the same map across attempts let a LATER candidate
	// read back an EARLIER candidate's own forced injection and
	// misreport clientAskedUsage=true to a client that never asked for
	// it, leaking the gateway's own suppressed usage-only SSE chunk.
	// Proven against a single-provider control; this is what fixes it
	// for openai, openai-audio, and copilot alike.
	pristineReq := make(map[string]any, len(req)+1)
	for k, v := range req {
		pristineReq[k] = v
	}
	if len(candidates) > 0 {
		pristineReq["model"] = candidates[0].upstreamModel
	}

	// preFailoverHeaders snapshots sw.Header() ONCE, before the loop below
	// ever runs (finding 7 fix, review-routes.md): every header present
	// here was staged by something OTHER than this loop — an earlier,
	// unrelated Traefik middleware (CORS, HSTS, request-id, ...) running
	// before this plugin, on the default, non-failover path — and a
	// failover iteration's own header cleanup (below) restores exactly
	// this snapshot instead of wiping every header unconditionally, which
	// deleted those staged headers too, not just the gateway's own. Clone
	// is a real copy (http.Header.Clone's own contract), so later
	// mutations of sw.Header() by this loop can never retroactively
	// change what gets restored.
	preFailoverHeaders := sw.Header().Clone()

	var lastErr error
	var lastProviderName string
	for i, cand := range candidates {
		if i > 0 {
			// F4 fix (adversarial-review): a shared request context an
			// EARLIER candidate already exhausted must never be
			// misattributed as THIS candidate's own failure. There is no
			// per-candidate timeout budget in this architecture (every
			// candidate shares r's SAME context — see README's own
			// documented limitation), so once an earlier candidate burns
			// the deadline, attempting the next one would fail near-
			// instantly for a reason that has nothing to do with its own
			// health. Stop here with the LAST REAL error instead of
			// chasing further doomed candidates and wrongly opening a
			// perfectly healthy provider's request-health breaker.
			if r.Context().Err() != nil {
				g.recordUpstreamEvent(scopes, candidates[i-1].canonical, lastProviderName, eventRouteForEndpoint(endpoint), lastErr)
				g.handleAdapterErrorEnvelope(sw, lastErr, lastProviderName, logPrefix, envelope, writeUpstream)
				return
			}

			// checkAndCount above (admitRequestForRoute, this file) already
			// counted this request exactly once — never double-count: a
			// failover attempt is a NEW upstream attempt, not a new
			// logical request, so nothing here calls checkAndCount
			// again. Per-provider ATTEMPT counters, below, still record
			// every attempt independently via the freshly-bound
			// recorder.
			//
			// Rate-limited (F10 fix, adversarial-review): during a
			// sustained outage this line would otherwise be one
			// blocking synchronous stderr write per request on shared
			// Traefik ingress — the exact failure shouldLogAuthFailure/
			// logStoreError (auth.go/limits.go) already exist to
			// prevent elsewhere in this package. The 404 line below is
			// deliberately NOT gated by this (operator ruling: it must
			// always log).
			if log, suppressed := g.failoverLogGate.shouldLog(time.Now()); log {
				suffix := ""
				if suppressed > 0 {
					suffix = fmt.Sprintf(" (%d more failovers suppressed since last log)", suppressed)
				}
				g.warnf("%s: failing over from provider %q to %q for model %q%s", logPrefix, candidates[i-1].providerName, cand.providerName, requestedModel, suffix)
			}

			// F2 fix, correctly scoped (adversarial-review round 2 — the
			// round-1 version cleared unconditionally, including i == 0,
			// which deleted response headers a DIFFERENT, earlier Traefik
			// middleware had already staged before this plugin ever ran
			// (CORS, HSTS, request-id, ...) on the default, non-failover
			// path — proven by differential harness, and it contradicted
			// this package's own "byte-identical to a gateway built
			// before this field existed" claim. Only i > 0 needs this at
			// all: newSSEWriter (sse.go) sets Content-Type: text/event-
			// stream, Cache-Control: no-cache, and X-Accel-Buffering: no
			// the moment a streaming attempt starts, before any byte is
			// written, and if THAT PRIOR candidate then failed before
			// writing (still safe to fail over, THE HARD CONSTRAINT
			// below), those staged headers must not survive into a
			// plain-JSON response from THIS candidate. Safe to run
			// unconditionally once i > 0: this loop only ever reaches a
			// new iteration while sw.wroteHeader is false, so nothing
			// committed is ever at risk of being touched.
			//
			// Finding 7 fix (review-routes.md): RESTORE preFailoverHeaders
			// rather than delete every key outright — the round-2 fix
			// above correctly scoped the wipe to i > 0 only, but still
			// deleted EVERY header present at that point, including
			// whatever the earlier Traefik middleware staged before this
			// plugin ever ran, on the FAILOVER path specifically (the
			// round-2 fix's own proof only covered i == 0, the
			// non-failover path). Deleting first, then copying the
			// snapshot back, leaves exactly the pre-loop set in place —
			// any earlier candidate's own staged headers (X-Llmgw-Cache,
			// Content-Type, ...) are gone, and anything Traefik itself
			// set before this plugin ran survives a failover exactly as
			// it already did on the non-failover path.
			hdr := sw.Header()
			for k := range hdr {
				delete(hdr, k)
			}
			for k, v := range preFailoverHeaders {
				hdr[k] = v
			}
		}

		// attemptReq is a FRESH top-level copy of pristineReq for every
		// candidate (F1 fix, this function's own doc comment above).
		// gatewayAliasKey is (re-)injected here: every providerAdapter
		// implementation unconditionally deletes it before it ever
		// touches the network (its own doc comment, providers.go), so
		// each fresh copy needs it set again to echo the client's own
		// requested alias correctly (anthropic/gemini-type adapters read
		// it to build their own response envelope).
		attemptReq := make(map[string]any, len(pristineReq)+1)
		for k, v := range pristineReq {
			attemptReq[k] = v
		}
		attemptReq[gatewayAliasKey] = requestedModel

		// Cache lookup for THIS candidate, immediately before its own
		// attempt (adversarial-review round 2 ruling — supersedes the
		// round-1 "check every candidate's cache up front" pre-pass,
		// which silently substituted ANOTHER candidate's cached response
		// while the actual primary was perfectly healthy: proven —
		// alpha healthy and serving a distinct body, never called, the
		// client instead got beta's cached body with no failure and no
		// "failing over" log line, i.e. failover with no failure. Health
		// (F3/F6, this file/failover.go) is the correct mechanism for
		// "do not call a dead primary": once a genuinely broken
		// candidate is marked request-unhealthy it is excluded from the
		// candidate list entirely (orderedFailoverCandidates,
		// failover.go), so the NEXT candidate's cache is still reached
		// on a later request without ever dialing the dead one — the
		// pre-pass solved that problem by accident, with the side
		// effect of mis-serving healthy traffic from an unrelated
		// candidate's cache. cacheKeyStr is computed from pristineReq,
		// BEFORE gatewayAliasKey was injected above would have mattered
		// — gatewayAliasKey is a purely internal echo-back mechanism
		// cacheKey does not itself strip (its own doc comment,
		// cache.go), so hashing attemptReq instead would add noise no
		// two requests would otherwise disagree on.
		var cacheKeyStr string
		if cacheable {
			cacheKeyStr = cacheKey(cand.providerName, cand.upstreamModel, requestedModel, endpoint, pristineReq)
			if cached, hit := g.cache.lookup(cacheKeyStr); hit {
				sw.Header().Set("X-Llmgw-Cache", "hit")
				if cached.ContentType != "" {
					sw.Header().Set("Content-Type", cached.ContentType)
				}
				sw.WriteHeader(cached.Status)
				_, _ = sw.Write(cached.Body)
				// Cache-hit counters (Q7, DECISIONS: "cache-hit body parse
				// only when cache stats on: accept") — fire-and-forget
				// (recordCacheHit's own l.spawn), so this never delays the
				// response already written above. saved is computed with a
				// no-op warn sink, not g.pricingWarn: this is a re-derivation
				// of a price already known when the response was originally
				// served and cached, not a fresh unknown-model discovery
				// worth logging a second time.
				//
				// P4 fix (admin dashboard redesign verify round):
				// recordCacheHit itself already no-ops when !statsAdmin, but
				// cachedUsage(cached.Body) unmarshals the FULL cached body
				// first — measured 639µs for a 1 MiB body (defaultCache
				// MaxBodyBytes) — so that parse, and the unifiedCostMicros
				// derivation it feeds, must never run at all for a cache-
				// enabled, admin-disabled deployment, exactly like Q7 already
				// requires for a cache-enabled, admin-ENABLED-but-stats-off
				// case. Gating here, not just inside recordCacheHit, is what
				// actually saves the parse.
				if g.limiter.statsAdmin {
					saved := unifiedCostMicros(cand.canonical, cand.upstreamModel, cachedUsage(cached.Body), g.cfg.Pricing, g.cfg.ModelMeta, func(string) {})
					g.limiter.recordCacheHit(cand.canonical, saved)
				}
				return
			}
			// Set before call() below writes anything — headers must
			// precede the body a miss is about to produce (spec §2). A
			// later iteration's own miss overwrites this harmlessly: it
			// is never observable until SOME candidate actually writes,
			// by which point the loop has already committed to that
			// candidate's outcome (THE HARD CONSTRAINT, this function's
			// own doc comment).
			sw.Header().Set("X-Llmgw-Cache", "miss")
		}

		// respWriter is sw, wrapped in a capture tee only when this
		// request is cacheable — cacheCaptureWriter buffers everything
		// written so a 200 non-stream response can be stored after
		// call() returns, without any adapter knowing caching exists
		// (adapterCall's contract is unchanged either way). A fresh
		// capture per candidate: reusing one across attempts would let a
		// candidate that never wrote anything (the common failover case)
		// leave a stale buffer behind — capture.status stays 0 either
		// way, so this is defensive clarity, not a proven bug fix.
		var respWriter http.ResponseWriter = sw
		var capture *cacheCaptureWriter
		if cacheable {
			capture = newCacheCaptureWriter(sw, g.cache.maxBodyBytes)
			respWriter = capture
		}

		// Feature A (v0.22) attempt recorder, REBOUND every iteration to
		// THIS candidate's own provider name (feat/failover's own "never
		// misattribute a retried attempt to the first provider"
		// requirement) — every upstream attempt call() makes, via
		// upstreamJSON's retryPolicy.do (providers.go), reports through
		// here. limiter.recordProviderAttempt keeps its own existing
		// per-raw-attempt classification unchanged (limits.go); feat/
		// failover's OWN request-path health gate is recorded separately
		// below, once per candidate rather than once per raw attempt —
		// see requestHealthTracker.record's own doc comment (failover.go)
		// for why (F3/F6 fix, adversarial-review).
		providerName := cand.providerName
		upstreamModel := cand.upstreamModel

		// Security audit run-1, finding F-1 (high). An unpriced model is
		// billed 0, a zero cost writes no cost counter at all
		// (limiter.account, limits.go), and checkAndCount then evaluates
		// the money budget against that never-written counter — so
		// costPerDayUSD/costPerMonthUSD can never accrue and can never
		// fire. Serving the request would spend the operator's money
		// against a control that provably cannot stop it.
		//
		// Placed HERE, not in admitRequestForRoute, because admitRequestForRoute runs
		// before the body is decoded (runUnified's own ordering) and so
		// before any model id exists; this is the first point where the
		// SERVING model is known. It is before call(), so nothing has been
		// written to the client yet and the refusal is a clean envelope.
		//
		// Deliberately narrow: it fires only when some scope actually
		// configured a money budget, so a deployment with none is
		// unaffected, and token/request budgets are untouched — those
		// accrue correctly for an unpriced model and keep working.
		// review-auth finding F1 fix: priceKnown (modelmeta.go) — not a
		// bare pricing-overrides/built-in-table lookup — also recognizes
		// an operator's own modelMeta entry marking the model explicitly
		// Free, so a locally-hosted/free model already declared that way
		// is never refused here as though its price were unrecorded.
		if !g.cfg.AllowUnpricedWithCostBudget &&
			!priceKnown(cand.canonical, cand.upstreamModel, g.cfg.Pricing, g.cfg.ModelMeta) &&
			scopesHaveCostBudget(scopes) {
			g.logf("%s: refusing model %q: it has no configured price, and a cost budget applies to this caller that cannot be enforced without one", logPrefix, cand.canonical)
			g.recordUnpricedRefusalEvent(scopes, cand.canonical, cand.providerName, eventRouteForEndpoint(endpoint)) // F3 hook 5 (item 5, this round)
			envelope(sw, http.StatusPaymentRequired, "invalid_request_error",
				fmt.Sprintf("model %q has no configured price, so the cost budget that applies to this request cannot be enforced; add a \"pricing\" entry for it, or set allowUnpricedWithCostBudget to serve it unbounded", cand.canonical))
			return
		}

		ctx := withAttemptRecorder(r.Context(), func(resp *http.Response, attemptErr error) {
			g.limiter.recordProviderAttempt(providerName, upstreamModel, resp, attemptErr)
		})
		// feat: instrument upstream latency — gated on metricsEnabled OR
		// admin.stats.latency (admin-redesign WP-A), so a deployment with
		// BOTH off wires nothing here at all (not even the closure
		// allocation), one level up from watchdogBody's own per-Read nil
		// check (timeout.go). latSample/hasLat are candidate-LOCAL vars
		// (declared fresh each loop iteration, assigned as two separate
		// statements — never a tuple assignment mixing a call with another
		// expression, the Yaegi trap this file's own failover loop already
		// avoids elsewhere): g.recordLatency only fires when metrics are
		// actually enabled, but the sample itself is captured whenever
		// EITHER condition wired the recorder, so accountWith's own
		// opt-in latency counters (below) see it even when Prometheus
		// metrics are off.
		var latSample latencySample
		var hasLat bool
		if metricsEnabled(g.cfg) || g.limiter.statsLatency {
			ctx = withLatencyRecorder(ctx, func(sample latencySample) {
				latSample = sample
				hasLat = true
				if metricsEnabled(g.cfg) {
					g.recordLatency(providerName, upstreamModel, sample)
				}
			})
		}
		result, callErr := call(cand.adapter, ctx, respWriter, attemptReq)

		// Request-path health, recorded ONCE per candidate per logical
		// request (F3/F6 fix, adversarial-review) — see
		// requestHealthTracker.record's own doc comment (failover.go)
		// for the full ruling: failoverEligible's classification
		// (everything except HTTP 400 is a failure) is reused directly,
		// not limiter's narrower isTransient/isDeadlineExceeded one, so
		// a provider answering 401/403/404 to every request is
		// eventually routed around instead of retried forever. Guarded
		// by g.failover.enabled (adversarial-review round 2, finding 3):
		// this state is never READ on the disabled path
		// (orderedFailoverCandidates' own early return never calls
		// healthy()), so recording it there was dead work plus a new
		// global-mutex contention point (requestHealthTracker.stateFor's
		// own t.mu) on EVERY metered request, disabled or not.
		//
		// requestHealthOutcome (finding 16 fix, review-routes.md) — not a
		// bare !failoverEligible(callErr) — decides both WHETHER to
		// record at all and, when it does, what outcome: a client cancel
		// or a gateway-side translation error is recorded as NEITHER
		// success nor failure, since neither says anything about this
		// provider's own health, and the previous shape's implicit
		// "not eligible for failover" == "success" conflation let a
		// provider mixed with impatient or misconfigured clients never
		// reach requestBreakerFailureThreshold, so failover never routed
		// around it.
		if g.failover.enabled {
			if success, ok := requestHealthOutcome(callErr); ok {
				g.failoverHealth.record(providerName, success)
			}
		}

		// Usage is accounted before the error branch below runs, not
		// after: every adapter that can fail mid-stream (forwardStream
		// in each of the three provider files) still returns whatever
		// usage it had already captured alongside the error — a usage
		// chunk that arrived just before a dropped connection must
		// still be billed, or a client that aborts right after that
		// frame arrives could repeat the trick to dodge every
		// token/cost budget. A non-streaming failure (providerHTTPError,
		// translateError, or a connection failure before any write)
		// always carries zero usage by contract, so accounting it here
		// is a no-op — account skips every write once both total tokens
		// and cost are zero. A STREAMING failure with zero reported usage
		// is the one exception (finding 1 fix, review-routes.md, the
		// switch below): it gets the same body-size estimate a
		// non-streaming zero-usage response already did, rather than
		// being billed zero purely because the connection dropped or the
		// client walked away before the trailing usage chunk arrived.
		// routes_messages.go's callTranslatedMessages/
		// callAnthropicMessagesPassthrough follow the identical contract
		// for their own error paths (item 1 fix, 2026-08-22 review). This
		// still runs exactly once per CANDIDATE, including a candidate
		// that goes on to fail over — its own usage is billed as a no-op
		// only when it is genuinely zero (a non-streaming failure), never
		// skipped, so a later successful candidate's real usage is never
		// silently doubled with a phantom first entry either.
		if result.total() == 0 {
			switch {
			case streaming && callErr != nil && sw.wroteHeader:
				// Finding 1 fix (review-routes.md): a streaming response
				// that fails or is canceled before its usage chunk
				// arrives — most commonly the client itself disconnecting
				// right after the last content chunk, before the
				// trailing usage frame — must still be billed something,
				// or an attentive client can dodge every token/cost
				// budget on every request just by hanging up early once
				// it has read what it wanted. Apply the same body-size
				// prompt estimate the non-streaming zero-usage branch
				// below already uses.
				//
				// Verify-core fix (round 4): gated on sw.wroteHeader —
				// the stream must actually have started reaching the
				// client — so a pre-send failure (providerHTTPError,
				// translateError, a dial failure, or errRequestBuildFailed,
				// none of which ever wrote a byte to the client) is never
				// estimated here. Those fall through to the "streaming"
				// case below with no estimate, exactly like a non-
				// streaming failure. A candidate that fails over after
				// writing nothing must cost the SAME zero as one that
				// never streamed at all, or a failover pair is double-
				// billed: the failed candidate's phantom estimate plus
				// the next candidate's real usage.
				result.prompt = int64(math.Ceil(float64(len(body)) / 4))
				result.estimated = true
				g.logf("%s: streaming response from model %q failed or was canceled after starting to write to the client with zero usage reported; accounting an estimate derived from request body size", logPrefix, cand.canonical)
			case streaming:
				g.logf("%s: zero usage reported for a streaming response from model %q; accounting the request only", logPrefix, cand.canonical)
			case callErr == nil:
				result.prompt = int64(math.Ceil(float64(len(body)) / 4))
				result.estimated = true
			}
		}
		// Q1 fix (DECISIONS; pre-existing defect, WP-A step 7): kindModel's
		// own req counter used to count EVERY candidate account() ran for,
		// including one that failed before writing anything — a failover
		// chain of 3 candidates where only the last succeeded inflated
		// that model's served-request count by 2 phantom attempts. Adding
		// the model scope only when this candidate actually served
		// something (callErr == nil: a genuine 2xx, even a zero-usage
		// streaming one) or reported nonzero usage (result.total() > 0: a
		// mid-stream failure that still delivered content, billed via the
		// estimate branch above) excludes exactly the one remaining case —
		// callErr != nil && result.total() == 0, a candidate that failed
		// with nothing to show for it — from ever touching kindModel's req
		// counter at all. Token/cost counters were never affected by this
		// bug: they were already gated on nonzero usage independently.
		acctScopes := scopes
		if callErr == nil || result.total() > 0 {
			acctScopes = withModelScope(scopes, cand.canonical)
		}
		// P3 fix (admin dashboard redesign verify round): cacheMiss used to
		// be set from `cacheable` alone, true for EVERY candidate that
		// attempted its own cache lookup and missed — including one that
		// went on to fail over. A 3-candidate failover chain where every
		// candidate missed its own cache lookup then counted 1 request as
		// up to 3 cmiss increments, and a chain where an earlier candidate
		// missed-then-failed while a later one HIT double-counted the same
		// request as both a miss and a hit. Gated on the SAME condition as
		// the model-scope Q1 fix above: only the candidate whose own result
		// actually gets served/billed (or that reported nonzero usage
		// before failing) ever reports a miss, matching plan §2(d)'s "miss
		// -> cacheMiss extras in served candidate's account".
		extras := accountExtras{provider: providerName, cacheMiss: cacheable && (callErr == nil || result.total() > 0)}
		if i > 0 {
			extras.failoverFrom = candidates[i-1].providerName
		}
		if hasLat {
			extras.lat = latSample
			extras.hasLat = true
		}
		g.limiter.accountWith(acctScopes, result, unifiedCostMicros(cand.canonical, cand.upstreamModel, result, g.cfg.Pricing, g.cfg.ModelMeta, g.pricingWarn), extras)
		if result.estimated {
			g.logf("%s: usage for model %q logged as estimated (%d prompt tokens derived from request body size, not the provider's reported usage)", logPrefix, cand.canonical, result.prompt)
		}

		// Usage provenance (feat: expose token-accounting provenance),
		// recorded ONLY for a candidate whose own response actually
		// completed (callErr == nil): a candidate that failed over never
		// bills more than a zero-total no-op (this function's own doc
		// comment above), and counting THAT as "unbilled" would drown the
		// one signal this feature exists to surface — a provider silently
		// serving completions free against every budget — in ordinary
		// failover noise. "unbilled" is re-derived from streaming &&
		// result.total() == 0 rather than a separate flag carried out of
		// the block above: result.prompt/result.estimated are only ever
		// mutated by that block's non-streaming branch, so this condition
		// is true here if and only if the streaming branch above ran.
		// Gated on metricsEnabled exactly like withLatencyRecorder's own
		// wiring above, so a deployment with metrics off pays nothing
		// extra here. Observation only: nothing recorded here feeds back
		// into result or the account call above, both of which already
		// ran — see g.recordUsageProvenance's own doc comment (metrics.go).
		if callErr == nil && metricsEnabled(g.cfg) {
			provenance := provenanceReported
			switch {
			case result.estimated:
				provenance = provenanceEstimated
			case streaming && result.total() == 0:
				provenance = provenanceUnbilled
			}
			g.recordUsageProvenance(providerName, provenance, result.total())
		}

		// A miss stores the response after everything above has already
		// run — accounting must never be skipped or delayed waiting on a
		// cache write. Only a genuine upstream 200 is stored (spec §2's
		// "on 200 non-stream, tee and SET"): a non-2xx status never
		// reaches here as capture.status (forwardJSON/translate error
		// paths return *providerHTTPError/*translateError instead of
		// writing through respWriter, so callErr is non-nil and
		// capture.status stays 0), and callErr == nil is checked
		// directly regardless. capture.oversize excludes a response
		// cacheCaptureWriter stopped buffering past maxBodyBytes:
		// store's own maxBodyBytes check would reject it too, but only
		// after being handed a silently truncated body — skip the call
		// outright instead of ever constructing a corrupt cache entry.
		if cacheable && callErr == nil && capture.status == http.StatusOK && !capture.oversize {
			g.cache.store(cacheKeyStr, capture.status, capture.contentType, capture.buf.Bytes(), effectiveTTL(g.cache, grp))
		}

		if callErr == nil {
			return
		}

		// THE HARD CONSTRAINT: once sw has written anything, status and
		// body are committed and can never be retried elsewhere — a
		// mid-stream failure (headers, or a partial SSE body, already
		// reached the client) always falls straight through to the
		// existing, unchanged error classification, exactly as it did
		// before this feature existed, regardless of how many candidates
		// remain.
		if sw.wroteHeader {
			g.recordUpstreamEvent(scopes, cand.canonical, providerName, eventRouteForEndpoint(endpoint), callErr)
			g.handleAdapterErrorEnvelope(sw, callErr, providerName, logPrefix, envelope, writeUpstream)
			return
		}

		lastErr = callErr
		lastProviderName = providerName

		hasNext := i < len(candidates)-1 && failoverEligible(callErr)
		if hasNext && isProviderNotFoundError(callErr) {
			// Loud, non-suppressed log line (brief: "Failover after a 404
			// must log loudly ... a 404 usually means a real
			// configuration mistake, and silently succeeding elsewhere
			// would hide it") — deliberately g.errorf, not g.logf, and
			// deliberately its own line rather than folded into the
			// generic "failing over" line above the next iteration
			// already prints, and deliberately NOT gated by
			// failoverLogGate (operator ruling: this one must always
			// log).
			g.errorf("%s: provider %q returned 404 for model %q; failing over to %q — this usually means a real provider/model configuration mistake, not a transient outage", logPrefix, providerName, requestedModel, candidates[i+1].providerName)
		}
		if !hasNext {
			g.recordUpstreamEvent(scopes, cand.canonical, providerName, eventRouteForEndpoint(endpoint), callErr)
			g.handleAdapterErrorEnvelope(sw, callErr, providerName, logPrefix, envelope, writeUpstream)
			return
		}
		// Falls through to the next candidate.
	}
}

// buildLimitScopes returns the limitScope slice runUnified passes to the
// limiter: a user scope and one group scope PER MEMBER GROUP (grp.
// memberScopeGroups, auth.go — one for an ordinary, single-membership
// group, more for a multi-group principal; UserConfig.Groups' own "each
// member group's own limits apply as its own limit scope" rule,
// llmgateway.go), ALWAYS all of them, unconditionally (v0.21 fix — see
// this function's doc comment history below for the bug this closes).
// The user scope is listed first, so checkAndCount reports a user's own
// violation ahead of their group's when both are breached by the same
// request.
//
// u.limits or a member group's own limits may be nil — that scope simply
// carries no limit for checkAndCount (limits.go) to enforce, and
// checkAndCount's own nil-limits check skips it during evaluation. It is
// NOT omitted from the slice: usage
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
	members := grp.memberScopeGroups()
	scopes := make([]limitScope, 0, 1+len(members))
	scopes = append(scopes, limitScope{limits: u.limits, kind: "user", id: u.name})
	for _, mg := range members {
		scopes = append(scopes, limitScope{limits: mg.limits, kind: "group", id: mg.name})
	}
	return scopes
}

// withTotalScope returns scopes with the synthetic total scope
// (totalScopeKind/totalScopeID, limits.go) appended — the single helper
// every metered route (runUnified above, via admitRequestForRoute below;
// handleImagesGenerations/handleAudioSpeech/handleAudioTranscriptions,
// routes_media.go, via the same admitRequestForRoute; handleMessages,
// routes_messages.go, via the same admitRequestForRoute; handlePassthrough,
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

// admitRequestForRoute enforces per-user/per-group/total REQUEST-RATE
// limits (checkAndCount) using only u and grp — never a request body —
// so every caller can, and does, call this before reading or decoding
// anything (security review finding 1a, 2026-08-22): a caller already
// over budget is refused on the strength of who they are alone, never
// after paying the cost of reading and json-decoding a body that turns
// out to be discarded anyway. scopes is returned alongside ok so the
// caller can reuse the identical scope slice for its own later account
// call, rather than rebuilding (and risking scope-list drift from) a
// second buildLimitScopes/withTotalScope pair. A violation writes its
// response to sw itself, via envelope (writeLimitViolationEnvelope,
// below), records a rate_limit/budget/store_down event (F3 hook 1, v0.3
// dashboard task) via recordLimitEvent so GET /admin/api/events can
// attribute the rejection to route — one of the Route vocabulary
// constants, events.go — and returns ok=false.
//
// Shared verbatim by every unified/media/messages route (runUnified
// above; handleImagesGenerations/handleAudioSpeech/
// handleAudioTranscriptions, routes_media.go; handleMessages,
// routes_messages.go) — see runUnified's own "BEHAVIOR CHANGE" doc-comment
// section for the two previously-silent side effects this reordering has
// for a caller who is both over budget and sending a malformed/
// unroutable request; they apply identically here. envelope (item 7 fix,
// 2026-08-22 review) lets routes_messages.go answer a violation in
// Anthropic's own error shape instead of OpenAI's, without a second copy
// of this function's own logic.
func (g *Gateway) admitRequestForRoute(sw *statusTrackingWriter, u *user, grp *group, envelope envelopeWriter, route string) (scopes []limitScope, ok bool) {
	scopes = withTotalScope(buildLimitScopes(u, grp))
	if violation := g.limiter.checkAndCount(scopes); violation != nil {
		g.recordLimitEvent(scopes, route, violation)
		writeLimitViolationEnvelope(sw, violation, envelope)
		return scopes, false
	}
	// P9 fix (admin dashboard redesign verify round): last-seen is
	// recorded ONLY once this request is confirmed admitted — see
	// recordLastSeen's own doc comment (limits.go).
	g.limiter.recordLastSeen(scopes, g.limiter.now())
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
// either way. envelope (item 7 fix, 2026-08-22 review) picks the JSON
// shape that 503 gets written in.
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
func (g *Gateway) acquireBodyAdmission(sw *statusTrackingWriter, envelope envelopeWriter) (release func(), ok bool) {
	select {
	case g.bodyAdmission <- struct{}{}:
		return func() { <-g.bodyAdmission }, true
	default:
		sw.Header().Set("Retry-After", strconv.Itoa(bodyAdmissionRetryAfterSeconds))
		envelope(sw, http.StatusServiceUnavailable, "server_error", "server is at capacity; try again shortly")
		// F3 hook 5 (v0.3 dashboard task, Q4: "capacity 503 without user:
		// record user-less"): this semaphore is shared across every
		// unified/media/messages caller (readAndDecodeUnifiedBody/
		// decodeMediaJSONRequest/readAdmittedCapped) and fires before any
		// caller-specific context is threaded through, so the event
		// carries no User/Group/route attribution — routeCapacity is a
		// fixed, documented simplification (events.go's own doc comment).
		g.recordEvent(gatewayEvent{Route: routeCapacity, Kind: eventKindCapacity, Message: "server is at capacity; try again shortly", Status: http.StatusServiceUnavailable})
		return func() {}, false
	}
}

// readAndDecodeUnifiedBody claims a body-admission slot (acquireBodyAdmission),
// reads r's body capped at maxUnifiedRequestBytes, decodes it as a JSON
// object, and releases the slot before returning — on every path: a
// successful decode, a read failure, an oversize body, a decode failure,
// or admission exhaustion itself (security review finding 1b, round 3,
// 2026-08-22). This is deliberately the ENTIRE scope of what the
// semaphore guards for the unified/messages routes: by the time this
// returns, req is fully decoded and nothing further re-amplifies memory
// the way the decode itself does, so model resolution, the cache lookup,
// and the upstream round trip all run after the slot is already
// released. A read failure or a decode failure has already written its
// 400 to sw, through envelope (item 7 fix, 2026-08-22 review — routes_
// messages.go passes writeAnthropicError here so this shared step
// answers in this route's own error shape); ok reports whether the
// caller may proceed.
//
// Finding 14 fix (review-routes.md): readCapped (routes_media.go),
// already used by handleAudioTranscriptions to distinguish "the body was
// larger than the cap" from "the body was valid JSON, just malformed",
// replaces the plain io.LimitReader-and-decode this used before —
// io.LimitReader alone silently truncates an oversized body mid-token,
// so a request that was genuinely just too big (a 4.5MiB inline image
// against the 4MiB maxUnifiedRequestBytes cap) got the misleading
// "invalid JSON body" 400 instead of 413, giving the caller no way to
// tell "your body is too large" apart from "your JSON is malformed".
func (g *Gateway) readAndDecodeUnifiedBody(sw *statusTrackingWriter, r *http.Request, envelope envelopeWriter) (body []byte, req map[string]any, ok bool) {
	release, admitted := g.acquireBodyAdmission(sw, envelope)
	defer release()
	if !admitted {
		return nil, nil, false
	}

	body, oversize, err := readCapped(r.Body, maxUnifiedRequestBytes)
	if err != nil {
		envelope(sw, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return nil, nil, false
	}
	if oversize {
		envelope(sw, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return nil, nil, false
	}

	if err = json.Unmarshal(body, &req); err != nil {
		envelope(sw, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return nil, nil, false
	}
	return body, req, true
}

// unifiedCostMicros resolves the price to charge one request's usage
// against: canonical ("provider/model") first, falling back to bare — the
// upstream model id alone — only when canonical has no configured price at
// all, in either overrides or the built-in table. lookupPricing is used
// purely as a side-effect-free existence probe on canonical, and the real
// computation (and its unknown-model warning, when neither id has a price)
// runs through the one costMicrosKnownFor call that is actually charged.
//
// meta is checked FIRST, via modelMetaFree (modelmeta.go) — the exact
// same two-key lookup priceKnown itself uses to decide the 402 cost-
// budget guard — so a model an operator declared modelMeta:{free:true}
// bills 0 and never falls through to a real price it happens to share a
// bare id with in the built-in LiteLLM table (verify-core fix, round 4:
// before this, priceKnown treated such a model as priced, but billing
// never consulted modelMeta at all and charged the table's real price
// for it anyway — the 402 guard and billing disagreeing about the same
// model). This is the one exception to Pricing/ModelMeta's normal
// layering (Config.ModelMeta's own doc comment, llmgateway.go): every
// OTHER modelMeta field still only drives metadata exposure, never cost.
//
// warn is the caller's own per-instance pricing-warning sink (every call
// site passes g.pricingWarn, logger.go), so an unknown-model warning logs
// under this Gateway instance's own `llmgw[name]` prefix.
func unifiedCostMicros(canonical, bare string, u usage, overrides map[string]*ModelPricing, meta map[string]*ModelMetaConfig, warn func(string)) int64 {
	if modelMetaFree(canonical, bare, meta) {
		return 0
	}
	if _, ok := lookupPricing(canonical, overrides); ok {
		cost, _ := costMicrosKnownFor(canonical, u, overrides, warn)
		return cost
	}
	cost, _ := costMicrosKnownFor(bare, u, overrides, warn)
	return cost
}

// writeModelResolveErrorEnvelope maps a modelRegistry.resolve error to
// its HTTP status/message, written through whichever envelope the caller
// supplies (item 7 fix, 2026-08-22 review): errModelUnknown is a 404 (no
// configured provider knows this model), errModelDenied is a 403 (a real
// model the caller's group cannot use). A *aliasTargetError (spec §5,
// v0.2) is also a 404, but with its own message naming both the alias and
// its unresolved target, checked first via a plain type assertion — not
// errors.As, matching this package's established yaegi-safe convention
// for a pointer error type (registry.go's aliasTargetError doc comment).
// Any other error is a defensive 500 — resolve's own contract promises
// only these sentinels/types, so reaching this branch would be a
// programming error, not a client mistake.
func writeModelResolveErrorEnvelope(w http.ResponseWriter, err error, envelope envelopeWriter) {
	if aerr, ok := err.(*aliasTargetError); ok {
		envelope(w, http.StatusNotFound, "invalid_request_error", aerr.Error())
		return
	}
	switch {
	case errors.Is(err, errModelUnknown):
		envelope(w, http.StatusNotFound, "invalid_request_error", "unknown model")
	case errors.Is(err, errModelDenied):
		envelope(w, http.StatusForbidden, "invalid_request_error", "model access denied")
	default:
		envelope(w, http.StatusInternalServerError, "server_error", "internal error")
	}
}

// writeModelResolveError is writeModelResolveErrorEnvelope pinned to the
// OpenAI envelope shape — the thin wrapper every existing OpenAI-shaped
// call site (this file's runMeteredCall, via writeOAIError;
// routes_media.go's resolveMediaModel) keeps calling unchanged.
func writeModelResolveError(w http.ResponseWriter, err error) {
	writeModelResolveErrorEnvelope(w, err, writeOAIError)
}

// writeLimitViolationEnvelope maps a limiter violation to its HTTP
// status/message, written through whichever envelope the caller supplies
// (item 7 fix, 2026-08-22 review): a storeDown violation — the configured
// limit store was unreachable and failOpen is false — is a 503
// server_error, since it is not an actual limit breach; any other
// violation is a 429 rate_limit_error carrying v's own message, plus a
// Retry-After header when the violation names a meaningful retry window.
func writeLimitViolationEnvelope(w http.ResponseWriter, v *limitViolation, envelope envelopeWriter) {
	if v.storeDown {
		envelope(w, http.StatusServiceUnavailable, "server_error", v.message)
		return
	}
	if v.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(v.retryAfter))
	}
	envelope(w, http.StatusTooManyRequests, "rate_limit_error", v.message)
}

// writeLimitViolation is writeLimitViolationEnvelope pinned to the
// OpenAI envelope shape — the thin wrapper every existing direct call
// site (mcp_a2a.go, mcp_federation.go, routes_passthrough.go, all of
// which call checkAndCount themselves rather than through admitRequestForRoute)
// keeps calling unchanged.
func writeLimitViolation(w http.ResponseWriter, v *limitViolation) {
	writeLimitViolationEnvelope(w, v, writeOAIError)
}

// midStreamErrorMessage derives the client-safe (message, errType) pair
// an OpenAI-style `data: {"error":{...}}` SSE event carries when a
// stream fails after headers were already sent (finding 15 fix, review-
// routes.md) — the SAME classification handleAdapterErrorEnvelope's own
// type-switch below applies for the not-yet-committed case, reused here
// for TEXT only, since no HTTP status can be sent once headers are
// already on the wire: providerHTTPError's message and errType match
// writeProviderUpstreamError's own "providerName upstream error"/
// "upstream_error" (its upstream body is not re-embedded here — this is
// a best-effort terminal marker on an already-partially-delivered
// stream, not a second full error envelope); translateError reuses its
// own terr.msg under "invalid_request_error", exactly as the
// non-streamed envelope(...) call for it does; responseTranslationError
// and every other error share the identical "server_error" text the
// non-streamed generic branches below already use. ok is false only for
// context.Canceled — the client is gone, and the non-streamed branch
// below already treats that as log-only with no envelope either, so
// there is nothing useful to write to a connection nobody is reading.
func midStreamErrorMessage(err error, providerName string) (msg, errType string, ok bool) {
	if errors.Is(err, context.Canceled) {
		return "", "", false
	}
	if _, isPerr := err.(*providerHTTPError); isPerr {
		return providerName + " upstream error", "upstream_error", true
	}
	if terr, isTerr := err.(*translateError); isTerr {
		return terr.msg, "invalid_request_error", true
	}
	if _, isRTerr := err.(*responseTranslationError); isRTerr {
		return "failed to translate provider response", "server_error", true
	}
	return "upstream connection error", "server_error", true
}

// handleAdapterErrorEnvelope is the shared adapter-error classification
// every metered route goes through (item 7 fix, 2026-08-22 review):
// sw.wroteHeader is checked first, ahead of every other case, since an
// error surfacing after the adapter already started writing a response
// (a mid-stream connection drop) must never get a second, conflicting
// STATUS-CARRYING envelope appended, whatever kind of error it is.
// envelope/writeUpstream pick the client-facing shape; logPrefix names
// the route in every log line.
//
// Finding 15 fix (review-routes.md): when that already-started response
// was itself an SSE stream (Content-Type: text/event-stream, set by
// newSSEWriter before the first byte and still readable off sw.Header()
// here even though WriteHeader has already run), a mid-stream failure
// now emits one OpenAI-style `data: {"error":{...}}` event — and
// deliberately never a trailing "data: [DONE]\n\n" afterward, since this
// is not a clean completion — before returning. Every provider adapter's
// own forwardStream (provider_openai.go/provider_anthropic.go/
// provider_gemini.go) writes [DONE] only once its OWN upstream read loop
// finishes without error, so a dropped connection previously left the
// client's stream simply ending with no terminal signal at all: an SDK
// that does not strictly require [DONE] returned a silently truncated
// completion as an apparent success. A non-streaming response that wrote
// a partial body before failing gets no such event — there is no
// text/event-stream framing to append an event onto, and doing so would
// only corrupt an already-partial JSON body further.
func (g *Gateway) handleAdapterErrorEnvelope(sw *statusTrackingWriter, err error, providerName, logPrefix string, envelope envelopeWriter, writeUpstream providerUpstreamErrorWriter) {
	// A cacheable request's miss path pre-sets X-Llmgw-Cache: miss before
	// call() runs (runMeteredCall), so headers precede a successful body —
	// but an adapter error means nothing was actually served from, or
	// stored to, the cache. Del is unconditional and harmless when the
	// header was never set (a no-op on an absent key), so this needs no
	// cacheable-specific branch here: every error response — 400, 404,
	// 429, 501, 502 — must never carry a stale cache header from a
	// request that turned out not to succeed.
	sw.Header().Del("X-Llmgw-Cache")

	if sw.wroteHeader {
		g.errorf("%s: adapter error after response started (provider %q): %v", logPrefix, providerName, err)
		if msg, errType, ok := midStreamErrorMessage(err, providerName); ok &&
			strings.Contains(strings.ToLower(sw.Header().Get("Content-Type")), "text/event-stream") {
			errBody, _ := json.Marshal(map[string]any{ // json.Marshal on a literal map[string]any of strings never fails
				"error": map[string]any{"message": msg, "type": errType},
			})
			_ = newSSEWriter(sw).writeData(errBody) // best-effort: the connection may already be gone
		}
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
		writeUpstream(sw, providerName, perr)
		return
	}

	if terr, ok := err.(*translateError); ok {
		status := http.StatusBadRequest
		if terr.notSupported {
			status = http.StatusNotImplemented
		}
		envelope(sw, status, "invalid_request_error", terr.msg)
		return
	}

	// *responseTranslationError (routes_messages.go, item 1/12 fix,
	// 2026-08-22 review) is the /v1/messages route's own: a translation
	// failure AFTER a successful, already-billed upstream call (its usage
	// already travels back through call's own return value regardless of
	// this branch — see runMeteredCall's own comment on that). This
	// branch only ever fires for routes_messages.go's own call closures;
	// runUnified's adapterCall implementations never produce this type,
	// so it is always a no-op miss for that route. Kept here, in the one
	// shared classification, rather than a second copy of this whole
	// cascade in routes_messages.go, so the classification logic itself
	// never exists twice — see handleAdapterErrorEnvelope's own doc
	// comment (item 7).
	if terr, ok := err.(*responseTranslationError); ok {
		g.errorf("%s: response translation failed after a billed upstream call (provider %q): %v", logPrefix, providerName, terr.err)
		envelope(sw, http.StatusBadGateway, "server_error", "failed to translate provider response")
		return
	}

	if errors.Is(err, context.Canceled) {
		g.logf("%s: client canceled request to provider %q: %v", logPrefix, providerName, err)
		return
	}

	g.errorf("%s: upstream connection error (provider %q): %v", logPrefix, providerName, err)
	envelope(sw, http.StatusBadGateway, "server_error", "upstream connection error")
}

// handleAdapterError is handleAdapterErrorEnvelope pinned to the OpenAI
// envelope shape — the thin wrapper every existing OpenAI-shaped call
// site (routes_media.go's three media handlers) keeps calling unchanged.
// runMeteredCall (this file) calls handleAdapterErrorEnvelope directly,
// with whichever envelope its own caller supplied.
func (g *Gateway) handleAdapterError(sw *statusTrackingWriter, err error, providerName string) {
	g.handleAdapterErrorEnvelope(sw, err, providerName, "unified route", writeOAIError, writeProviderUpstreamError)
}

// decodeUpstreamErrorBody decodes perr.body as JSON when it parses as
// one, or returns it as a raw string otherwise — the shared read
// writeProviderUpstreamError and routes_messages.go's
// writeAnthropicProviderUpstreamError both embed under "upstream" in
// their own envelope shape (item 7/8 fix, 2026-08-22 review).
func decodeUpstreamErrorBody(perr *providerHTTPError) any {
	var upstream any = string(perr.body)
	var parsed any
	if json.Unmarshal(perr.body, &parsed) == nil {
		upstream = parsed
	}
	return upstream
}

// writeProviderUpstreamError passes perr's status through to the client,
// wrapped in the gateway's own envelope shape rather than perr.body
// verbatim (unified-route callers get this wrapped form; the openai-type
// adapter's own passthrough forwards a non-2xx body unwrapped, since that
// path never reaches this function). perr.body is embedded under
// error.upstream (decodeUpstreamErrorBody, above), decoded to a JSON
// value when it parses as one and left as a raw string otherwise, so a
// caller sees exactly what the provider said either way.
func writeProviderUpstreamError(w http.ResponseWriter, providerName string, perr *providerHTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(perr.status)
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"error": map[string]any{
			"message":  providerName + " upstream error",
			"type":     "upstream_error",
			"code":     strconv.Itoa(perr.status),
			"upstream": decodeUpstreamErrorBody(perr),
		},
	})
}

// cacheCaptureWriter tees a cacheable request's response into an
// in-memory buffer, capped at maxBodyBytes regardless of how large the
// real response turns out to be, while writing everything through to the
// wrapped *statusTrackingWriter unchanged — the mechanism runMeteredCall
// uses to fill the response cache (cache.go) on a miss without any
// adapter, or forwardJSON/forwardStream inside one, needing to know
// caching exists.
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
// maintains (handleAdapterErrorEnvelope inspects it via the original sw
// pointer, not through this wrapper, so a cacheCaptureWriter's
// WriteHeader/Write must delegate to the embedded pointer, never shadow
// its state).
type cacheCaptureWriter struct {
	*statusTrackingWriter
	contentType  string
	buf          bytes.Buffer
	maxBodyBytes int
	status       int
	// oversize is true once buf has reached maxBodyBytes: Write stops
	// appending to buf from that point on (the real write to the client
	// is never affected), and runMeteredCall skips calling store() entirely
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
