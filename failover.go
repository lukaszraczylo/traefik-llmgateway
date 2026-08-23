package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// This file implements feat/failover: cross-provider failover for a bare
// model id more than one configured provider serves (registry.go's
// bareWinner collision case — production logs show whisper-1, tts-1,
// tts-1-hd, text-embedding-3-small, text-embedding-3-large, and
// text-embedding-ada-002 served by two or more of openai/openai-audio/
// copilot today). Before this feature, registry.go's resolve returns
// exactly one provider adapter and retry.go's retryPolicy only ever
// retries the SAME provider — so a down provider fails every request for
// its models even when another configured provider serves the identical
// model. This file turns that already-existing redundancy into a hot
// standby, hooked into routes_unified.go's runMeteredCall, the ONE shared
// metered-call path both /v1/chat/completions and /v1/messages already go
// through.
//
// Off by default (FailoverConfig.Enabled defaults to false — coordinator
// ruling, overriding this feature's original brief: new behavior that can
// change which provider serves a request goes behind a flag defaulting
// off). An operator who opts in also gets a non-optional cost guard
// (candidateCostAllowed, below): failover never routes to a candidate
// priced higher than the provider it is failing over from, on either
// input or output cost per token — there is no config knob to disable
// just that part.
//
// SCOPE: routes_media.go's three media routes (images/generations,
// audio/speech, audio/transcriptions) are deliberately NOT wired into
// failover in this change, even though two of the brief's own named
// overlapping models (whisper-1, tts-1/tts-1-hd) are media-route
// traffic. Each of those three handlers has its own request-shape
// preprocessing (multipart rebuild for transcriptions, a JSON
// re-marshal for speech, a binary/streaming response for speech) that
// would need its own careful "nothing written yet" analysis and its own
// test coverage, rather than reusing runMeteredCall's shared shape — the
// brief's own hook-point instruction ("do not fork a second
// implementation") is about not duplicating the chat/messages pipeline,
// not about extending every possible call site in one pass. Left as a
// scoped follow-up; see this branch's own report for the explicit
// call-out.

// FailoverConfig configures feat/failover. Enabled is a *bool; nil or
// false (the default) means OFF — coordinator ruling, overriding this
// feature's original brief: a version upgrade with no config change must
// preserve prior behavior, and new behavior that can change which
// provider serves a request (shifting cost attribution, and able to mask
// a broken provider key behind an apparent success) goes behind a flag
// that defaults off, not on. Set Enabled: true to opt in. A
// single-provider deployment, or one where no two providers ever serve
// the identical bare model id, behaves exactly as before regardless of
// this setting either way: there is never more than one candidate to try
// (orderedFailoverCandidates' own doc comment).
type FailoverConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
	// MaxAttempts caps how many DIFFERENT PROVIDERS runMeteredCall tries
	// in total for one logical request, the primary included — distinct
	// from RetryConfig.Attempts (retry.go), which retries the SAME
	// provider. 0 (the default) uses defaultFailoverMaxAttempts. A value
	// outside 1..maxFailoverMaxAttempts is a construction error.
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

// defaultFailoverMaxAttempts is applied when FailoverConfig.MaxAttempts is
// left at 0. maxFailoverMaxAttempts bounds an explicit override, the same
// "cheap sanity ceiling on an operator value" precedent
// maxBreakerFailureThreshold/maxRetryAttempts already set (registry.go/
// retry.go).
const (
	defaultFailoverMaxAttempts = 3
	maxFailoverMaxAttempts     = 10
)

// failoverConfig is FailoverConfig, validated and defaulted once at
// construction (newGateway, llmgateway.go) — mirrors breakerConfig/
// retryPolicy's own resolved-value-not-raw-config shape (registry.go/
// retry.go).
type failoverConfig struct {
	maxAttempts int
	enabled     bool
}

// validateFailoverConfig validates fc and returns the resolved
// failoverConfig newGateway attaches to the Gateway. enabled defaults to
// false (coordinator ruling, FailoverConfig's own doc comment) — every
// config that predates this field, and every config that never sets
// failover.enabled at all, keeps behaving exactly as before this feature
// existed.
func validateFailoverConfig(fc FailoverConfig) (failoverConfig, error) {
	enabled := false
	if fc.Enabled != nil {
		enabled = *fc.Enabled
	}

	maxAttempts := fc.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultFailoverMaxAttempts
	}
	if maxAttempts < 1 || maxAttempts > maxFailoverMaxAttempts {
		return failoverConfig{}, fmt.Errorf("llmgateway: failover.maxAttempts must be between 1 and %d, got %d", maxFailoverMaxAttempts, maxAttempts)
	}

	return failoverConfig{enabled: enabled, maxAttempts: maxAttempts}, nil
}

// resolveCandidate is one provider that can serve a client-requested model
// id — modelRegistry.resolve's own (adapter, upstreamModel, canonical)
// triple, tagged with the provider's own name so a failover attempt can
// rebind its cache key, attempt recorder, and error log to the ACTUAL
// provider that served or failed the request, never the first one resolve
// happened to pick.
type resolveCandidate struct {
	adapter       providerAdapter
	providerName  string
	upstreamModel string
	canonical     string
}

// resolveWithCandidates resolves id exactly as modelRegistry.resolve
// (registry.go) does — identical winner, identical error — and
// additionally reports every OTHER configured provider that also serves
// the same bare upstream model id and remains authorized for grp, as
// feat/failover's candidate pool.
//
// primary/err are byte-identical to what resolve(id, grp) alone would
// return: this method calls resolve internally and never second-guesses
// its winner or its error — feat/failover only ADDS candidates after an
// already-successful resolution, it never changes which model or error a
// request resolves to (in particular, authorization denial/unknown-model
// behavior for the primary winner is completely unchanged by this
// branch). extra is nil whenever there is nothing to add:
//
//   - id resolved via an explicit "provider/model" prefix, or an alias
//     whose target is itself provider-prefixed: the requester's own
//     explicit provider choice is never a candidate for pool
//     substitution — ruling, this feature's own design: "provider/model"
//     addressing is exact, not eligible for failover.
//   - id resolved via a bare-id collision winner (bareWinner), or an
//     alias whose target is itself a bare-id collision winner: extra is
//     every OTHER configured provider whose known model set also
//     contains that identical bare id string, in
//     modelRegistry.providerNames (sorted) order, and authorized for grp
//     on that provider.
func (m *modelRegistry) resolveWithCandidates(id string, grp *group) (resolveCandidate, []resolveCandidate, error) {
	adapter, upstreamModel, canonical, err := m.resolve(id, grp)
	if err != nil {
		return resolveCandidate{}, nil, err
	}
	primary := resolveCandidate{adapter: adapter, providerName: adapter.name(), upstreamModel: upstreamModel, canonical: canonical}

	bareID := id
	if target, isAlias := m.aliases[id]; isAlias {
		if _, _, ok := m.splitConfiguredProvider(target); ok {
			return primary, nil, nil
		}
		bareID = target
	} else if _, _, ok := m.splitConfiguredProvider(id); ok {
		return primary, nil, nil
	}

	return primary, m.failoverCandidates(bareID, primary.providerName, grp), nil
}

// resolvePrimaryOnly resolves id exactly like resolveWithCandidates'
// primary return (identical winner, identical error) WITHOUT ever
// scanning for or building failover candidates — the short-circuit
// runMeteredCall (routes_unified.go) takes when g.failover.enabled is
// false, so a deployment that never opted into failover pays no extra
// cost for a pool it will never use (measured: 74ns/1 alloc versus
// 521ns/13 allocs at 8 providers x 200 models for the full
// resolveWithCandidates scan — irrelevant next to a network round trip,
// but free to avoid).
func (m *modelRegistry) resolvePrimaryOnly(id string, grp *group) (resolveCandidate, error) {
	adapter, upstreamModel, canonical, err := m.resolve(id, grp)
	if err != nil {
		return resolveCandidate{}, err
	}
	return resolveCandidate{adapter: adapter, providerName: adapter.name(), upstreamModel: upstreamModel, canonical: canonical}, nil
}

// failoverCandidates returns every configured provider other than
// primaryProvider whose known model set contains bareID and that grp is
// authorized to use, in modelRegistry.providerNames (sorted) order — the
// raw candidate pool orderedFailoverCandidates (below) narrows and
// reorders further by health.
//
// Only grp.allowsProvider is re-checked per candidate: grp.allowsModel is
// already known true from the primary winner's own successful resolve
// call, and — for the identical bare id string every candidate here
// shares by construction — allowsModel's result does not vary by
// provider (auth.go's allowsModel takes no provider argument), so
// re-checking it per candidate would be redundant work, not a
// correctness gap.
func (m *modelRegistry) failoverCandidates(bareID, primaryProvider string, grp *group) []resolveCandidate {
	var extra []resolveCandidate
	for _, name := range m.providerNames {
		if name == primaryProvider {
			continue
		}
		if !m.states[name].hasModel(bareID) || !grp.allowsProvider(name) {
			continue
		}
		// canonical assigned to a local before the struct literal, matching
		// resolveAgainst's own documented Yaegi precedent (registry.go): a
		// defensive, cheap precaution against the same class of interpreter
		// trap, not a proven necessity for a struct-literal field (only a
		// multi-value RETURN tuple is the proven-bad shape there).
		canonical := name + "/" + bareID
		extra = append(extra, resolveCandidate{
			adapter:       m.adapters[name],
			providerName:  name,
			upstreamModel: bareID,
			canonical:     canonical,
		})
	}
	return extra
}

// orderedFailoverCandidates builds runMeteredCall's actual try list from
// resolveWithCandidates' own (primary, extra) split.
//
// Failover is a no-op (returns exactly [primary]) whenever it is disabled
// (g.failover.enabled false) or there is nothing to add (len(extra) ==
// 0) — the second case is what makes "single-provider deployment behaves
// exactly as before" hold unconditionally, regardless of the enabled
// switch: there is never more than one candidate to try when no other
// configured provider shares this bare model id.
//
// Otherwise, two health signals apply, deliberately kept separate
// (modelRegistry.discoveryHealthy's own doc comment, registry.go, is
// binding on this):
//
//   - REQUEST-PATH health (g.failoverHealth, this file) is a HARD SKIP
//     gate: a candidate it reports unhealthy is excluded outright,
//     never attempted. If that filtering would remove every candidate
//     (this local, per-pod signal reporting the whole fleet down
//     simultaneously is more likely stale than true), it is discarded
//     and every original candidate is tried anyway — failing open
//     against the gateway's own imperfect signal rather than refusing
//     to route a request at all.
//   - DISCOVERY health (g.registry.discoveryHealthy) is an ORDERING HINT
//     ONLY, exactly as its own doc comment requires: a discovery-
//     unhealthy candidate is moved after every discovery-healthy/
//     half-open one (a stable partition, not a full sort — relative
//     order within each bucket is preserved), never dropped.
//
// This does NOT apply the cost guard or the maxAttempts cap — see
// runMeteredCall's own call sequence (routes_unified.go): candidates[0]
// coming out of THIS function is what filterCandidatesByCost (below)
// treats as "the primary" for pricing-guard purposes, so cost filtering
// must run against the health/discovery-ordered result, not the raw
// resolveWithCandidates order; and the maxAttempts cap runs LAST, after
// cost filtering, so a cap never re-admits an expensive candidate purely
// because a cheaper one ahead of it was removed on cost.
func (g *Gateway) orderedFailoverCandidates(primary resolveCandidate, extra []resolveCandidate) []resolveCandidate {
	if !g.failover.enabled || len(extra) == 0 {
		return []resolveCandidate{primary}
	}

	all := make([]resolveCandidate, 0, len(extra)+1)
	all = append(all, primary)
	all = append(all, extra...)

	healthy := make([]resolveCandidate, 0, len(all))
	for _, c := range all {
		if g.failoverHealth.healthy(c.providerName) {
			healthy = append(healthy, c)
		}
	}
	if len(healthy) == 0 {
		healthy = all
	}

	ordered := make([]resolveCandidate, 0, len(healthy))
	var deprioritized []resolveCandidate
	for _, c := range healthy {
		if g.registry.discoveryHealthy(c.providerName) {
			ordered = append(ordered, c)
		} else {
			deprioritized = append(deprioritized, c)
		}
	}
	return append(ordered, deprioritized...)
}

// resolveUnifiedPricing resolves (canonical, bare)'s price using the
// IDENTICAL resolution unifiedCostMicros itself uses (routes_unified.go):
// canonical id first, falling back to the bare upstream model id only
// when canonical has no configured price at all. ok is lookupPricing's
// own known/unknown distinction (pricing.go) — false means neither id
// resolved to a price, exactly the case where unifiedCostMicros would
// warn and bill 0, which candidateCostAllowed (below) must NEVER treat
// as an actual known-zero price.
func resolveUnifiedPricing(canonical, bare string, overrides map[string]*ModelPricing) (ModelPricing, bool) {
	if p, ok := lookupPricing(canonical, overrides); ok {
		return p, true
	}
	return lookupPricing(bare, overrides)
}

// candidateCostAllowed reports whether candidate is allowed to serve as a
// failover destination for primary, per the operator's binding ruling:
// failover must never route to a candidate more expensive than the
// provider it is failing over FROM, checked on BOTH the input and the
// output per-token price — a candidate cheaper on input but dearer on
// output can still cost more on a completion-heavy request, so a single
// blended number is not sufficient. This is NOT configurable (ruling:
// "this behaviour is ON and not optional").
//
// Four cases, all by ruling, not inferred:
//
//   - both known: allowed only when candidate's input AND output price
//     are each <= primary's. Equal is allowed either way.
//   - primary known, candidate UNKNOWN: never allowed — an unknown price
//     cannot be proven not-more-expensive.
//   - primary UNKNOWN, candidate known and non-zero: never allowed —
//     moving from unbilled to billed is itself a spend increase.
//   - primary UNKNOWN, candidate known and EXACTLY zero (both
//     dimensions), or both unknown: allowed — they are equal (a known
//     zero can never be more expensive than an unknown amount, and two
//     unknowns are trivially equal to each other).
//
// Unknown pricing is never treated as zero here, unlike costMicros' own
// billing behavior (pricing.go) — the gateway RECORDING zero for an
// unpriced model is an accounting shortcut, not proof the provider
// invoices zero, and this guard exists specifically to protect real
// money, not the gateway's own displayed number.
func candidateCostAllowed(primary ModelPricing, primaryKnown bool, candidate ModelPricing, candidateKnown bool) bool {
	switch {
	case primaryKnown && candidateKnown:
		return candidate.InputPerM <= primary.InputPerM && candidate.OutputPerM <= primary.OutputPerM
	case primaryKnown && !candidateKnown:
		return false
	case !primaryKnown && candidateKnown:
		return candidate.InputPerM == 0 && candidate.OutputPerM == 0
	default: // both unknown
		return true
	}
}

// filterCandidatesByCost drops every candidate whose resolved price
// candidateCostAllowed rejects against candidates[0] — the FIRST
// candidate actually attempted, never the cheapest in the list, per the
// operator's own definition of "primary" for this rule. Pricing comes
// from resolveUnifiedPricing against g.cfg.Pricing, the SAME overrides
// map unifiedCostMicros itself resolves against, so this compares
// against what would actually be billed. requestedModel names the
// client's own request in the log line a skip always produces — an
// operator-visible g.warnf, unthrottled, naming both providers, the
// model, and both resolved prices: a silently shortened candidate list
// would look identical to having no redundancy at all, which the
// operator must be able to tell apart from a cost-guard skip.
func (g *Gateway) filterCandidatesByCost(candidates []resolveCandidate, requestedModel string) []resolveCandidate {
	if len(candidates) < 2 {
		return candidates
	}
	primary := candidates[0]
	primaryPrice, primaryKnown := resolveUnifiedPricing(primary.canonical, primary.upstreamModel, g.cfg.Pricing)

	out := make([]resolveCandidate, 1, len(candidates))
	out[0] = primary
	for _, cand := range candidates[1:] {
		candPrice, candKnown := resolveUnifiedPricing(cand.canonical, cand.upstreamModel, g.cfg.Pricing)
		if candidateCostAllowed(primaryPrice, primaryKnown, candPrice, candKnown) {
			out = append(out, cand)
			continue
		}
		g.warnf("failover: skipping candidate provider %q for model %q: would cost more than primary provider %q (primary input=%.4f/M output=%.4f/M usd known=%v; candidate input=%.4f/M output=%.4f/M usd known=%v)",
			cand.providerName, requestedModel, primary.providerName,
			primaryPrice.InputPerM, primaryPrice.OutputPerM, primaryKnown,
			candPrice.InputPerM, candPrice.OutputPerM, candKnown)
	}
	return out
}

// capFailoverCandidates truncates candidates to at most max entries,
// applied as the LAST step of runMeteredCall's own candidate-building
// sequence (routes_unified.go) — after health/discovery ordering and
// after cost filtering, so the cap can never re-admit a candidate cost
// filtering already removed, and never discards a cheaper candidate that
// cost filtering kept purely because a removed, more expensive one used
// to sit ahead of it in the raw list. max <= 0 is a no-op (defensive;
// validateFailoverConfig never produces one).
func capFailoverCandidates(candidates []resolveCandidate, max int) []resolveCandidate {
	if max > 0 && len(candidates) > max {
		return candidates[:max]
	}
	return candidates
}

// failoverEligible reports whether callErr — runMeteredCall's own
// adapterCall return, evaluated only once nothing has reached the client
// yet (the hard constraint this whole feature is built around: once an
// adapter has written anything, status and body are committed and can
// never be retried elsewhere) — should try the next candidate instead of
// answering the client with callErr.
//
// Ruling (brief, "Which failures fall through" — decided, not
// relitigated here): every failure except HTTP 400 falls through. 5xx,
// connection failures, timeouts, 429, 401, 403, and 404 all do; 400
// never does, because the request itself is malformed and every provider
// would reject it identically, so retrying just multiplies latency and
// burns budget.
//
// Two failure shapes are excluded for the SAME "retrying changes
// nothing" reasoning 400 itself is excluded for, even though neither is
// literally an HTTP 400: context.Canceled (the client walked away —
// failing over would only waste an attempt against a connection nobody
// is waiting on) and errRequestBuildFailed (upstreamBytes, providers.go
// — the identical malformed request shape reaches every candidate the
// same way, differing only in which operator-configured base URL it is
// built against). A *translateError or *responseTranslationError is this
// gateway's OWN translation code failing on an already-decoded request —
// not a signal about the PROVIDER's availability at all — so neither is
// eligible either.
func failoverEligible(callErr error) bool {
	if callErr == nil {
		return false
	}
	if errors.Is(callErr, context.Canceled) || errors.Is(callErr, errRequestBuildFailed) {
		return false
	}
	// Plain type assertions, not errors.As — matching this package's
	// established Yaegi-safe convention for these exact two pointer error
	// types (providerHTTPError's own doc comment, providers.go;
	// handleAdapterErrorEnvelope, routes_unified.go, does the identical
	// assertion for the identical reason).
	if perr, ok := callErr.(*providerHTTPError); ok {
		return perr.status != http.StatusBadRequest
	}
	if _, ok := callErr.(*translateError); ok {
		return false
	}
	if _, ok := callErr.(*responseTranslationError); ok {
		return false
	}
	return true
}

// isProviderNotFoundError reports whether err is a *providerHTTPError
// carrying HTTP 404 — routes_unified.go's runMeteredCall uses this to
// decide when a failover needs its own loud, non-suppressed log line
// (brief: "Failover after a 404 must log loudly ... a 404 usually means
// a real configuration mistake, and silently succeeding elsewhere would
// hide it").
func isProviderNotFoundError(err error) bool {
	perr, ok := err.(*providerHTTPError)
	return ok && perr.status == http.StatusNotFound
}

// requestBreakerFailureThreshold/requestBreakerOpenDuration/
// requestBreakerMaxOpenDuration size requestHealthTracker's own breaker —
// deliberately separate constants from registry.go's discovery breaker
// (defaultBreakerFailureThreshold/defaultBreakerOpenDuration/
// defaultBreakerMaxOpenDuration): a request-path outage needs to be
// noticed and routed around within seconds, not the discovery breaker's
// minutes-to-hours cadence, and modelRegistry.discoveryHealthy's own doc
// comment forbids widening that breaker to cover this outcome stream at
// all. Fixed constants, not configurable — unlike FailoverConfig above,
// the brief only asks for max-attempts and an on/off switch to be
// operator-configurable; this breaker's thresholds are an internal
// implementation detail of the routing-gate signal, not a documented
// tuning surface.
const (
	requestBreakerFailureThreshold = 3
	requestBreakerOpenDuration     = 30 * time.Second
	requestBreakerMaxOpenDuration  = 5 * time.Minute
)

// requestHealthState is one provider's REQUEST-PATH health (feat/
// failover), mu-guarded — mirrors providerState's per-provider,
// individually-mutexed shape (registry.go) at a much smaller scale: this
// tracks exactly one outcome stream (request-path attempts), never
// discovery. The zero value is closed/healthy, so a provider that never
// fails never touches anything below open/backoff/openUntil.
//
// Field order below is fieldalignment-verified (golangci-lint's govet
// enable-all, run with -fix against a scratch copy to derive the exact
// zero-waste sequence, then hand-applied here so this doc comment
// survives — the same convention providerState's own doc comment
// explains, registry.go).
type requestHealthState struct {
	openUntil           time.Time
	backoff             time.Duration
	consecutiveFailures int
	mu                  sync.Mutex
	open                bool
}

// requestHealthTracker is feat/failover's own per-pod, in-memory,
// request-path health signal — deliberately separate from registry.go's
// discovery circuit breaker (modelRegistry.discoveryHealthy's own doc
// comment forbids widening that one to cover request-path traffic). Fed
// by failoverEligible's own classification (record's own doc comment
// explains why, not limiter.recordProviderAttempt's narrower isTransient/
// isDeadlineExceeded one), once per candidate per logical request, via
// record's own callers in routes_unified.go, but keeps its own
// independent state per provider name.
//
// Every method is nil-receiver-safe (both report "healthy"/no-op on a
// nil *requestHealthTracker), matching this package's own nil-means-
// disabled convention for an optional subsystem (responseCache's own doc
// comment, llmgateway.go's Gateway.cache field) — a Gateway assembled
// directly in a test without going through newGateway, or any future
// caller that does the same, degrades to "failover never skips a
// candidate for request health" rather than a nil-pointer panic.
type requestHealthTracker struct {
	states map[string]*requestHealthState
	nowFn  func() time.Time
	mu     sync.Mutex
}

// newRequestHealthTracker returns an empty tracker — every provider name
// reads healthy until its first recorded failure.
func newRequestHealthTracker() *requestHealthTracker {
	return &requestHealthTracker{states: make(map[string]*requestHealthState), nowFn: time.Now}
}

// stateFor returns provider's requestHealthState, creating it on first
// use.
func (t *requestHealthTracker) stateFor(provider string) *requestHealthState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.states[provider]
	if !ok {
		st = &requestHealthState{}
		t.states[provider] = st
	}
	return st
}

// record accounts ONE LOGICAL REQUEST's outcome for provider — called
// exactly once per candidate per client request, after call() has
// already exhausted whatever same-provider retries retryPolicy.do
// performed internally (routes_unified.go's runMeteredCall), never once
// per raw upstream attempt. success=false must be
// !failoverEligible(callErr) (routes_unified.go/failover.go's own
// classification of the FINAL adapter-level error) — adversarial-review
// ruling (F3/F6): an EARLIER version fed this from isTransient(resp,
// err) || isDeadlineExceeded(err), limits.go/retry.go's own
// retry-shaped classification, once per RAW attempt inside
// retryPolicy.do. That was wrong on two counts. isTransient treats a
// non-429 4xx as "the provider answered correctly to a request it did
// not like" — correct for deciding whether retrying the SAME provider
// is worth it, wrong for deciding whether the POOL should route around
// it: a provider whose API key is dead answers 401 to every request and
// must eventually be skipped, not retried forever, which is exactly the
// "masking a broken provider key" risk this feature exists to reduce.
// And recording per raw attempt let ONE client request with
// retry.enabled (attempts: 3) report FOUR failures against a threshold
// documented as three REQUESTS. failoverEligible already classifies
// "everything except HTTP 400" as a failure, matching what this gate
// needs with no separate function required.
//
// requestBreakerFailureThreshold consecutive failures opens the breaker
// for requestBreakerOpenDuration. While open, healthy (below) reports
// unhealthy until that window passes; once it has, the NEXT record call
// for this provider — success or failure — is what healthy already
// exposed as a post-cooldown probe attempt: success closes the breaker
// outright, failure re-opens it with a doubled backoff (capped at
// requestBreakerMaxOpenDuration). This is the same probe-then-escalate-
// or-close shape providerState's discovery breaker uses (registry.go's
// recordHealthLocked/openBreakerLocked), reimplemented independently
// rather than shared — the two track different outcome streams, and
// discoveryHealthy's own doc comment forbids widening it to cover this
// one.
//
// A failure arriving while ALREADY open and still within its cooldown
// window is a deliberate no-op (adversarial-review fix, F5): an earlier
// version fell through to the closed-state consecutiveFailures++ branch
// for this case too, which reset backoff to the flat base duration and
// slid openUntil forward on every such failure instead of leaving the
// existing window alone — doubling never actually happened outside the
// narrow post-cooldown probe path the doc comment claimed was the only
// place it did.
//
// Deliberately no single-flight guard on the probe window (unlike
// registry.go's tryBeginRefresh inFlight flag for its own, much lower-
// volume, background-goroutine probe): this signal sits on the live
// request path, where many concurrent requests can observe the same
// post-cooldown window at once. Serializing them would need its own
// lock-and-CAS dance for a benefit — one broken provider getting at most
// one real probe instead of a short burst of them right as its backoff
// expires — this per-pod, best-effort signal does not need to pay for.
func (t *requestHealthTracker) record(provider string, success bool) {
	if t == nil || provider == "" {
		return
	}
	st := t.stateFor(provider)
	st.mu.Lock()
	defer st.mu.Unlock()
	now := t.nowFn()

	if success {
		st.open = false
		st.consecutiveFailures = 0
		st.backoff = 0
		st.openUntil = time.Time{}
		return
	}

	// Already open, cooldown not yet elapsed: this failure changes
	// nothing (F5 fix, above).
	if st.open && now.Before(st.openUntil) {
		return
	}

	if st.open {
		// Cooldown elapsed: this failure IS the probe outcome. Escalate.
		if st.backoff <= 0 {
			st.backoff = requestBreakerOpenDuration
		} else {
			st.backoff *= 2
		}
		if st.backoff > requestBreakerMaxOpenDuration {
			st.backoff = requestBreakerMaxOpenDuration
		}
		st.openUntil = now.Add(st.backoff)
		return
	}

	st.consecutiveFailures++
	if st.consecutiveFailures >= requestBreakerFailureThreshold {
		st.open = true
		st.backoff = requestBreakerOpenDuration
		st.openUntil = now.Add(st.backoff)
	}
}

// healthy reports whether provider's request-path breaker currently
// permits routing to it — feat/failover's hard SKIP gate
// (orderedFailoverCandidates, above), distinct from
// modelRegistry.discoveryHealthy's deprioritize-only contract. An
// unrecorded provider (never failed, or never attempted at all)
// reads healthy — the same "never seen a failure" default the discovery
// breaker's own zero value already applies (providerState's doc
// comment, registry.go).
func (t *requestHealthTracker) healthy(provider string) bool {
	if t == nil || provider == "" {
		return true
	}
	st := t.stateFor(provider)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.open {
		return true
	}
	return !t.nowFn().Before(st.openUntil)
}
