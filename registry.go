package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultDiscoveryInterval is the refresh interval applied to a
// discovery-enabled provider whose ProviderConfig.DiscoveryInterval is
// empty.
const defaultDiscoveryInterval = time.Hour

// warmFillTimeout bounds newGateway's synchronous first discovery fill, per
// provider: a slow or unreachable upstream must not stall plugin
// construction indefinitely.
const warmFillTimeout = 5 * time.Second

// warmFillRetryDelay is how long warmFill waits, after a provider's
// synchronous initial discovery fetch fails, before retrying that ONE
// provider once in the background. Production incident (three separate
// pod-start rollouts, "openai-audio" and "anthropic" both observed):
// occasional cold-start egress latency lets a provider's listModels call
// exceed warmFillTimeout (5s) even though the same provider answers fine
// a few seconds later — without a retry, the provider is left with zero
// discovered models until the next request-driven maybeRefresh window,
// which tryBeginRefresh's interval gate can put up to the provider's own
// discoveryInterval away (defaultDiscoveryInterval: 1h), since
// finishRefresh advances lastRefresh on a FAILED attempt too, so the
// clock starts from the failed warm fill, not from whenever a request
// happens to arrive.
//
// A short, fixed delay — not a config knob — because this recovers a
// one-off cold-start blip, not a steady-state retry policy: a provider
// that fails again after this single retry falls back to the existing
// breaker behavior unchanged (recordHealthLocked, reached through the
// same finishRefresh bookkeeping the retry itself uses).
const warmFillRetryDelay = 10 * time.Second

// backgroundRefreshTimeout bounds one background refresh goroutine spawned
// by maybeRefresh. Not specified by the task brief; chosen generously
// enough for a provider's models-list call while still bounding a leaked
// goroutine's lifetime if an upstream hangs.
const backgroundRefreshTimeout = 30 * time.Second

// defaultBreakerFailureThreshold is how many CONSECUTIVE failed discovery
// refreshes open a provider's circuit breaker (feat/provider-health),
// applied when BreakerConfig.FailureThreshold is left at 0. Chosen so a
// single transient blip (one bad refresh) never trips it — only a
// provider that is persistently broken, like xiaomi's ongoing 401s, does.
const defaultBreakerFailureThreshold = 3

// maxBreakerFailureThreshold bounds BreakerConfig.FailureThreshold — a
// generous ceiling; there is no legitimate reason to require more
// consecutive failures than this before backing off at all.
const maxBreakerFailureThreshold = 100

// defaultBreakerOpenDuration is the base backoff a newly opened breaker
// waits before its first half-open probe, applied when BreakerConfig.
// OpenDuration is left empty.
const defaultBreakerOpenDuration = time.Minute

// defaultBreakerMaxOpenDuration caps the exponential backoff the breaker
// doubles into on every further failed probe, applied when BreakerConfig.
// MaxOpenDuration is left empty. Deliberately LONGER than
// defaultDiscoveryInterval (1h) — design ruling, adversarial review round
// 3: a breaker whose backoff can never exceed the plain interval cannot
// suppress anything, since tryBeginRefresh's interval gate (see its own
// doc comment) already applies unconditionally and only the LONGER of the
// two ever binds. An earlier value here (30m, shorter than the 1h
// interval) made the breaker cadence-neutral under stock config — exactly
// as strict an interval-only gate would already produce, with none of the
// suppression this feature exists to provide.
//
// L6 fix — corrected cadence: openBreakerLocked (below) doubles from
// defaultBreakerOpenDuration (1m), not from this constant, so the backoff
// stays UNDER the 1h interval gate — and therefore fully masked by it,
// per tryBeginRefresh's "whichever of the two is longer wins" rule — for
// its first ~6 doublings (1m, 2m, 4m, 8m, 16m, 32m): those probes still
// run once per hour, exactly as an interval-only gate would. Only once
// the backoff itself exceeds 1h does it start to govern: 64m, 128m,
// 256m, then capped at this constant's 360m (6h) from then on. A
// permanently broken provider sees roughly 13 discovery attempts on the
// day the breaker trips (the hourly-gated ramp plus the first few
// backoff-governed probes), settling to a steady state of 24h/360m =
// roughly 4 attempts a day thereafter, not 5.
const defaultBreakerMaxOpenDuration = 6 * time.Hour

// maxBreakerOpenDuration bounds BreakerConfig.MaxOpenDuration — the same
// ceiling-on-a-config-value precedent maxBreakerFailureThreshold already
// sets above, applied here because openBreakerLocked's backoff doubling
// has no overflow guard of its own: it relies on breakerOpenMax staying
// small enough that doubling a value already at or below it can never
// approach time.Duration's int64-nanosecond range. A pathological operator
// value with no ceiling (adversarial-review finding, round 2) would let
// repeated doublings wrap negative after ~28 probes, silently DISABLING
// backoff (a negative openUntil is always in the past). 24 hours is
// already an extremely generous cap — far longer than any deployment
// should ever want a single provider re-probed — and doubling twice from
// it (worst case before the cap check fires) lands nowhere near int64
// overflow, so the ceiling alone is the fix; openBreakerLocked itself
// needs no separate runtime guard.
const maxBreakerOpenDuration = 24 * time.Hour

// errModelUnknown is returned by modelRegistry.resolve when id does not
// match any provider's known model set (explicit config plus the last
// successful discovery fetch) — 404 semantics for the caller.
var errModelUnknown = errors.New("llmgateway: unknown model")

// errModelDenied is returned by modelRegistry.resolve when id is a known
// model but the requesting group is not authorized for it — 403 semantics
// for the caller. Distinct from errModelUnknown so the caller can tell "no
// such model" from "that model exists, you cannot use it".
var errModelDenied = errors.New("llmgateway: model access denied")

// breakerState is one provider's discovery circuit breaker state
// (feat/provider-health): closed (normal — refreshes run on the
// configured interval, exactly like before this feature existed), open
// (discovery refreshes are skipped until openUntil, backing off
// exponentially on repeated failure), or halfOpen (exactly one probe
// refresh is in flight, deciding whether to close the breaker again).
// The zero value is closed, so a providerState built without ever
// touching health fields (every existing construction path, and every
// provider that never fails) behaves exactly as it did before this type
// existed.
type breakerState int32

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// String renders bs for logs and the admin dashboard (feat/
// provider-health) — lower-case, hyphenated for the two-word state, to
// read naturally as a status word in either place.
func (bs breakerState) String() string {
	switch bs {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// providerState tracks one provider's known model ids and discovery
// bookkeeping. explicit is set once at construction from
// ProviderConfig.Models and never mutated afterward; discovered is
// stale-while-error — a failed refresh leaves the previous discovered set
// in place rather than clearing it. mu guards every mutable field below it.
//
// Field order below is fieldalignment-sensitive (golangci-lint's govet
// enable-all): every pointer-containing field (time.Time — its loc
// *Location field carries a pointer — plus every map/string) is grouped
// first, every pointer-free field (durations, sync.Mutex, the breaker's
// own int/int32 counters, and the two trailing bools) last. Keep new
// fields in the matching group rather than appending at the very end.
type providerState struct {
	lastRefresh time.Time
	// openUntil is when an open breaker (feat/provider-health) next
	// allows a half-open probe — see recordHealthLocked/tryBeginRefresh.
	// Grouped with lastRefresh above (both time.Time) rather than beside
	// the rest of the breaker fields further down, which are pointer-free.
	openUntil  time.Time
	explicit   map[string]bool
	discovered map[string]bool
	// discoveredContext holds discovery-captured per-model context
	// lengths (feature v0.23, ProviderConfig.MetadataPath — currently
	// only openai-type adapters ever populate this, via
	// modelMetadataFetcher), keyed by upstream model id. nil for a
	// provider with no metadataPath configured, or whose adapter does
	// not implement modelMetadataFetcher at all — contextFor treats a
	// nil map the same as an absent entry: (0, false). Stale-while-
	// error, the same convention discovered itself already uses: a
	// failed capture leaves the previous map in place rather than
	// clearing it (captureModelMetadata, below).
	discoveredContext map[string]int
	// lastErr is the most recent finishRefresh call's error message, or
	// "" when that call succeeded (or discovery is disabled and
	// finishRefresh was never called at all). Read by snapshot for the
	// admin dashboard (spec §4, v0.2) — it reflects the latest attempt's
	// outcome, not the stale-while-error discovered set: a provider can
	// show a non-empty lastErr while modelCount still reflects its last
	// successful discovery.
	lastErr  string
	interval time.Duration
	mu       sync.Mutex
	// --- feat/provider-health: discovery circuit breaker, guarded by mu
	// like every other mutable field above. consecutiveFailures counts
	// unbroken failures while closed, toward breakerThreshold. backoff is
	// the duration that produced openUntil above (doubled on the next
	// failed probe, capped at breakerOpenMax). breakerThreshold/
	// breakerOpenBase/breakerOpenMax are resolved once, at construction
	// (newModelRegistry, from Config.Breaker via validateBreakerConfig),
	// and never change afterward — copied onto every providerState
	// rather than shared from one place, the same "baked in at
	// construction" convention interval above already uses, so
	// recordHealthLocked/tryBeginRefresh never need a second parameter or
	// a pointer back to the registry. health is the breaker's current
	// state, declared last among these since breakerState is a 4-byte
	// int32, smaller than its 8-byte neighbors above.
	consecutiveFailures int
	backoff             time.Duration
	breakerThreshold    int
	breakerOpenBase     time.Duration
	breakerOpenMax      time.Duration
	health              breakerState
	discoveryEnabled    bool
	inFlight            bool
}

// hasModel reports whether id is in this provider's explicit or discovered
// model set.
func (st *providerState) hasModel(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.explicit[id] || st.discovered[id]
}

// knownIDsLocked returns the sorted union of st.explicit and
// st.discovered. It takes no lock itself — the caller must already hold
// st.mu — so both knownIDs (below) and snapshot (which needs the
// identical merge inside its own, already-held, critical section
// alongside lastRefresh/lastErr) can share this one implementation
// without knownIDs' own st.mu.Lock() double-locking (sync.Mutex is not
// reentrant) when called from inside snapshot.
func (st *providerState) knownIDsLocked() []string {
	set := make(map[string]bool, len(st.explicit)+len(st.discovered))
	for id := range st.explicit {
		set[id] = true
	}
	for id := range st.discovered {
		set[id] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// knownIDs returns the sorted union of this provider's explicit and
// discovered model ids.
func (st *providerState) knownIDs() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.knownIDsLocked()
}

// contextFor returns id's discovery-captured context length and whether
// one is known (feature v0.23) — mirroring hasModel's own locked-read
// shape.
func (st *providerState) contextFor(id string) (int, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	n, ok := st.discoveredContext[id]
	return n, ok
}

// setDiscoveredContext replaces st's discoveredContext wholesale on a
// successful metadata capture (feature v0.23, captureModelMetadata below)
// — the same replace-on-success, stale-while-error shape finishRefresh
// already applies to discovered.
func (st *providerState) setDiscoveredContext(m map[string]int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.discoveredContext = m
}

// tryBeginRefresh reports whether now permits starting a new refresh, and
// if so marks the provider inFlight so a concurrent caller cannot start a
// second one. The caller must pair a true result with a later finishRefresh
// call.
//
// The interval gate (now far enough past lastRefresh, or this is the first
// refresh) applies UNCONDITIONALLY, closed or open alike — fix for the
// adversarial-review blocker (round 2): an EARLIER version of this method
// let an open breaker's own backoff window (openUntil) REPLACE the
// interval gate rather than add to it. Since that round's default
// maxOpenDuration was shorter than the default interval, a permanently
// broken provider got probed roughly TWICE as often as before this
// feature existed — the exact opposite of "back off". Backoff can only
// ever make retries LESS frequent than the plain interval, never more: a
// closed breaker is gated by interval alone (exactly as before this
// feature existed — a provider that never fails never touches the
// breaker branch below at all); an open breaker is additionally gated by
// openUntil, so whichever of the two — interval or backoff — is currently
// longer wins. Reaching both starts exactly one half-open probe (state
// moves to halfOpen here, before the caller's fetch even runs) rather
// than resuming normal per-interval refreshing outright —
// recordHealthLocked decides whether that probe closes the breaker or
// reopens it with a longer backoff.
//
// PRACTICAL CONSEQUENCE, design ruling (round 3): whichever of interval
// and maxOpenDuration is larger is the one that actually governs a
// persistently broken provider's re-probe cadence — the smaller of the
// two is masked entirely, never merely "adds a little more delay". A
// deployment whose maxOpenDuration does not exceed its discoveryInterval
// gets NO suppression benefit from this feature at all; see
// defaultBreakerMaxOpenDuration's own doc comment for why the shipped
// defaults are chosen to avoid exactly that trap.
func (st *providerState) tryBeginRefresh(now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.inFlight {
		return false
	}
	if now.Sub(st.lastRefresh) < st.interval {
		return false
	}
	if st.health == breakerOpen {
		if now.Before(st.openUntil) {
			return false
		}
		st.health = breakerHalfOpen
	}
	st.inFlight = true
	return true
}

// tryBeginWarmFillRetry reports whether warmFill's one-shot background
// retry (scheduleWarmFillRetry/retryWarmFill) is still allowed to run
// against st, and if so marks it inFlight — the SAME field, and the SAME
// mutual-exclusion contract, tryBeginRefresh's own doc comment above
// already establishes for maybeRefresh's request-driven path. Fix 2/3
// (verify-retry.md): an EARLIER version of retryWarmFill called neither
// this nor tryBeginRefresh at all, so its listModels call could run
// CONCURRENTLY with an ordinary request-driven refresh already in flight
// against the identical provider — two in-progress fetches racing to
// finishRefresh in arbitrary completion order, plus (with a half-open
// breaker) a failed retry double-counting as the half-open probe. Sharing
// st.inFlight with tryBeginRefresh, under the SAME st.mu, makes the two
// mutually exclusive unconditionally: whichever caller's check-and-set
// runs first wins; the other's own guard (this one, or tryBeginRefresh's)
// observes inFlight already true and refuses — never a race window where
// both believe they won.
//
// sinceWarmFill is the exact finishRefresh timestamp the warm fill that
// scheduled THIS retry was recorded with (warmFillFailure.at, set once in
// warmFill and threaded through scheduleWarmFillRetry/
// scheduleOneWarmFillRetry unchanged). A refresh whose lastRefresh has
// already moved PAST that instant by the time this retry's timer fires —
// most plausibly a request-driven refresh admitted by a very short
// discoveryInterval, but any refresh path qualifies — has already
// recorded a result superseding whatever this retry would fetch, so the
// retry is redundant and skips even though inFlight is currently false
// again. This is also what makes tools/yaegi-check's own "brk" provider
// probe safe (fix 3, verify-retry.md): its 1ms discoveryInterval means a
// request-driven refresh reliably beats the 10s retry delay, so the
// retry always finds lastRefresh already past sinceWarmFill and skips,
// rather than racing exerciseBreaker's own hit-count assertions.
//
// The caller must pair a true result with a later finishRefresh call —
// identical contract to tryBeginRefresh's own — which clears inFlight
// unconditionally regardless of which of the two guards set it.
func (st *providerState) tryBeginWarmFillRetry(sinceWarmFill time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.inFlight {
		return false
	}
	if st.lastRefresh.After(sinceWarmFill) {
		return false
	}
	st.inFlight = true
	return true
}

// finishRefresh records the outcome of a refresh attempt started by
// tryBeginRefresh (or, for the synchronous warm fill, run unconditionally).
// lastRefresh always advances to now, on success or failure alike — that is
// what throttles a failing provider to one attempt per interval instead of
// retrying on every call. On success (err == nil) ids replaces the
// discovered set; on failure the previous discovered set is kept
// (stale-while-error). Either way, recordHealthLocked (feat/provider-health)
// updates the discovery circuit breaker from this same outcome — a healthy
// provider's breaker never leaves closed, so this adds no new behavior for
// the common case, only for a provider that is actually failing.
func (st *providerState) finishRefresh(now time.Time, ids []string, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.inFlight = false
	st.lastRefresh = now
	if err != nil {
		st.lastErr = err.Error()
	} else {
		st.lastErr = ""
		discovered := make(map[string]bool, len(ids))
		for _, id := range ids {
			discovered[id] = true
		}
		st.discovered = discovered
	}
	st.recordHealthLocked(now, err)
}

// recordHealthLocked updates st's discovery circuit breaker (feat/
// provider-health) from one finished refresh attempt's outcome. Caller must
// already hold st.mu.
//
// context.Canceled is NEUTRAL — checked first, before any classification —
// per adversarial-review ruling (round 2): a canceled discovery fetch
// reflects the CALLER walking away (plugin shutdown, an outer context
// canceled upstream), not the provider's fault. It must not count as a
// FAILURE (would wrongly trip an otherwise-healthy provider's breaker on a
// well-timed shutdown) and — the sharper bug an earlier version of this
// method had — must not count as a SUCCESS either: treating it as success
// would CLOSE an already-open breaker and wipe its backoff/counters purely
// because a half-open probe got interrupted before it could complete,
// reporting a still-broken provider as healthy again. Every OTHER field
// this method could touch is left exactly as it was; only lastRefresh/
// lastErr (finishRefresh, above, unconditionally) and inFlight change.
//
// A canceled probe while halfOpen is the one exception (round 3, MEDIUM
// finding): health reverts to OPEN with its backoff/openUntil left
// completely untouched, rather than staying at halfOpen. discoveryHealthy
// reports true for halfOpen (by design — a provider actively being
// re-probed must stay routable), so leaving a canceled probe's state at
// halfOpen indefinitely would report a still-broken provider healthy
// until the next attempt happens to land, which the plain interval gate
// alone does not bound tightly. Reverting to open costs nothing extra —
// tryBeginRefresh's own interval gate already runs before this is ever
// reached, so the NEXT attempt is admitted at the identical instant
// either way; only what discoveryHealthy reports in the meantime differs.
//
// context.DeadlineExceeded is NOT given the same neutral treatment as
// Canceled — deliberately: it counts as a plain failure, below, matching
// recordProviderAttempt's own SHOULD-1 ruling (limits.go) that a deadline
// the GATEWAY set and the upstream never answered inside is a genuine
// provider-health signal, not a caller-walked-away one. Three consecutive
// backgroundRefreshTimeout (30s) overruns trip the breaker exactly like
// three plain errors would.
//
// failed classifies every OTHER error explicitly for provider health,
// deliberately NOT reusing isTransient's retry-shaped semantics (retry.go)
// wholesale — an earlier version of this function did, and adversarial
// review found the gap: isTransient returns false for errRequestBuildFailed
// (retrying a malformed request build within the SAME attempt would never
// help, correct for RETRY purposes), so a provider whose baseURL is
// malformed — nothing validates it at construction — built errRequestBuildFailed
// on every single discovery cycle forever, and the old code routed that
// straight to closeBreakerLocked, wiping state every time: the breaker
// could never open despite continuous, permanent failure. For HEALTH
// purposes there is no such nuance to preserve: any non-nil, non-canceled
// error means this discovery attempt did not succeed, full stop.
//
// While closed, breakerThreshold consecutive failures open the breaker at
// its base backoff (breakerOpenBase); any success resets the counter to
// zero. breakerThreshold <= 0 (unreachable through newModelRegistry, which
// always resolves it to >= 1 via validateBreakerConfig, but reachable from
// a providerState assembled directly — every test in this file does that)
// disables the breaker entirely rather than tripping on the very first
// failure with a zero backoff: this is what makes providerState's own
// documented "zero value behaves exactly as before" claim (its own doc
// comment, above) actually true, not just aspirational.
//
// While halfOpen (the only other state finishRefresh can observe —
// tryBeginRefresh never starts a refresh from open without first moving to
// halfOpen), a successful probe closes the breaker outright; a failed one
// reopens it with a DOUBLED backoff, capped at breakerOpenMax. This ONLY
// suppresses re-probing when the resulting backoff exceeds discoveryInterval
// — design ruling, round 3: tryBeginRefresh's interval gate (its own doc
// comment) applies regardless of breaker state, so a maxOpenDuration
// shorter than discoveryInterval is masked entirely and this escalation
// changes nothing observable. defaultBreakerMaxOpenDuration is deliberately
// set longer than defaultDiscoveryInterval so this DOES suppress re-probing
// under stock config — see that constant's own doc comment for the
// resulting cadence.
func (st *providerState) recordHealthLocked(now time.Time, err error) {
	if errors.Is(err, context.Canceled) {
		if st.health == breakerHalfOpen {
			st.health = breakerOpen
		}
		return
	}
	failed := err != nil
	if st.health == breakerHalfOpen {
		if failed {
			st.openBreakerLocked(now, true)
		} else {
			st.closeBreakerLocked()
		}
		return
	}
	if !failed {
		st.closeBreakerLocked()
		return
	}
	if st.breakerThreshold <= 0 {
		return
	}
	st.consecutiveFailures++
	if st.consecutiveFailures >= st.breakerThreshold {
		st.openBreakerLocked(now, false)
	}
}

// closeBreakerLocked resets st to a healthy, closed breaker — reachable
// from a successful attempt while closed (nothing to do in practice, since
// consecutiveFailures is already 0, but harmless to reset) or a successful
// half-open probe (the "a provider that recovers must never stay dead"
// requirement). Caller must already hold st.mu.
func (st *providerState) closeBreakerLocked() {
	st.health = breakerClosed
	st.consecutiveFailures = 0
	st.backoff = 0
	st.openUntil = time.Time{}
}

// openBreakerLocked opens st's breaker as of now, computing the backoff
// window a subsequent tryBeginRefresh must wait out before its next
// half-open probe. escalate true (a failed half-open probe re-opening)
// doubles the PREVIOUS backoff; escalate false (the closed->open trip)
// starts fresh at breakerOpenBase — either way capped at breakerOpenMax so
// a persistently broken provider's backoff grows but never exceeds it.
// Caller must already hold st.mu.
func (st *providerState) openBreakerLocked(now time.Time, escalate bool) {
	if escalate && st.backoff > 0 {
		st.backoff *= 2
	} else {
		st.backoff = st.breakerOpenBase
	}
	if st.backoff > st.breakerOpenMax {
		st.backoff = st.breakerOpenMax
	}
	st.health = breakerOpen
	st.openUntil = now.Add(st.backoff)
}

// discoveryHealthy reports whether st's DISCOVERY circuit breaker is
// currently closed or half-open (not open) — see modelRegistry.
// discoveryHealthy's doc comment for the full contract this backs, in
// particular the scope limitation its name is deliberately chosen to
// signal: this reflects the discovery endpoint only.
func (st *providerState) discoveryHealthy() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.health != breakerOpen
}

// snapshot returns st's read-only view for the admin dashboard (spec §4,
// v0.2): its known model ids (explicit ∪ discovered, sorted — the
// provider-model-accordion task; a caller wanting just the count uses
// len(models)), last refresh time, and last refresh error message.
//
// models comes from knownIDsLocked (this method already holds st.mu, so
// it uses the lock-free variant directly rather than the locking
// knownIDs, which would deadlock reentering the same non-reentrant
// sync.Mutex), read in the SAME critical section as lastRefresh/lastErr
// below — not two separate locked calls. That matters functionally, not
// just stylistically: a concurrent finishRefresh landing between two
// separate locked sections could otherwise hand the admin dashboard a
// models list whose length disagrees with a separately-read modelCount;
// reading all three from one locked pass makes that impossible.
//
// A provider whose discovery is mid-refresh (or has never refreshed
// since a config reload) returns exactly this stale-while-error set —
// finishRefresh's own doc comment covers why: a failed refresh keeps the
// previous discovered set rather than clearing it. This method applies
// no special handling for that case; it is simply the same set every
// other snapshot consumer (modelCount, listFor) already reads.
// health and openUntil (feat/provider-health) join the same locked pass for
// the identical reason the doc comment above already gives for models/
// lastRefresh/lastErr: a concurrent finishRefresh must never be able to
// hand the admin dashboard a breaker state that disagrees with the
// lastErr/models it is shown alongside.
//
// openUntil is NOT returned raw (round 3 MEDIUM finding): since
// tryBeginRefresh's interval gate applies unconditionally (its own doc
// comment), the REAL next probe time is max(st.openUntil,
// lastRefresh+interval), which can differ sharply from st.openUntil alone
// — under stock defaults, a fresh trip sets st.openUntil to +1m while the
// first admitted probe is actually at +1h, a 59-minute skew an operator
// watching the dashboard would otherwise see as a broken countdown
// (formatUntil renders "in 60s", nothing happens, then the timestamp goes
// stale and the countdown vanishes for the remaining 59 minutes). This
// correction only ever applies while OPEN — st.openUntil is always the
// zero time while closed (closeBreakerLocked) or a past instant while
// half-open (a probe only ever starts once now has already reached it),
// neither of which this dashboard-facing value should be adjusted for.
func (st *providerState) snapshot() (models []string, lastRefresh time.Time, lastErr string, health breakerState, openUntil time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	effectiveOpenUntil := st.openUntil
	if st.health == breakerOpen {
		if floor := st.lastRefresh.Add(st.interval); floor.After(effectiveOpenUntil) {
			effectiveOpenUntil = floor
		}
	}
	return st.knownIDsLocked(), st.lastRefresh, st.lastErr, st.health, effectiveOpenUntil
}

// modelRegistry aggregates every configured provider's explicit and
// discovered model ids into the gateway's model-resolution and /v1/models
// listing surfaces. adapters, providerNames, and states are all fixed at
// construction; only providerState's own fields mutate afterward (each
// guarded by its own mutex). modelRegistry itself needs warnedMu for the
// collision-log dedup below, and modelsCacheMu for modelsCache (perf
// finding 3) — modelsGen is read/written only via sync/atomic and needs
// no mutex of its own.
type modelRegistry struct {
	adapters map[string]providerAdapter
	states   map[string]*providerState
	// log is a bound method value (g.errorf), injected rather than holding
	// a *Gateway directly. Every call site below pre-formats with
	// fmt.Sprintf and calls log("%s", msg) — never log(format, a, b, ...)
	// with two or more variadic arguments. Yaegi v0.16.1's CFG builder
	// panics ("index out of range") compiling a call to a struct FIELD of
	// variadic func type with 2+ variadic arguments; the identical call
	// shape through a method or interface method is unaffected — this is
	// specific to a field holding a func value. Verified empirically
	// against yaegi v0.16.1 (see tools/yaegi-check) and does not apply to
	// g.logf/g.errorf, which are methods, not fields. modelsJSON (perf
	// finding 3) follows this same rule: m.log("%s", fmt.Sprintf(...)),
	// never a second variadic argument.
	log func(string, ...any)
	// warn is the warning-level counterpart of log (g.warnf), for a
	// condition this registry resolved by itself with no request affected
	// — a model id two providers both serve, which resolve settles
	// deterministically. It is set by the production caller AFTER
	// newModelRegistry returns rather than being a fourth constructor
	// parameter, so the 45 existing call sites (almost all tests passing a
	// no-op) stay untouched: threading a parameter through all of them
	// would bury a log-severity change in unrelated churn. A nil warn
	// falls back to log, so a registry built without one keeps its old
	// behavior instead of panicking or silently dropping the line.
	//
	// The same Yaegi CFG restriction the log field documents above applies
	// here, for the same reason — it is a struct FIELD of variadic func
	// type: call it only as m.warn("%s", fmt.Sprintf(...)), never with two
	// or more variadic arguments.
	warn func(string, ...any)
	// info is the info-level counterpart of log/warn (g.logf), used only
	// by retryWarmFill's own "recovered after retry" success line: an
	// operator watching INFO-level logs during a rollout should see a
	// transient cold-start discovery failure self-heal, not silence.
	// Set by the production caller AFTER newModelRegistry returns, the
	// same "not a fourth constructor parameter" convention the warn
	// field's own doc comment explains above. A nil info falls back to
	// log (infof, below) — the same fallback shape warnf already has —
	// so a registry built without one (every existing test) keeps
	// compiling and reports the recovery at ERROR severity instead of
	// panicking or silently dropping the line.
	//
	// The same Yaegi CFG restriction the log/warn fields document above
	// applies here too: call it only as m.info("%s", fmt.Sprintf(...)),
	// never with two or more variadic arguments.
	info func(string, ...any)
	// aliases is the validated alias->target map from Config.ModelAliases
	// (spec §5, v0.2), built once by newModelRegistry via
	// validateModelAliases and never mutated afterward — resolve checks
	// it first, ahead of splitConfiguredProvider/bareWinner (see resolve's
	// own doc comment). nil (not merely empty) when the operator
	// configured no aliases at all, so every alias-aware code path is a
	// cheap nil-map lookup (always false/zero) that adds no branching
	// cost to the v0.1 no-aliases case.
	aliases map[string]string
	// modelMeta is the validated Config.ModelMeta config-override map
	// (feature v0.23), built once by newModelRegistry via
	// validateModelMeta and never mutated afterward. nil (not merely
	// empty) when the operator configured no metadata overrides at all,
	// so resolveModelMeta's config-override layer is a cheap nil-map
	// lookup (always a miss) that adds no branching cost to the
	// no-overrides case — the same nil-vs-empty convention aliases above
	// already establishes.
	modelMeta map[string]*ModelMetaConfig
	nowFn     func() time.Time
	warned    map[string]bool
	// modelsCache holds one encoded GET /v1/models response body per
	// group, tagged with the modelsGen it was built from (perf finding 3,
	// 2026-08-2x audit: 15.9ms/21.3MB/552,884 allocs per uncached call at
	// the live 1,033-model catalog) — see modelsJSON's own doc comment,
	// including MEDIUM-3's (review round 2) own key-shape correction:
	// keyed by group.modelsCacheKey()'s stable STRING signature, not a
	// *group pointer. Guarded by modelsCacheMu.
	modelsCache   map[string]modelsCacheEntry
	providerNames []string
	// modelsGen counts how many times finishRefresh (below) has recorded a
	// discovery attempt for ANY provider — bumped unconditionally, success
	// or failure, since a failed listModels fetch can still pair with a
	// successful metadata capture (captureModelMetadata's own doc comment)
	// that changes what listFor/modelsJSON would produce. Read and written
	// only via sync/atomic — modelsJSON (below) reads it outside any lock,
	// finishRefresh writes it from whichever goroutine (warmFill's caller,
	// or maybeRefresh's background goroutine) is recording that refresh.
	modelsGen int64
	// warmFillRetryDelay is the injectable form of the warmFillRetryDelay
	// constant above (its own doc comment has the full rationale) —
	// resolved once at construction (newModelRegistry, below) to that
	// constant, the same "baked in, never reassigned outside a test"
	// convention nowFn already establishes, so a test can shorten it
	// directly (registry_test.go) without a fake clock: unlike interval/
	// lastRefresh, this genuinely sleeps on a real timer (time.AfterFunc),
	// which m.nowFn's overridable fake clock cannot accelerate. Grouped
	// down here with the other pointer-free fields (fieldalignment), not
	// beside nowFn above, despite the doc-comment cross-reference.
	warmFillRetryDelay time.Duration
	// warmFillTimeout is the injectable form of the warmFillTimeout
	// constant above (its own doc comment has the full rationale) —
	// resolved once at construction (newModelRegistry, below) to that
	// constant, the same "baked in, never reassigned outside a test"
	// convention warmFillRetryDelay above already establishes. warmFill,
	// retryWarmFill, and captureModelMetadata's warm-fill budget (both
	// call sites, within warmFill and retryWarmFill) all read this field
	// rather than the package constant, so a test can shorten the whole
	// warm-fill budget directly (registry_test.go) without waiting out
	// the real 5s default — refreshProvider's own steady-state refresh
	// stays on backgroundRefreshTimeout, unaffected.
	warmFillTimeout time.Duration
	warnedMu        sync.Mutex
	// modelsCacheMu guards modelsCache.
	modelsCacheMu sync.Mutex
}

// modelsCacheEntry is one cached, encoded GET /v1/models response body
// (modelRegistry.modelsCache) plus the modelsGen it was built from.
type modelsCacheEntry struct {
	body []byte
	gen  int64
}

// breakerConfig is Config.Breaker (feat/provider-health), validated and
// defaulted once by validateBreakerConfig — the resolved values
// newModelRegistry copies onto every provider's providerState at
// construction (providerState's own doc comment explains why copied,
// not shared).
type breakerConfig struct {
	failureThreshold int
	openBase         time.Duration
	openMax          time.Duration
}

// validateBreakerConfig validates bc and returns the resolved breakerConfig
// (feat/provider-health) — mirrors newRetryPolicy (retry.go) and
// validateCacheConfig (cache.go)'s own zero-means-default, validated-and-
// resolved shape. bc's zero value (BreakerConfig{}, an operator who
// configures no breaker block at all) resolves to every default constant
// above unchanged: a provider that never fails never has a reason to
// consult any of these three numbers, so their defaults exist only to
// bound how a FAILING provider backs off, never to alter a healthy one.
func validateBreakerConfig(bc BreakerConfig) (breakerConfig, error) {
	threshold := bc.FailureThreshold
	if threshold == 0 {
		threshold = defaultBreakerFailureThreshold
	}
	if threshold < 1 || threshold > maxBreakerFailureThreshold {
		return breakerConfig{}, fmt.Errorf("llmgateway: breaker.failureThreshold must be between 1 and %d, got %d", maxBreakerFailureThreshold, threshold)
	}

	openBase := defaultBreakerOpenDuration
	if bc.OpenDuration != "" {
		d, err := time.ParseDuration(bc.OpenDuration)
		if err != nil {
			return breakerConfig{}, fmt.Errorf("llmgateway: breaker.openDuration %q is invalid: %w", bc.OpenDuration, err)
		}
		if d <= 0 {
			return breakerConfig{}, fmt.Errorf("llmgateway: breaker.openDuration must be positive, got %q", bc.OpenDuration)
		}
		openBase = d
	}

	openMax := defaultBreakerMaxOpenDuration
	if bc.MaxOpenDuration != "" {
		d, err := time.ParseDuration(bc.MaxOpenDuration)
		if err != nil {
			return breakerConfig{}, fmt.Errorf("llmgateway: breaker.maxOpenDuration %q is invalid: %w", bc.MaxOpenDuration, err)
		}
		if d <= 0 {
			return breakerConfig{}, fmt.Errorf("llmgateway: breaker.maxOpenDuration must be positive, got %q", bc.MaxOpenDuration)
		}
		// Ceiling (adversarial-review finding, round 2): see
		// maxBreakerOpenDuration's own doc comment for why an unbounded
		// value here is an overflow risk, not merely an odd config choice.
		if d > maxBreakerOpenDuration {
			return breakerConfig{}, fmt.Errorf("llmgateway: breaker.maxOpenDuration must be at most %s, got %q", maxBreakerOpenDuration, bc.MaxOpenDuration)
		}
		openMax = d
	}

	if openMax < openBase {
		return breakerConfig{}, fmt.Errorf("llmgateway: breaker.maxOpenDuration (%s) must be >= breaker.openDuration (%s)", openMax, openBase)
	}

	return breakerConfig{failureThreshold: threshold, openBase: openBase, openMax: openMax}, nil
}

// newModelRegistry builds a modelRegistry from adapters and cfg's matching
// ProviderConfig entries. It does not perform discovery itself — the
// caller (newGateway) runs the synchronous first fill separately via
// warmFill, then ServeHTTP entry keeps it fresh via maybeRefresh. A
// provider's ProviderConfig.DiscoveryInterval that fails time.ParseDuration
// is a constructor error; an empty one defaults to defaultDiscoveryInterval.
func newModelRegistry(adapters map[string]providerAdapter, cfg *Config, log func(string, ...any)) (*modelRegistry, error) {
	breaker, err := validateBreakerConfig(cfg.Breaker)
	if err != nil {
		return nil, err
	}

	m := &modelRegistry{
		adapters:           adapters,
		states:             make(map[string]*providerState, len(adapters)),
		log:                log,
		nowFn:              time.Now,
		warmFillRetryDelay: warmFillRetryDelay,
		warmFillTimeout:    warmFillTimeout,
		warned:             make(map[string]bool),
		providerNames:      make([]string, 0, len(adapters)),
		modelsCache:        make(map[string]modelsCacheEntry),
	}
	for name := range adapters {
		m.providerNames = append(m.providerNames, name)
	}
	sort.Strings(m.providerNames)

	// explicitModels is the union of every configured provider's EXPLICIT
	// model ids only (never discovered ones — discovery has not run yet
	// at this point in construction) — validateModelAliases' collision
	// check needs exactly this set (spec §5: "alias must not equal an
	// explicit model id of any provider").
	explicitModels := make(map[string]bool)
	for _, name := range m.providerNames {
		pc := cfg.Providers[name]
		if pc == nil {
			pc = &ProviderConfig{}
		}

		interval := defaultDiscoveryInterval
		if pc.DiscoveryInterval != "" {
			// parseErr, not err: newModelRegistry's own outer err (from
			// validateBreakerConfig above) is already in scope here, and
			// govet's shadow check (golangci-lint's govet enable-all)
			// flags a nested ":=" reusing that identifier even though it
			// is scoped to this if-block only.
			d, parseErr := time.ParseDuration(pc.DiscoveryInterval)
			if parseErr != nil {
				return nil, fmt.Errorf("llmgateway: provider %q: invalid discoveryInterval %q: %w", name, pc.DiscoveryInterval, parseErr)
			}
			// L5 fix: a non-positive discoveryInterval ("0s", or a
			// negative value ParseDuration itself does not reject) is a
			// misconfiguration, not a valid "always refresh" setting —
			// tryBeginRefresh's own throttle gate (below) never suppresses
			// a same-window re-probe when the interval is <= 0, so every
			// request would spawn a new listModels the instant the
			// previous one finishes, hammering the upstream back-to-back.
			// Mirrors resolveRequestTimeout's identical "%s must be
			// positive" rejection (timeout.go) for the same class of
			// field.
			if d <= 0 {
				return nil, fmt.Errorf("llmgateway: provider %q: discoveryInterval must be positive, got %q", name, pc.DiscoveryInterval)
			}
			interval = d
		}

		explicit := make(map[string]bool, len(pc.Models))
		for _, id := range pc.Models {
			explicit[id] = true
			explicitModels[id] = true
		}

		m.states[name] = &providerState{
			explicit:         explicit,
			discovered:       make(map[string]bool),
			discoveryEnabled: pc.Discovery,
			interval:         interval,
			breakerThreshold: breaker.failureThreshold,
			breakerOpenBase:  breaker.openBase,
			breakerOpenMax:   breaker.openMax,
		}
	}

	aliases, err := validateModelAliases(cfg.ModelAliases, m.providerNames, explicitModels)
	if err != nil {
		return nil, err
	}
	m.aliases = aliases

	modelMeta, err := validateModelMeta(cfg.ModelMeta)
	if err != nil {
		return nil, err
	}
	m.modelMeta = modelMeta

	return m, nil
}

// isValidModelID reports whether id satisfies spec §5's alias model-id
// shape: non-empty, and free of ASCII control characters (0x00-0x1F and
// 0x7F) — the same class of raw value mcp_a2a.go's target-URL validation
// already rejects, applied here to a model/alias id string instead of a
// URL.
func isValidModelID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validateModelAliases validates cfg.ModelAliases per spec §5's
// validation table and returns the accepted alias->target map (nil when
// raw is empty — newGateway's default, zero-behavior-change case).
// providerNames is the registry's own sorted provider-name list (used for
// the prefix-shadow check); explicitModels is the union of every
// configured provider's EXPLICIT model ids only (used for the collision
// check) — a DISCOVERED model colliding with an alias is NOT a
// construction error (discovery is dynamic and runs after this), and is
// instead resolved in the alias's favor by resolve's own precedence; see
// resolve's warnAliasShadowsDiscoveredOnce call for the one-time runtime
// log that implies.
//
// raw is iterated in sorted alias-key order, not Go's randomized map
// order, so a config with more than one invalid alias always reports the
// same one first, deterministically, across repeated runs.
func validateModelAliases(raw map[string]string, providerNames []string, explicitModels map[string]bool) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	providerSet := make(map[string]bool, len(providerNames))
	for _, p := range providerNames {
		providerSet[p] = true
	}

	names := make([]string, 0, len(raw))
	for alias := range raw {
		names = append(names, alias)
	}
	sort.Strings(names)

	out := make(map[string]string, len(raw))
	for _, alias := range names {
		target := raw[alias]
		if !isValidModelID(alias) {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q is invalid: must be non-empty with no control characters", alias)
		}
		if prefix, _, hasSlash := strings.Cut(alias, "/"); hasSlash && providerSet[prefix] {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q is invalid: its prefix %q names a configured provider and would be shadowed by provider-prefix resolution", alias, prefix)
		}
		if explicitModels[alias] {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q collides with an explicitly configured model id of the same name", alias)
		}
		if target == "" {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q has an empty target", alias)
		}
		if _, targetIsAlias := raw[target]; targetIsAlias {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q targets %q, which is itself an alias (alias chains are not allowed)", alias, target)
		}
		out[alias] = target
	}
	return out, nil
}

// now returns the registry's current time, via nowFn — overridable in
// tests to exercise refresh throttling deterministically.
func (m *modelRegistry) now() time.Time {
	return m.nowFn()
}

// discoveryHealthy reports whether name's DISCOVERY circuit breaker is
// currently closed or half-open (feat/provider-health's queryable health
// signal). false only while the breaker is OPEN: an unconfigured provider
// name, one with discovery disabled (it has no health signal to trip the
// breaker with, so it stays closed forever), and one whose breaker is
// closed OR half-open all report true — a half-open provider is actively
// being re-probed by the next discovery cycle and must stay routable, per
// this feature's own "a provider that recovers must never stay dead"
// requirement; only an OPEN breaker, still backing off, reports false.
//
// SCOPE, per adversarial-review ruling (round 2) — read this before wiring
// it into any routing decision: this reflects the DISCOVERY endpoint
// (/v1/models-equivalent) ONLY. It is driven exclusively by
// finishRefresh's own outcomes and knows nothing about
// /v1/chat/completions or any other request-path traffic. A provider
// whose discovery listing 401s or 404s while its actual completion
// endpoint serves requests perfectly fine — common behind a proxy that
// exposes chat but not model listing — reports false here despite being
// entirely usable for real traffic. feat/failover MUST NOT treat a false
// result here as a sole reason to refuse routing; request-path health is
// a SEPARATE signal, fed by limiter.recordProviderAttempt (limits.go),
// deliberately kept apart so discovery outages and request-path outages
// are never conflated under one name.
//
// This is a single mutex-guarded read of state finishRefresh/
// tryBeginRefresh already maintain — no round trip, no store lookup — per
// this feature's own design: per-pod, in-memory, a latency optimization
// rather than a correctness guarantee. Three pods learning independently
// (each with its own view of a provider's health) is accepted and
// intended, not a bug.
func (m *modelRegistry) discoveryHealthy(name string) bool {
	st, ok := m.states[name]
	if !ok {
		return true
	}
	return st.discoveryHealthy()
}

// finishRefresh records the outcome of a refresh attempt (st.finishRefresh)
// and invalidates modelsJSON's cache by bumping modelsGen — the ONLY two
// places that ever mutate st.discovered or st.discoveredContext both
// funnel through here (warmFill, refreshProvider), so every change
// listFor's output could possibly reflect is covered. Bumped
// unconditionally, not just on a successful refresh: captureModelMetadata
// (both call sites' own doc comments) always runs immediately alongside
// this same refresh cycle regardless of whether listModels itself
// succeeded, and a metadata-only change must invalidate the cache just as
// much as a discovered-set change would.
func (m *modelRegistry) finishRefresh(st *providerState, now time.Time, ids []string, err error) {
	st.finishRefresh(now, ids, err)
	atomic.AddInt64(&m.modelsGen, 1)
}

// warmFillFailure records ONE provider whose synchronous warmFill
// discovery fetch just failed and is eligible for scheduleWarmFillRetry's
// one-shot background retry (production incident fix, warmFillRetryDelay's
// own doc comment). at is the exact finishRefresh timestamp that failed
// attempt was recorded with — the SAME m.now() value warmFill itself used
// for THIS provider's finishRefresh call, threaded unchanged through
// scheduleWarmFillRetry/scheduleOneWarmFillRetry to retryWarmFill's own
// tryBeginWarmFillRetry guard (fix 2/3, verify-retry.md) as sinceWarmFill:
// a refresh whose lastRefresh has already moved past this exact instant
// by the time the retry's timer fires has already superseded whatever
// this retry would fetch, so the retry skips rather than racing or
// duplicating it.
type warmFillFailure struct {
	at   time.Time
	name string
}

// warmFill performs newGateway's synchronous first discovery fill: for
// every discovery-enabled provider, it fetches listModels once, bounded by
// warmFillTimeout, and records the result via finishRefresh. A fetch error
// is logged and otherwise non-fatal — construction still succeeds, and the
// provider's known model set is whatever its explicit config already
// provides (empty, if it configured neither explicit models nor a
// reachable discovery endpoint) until either the one-shot retry below
// recovers it, or the next maybeRefresh window does.
//
// Production incident fix: every provider whose fetch failed here is
// handed to scheduleWarmFillRetry (below), which retries each of them
// exactly once, in the background, after warmFillRetryDelay — see that
// constant's own doc comment for why a plain interval-gated maybeRefresh
// alone leaves a cold-start blip unrecovered for up to an hour. This adds
// no cost to warmFill itself: scheduling is a single time.AfterFunc call
// per failed provider, not a blocking wait.
//
// A fetch that failed because the PARENT ctx was canceled (plugin
// construction itself aborted — shutdown, not a slow/broken upstream) is
// excluded from the retry list (fix 4a, verify-retry.md): retryWarmFill
// has no shutdown signal of its own to listen on (its own doc comment
// explains why), so scheduling one anyway would keep retrying a provider
// nobody is waiting on any more. Mirrors recordHealthLocked's own
// "Canceled is neutral, not a failure" treatment of the breaker, extended
// here to the separate scheduling decision.
func (m *modelRegistry) warmFill(ctx context.Context) {
	var failed []warmFillFailure
	for _, name := range m.providerNames {
		st := m.states[name]
		if !st.discoveryEnabled {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, m.warmFillTimeout)
		ids, err := m.adapters[name].listModels(fctx)
		cancel()
		now := m.now()
		m.finishRefresh(st, now, ids, err)
		if err != nil {
			// sanitizeProviderErr (admin.go): the error commonly embeds the
			// dialed URL verbatim, so a baseUrl an operator misconfigured
			// with embedded credentials would otherwise be written to
			// stderr in clear — which m.log (g.errorf, logger.go) does
			// unconditionally and unfiltered, against its own "Never log
			// key material" contract. Security audit run-1, finding F-2.
			m.log("%s", fmt.Sprintf("model registry: initial discovery for provider %q failed: %v",
				name, sanitizeProviderErr(err.Error(), m.adapters[name].base())))
			// Fix 4a (verify-retry.md): see this function's own doc
			// comment above — a parent-ctx cancellation gets no retry.
			if !errors.Is(err, context.Canceled) {
				failed = append(failed, warmFillFailure{name: name, at: now})
			}
		}
		// Own timeout budget, review fix (SHOULD-4): fctx above is
		// spent by listModels — reusing it here would hand the metadata
		// fetch an already-near-expired (or fully expired, on a slow/
		// timed-out listModels) context every warm fill, guaranteeing a
		// spurious deadline error and log line even for a healthy
		// provider whose model-list call merely took a while.
		mctx, mcancel := context.WithTimeout(ctx, m.warmFillTimeout)
		m.captureModelMetadata(mctx, name, m.adapters[name], st)
		mcancel()
	}
	if len(failed) > 0 {
		m.scheduleWarmFillRetry(failed)
	}
}

// modelMetadataFetcher is an OPTIONAL providerAdapter capability (feature
// v0.23): an adapter that can also fetch per-model discovery metadata
// (currently: context window) beyond the plain model-id list listModels
// returns. Only openai-type adapters configured with ProviderConfig.
// MetadataPath implement it (provider_openai.go's openaiAdapter) —
// anthropic/gemini adapters do not, so captureModelMetadata checks for
// this via a comma-ok type assertion rather than every providerAdapter
// implementation growing a no-op method for it.
//
// Safe under Yaegi despite the documented Trap-4 restriction
// (matchesSentinel's own doc comment, limits.go): that trap is a comma-ok
// assertion of a COMPILED concrete value (a stdlib type, e.g.
// *fmt.wrapErrors or *url.Error) against an INTERPRETER-declared
// interface, which silently returns ok=false. Every adapter's dynamic
// type behind the providerAdapter interface here (*openaiAdapter,
// *anthropicAdapter, *geminiAdapter) is itself plugin-declared and
// interpreted right alongside modelMetadataFetcher — an
// interpreted-concrete-value-against-interpreted-interface assertion,
// the opposite, unaffected direction.
type modelMetadataFetcher interface {
	// fetchModelMetadata returns discovery-captured per-model context
	// lengths, keyed by upstream model id. A nil map with a nil error
	// means the adapter has nothing to report (e.g. metadataPath left
	// unconfigured) — a fast, cheap no-op the caller can invoke
	// unconditionally on every adapter that implements this interface.
	fetchModelMetadata(ctx context.Context) (map[string]int, error)
}

// captureModelMetadata is warmFill/refreshProvider's shared, best-effort
// discovery-metadata step (feature v0.23): when adapter also implements
// modelMetadataFetcher (see that interface's own doc comment for why
// this assertion is yaegi-safe), it fetches per-model context lengths
// and records them on st. A fetch error, or an adapter that does not
// implement the interface at all, is intentionally non-fatal beyond one
// log line — per this feature's own "absent/failed = no metadata
// captured" ruling, it must never fail discovery itself, which
// listModels (warmFill/refreshProvider's own caller) already accounts
// for independently.
//
// The deferred recover (review fix, SHOULD-5) is this function's OWN,
// separate from refreshProvider's outer recover: without it, a panic
// inside fetchModelMetadata would unwind into refreshProvider's deferred
// recover instead, which sets its OWN err variable and would then report
// listModels' own, already-successful discovery fetch as failed —
// discarding a good, freshly-fetched model list over an unrelated bug in
// the metadata path. Catching it here, before it can ever reach that
// outer recover, is what actually enforces "metadata must never fail
// discovery" on the panic path, not just the plain-error-return one
// already handled below.
func (m *modelRegistry) captureModelMetadata(ctx context.Context, name string, adapter providerAdapter, st *providerState) {
	defer func() {
		if rec := recover(); rec != nil {
			m.log("%s", fmt.Sprintf("model registry: metadata capture for provider %q panicked: %v", name, rec))
		}
	}()

	mf, ok := adapter.(modelMetadataFetcher)
	if !ok {
		return
	}
	meta, err := mf.fetchModelMetadata(ctx)
	if err != nil {
		// Scrubbed for the same reason as warmFill's own discovery-failure
		// line: fetchModelMetadata dials adapter.base()+metadataPath, so
		// this error carries the identical credential-bearing URL.
		// Security audit run-1, finding F-2.
		m.log("%s", fmt.Sprintf("model registry: metadata capture for provider %q failed: %v",
			name, sanitizeProviderErr(err.Error(), adapter.base())))
		return
	}
	if meta == nil {
		return
	}
	st.setDiscoveredContext(meta)
}

// infof writes msg through the info field, falling back to log when no
// info was injected (mirrors warnf's own fallback, above) — same
// single-variadic-argument Yaegi-safety shape the log/warn/warnf call
// sites already document.
func (m *modelRegistry) infof(msg string) {
	if m.info != nil {
		m.info("%s", msg)
		return
	}
	m.log("%s", msg)
}

// scheduleWarmFillRetry schedules exactly ONE background retry, after
// m.warmFillRetryDelay, for each provider name in failed — providers
// whose synchronous warmFill discovery fetch (above) just failed.
// warmFillRetryDelay's own doc comment has the full production-incident
// rationale; this method is purely the scheduling mechanics.
//
// Each retry runs on its own goroutine via time.AfterFunc, self-bounded
// by context.Background() plus its own warmFillTimeout budget in
// retryWarmFill (below) — the SAME self-terminating shape refreshProvider
// already uses for its maybeRefresh-spawned goroutine, and for the
// identical reason documented there: this plugin has no wired shutdown/
// Close hook a background goroutine could listen on instead (Close,
// llmgateway.go, only releases redisClient — its own doc comment explains
// why nothing in this codebase calls it automatically, and Traefik's own
// plugin contract gives a discarded instance no teardown signal at
// config-reload swap time either). Binding to a fixed delay+timeout
// instead of an external cancel signal means a Gateway discarded mid-
// flight (a config reload landing inside the retry window) leaks at most
// one goroutine per still-failed provider, for at most
// warmFillRetryDelay+2×warmFillTimeout (worst case ~20s at the shipped
// defaults: the retry's own listModels call budgeted by warmFillTimeout,
// PLUS captureModelMetadata's own separate warmFillTimeout budget within
// retryWarmFill, below — not merely the first of the two) — bounded, not
// a steady-state leak, matching refreshProvider's own accepted worst case
// (its own doc comment, below).
//
// The retry goes through the SAME finishRefresh bookkeeping warmFill/
// refreshProvider already use (retryWarmFill, below), so breaker state
// (recordHealthLocked) stays consistent: a successful retry is recorded
// exactly like any other successful discovery attempt, closing an
// already-open breaker if the failed warm fill alone had somehow tripped
// one; a failed retry is simply a second consecutive failure toward the
// ordinary breaker threshold — existing breaker behavior, unchanged.
//
// Each iteration calls scheduleOneWarmFillRetry (below) rather than
// inlining the time.AfterFunc closure directly in this loop — the SAME
// "close over named parameters, not loop variables" defense
// refreshProvider's own doc comment already documents for maybeRefresh,
// below. This is not merely stylistic: an EARLIER version of this method
// built the closure right here, over this loop's own range variable, and
// tools/yaegi-check caught it misbehaving under the real interpreter —
// with two failed providers, BOTH scheduled timers ended up firing
// against whichever provider's state the range variable held LAST,
// double-retrying it and never touching the other at all. Compiled Go
// (go.mod: go 1.22) never showed this: the range variable is per-
// iteration there, so the bug was invisible to `go test`, only to Yaegi.
func (m *modelRegistry) scheduleWarmFillRetry(failed []warmFillFailure) {
	for _, f := range failed {
		m.scheduleOneWarmFillRetry(f.name, f.at)
	}
}

// scheduleOneWarmFillRetry schedules name's single retry timer
// (scheduleWarmFillRetry, above). name is a plain function PARAMETER
// here, not a loop variable of any kind, so the closure below captures a
// binding that is unambiguously fresh per call under every Go version —
// and, per this method's own existence, under Yaegi's interpretation of
// one too.
func (m *modelRegistry) scheduleOneWarmFillRetry(name string, at time.Time) {
	st := m.states[name]
	adapter := m.adapters[name]
	time.AfterFunc(m.warmFillRetryDelay, func() {
		m.retryWarmFill(name, st, adapter, at)
	})
}

// retryWarmFill runs one deferred warm-fill retry (scheduleWarmFillRetry,
// above) for a single provider whose synchronous warm fill just failed.
// Mirrors warmFill's own per-provider body — including the same
// captureModelMetadata step — rather than refreshProvider's shape below:
// this is still a first-DISCOVERY attempt, budgeted by warmFillTimeout,
// not a steady-state background refresh budgeted by
// backgroundRefreshTimeout.
//
// On success, logs an INFO "recovered after retry" line (m.infof) so an
// operator watching a rollout sees the transient cold-start failure
// self-heal instead of staying silent; on failure, logs the existing
// ERROR line unchanged and leaves the provider exactly where the
// ordinary breaker/refresh path already would.
//
// The deferred recover mirrors refreshProvider's own, for the identical
// reason: this runs off a time.AfterFunc goroutine with no ServeHTTP
// caller to unwind into, so an unrecovered panic here would crash the
// whole Traefik process, not just fail one request. finishRefresh still
// runs on the panic path (with a synthetic error) — but ONLY once the
// guard below has actually started something: sinceWarmFill's own
// tryBeginWarmFillRetry call is this function's FIRST statement,
// deliberately ahead of the recover-guarded section, so a guard refusal
// returns before the deferred finishRefresh is even registered.
//
// Fix 2/3 (verify-retry.md) — corrected: an EARLIER version of this
// function never called tryBeginRefresh (or any guard) at all, so its
// listModels call could run CONCURRENTLY with an ordinary request-driven
// refresh already in flight against the identical provider. It now calls
// tryBeginWarmFillRetry (providerState, above) — the SAME st.inFlight
// field and st.mu, so the two are mutually exclusive exactly like two
// request-driven refreshes already were — and, on a true result, is
// bound by the identical "pair with a later finishRefresh" contract
// tryBeginRefresh's own doc comment establishes; finishRefresh clears
// inFlight unconditionally regardless of which of the two guards set it.
func (m *modelRegistry) retryWarmFill(name string, st *providerState, adapter providerAdapter, sinceWarmFill time.Time) {
	// Guard (fix 2/3, verify-retry.md): skip entirely, before touching
	// anything else, when a refresh is already in flight or one has
	// already completed since the warm fill that scheduled this retry —
	// tryBeginWarmFillRetry's own doc comment has the full contract. A
	// skip here means NOTHING was attempted, so finishRefresh below must
	// not run either: there is no outcome to record, and calling it
	// anyway would wrongly overwrite whatever the winning refresh already
	// recorded with a synthetic no-op result.
	if !st.tryBeginWarmFillRetry(sinceWarmFill) {
		return
	}

	var ids []string
	var err error
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
		m.finishRefresh(st, m.now(), ids, err)
		if err != nil {
			// Scrubbed for the same reason as warmFill's own
			// discovery-failure line (security audit run-1, finding F-2).
			m.log("%s", fmt.Sprintf("model registry: warm-fill retry for provider %q failed: %v",
				name, sanitizeProviderErr(err.Error(), adapter.base())))
			return
		}
		m.infof(fmt.Sprintf("model registry: initial discovery for provider %q recovered after retry", name))
	}()

	// Both cancels are deferred, mirroring refreshProvider's own reasoning
	// below: this function has a recover above, so a panicking listModels
	// would skip an inline cancel() and hold the timer until
	// warmFillTimeout fires. Deferring costs nothing and survives the
	// panic path.
	fctx, cancel := context.WithTimeout(context.Background(), m.warmFillTimeout)
	defer cancel()
	ids, err = adapter.listModels(fctx)
	if err != nil {
		return
	}

	// Own timeout budget, same reasoning as warmFill/refreshProvider's
	// identical comment: reusing fctx here would hand the metadata fetch
	// an already-spent context whenever listModels itself ran long.
	mctx, mcancel := context.WithTimeout(context.Background(), m.warmFillTimeout)
	defer mcancel()
	m.captureModelMetadata(mctx, name, adapter, st)
}

// maybeRefresh is called at every ServeHTTP entry. For each discovery-
// enabled provider whose last refresh is older than its interval (and that
// is not already mid-refresh), it spawns exactly one background goroutine
// to pull listModels and merge the result. The spawned goroutine
// deliberately does not inherit ctx's cancellation: ctx is the incoming
// request's context, which net/http cancels once ServeHTTP returns — using
// it here would abort the background refresh moments after it starts on
// every request, so the goroutine gets its own backgroundRefreshTimeout
// budget instead. ctx is still consulted before spawning: a request whose
// context is already done skips starting new background work for it.
func (m *modelRegistry) maybeRefresh(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := m.now()
	for _, name := range m.providerNames {
		st := m.states[name]
		if !st.discoveryEnabled || !st.tryBeginRefresh(now) {
			continue
		}
		adapter := m.adapters[name]
		go m.refreshProvider(name, st, adapter) //nolint:gosec // G118: refreshProvider intentionally uses its own context.Background()-derived timeout, not r.Context(), per this function's doc comment above — the request context is canceled once ServeHTTP returns and would abort the background fetch moments after it starts
	}
}

// refreshProvider runs one background discovery fetch for a provider
// already marked inFlight by tryBeginRefresh, and records its outcome via
// finishRefresh. Split out from maybeRefresh so the goroutine closes over
// named parameters, not loop variables.
//
// This goroutine (maybeRefresh's only "go" statement in the whole plugin)
// is a deliberate, narrow exception to memoryStore's no-background-goroutine
// rule (limits.go:144-149): unlike a ticker that would outlive a Yaegi
// middleware instance rebuilt on every config reload, this goroutine is
// self-terminating — at most one per provider per interval
// (tryBeginRefresh's inFlight guard), bounded to backgroundRefreshTimeout
// (30s). A rebuild mid-flight can transiently leak one goroutine for at
// most 30s; that is accepted, not a steady-state leak.
//
// The deferred recover below is required precisely because this runs off
// any request's goroutine: an unrecovered panic here has no ServeHTTP
// caller to unwind into and would crash the whole Traefik process, not just
// fail one request. finishRefresh still runs on the panic path (with a
// synthetic error) so inFlight is always released and the next interval
// retries, exactly as a normal fetch failure would.
func (m *modelRegistry) refreshProvider(name string, st *providerState, adapter providerAdapter) {
	var ids []string
	var err error
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
		m.finishRefresh(st, m.now(), ids, err)
		if err != nil {
			// Scrubbed (security audit run-1, finding F-2). This line
			// repeats on EVERY background refresh interval with no dedup,
			// so an unreachable provider whose baseUrl embeds a credential
			// would otherwise write it to stderr indefinitely.
			m.log("%s", fmt.Sprintf("model registry: discovery refresh for provider %q failed: %v",
				name, sanitizeProviderErr(err.Error(), adapter.base())))
		}
	}()

	// Both cancels are deferred: this function has a recover above, so a
	// panicking listModels would skip an inline cancel() and hold the
	// timer until backgroundRefreshTimeout fires. Deferring costs nothing
	// (the function ends right after the capture) and survives the panic
	// path.
	fctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
	defer cancel()
	ids, err = adapter.listModels(fctx)

	// Own timeout budget, review fix (SHOULD-4) — see warmFill's
	// identical comment: reusing fctx here would hand the metadata fetch
	// an already-spent context on every refresh whose listModels call
	// ran long, not just a slow or timed-out one.
	mctx, mcancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
	defer mcancel()
	m.captureModelMetadata(mctx, name, adapter, st)
}

// splitConfiguredProvider reports whether id has "prefix/rest" form where
// prefix names a currently configured provider. Per ruling (a): a slash in
// id is only ever a provider-prefix separator when the part before it is a
// real, configured provider name; otherwise (including when id has no
// slash, or its prefix names no configured provider) the whole string is a
// bare model id — this is what lets an upstream model id that itself
// contains a slash (e.g. "uni/deepseek-v4-flash-0731" on a provider that
// happens not to be named "uni") resolve correctly as a bare id.
func (m *modelRegistry) splitConfiguredProvider(id string) (providerName, rest string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] != '/' {
			continue
		}
		prefix, tail := id[:i], id[i+1:]
		if _, known := m.adapters[prefix]; known {
			return prefix, tail, true
		}
		return "", "", false
	}
	return "", "", false
}

// bareEntryWinner returns the first provider in owners (already sorted
// and already PROVIDER-authorized for grp — bareWinner's own "visible",
// or listFor's identical "visible") that grp additionally authorizes id
// (plus any further candidate resolveAgainst would also check for this
// specific call site, e.g. resolveAliasTarget's own alias name) ON —
// HIGH-2 fix, review round 2. Shared by bareWinner (below) and listFor's
// own bare-entry selection, so the two can never drift into picking a
// different winner for the identical scenario.
//
// For a single-grant grp (grp.grantCount() <= 1 — the overwhelmingly
// common case), this is exactly owners[0]: a single grant's own provider
// authorization and model authorization are independent axes, already
// checked SEPARATELY by every caller (resolveAgainst's own model check;
// listFor's own grp.allowsProviderModel check on the returned winner), so
// this never needs to consult id/extraIDs for that case at all — the
// identical, unconditional "first provider-visible owner" bareWinner
// always returned before this fix existed.
//
// For a MULTI-grant grp, owners[0] alone is not good enough: it might be
// authorized for PROVIDER ONLY by one grant while a LATER owner in the
// same list is authorized for BOTH provider and model by a DIFFERENT
// grant. Before this fix, both bareWinner and listFor picked owners[0]
// unconditionally and let the caller's own separate model check deny it
// — silently taking away a model the principal could actually reach via
// a later, still-visible owner. ok is false only when NO owner's own
// grant covers both provider and (any of) id/extraIDs.
func bareEntryWinner(owners []string, grp *group, id string, extraIDs ...string) (string, bool) {
	if len(owners) == 0 {
		return "", false
	}
	if grp.grantCount() <= 1 {
		return owners[0], true
	}
	ids := append([]string{id}, extraIDs...)
	for _, name := range owners {
		if grp.allowsProviderModel(name, ids...) {
			return name, true
		}
	}
	return "", false
}

// bareWinner returns the provider that owns bare id when more than one
// provider's known model set contains it: the first, in sorted
// provider-name order, among providers grp is authorized to use (ruling
// (g), revised 2026-08-27 — bareWinner used to ignore grp entirely and
// always return the GLOBAL sorted-first owning provider, so a caller was
// denied a model they were actually entitled to whenever some OTHER,
// group-invisible provider happened to sort earlier and also served that
// id).
//
// MULTI-GRANT (HIGH-2 fix, review round 2): for a principal with more
// than one grant, "authorized to use" additionally requires the SAME
// grant to also cover id (plus extraIDs, the caller's own further
// resolveAgainst candidate — e.g. resolveAliasTarget passes its alias
// name), via the shared bareEntryWinner helper above — not merely the
// coarser grp.allowsProvider union check a single-grant principal still
// uses. Before this fix, a multi-grant principal's bareWinner could pick
// a provider visible only through a DIFFERENT grant than the one that
// actually covers this model, which resolveAgainst then denied — even
// when a LATER owning provider, further down the sorted list, was fully
// authorized by one of the principal's own grants. Consulting the
// eventual model check up front avoids that false deny. Single-grant
// principals are unaffected: bareEntryWinner's own fast path keeps this
// byte-identical to the original, provider-only preference.
//
// When grp allows none of id's owning providers at all (no grant even
// matches the PROVIDER for any owner), this still returns the GLOBAL
// sorted-first owner with ok true, exactly as the pre-fix version always
// did, rather than reporting "not found": id is still a genuinely known
// model, just not one this group may reach, and the caller
// (resolveAgainst) must see it as known-but-denied (errModelDenied) —
// the same outcome grp got before this fix — not the wrong
// errModelUnknown a "no visible owner" result would produce. The same
// fallback covers the multi-grant case where grp allows the PROVIDER for
// one or more owners but no single grant covers both that provider and
// this model: bareEntryWinner reports no winner, so this falls back to
// the first PROVIDER-visible owner instead (visible[0]) —
// resolveAgainst will independently re-derive errModelDenied against it,
// so the eventual error is unaffected, only which provider it is denied
// against.
func (m *modelRegistry) bareWinner(id string, grp *group, extraIDs ...string) (string, bool) {
	var owners, visible []string
	for _, name := range m.providerNames {
		if !m.states[name].hasModel(id) {
			continue
		}
		owners = append(owners, name)
		if grp.allowsProvider(name) {
			visible = append(visible, name)
		}
	}
	if len(owners) == 0 {
		return "", false
	}
	if len(visible) == 0 {
		return owners[0], true
	}
	if winner, ok := bareEntryWinner(visible, grp, id, extraIDs...); ok {
		return winner, true
	}
	return visible[0], true
}

// resolve maps a client-requested model id to the adapter that serves it.
// It returns the adapter, the upstream model id to send that adapter (the
// id form the provider itself knows, with any gateway-side provider prefix
// stripped), and the canonical "provider/upstreamModel" id. The original,
// exactly-as-requested id the caller passed in is not returned separately —
// the caller already holds it, in the id argument itself.
//
// An EXACT alias match (spec §5, v0.2) wins before any other rule below —
// checked first, ahead of provider-prefix splitting, so an alias id that
// happens to look like "prefix/rest" is never mistaken for one (and
// validateModelAliases already rejects an alias whose prefix names an
// actually configured provider at construction, so this ordering never
// has to arbitrate a genuine conflict between the two forms). id also
// happening to collide with a since-DISCOVERED model of some provider
// (impossible for an explicit one; validateModelAliases already rejects
// that at construction) is resolved in the alias's favor by this same
// ordering — warnAliasShadowsDiscoveredOnce logs that once per id.
//
// id in "provider/model" form (ruling (a): only when "provider" names a
// configured provider) resolves directly against that provider. Any other
// id is a bare id, resolved against the first provider — in sorted name
// order, among providers grp is authorized to use, falling back to the
// global sorted-first owner only when grp is authorized for none of them
// (bareWinner's own doc comment) — whose known model set contains it,
// ruling (g)'s collision rule as revised 2026-08-27 for group-awareness.
// Either way, authorization requires grp.allowsProviderModel for the
// resolved provider (ruling (c)); a model that exists but fails
// authorization returns errModelDenied, distinct from errModelUnknown for
// a model no configured provider knows at all.
func (m *modelRegistry) resolve(id string, grp *group) (providerAdapter, string, string, error) {
	if target, isAlias := m.aliases[id]; isAlias {
		// Warned-set checked FIRST (review fix): bareWinner is an
		// O(providerNames) scan, and every request for an already-warned
		// alias would otherwise pay it just to decide not to log again.
		// Once warned, this collapses to one cheap mutex-guarded map
		// lookup per request instead.
		//
		// grp is passed through here too even though this call only uses
		// the boolean "does id collide at all" — bareWinner's ok result is
		// group-independent (it falls back to the global owner rather than
		// reporting not-found when grp allows none of them), so this stays
		// the same collision signal it always was.
		if !m.aliasShadowWarned(id) {
			if _, discoveredCollision := m.bareWinner(id, grp); discoveredCollision {
				m.warnAliasShadowsDiscoveredOnce(id)
			}
		}
		return m.resolveAliasTarget(id, target, grp)
	}
	if providerName, rest, ok := m.splitConfiguredProvider(id); ok {
		return m.resolveAgainst(providerName, rest, id, "", grp, errModelUnknown)
	}
	providerName, ok := m.bareWinner(id, grp)
	if !ok {
		return nil, "", "", errModelUnknown
	}
	return m.resolveAgainst(providerName, id, id, "", grp, errModelUnknown)
}

// aliasTargetError is resolveAgainst's notFoundErr when the provider it
// was called against, and the upstream model id on it, both came from
// resolving an alias's TARGET (resolveAliasTarget) rather than a direct
// client request — spec §5's "Targets referencing not-yet-discovered
// models are allowed at construction and resolve lazily; an unresolvable
// target at request time → 404 whose message names the alias AND the
// missing target". writeModelResolveError (routes_unified.go) type-
// asserts for *aliasTargetError explicitly, ahead of its
// errors.Is(errModelUnknown) switch — a plain type assertion, not
// errors.As, matching this package's established yaegi-safe convention
// for a pointer error type (providers.go's providerHTTPError doc
// comment: errors.As panics under yaegi checking whether an interpreted
// pointer type implements error).
type aliasTargetError struct {
	alias, target string
}

// Error names both the alias and its unresolved target, per spec §5.
func (e *aliasTargetError) Error() string {
	return fmt.Sprintf("llmgateway: alias %q targets %q, which is not a known model", e.alias, e.target)
}

// resolveAliasTarget finishes resolve for id already known to be an exact
// alias match: target is resolved through the SAME rules as any other id
// — provider-prefixed direct, or bare-id collision winner — via the same
// resolveAgainst helper, with two differences from a direct request:
//
//   - Authorization (spec §5): "a group may use an alias when its model
//     globs match the ALIAS name OR the resolved target (either
//     grants)". alias is passed to resolveAgainst as its extraModelName,
//     so a request denied by every target-side candidate can still
//     succeed purely because the group's model glob names the alias
//     itself.
//   - A target that does not resolve to any known model at request time
//     returns *aliasTargetError (naming both ids), not the bare
//     errModelUnknown a direct request's unknown-model case returns.
func (m *modelRegistry) resolveAliasTarget(alias, target string, grp *group) (providerAdapter, string, string, error) {
	notFound := &aliasTargetError{alias: alias, target: target}
	if providerName, rest, ok := m.splitConfiguredProvider(target); ok {
		return m.resolveAgainst(providerName, rest, target, alias, grp, notFound)
	}
	// alias passed through as bareWinner's own extraIDs (HIGH-2 fix,
	// review round 2): the SAME extraModelName candidate resolveAgainst
	// itself checks two lines below — a multi-grant principal whose only
	// grant reaching some owning provider does so via the alias's own
	// name (not target's bare form) must still be preferred over a
	// grant-mismatched, merely PROVIDER-visible owner earlier in sorted
	// order.
	providerName, ok := m.bareWinner(target, grp, alias)
	if !ok {
		return nil, "", "", notFound
	}
	return m.resolveAgainst(providerName, target, target, alias, grp, notFound)
}

// aliasShadowWarnKey builds the m.warned key for alias's discovered-
// collision warning — shared by aliasShadowWarned and
// warnAliasShadowsDiscoveredOnce so the two can never drift apart into
// mismatched keys. Prefixed with "alias-shadow:" so it can never collide
// with warnCollisionOnce's own plain-id keys in the same m.warned map.
func aliasShadowWarnKey(alias string) string {
	return "alias-shadow:" + alias
}

// aliasShadowWarned reports whether warnAliasShadowsDiscoveredOnce has
// already logged for alias, with no other side effect — resolve's fast
// path (its own doc comment) checks this BEFORE paying bareWinner's
// O(providerNames) scan, so a registry with many providers does not pay
// that cost on every request for an alias once its one-time warning has
// already fired.
func (m *modelRegistry) aliasShadowWarned(alias string) bool {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	return m.warned[aliasShadowWarnKey(alias)]
}

// warnAliasShadowsDiscoveredOnce logs, at most once per colliding alias id
// for this registry's lifetime, that alias also names a model some
// provider's discovery fetch found after construction — resolve's
// documented precedence (its own doc comment above): the alias always
// wins, since only an EXPLICIT collision is rejected at construction
// (validateModelAliases); discovery is dynamic and runs after that check,
// so this collision can only be caught, and only warned about, here at
// request time.
func (m *modelRegistry) warnAliasShadowsDiscoveredOnce(alias string) {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	key := aliasShadowWarnKey(alias)
	if m.warned[key] {
		return
	}
	m.warned[key] = true
	m.log("%s", fmt.Sprintf("model registry: alias %q also names a discovered model; the alias takes precedence", alias))
}

// resolveAgainst finishes resolve for a providerName already chosen (either
// the explicit prefix or the bare-id collision winner): it checks
// upstreamModel is actually known to that provider, then checks
// authorization, in that order — so a real model behind a provider the
// group cannot use reports errModelDenied, not errModelUnknown.
//
// allowsProviderModel matches each candidate model id against grp's model
// globs with an exact glob match (matchesGlob, auth.go) — no prefix-
// stripping of its own — so resolveAgainst generates both candidate
// strings itself:
// requestedID (the provider-prefixed form for a direct request, e.g.
// "openai/gpt-test") and upstreamModel (its bare suffix, e.g. "gpt-test"),
// and allows if either matches — that is what lets a pattern like "gpt-*"
// reach a provider-prefixed request. For a bare-id request the caller
// passes the same string as both arguments, so the two checks collapse to
// one: no bare-suffix candidate is invented for an id that was never
// legitimately provider-prefixed in the first place (ruling: a pattern like
// "deepseek-*" must not match a bare id that merely contains a slash, e.g.
// "uni/deepseek-v4-flash-0731", when "uni" is not a configured provider).
//
// extraModelName is a further authorization candidate checked via
// grp.allowsProviderModel, alongside requestedID and upstreamModel: empty ("") for
// a direct (non-alias) resolution, or an alias's own id when resolveAliasTarget
// (spec §5, v0.2) is resolving that alias's target — "a group may use an
// alias when its model globs match the ALIAS name OR the resolved target
// (either grants)".
//
// notFoundErr is returned, unmodified, when upstreamModel is not known to
// providerName: resolve's own two call sites pass the bare errModelUnknown
// sentinel (unchanged from before this parameter existed — checked by "err
// != errModelUnknown" equality in registry_test.go, not errors.Is, so it
// must stay the exact sentinel value there); resolveAliasTarget passes a
// *aliasTargetError naming both the alias and its unresolved target (spec
// §5's 404 message requirement).
func (m *modelRegistry) resolveAgainst(providerName, upstreamModel, requestedID, extraModelName string, grp *group, notFoundErr error) (providerAdapter, string, string, error) {
	if !m.states[providerName].hasModel(upstreamModel) {
		return nil, "", "", notFoundErr
	}
	// allowsProviderModel (multi-group/personal-grant feature, auth.go)
	// requires provider AND model to both come from the SAME grant — the
	// no-cross-grant-leak rule. For an ordinary single-grant group this
	// is byte-identical to the combined "model matches (requestedID OR
	// upstreamModel OR extraModelName) AND provider matches" check this
	// replaced.
	ids := []string{requestedID, upstreamModel}
	if extraModelName != "" {
		ids = append(ids, extraModelName)
	}
	if !grp.allowsProviderModel(providerName, ids...) {
		return nil, "", "", errModelDenied
	}
	// canonical is assigned to a local before the return, not inlined into
	// it: yaegi v0.16.1 panics ("reflect.Set: value of type
	// interp.valueInterface is not assignable to type string") building a
	// multi-value return tuple when one element is an inline string
	// concatenation of two parameters. The identical expression assigned
	// to a variable first, then returned, does not trigger it. Verified
	// empirically against yaegi v0.16.1 running this exact function under
	// real Traefik (Task 15's integration suite) — tools/yaegi-check never
	// exercises this call path (it only drives GET /v1/models), which is
	// why the existing yaegi gate never caught it.
	canonical := providerName + "/" + upstreamModel
	return m.adapters[providerName], upstreamModel, canonical, nil
}

// warnCollisionOnce logs, at most once per colliding bare id for this
// registry's lifetime, that provs (sorted) all provide id and winner owns
// its bare form.
//
// Decision (2026-08-27, group-aware bare-winner fix): stays keyed on the
// GLOBAL owner set and the global sorted-first winner, not narrowed per
// calling group, even though bareWinner (above) now prefers a
// group-visible owner over this global one when resolving an actual
// request. A provider collision on one model id is a fact about the
// fleet's configuration — two providers were pointed at the same upstream
// model — true and worth an operator's attention regardless of which
// group's request happens to trigger listFor first. Narrowing this to
// what one group can see would either (a) log the same underlying
// collision repeatedly, once per differently-scoped group, forcing an
// operator to de-duplicate by hand, or (b) suppress it entirely for any
// collision no single group can see both sides of, hiding a real
// configuration duplicate from the operator who needs to see it to fix
// it (rename one provider's model, or accept the collision knowingly).
// The log message is worded below to say winner is only the global
// default, since a given group's own request may now resolve to a
// different provider than winner.
func (m *modelRegistry) warnCollisionOnce(id, winner string, provs []string) {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	if m.warned[id] {
		return
	}
	m.warned[id] = true
	m.warnf(fmt.Sprintf("model registry: model id %q is provided by multiple providers %v; %q is the global bare-id winner (a caller's group may instead be routed to whichever of these providers it is authorized to use)", id, provs, winner))
}

// warnf writes msg through the warn field, falling back to log when no
// warn was injected (see the warn field's own doc comment). It takes an
// ALREADY-FORMATTED string rather than a format plus arguments, so that
// both call shapes below stay single-variadic-argument — the Yaegi CFG
// restriction that applies to any struct field of variadic func type.
func (m *modelRegistry) warnf(msg string) {
	if m.warn != nil {
		m.warn("%s", msg)
		return
	}
	m.log("%s", msg)
}

// modelsJSON returns grp's encoded GET /v1/models response body
// ({"object":"list","data":[...]} — routes_unified.go's handleModels
// writes this straight to the response), cached per (group, modelsGen)
// (perf finding 3, 2026-08-2x audit: 15.9ms/21.3MB/552,884 allocs per
// call, uncached, at the live 1,033-model catalog — registry.go's listFor
// plus routes_unified.go's json.Encoder, called on every request an
// OpenAI SDK client's session-start model poll happens to land on).
// listFor's output only changes when a discovery refresh runs
// (finishRefresh, above, bumps modelsGen unconditionally on every
// attempt) — between refreshes, every call for the same group returns
// the identical cached bytes without re-walking every provider's model
// set or re-marshaling.
//
// Cached by grp.modelsCacheKey()'s stable STRING signature (MEDIUM-3
// fix, review round 2), NOT by *group pointer identity: that assumption
// held for every ordinary, named group — authStore builds those once, at
// construction (auth.go's newAuthStore), and never rebuilds or replaces
// them afterward, so a config reload (which constructs an entirely new
// Gateway, and so a new modelRegistry with its own empty modelsCache) was
// never actually the risk. It broke for a SYNTHETIC multi-group/
// personal-grant principal (effectiveGroup, auth.go): that function
// allocates a FRESH *group on every call, so authStore.replaceFileUsers
// (users_file.go's hot-reload path) rebuilds a semantically identical
// principal as a brand-new pointer on every reload — a *group-pointer
// key left the OLD pointer's entry permanently unreachable, growing this
// cache without bound across repeated reloads even when nothing about
// the user's membership changed (reviewer-reproduced: 5 identical
// reloads of one two-group user left 6 entries). group.modelsCacheKey()
// derives a signature from what actually determines this cache's VALUE
// (member group names plus the personal grant's own providers/models),
// so two principals with identical membership — including the SAME
// principal rebuilt across reloads — always share one entry.
func (m *modelRegistry) modelsJSON(grp *group) []byte {
	gen := atomic.LoadInt64(&m.modelsGen)
	key := grp.modelsCacheKey()

	m.modelsCacheMu.Lock()
	if entry, ok := m.modelsCache[key]; ok && entry.gen == gen {
		m.modelsCacheMu.Unlock()
		return entry.body
	}
	m.modelsCacheMu.Unlock()

	body, err := json.Marshal(map[string]any{
		"object": "list",
		"data":   m.listFor(grp),
	})
	if err != nil {
		// listFor's map[string]any values are all JSON-safe primitives
		// (string, int, float64, bool, and nested maps/slices of the
		// same) — Marshal cannot fail on them in practice. Not caching a
		// failure keeps this equivalent to the pre-cache
		// json.NewEncoder(w).Encode call site, which also had nothing
		// useful to do on an encode failure beyond logging.
		m.log("%s", fmt.Sprintf("model registry: encoding /v1/models response for group %q failed: %v", grp.name, err))
		return nil
	}
	// json.Encoder.Encode (the pre-cache call site) always appends a
	// trailing '\n' after the value; json.Marshal does not — appended
	// here so the cached body is byte-identical to what the old
	// uncached path wrote (review fix, Should-Fix 5).
	body = append(body, '\n')

	m.modelsCacheMu.Lock()
	m.modelsCache[key] = modelsCacheEntry{gen: gen, body: body}
	m.modelsCacheMu.Unlock()
	return body
}

// listFor returns grp's visible model catalog as OpenAI-compatible model
// objects ({"id","object":"model","owned_by"}), sorted by id. A bare id
// owned by only one provider grp can see is listed once, bare. A bare id
// owned by more than one provider grp can see (ruling (g)/(h), revised
// 2026-08-27 for group-awareness) is listed once bare — under the
// sorted-first of THOSE group-visible owners, not the global sorted-first
// owner — plus once more per group-visible owning provider in "provider/id"
// form, so every provider's copy stays reachable through explicit
// addressing even when it lost the bare-id collision. A provider grp
// cannot use is never a candidate for the bare id and never gets a
// "provider/id" entry either: a prefix that only disambiguates against an
// invisible provider is noise, not information, to this group. Each listed
// entry is independently filtered by grp.allowsProviderModel;
// grp.allowsProvider filtering happens once, up front, when the
// group-visible owner subset is built, rather than per candidate entry.
func (m *modelRegistry) listFor(grp *group) []map[string]any {
	owners := make(map[string][]string)
	for _, name := range m.providerNames {
		for _, id := range m.states[name].knownIDs() {
			owners[id] = append(owners[id], name) // m.providerNames is sorted, so owners[id] accumulates in sorted order
		}
	}

	out := make([]map[string]any, 0, len(owners))
	// aliasEntries is every alias grp may actually use, resolved ONCE up
	// front (spec §5) and keyed by alias id, so the id loop below can see
	// which bare ids an alias shadows before it emits anything for them.
	// An alias whose target does not resolve right now, or which grp is
	// authorized for neither by alias name nor by target, is absent here
	// and shadows nothing — the discovered model keeps its own bare entry
	// in that case.
	aliasEntries := m.aliasEntriesFor(grp)
	visible := make([]string, 0, 4) // reused per id; provs is at most len(m.providerNames) long
	for id, provs := range owners {
		// warnCollisionOnce stays keyed on the GLOBAL owner set, not grp's
		// view of it (decision, see registry.go's collision-logging note
		// below resolveAgainst): a config collision is a fact about the
		// fleet, true regardless of which group happens to list models
		// first, and operators need one stable signal to act on rather
		// than a different, group-shaped warning per requester.
		if len(provs) >= 2 {
			m.warnCollisionOnce(id, provs[0], provs)
		}

		visible = visible[:0]
		for _, p := range provs {
			if grp.allowsProvider(p) {
				visible = append(visible, p) // provs is sorted, so visible stays sorted too
			}
		}
		if len(visible) == 0 {
			continue // grp cannot reach any provider serving id at all
		}

		// A discovered model whose bare id collides with a usable alias id
		// is SHADOWED (bug fix, 2026-09-10): resolve gives the exact alias
		// match precedence over every discovered bare id (its own doc
		// comment), so the listing must agree — emitting the discovered
		// model's own entry here would advertise that provider's owner and
		// metadata for an id every request routes to the alias target
		// instead. The alias's entry is appended after this loop; the
		// shadowed model itself stays listed, and addressable, in
		// "provider/id" form below, which is exactly how a client must
		// address it once the alias owns the bare id.
		_, shadowed := aliasEntries[id]

		// bareEntryWinner (HIGH-2 fix, review round 2), the same shared
		// helper bareWinner uses: for a single-grant grp this is exactly
		// visible[0] (today's pre-fix behavior); for a multi-grant grp it
		// skips a merely PROVIDER-visible owner whose OWN grant does not
		// also cover id, in favor of a later visible owner whose grant
		// covers both — never silently dropping a model the principal can
		// actually reach through some other visible provider.
		// ambiguousBareID (H3 fix): id itself takes the "configuredProvider/
		// rest" shape splitConfiguredProvider's resolve-side parser peels a
		// prefix from. A discovered id like "openai/gpt-4o" surfaced by
		// provider "openrouter" (owners[id]==["openrouter"]) is exactly this
		// case when "openai" also happens to be configured: listing it bare
		// would hand a client an id that resolve(), given that same id back,
		// routes to provider "openai" instead of the "openrouter" that
		// actually owns it (or 404s if "openai" doesn't know it). So this id
		// is never listed bare — only under its true owner's own
		// "provider/id" disambiguating form below, which resolve parses
		// unambiguously (splitConfiguredProvider only ever inspects the
		// FIRST "/", so "openrouter/openai/gpt-4o" always resolves against
		// "openrouter").
		_, _, ambiguousBareID := m.splitConfiguredProvider(id)

		groupWinner, gwOK := bareEntryWinner(visible, grp, id)
		// allowsProviderModel (multi-group/personal-grant feature,
		// auth.go), checked against groupWinner specifically — the
		// no-cross-grant-leak rule: a grant that allows some OTHER
		// visible provider must never authorize THIS entry, which is
		// attributed to groupWinner alone. Redundant with
		// bareEntryWinner's own multi-grant check when gwOK came from
		// that branch (harmless — matchesGlob is cheap), but still
		// REQUIRED for the single-grant fast path, which never consults
		// id at all.
		if gwOK && grp.allowsProviderModel(groupWinner, id) && !shadowed && !ambiguousBareID {
			out = append(out, modelObject(id, groupWinner, m.resolveMetaFor(groupWinner, id)))
		}
		if len(visible) < 2 && !shadowed && !ambiguousBareID {
			continue // only one provider serving id is visible to grp: no disambiguating prefix to add
		}
		for _, p := range visible {
			pid := p + "/" + id
			// allowsProviderModel's model-matching does no prefix-stripping
			// (auth.go): check both the prefixed form and its bare suffix,
			// same as resolveAgainst does for the equivalent client request
			// — allowsProviderModel(p, ...) requires BOTH candidates come
			// from a grant that also allows p itself (no cross-grant leak).
			if grp.allowsProviderModel(p, pid, id) {
				out = append(out, modelObject(pid, p, m.resolveMetaFor(p, id)))
			}
		}
	}

	// Aliases (spec §5, v0.2): appended after every real model, exactly
	// once each. An alias id can never duplicate an entry emitted above —
	// the id loop suppresses the bare entry of any id aliasEntries
	// shadows, and an alias id can never take the "provider/id" form of a
	// prefixed entry either (validateModelAliases rejects an alias whose
	// prefix names a configured provider at construction).
	for _, entry := range aliasEntries {
		out = append(out, entry)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string) //nolint:forcetypeassert // modelObject always sets id to a string
	})
	return out
}

// aliasEntriesFor resolves every configured alias grp may actually use
// into its finished listing entry, keyed by alias id (spec §5, v0.2). An
// alias is present only when its target resolves right now (a target
// awaiting its provider's first discovery fetch has nothing yet to name
// as owned_by) and grp is authorized for it: resolveAliasTarget applies
// the exact same "alias name OR target" authorization rule request-time
// resolve uses (its own doc comment), so a listing built from this can
// never promise access resolve would then deny. owned_by is recovered
// from the successful call's own canonical return value
// ("provider/upstreamModel") by splitting on the first "/" — exact,
// because a registry provider name can never itself contain one
// (configNamePattern, providers.go).
//
// Resolved once per listFor call rather than per candidate id: listFor
// needs the same set twice — to know which bare ids an alias shadows,
// and to emit the alias entries themselves.
func (m *modelRegistry) aliasEntriesFor(grp *group) map[string]map[string]any {
	if len(m.aliases) == 0 {
		return nil // the v0.1 no-aliases case allocates nothing
	}
	entries := make(map[string]map[string]any, len(m.aliases))
	for alias, target := range m.aliases {
		_, _, canonical, err := m.resolveAliasTarget(alias, target, grp)
		if err != nil {
			continue
		}
		providerName, bareTarget, _ := strings.Cut(canonical, "/")
		entries[alias] = modelObject(alias, providerName, m.resolveMetaForAlias(alias, providerName, bareTarget))
	}
	return entries
}

// modelObject builds one OpenAI-compatible model list entry, extended
// with OpenAI-compat-safe extension fields (feature v0.23): the context
// window and "pricing" ({"input_per_mtok_usd", "output_per_mtok_usd"}
// floats, USD per million tokens) — both omitted entirely when meta
// reports them unknown, never emitted as a misleading zero. "pricing" IS
// emitted with zeros for an explicitly free model (meta.CostKnown true,
// both cost fields 0) — that is a known, meaningful zero, not an absent
// one.
//
// The context window is emitted under THREE keys carrying the identical
// integer: "context_window", "context_length" (the OpenRouter spelling)
// and "max_model_len" (the vLLM spelling). OpenAI's own /v1/models
// schema has no context field at all, so every client that wants one
// reads some vendor dialect, and a client whose dialect is missing
// silently substitutes its own default instead of asking. Emitting all
// three costs two map entries and removes that whole failure class —
// the same reason the metadata FETCHER already parses all of these
// dialects on the inbound side (provider_openai.go's modelMetadata).
func modelObject(id, ownedBy string, meta resolvedModelMeta) map[string]any {
	obj := map[string]any{"id": id, "object": "model", "owned_by": ownedBy}
	if meta.ContextKnown {
		obj["context_window"] = meta.ContextTokens
		obj["context_length"] = meta.ContextTokens
		obj["max_model_len"] = meta.ContextTokens
	}
	if meta.CostKnown {
		obj["pricing"] = map[string]any{
			"input_per_mtok_usd":  microUSDPerMTokToUSD(meta.InputCostPerMTokMicroUSD),
			"output_per_mtok_usd": microUSDPerMTokToUSD(meta.OutputCostPerMTokMicroUSD),
		}
	}
	return obj
}

// resolveMetaFor resolves (provider, bareModel)'s metadata (feature
// v0.23), supplying this registry's config-override map and that
// provider's own discovery-captured context for bareModel.
func (m *modelRegistry) resolveMetaFor(provider, bareModel string) resolvedModelMeta {
	var ctxVal int
	var ctxKnown bool
	if st, ok := m.states[provider]; ok {
		ctxVal, ctxKnown = st.contextFor(bareModel)
	}
	return resolveModelMeta(provider, bareModel, m.modelMeta, ctxVal, ctxKnown)
}

// resolveMetaForAliasName resolves alias's metadata for the admin
// dashboard's unrestricted view (feature v0.23, hover-detail
// refinement): unlike listFor's request-authorized alias handling, this
// ignores group authorization entirely (an internal, unrestricted
// &group{} — GroupConfig's own documented "empty means all" semantics,
// llmgateway.go), matching buildAdminOverview's existing behavior of
// showing every configured provider/alias unconditionally, with no
// group filtering anywhere else in its response either. Returns an
// entirely-unknown resolvedModelMeta when alias is not a configured
// alias at all, or its target does not resolve to any known model yet.
func (m *modelRegistry) resolveMetaForAliasName(alias string) resolvedModelMeta {
	target, ok := m.aliases[alias]
	if !ok {
		return resolvedModelMeta{}
	}
	_, _, canonical, err := m.resolveAliasTarget(alias, target, &group{})
	if err != nil {
		return resolvedModelMeta{}
	}
	providerName, bareTarget, _ := strings.Cut(canonical, "/")
	return m.resolveMetaForAlias(alias, providerName, bareTarget)
}

// resolveMetaForAlias resolves alias's metadata (feature v0.23) via
// resolveAliasModelMeta: target-inherited unless the alias itself has
// its own modelMeta entry — see that function's own doc comment.
func (m *modelRegistry) resolveMetaForAlias(alias, targetProvider, targetModel string) resolvedModelMeta {
	var ctxVal int
	var ctxKnown bool
	if st, ok := m.states[targetProvider]; ok {
		ctxVal, ctxKnown = st.contextFor(targetModel)
	}
	return resolveAliasModelMeta(alias, targetProvider, targetModel, m.modelMeta, ctxVal, ctxKnown)
}

// providerSnapshot is one provider's read-only view for the admin
// dashboard (spec §4, v0.2). baseURL is not secret (the spec's NEVER-
// exposed list is API keys, digests, redis password, and users-file path
// contents only) — it is included so an operator can see which upstream
// a provider actually targets. models — the sorted explicit∪discovered
// id set (provider-model-accordion task) — is likewise not secret: a
// model id names no credential, and this same set is already public
// through /v1/models (listFor) to any authenticated caller the group
// allows.
// Field order below is fieldalignment-sensitive, the same convention
// providerState's own doc comment explains: pointer-containing fields
// (time.Time, string, []string) grouped first, pointer-free fields
// (modelCount, health, discoveryEnabled) last.
type providerSnapshot struct {
	lastRefresh time.Time
	// openUntil mirrors providerState's own field of the same name
	// (feat/provider-health) — see providerState.snapshot's doc comment
	// for why it is read in the same locked pass as the rest of this
	// struct's fields.
	openUntil  time.Time
	name       string
	typeName   string
	baseURL    string
	lastErr    string
	models     []string
	modelCount int
	// health mirrors providerState's own field of the same name (feat/
	// provider-health): the admin dashboard's breaker-state column. Every
	// provider reads closed until a discovery failure actually trips its
	// breaker — a healthy deployment's snapshot is unaffected.
	health breakerState
	// discoveryEnabled mirrors providerState.discoveryEnabled (Feature B,
	// v0.22 last-refresh label honesty): read directly rather than through
	// providerState.snapshot(), because it is set once at construction
	// (newModelRegistry, from ProviderConfig.Discovery) and never
	// reassigned afterward — every other providerState field warmFill/
	// maybeRefresh/finishRefresh mutate only reads it, never writes it — so
	// it needs neither the mutex snapshot() takes for the fields that DO
	// mutate
	// (models/lastRefresh/lastErr) nor a place in that method's return
	// shape. The admin dashboard needs it to tell "discovery is off" apart
	// from "discovery is on but has not refreshed yet" — both currently
	// read identically as a zero LastRefresh, which GET /admin/api/overview
	// (admin.go) and the Providers tab (webui) previously rendered as the
	// misleading "refreshed never" for a provider that will never refresh
	// by design.
	discoveryEnabled bool
}

// aliasSnapshotEntry is one configured alias's read-only view for the
// admin dashboard (spec §5, v0.2): the pair exactly as an operator wrote
// it. No secrets involved. The target's actual resolved provider is
// deliberately not included — a target still awaiting its provider's
// first discovery fetch has none yet, and the configured pair is the
// meaningful, stable fact an operator wants to see either way.
type aliasSnapshotEntry struct {
	Alias  string
	Target string
}

// aliasSnapshot returns every configured alias's alias->target pair,
// sorted by alias — the admin dashboard's alias table (spec §5, v0.2).
func (m *modelRegistry) aliasSnapshot() []aliasSnapshotEntry {
	names := make([]string, 0, len(m.aliases))
	for alias := range m.aliases {
		names = append(names, alias)
	}
	sort.Strings(names)

	out := make([]aliasSnapshotEntry, len(names))
	for i, alias := range names {
		out[i] = aliasSnapshotEntry{Alias: alias, Target: m.aliases[alias]}
	}
	return out
}

// snapshot returns every configured provider's read-only view, sorted by
// name (m.providerNames is already sorted at construction) — the admin
// dashboard's provider table (spec §4, v0.2).
func (m *modelRegistry) snapshot() []providerSnapshot {
	out := make([]providerSnapshot, 0, len(m.providerNames))
	for _, name := range m.providerNames {
		adapter := m.adapters[name]
		// Five-value multi-assign straight from the call, no intermediate
		// named variables reused across a loop iteration boundary — Yaegi
		// has had multi-assign edge cases around reused/shadowed loop
		// variables in other parts of this codebase's history; this shape
		// (fresh locals every iteration, assigned once, read once) avoids
		// that class of trap entirely. health/openUntil (feat/provider-
		// health) widen the same tuple rather than a second locked call.
		models, lastRefresh, lastErr, health, openUntil := m.states[name].snapshot()
		out = append(out, providerSnapshot{
			name:             adapter.name(),
			typeName:         adapter.typeName(),
			baseURL:          adapter.base(),
			models:           models,
			modelCount:       len(models),
			lastRefresh:      lastRefresh,
			lastErr:          lastErr,
			discoveryEnabled: m.states[name].discoveryEnabled,
			health:           health,
			openUntil:        openUntil,
		})
	}
	return out
}
