package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file implements feat/target-health: a per-replica, in-memory
// health tracker for MCP servers and A2A agents ("targets"), which have
// no health signal today the way providers do (registry.go's discovery
// circuit breaker, failover.go's requestHealthTracker). Two feeds keep
// it current: always-on PASSIVE recording from real request traffic
// (handleTargetProxy, mcp_a2a.go; mcpFederatedToolsList/
// mcpFederatedToolsCall, mcp_federation.go) and an opt-in, lazy ACTIVE
// probe sweep (maybeSweepTargetHealth, below). Exposure is always on
// too: GET /admin/api/targets (admin.go) and the llmgateway_target_healthy
// gauge (metrics.go) both read this tracker regardless of
// Config.TargetHealth.Enabled — that flag gates only the active probe
// sweep. This tracker is OBSERVE-ONLY: nothing in mcp_a2a.go or
// mcp_federation.go ever skips a server or agent because it reports
// unhealthy — federation routing behavior is unchanged by this feature.

// targetHealthState is the tri-state health readout stateOf/snapshot
// report for one target, mirroring the discovery breaker's healthState
// string shape (registry.go's providerState) so the admin API and
// dashboard can treat both consistently.
type targetHealthState string

const (
	// targetHealthUnknown covers two distinct situations (F4,
	// feat/target-health review), both read the SAME state on purpose —
	// neither has ever demonstrated the target actually works:
	//  1. never observed at all — the tracker has no entry for this
	//     (kind, name) yet, a target configured but never yet reached.
	//  2. observed, but never once succeeded — every observation so far
	//     is a failure, and consecutiveFailures has not yet reached
	//     failureThreshold. Reading "healthy" here (the pre-F4 behavior)
	//     would be a false claim from a target this tracker has never
	//     actually seen answer; reading "unhealthy" before the threshold
	//     is reached would discard the threshold's own protection
	//     against a single transient first failure. See
	//     targetHealthSnapshot.observed for how the admin view (admin.go)
	//     tells these two apart despite sharing this one state.
	targetHealthUnknown targetHealthState = "unknown"
	// targetHealthHealthy: succeeded at least once, and consecutiveFailures
	// has not yet reached failureThreshold. This DELIBERATELY includes a
	// target whose most recent single observation was itself a failure:
	// 1 or 2 recent failures after a success still read "healthy" until
	// the configured threshold is actually crossed — the same
	// consecutive-failure gate the discovery circuit breaker uses
	// (registry.go's providerState/recordHealthLocked), reimplemented
	// independently here since this tracker carries no open/half-open
	// state machine of its own (see this file's own package doc comment
	// for why).
	targetHealthHealthy targetHealthState = "healthy"
	// targetHealthUnhealthy: consecutiveFailures >= failureThreshold,
	// regardless of whether the target has ever succeeded — the
	// threshold gate always takes priority over the never-succeeded
	// check above.
	targetHealthUnhealthy targetHealthState = "unhealthy"
)

// Source values record's callers pass — which mechanism produced the
// last observation, exposed on GET /admin/api/targets and stored per
// entry.
const (
	targetHealthSourceProbe   = "probe"
	targetHealthSourceTraffic = "traffic"
)

// targetHealthMaxErrorRunes bounds targetHealthEntry.lastError: an
// upstream MCP/A2A target's own error text is not operator-controlled
// the way its URL is, so a pathological error string must not grow this
// in-memory tracker unbounded. Runes, not bytes (truncateRunes, below)
// — a byte-level cut could split a multi-byte UTF-8 sequence.
const targetHealthMaxErrorRunes = 200

// targetHealthKey identifies one tracked target by routing kind
// (targetKindMCP or targetKindAgent, mcp_a2a.go — NOT scopeKindAgent,
// which is the limiter/admin-API-facing "agent" spelling for a2a
// targets) and configured name.
type targetHealthKey struct {
	kind string
	name string
}

// targetHealthEntry is one target's current health state, guarded by its
// owning tracker's own mu (never its own) — mirrors latencyHistogram's
// identical "guarded by the parent store's lock" convention
// (metrics.go).
type targetHealthEntry struct {
	lastCheck           time.Time
	lastError           string
	source              string
	latency             time.Duration
	consecutiveFailures int
	// everOK is set true the first time record observes a success for
	// this entry, and never cleared afterward — stateForLocked's F4
	// "never succeeded yet reads unknown, not healthy" rule reads this,
	// not consecutiveFailures alone.
	everOK bool
}

// targetHealthTracker is feat/target-health's own per-pod, in-memory
// health signal for every configured MCP server and A2A agent.
// Deliberately separate from requestHealthTracker (failover.go) and the
// discovery circuit breaker (registry.go): this tracker carries no
// open/half-open/backoff state machine and never gates routing — it
// exists purely for operational visibility.
//
// Every method is nil-receiver-safe, matching this package's own
// nil-means-disabled convention (requestHealthTracker's own doc
// comment, failover.go) — a Gateway assembled directly in a test,
// bypassing newGateway, degrades to "every target reads unknown" rather
// than a nil-pointer panic, even though newGateway itself always
// constructs a real one (see Gateway.targetHealth's own doc comment,
// llmgateway.go).
type targetHealthTracker struct {
	entries map[targetHealthKey]*targetHealthEntry
	nowFn   func() time.Time
	// failureThreshold is TargetHealthConfig.FailureThreshold, validated
	// and defaulted once at construction (validateTargetHealthConfig) —
	// copied here rather than re-read from Config on every stateOf call,
	// the same resolved-value-not-raw-config shape breakerConfig/
	// failoverConfig already use.
	failureThreshold int
	mu               sync.Mutex
}

// newTargetHealthTracker returns an empty tracker: every target reads
// "unknown" until its first recorded observation. threshold is the
// resolved (validated, defaulted) TargetHealthConfig.FailureThreshold.
func newTargetHealthTracker(threshold int) *targetHealthTracker {
	return &targetHealthTracker{entries: make(map[targetHealthKey]*targetHealthEntry), nowFn: time.Now, failureThreshold: threshold}
}

// record accounts one observation (a real proxied request, or an active
// probe) for (kind, name): success resets consecutiveFailures to 0 and
// clears lastError; failure increments consecutiveFailures and stores
// err's message, truncated to targetHealthMaxErrorRunes. Every caller
// today (probeMCPTarget/probeAgentTarget/recordTargetProxyHealth, below;
// mcpFederatedToolsList/mcpFederatedToolsCall, mcp_federation.go) always
// passes a non-nil err alongside ok=false — a failure with no Go error to
// attach is not a shape this tracker currently has to handle.
// rawURL is (kind, name)'s configured target URL — the exact string a
// caller dialed, used ONLY to scrub err's text through sanitizeTargetErr
// (below) before it is stored: a Go *url.Error embeds the dialed URL —
// verbatim, OR with its password masked by net/http's own stripPassword
// when one is present — so an operator-configured target URL carrying
// credentials (an "api-key" query parameter, userinfo) would otherwise
// leak through GET /admin/api/targets' lastError field the same way an
// unsanitized provider error once could (F1, feat/target-health review;
// G1, review round 2, for the password-masked form). The scrub runs
// BEFORE truncateRunes, not after: truncating first could cut a long
// embedded URL in half, leaving the scrub's exact-substring match unable
// to find it and a fragment of the credential in the truncated result.
// source is targetHealthSourceProbe or
// targetHealthSourceTraffic — whichever mechanism produced this
// observation.
func (t *targetHealthTracker) record(kind, name, rawURL string, ok bool, err error, latency time.Duration, source string) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := targetHealthKey{kind: kind, name: name}
	e, exists := t.entries[key]
	if !exists {
		e = &targetHealthEntry{}
		t.entries[key] = e
	}
	e.lastCheck = t.nowFn()
	e.latency = latency
	e.source = source
	if ok {
		e.everOK = true
		e.consecutiveFailures = 0
		e.lastError = ""
		return
	}
	e.consecutiveFailures++
	if err != nil {
		e.lastError = truncateRunes(sanitizeTargetErr(err.Error(), rawURL), targetHealthMaxErrorRunes)
	}
}

// sanitizeTargetErr scrubs msg of rawURL's own credentials, in both forms
// a real error from a call against rawURL can actually carry it (G1,
// feat/target-health review round 2). Go's net/http builds a *url.Error
// via its own unexported stripPassword (net/http/client.go) whenever the
// dialed URL's userinfo carries a password: it rewrites "user:pass@" (and
// a token-as-username-with-no-password, "token:@") to "user:***@" IN THE
// URL BEFORE the error string is ever built — so for such a URL, msg
// never contains rawURL verbatim, and a plain sanitizeProviderErr(msg,
// rawURL) call (admin.go) — which only matches rawURL verbatim — finds
// nothing to scrub, leaking the password and any query-string credential
// riding the same URL (an "api-key" value) straight through. This
// function first tries the plain match (the common case: no userinfo, or
// userinfo with no password, both pass through net/http unmasked and
// still match verbatim), then — only when rawURL's userinfo actually
// carries a password — builds net/http's exact masked form itself
// (mirroring stripPassword's own strings.Replace call precisely) and
// scrubs against that too.
func sanitizeTargetErr(msg, rawURL string) string {
	msg = sanitizeProviderErr(msg, rawURL)
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil || u.User == nil {
		return msg
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return msg
	}
	masked := strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	return sanitizeProviderErr(msg, masked)
}

// stateOf reports (kind, name)'s current targetHealthState — see this
// type's own doc comment for the exact unknown/healthy/unhealthy rule.
func (t *targetHealthTracker) stateOf(kind, name string) targetHealthState {
	if t == nil {
		return targetHealthUnknown
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[targetHealthKey{kind: kind, name: name}]
	if !ok {
		return targetHealthUnknown
	}
	return t.stateForLocked(e)
}

// stateForLocked derives e's targetHealthState. Caller must hold t.mu.
// Split out of stateOf/snapshot so both apply the identical rule. The
// threshold check runs first and always wins (F4): a target that has
// failed failureThreshold times straight reads unhealthy whether or not
// it has ever succeeded. Below threshold, a target that has never once
// succeeded reads unknown rather than healthy — see targetHealthUnknown's
// own doc comment for why.
func (t *targetHealthTracker) stateForLocked(e *targetHealthEntry) targetHealthState {
	if e.consecutiveFailures >= t.failureThreshold {
		return targetHealthUnhealthy
	}
	if !e.everOK {
		return targetHealthUnknown
	}
	return targetHealthHealthy
}

// targetHealthSnapshot is one target's fully-copied health state, safe
// to read after targetHealthTracker.snapshot returns without holding its
// lock — mirrors latencySnapshot's identical shape (metrics.go).
type targetHealthSnapshot struct {
	lastCheck           time.Time
	state               targetHealthState
	lastError           string
	source              string
	latency             time.Duration
	consecutiveFailures int
	// observed is true iff this tracker holds an entry for (kind, name)
	// — i.e. record has been called at least once, regardless of
	// success or failure. F4, feat/target-health review: state alone
	// cannot tell apart targetHealthUnknown's two cases (never observed
	// vs. observed-but-never-succeeded), so targetHealthView (admin.go)
	// reads this field, not state, to decide whether to omit
	// lastCheck/lastError/source/latencyMs — omit only while !observed.
	observed bool
}

// snapshot returns a copy of (kind, name)'s current health state —
// buildAdminTargets (admin.go) and writeTargetMetrics (metrics.go) both
// read through this, once per configured target, rather than exposing
// t.entries directly. A target this tracker has never observed returns
// a zero-valued, !observed snapshot with state targetHealthUnknown.
func (t *targetHealthTracker) snapshot(kind, name string) targetHealthSnapshot {
	if t == nil {
		return targetHealthSnapshot{state: targetHealthUnknown}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[targetHealthKey{kind: kind, name: name}]
	if !ok {
		return targetHealthSnapshot{state: targetHealthUnknown}
	}
	return targetHealthSnapshot{
		observed:            true,
		state:               t.stateForLocked(e),
		lastCheck:           e.lastCheck,
		lastError:           e.lastError,
		source:              e.source,
		latency:             e.latency,
		consecutiveFailures: e.consecutiveFailures,
	}
}

// truncateRunes bounds s to at most n runes, decoding as UTF-8 rather
// than slicing raw bytes — a byte-level cut on an arbitrary upstream
// error string could split a multi-byte rune (escapeLabelValue's own
// doc comment, metrics.go, is the identical concern applied to a
// different string).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// --- config: TargetHealthConfig validation ---

// defaultTargetHealthProbeInterval/min/max bound TargetHealthConfig.
// ProbeInterval; defaultTargetHealthFailureThreshold/max bound
// TargetHealthConfig.FailureThreshold. See TargetHealthConfig's own doc
// comment (llmgateway.go) for what each governs.
const (
	defaultTargetHealthProbeInterval    = 60 * time.Second
	minTargetHealthProbeInterval        = 10 * time.Second
	maxTargetHealthProbeInterval        = time.Hour
	defaultTargetHealthFailureThreshold = 3
	maxTargetHealthFailureThreshold     = 100
)

// targetHealthConfig is TargetHealthConfig, validated and defaulted once
// at construction (newGateway, llmgateway.go) — mirrors breakerConfig/
// failoverConfig's own resolved-value-not-raw-config shape.
type targetHealthConfig struct {
	probeInterval    time.Duration
	failureThreshold int
	enabled          bool
}

// validateTargetHealthConfig validates thc and returns the resolved
// targetHealthConfig newGateway attaches to the Gateway. thc's zero
// value (no targetHealth block at all) resolves to every default below
// unchanged: enabled stays false (a deployment that predates this
// feature, or never configures this block, sends no active probe
// traffic), matching FailoverConfig's own "off unless explicitly opted
// in" precedent (failover.go).
func validateTargetHealthConfig(thc TargetHealthConfig) (targetHealthConfig, error) {
	interval := defaultTargetHealthProbeInterval
	if thc.ProbeInterval != "" {
		d, err := time.ParseDuration(thc.ProbeInterval)
		if err != nil {
			return targetHealthConfig{}, fmt.Errorf("llmgateway: targetHealth.probeInterval %q is invalid: %w", thc.ProbeInterval, err)
		}
		if d < minTargetHealthProbeInterval || d > maxTargetHealthProbeInterval {
			return targetHealthConfig{}, fmt.Errorf("llmgateway: targetHealth.probeInterval must be between %s and %s, got %q", minTargetHealthProbeInterval, maxTargetHealthProbeInterval, thc.ProbeInterval)
		}
		interval = d
	}

	threshold := thc.FailureThreshold
	if threshold == 0 {
		threshold = defaultTargetHealthFailureThreshold
	}
	if threshold < 1 || threshold > maxTargetHealthFailureThreshold {
		return targetHealthConfig{}, fmt.Errorf("llmgateway: targetHealth.failureThreshold must be between 1 and %d, got %d", maxTargetHealthFailureThreshold, threshold)
	}

	return targetHealthConfig{enabled: thc.Enabled, probeInterval: interval, failureThreshold: threshold}, nil
}

// --- active probes: opt-in, lazy, single-flight ---

// targetHealthMaxProbeTimeout caps how long any ONE active probe may
// run, regardless of Config.RequestTimeout — a probe is a cheap,
// synchronous health check, not real traffic, and defaultRequestTimeout
// (timeout.go) is 5 minutes: without this cap, one hung MCP server or
// agent could tie up a probe goroutine for that entire duration.
const targetHealthMaxProbeTimeout = 10 * time.Second

// targetHealthProbeTimeout resolves the timeout ONE active probe gets:
// requestTimeout (g.targetTimeout), capped at
// targetHealthMaxProbeTimeout. Written as a small helper rather than the
// Go 1.21+ builtin min: this plugin runs interpreted under Yaegi, and
// this repo's own constraints call for stdlib-only, builtin-free helpers
// where a construct's interpreter support is unverified.
func targetHealthProbeTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 || requestTimeout > targetHealthMaxProbeTimeout {
		return targetHealthMaxProbeTimeout
	}
	return requestTimeout
}

// maybeSweepTargetHealth is called at the top of every route a scrape or
// a dashboard poll can reach (serveMetrics, metrics.go; serveAdminTargets/
// handleMCPServers/handleAgents, admin.go/mcp_a2a.go) — that traffic IS
// the heartbeat: this plugin has no Close hook under Traefik's hot
// reload (Gateway.Close's own doc comment, llmgateway.go) and therefore
// starts no timer or long-lived goroutine of its own here either,
// mirroring modelRegistry.maybeRefresh's identical reasoning
// (registry.go).
//
// Returns immediately when disabled. Otherwise: g.targetHealthSweeping
// is the single-flight gate (an atomic CAS, not a mutex, since every
// caller reaches this on a live request path and must never block
// behind a lock another such call already holds) — a CAS that fails
// means either a sweep is already in flight, in which case returning is
// exactly right, or another goroutine just won the race to start this
// interval's sweep, in which case returning is exactly right too. The
// CAS alone enforces only single-flight — "at most one sweep in flight
// at a time" — NOT the probe interval: the interval load above it and
// the CAS are not atomic with each other, so a caller whose own interval
// read predates an entirely separate sweep's full start-and-finish could
// still win the CAS once that sweep clears the flag (worst at startup,
// when every caller's first read sees last == 0). F3, feat/target-health
// review: re-check the interval AFTER winning the CAS, against
// whatever targetHealthLastSweepUnixNano holds now — set by the sweep
// that raced ahead, if any — and back out without spawning when that
// sweep already started within the interval.
func (g *Gateway) maybeSweepTargetHealth() {
	if !g.targetHealthCfg.enabled {
		return
	}
	if !targetHealthIntervalElapsed(atomic.LoadInt64(&g.targetHealthLastSweepUnixNano), g.targetHealthCfg.probeInterval) {
		return
	}
	if !atomic.CompareAndSwapInt32(&g.targetHealthSweeping, 0, 1) {
		return
	}
	now := time.Now().UnixNano()
	if !targetHealthIntervalElapsed(atomic.LoadInt64(&g.targetHealthLastSweepUnixNano), g.targetHealthCfg.probeInterval) {
		// Lost the race: some other sweep already started (and, since
		// the flag was 0 again for this CAS to succeed, already
		// finished) within the interval while this call was still
		// working from its own, now-stale, interval read above.
		atomic.StoreInt32(&g.targetHealthSweeping, 0)
		return
	}
	atomic.StoreInt64(&g.targetHealthLastSweepUnixNano, now)
	go g.sweepTargetHealth() //nolint:gosec // G118: self-terminating, bounded by mcpFederatedFanoutConcurrency*targetHealthMaxProbeTimeout worst case, and always clears targetHealthSweeping on return (including on panic) — see maybeRefresh's identical reasoning, registry.go, for why this plugin accepts a bounded, self-terminating background goroutine despite having no Close hook to await it
}

// targetHealthIntervalElapsed reports whether probeInterval has passed
// since lastSweepUnixNano (0 meaning "never swept", which always counts
// as elapsed) — the exact rule maybeSweepTargetHealth applies both above
// and, again, under the CAS.
func targetHealthIntervalElapsed(lastSweepUnixNano int64, probeInterval time.Duration) bool {
	return lastSweepUnixNano == 0 || time.Duration(time.Now().UnixNano()-lastSweepUnixNano) >= probeInterval
}

// sweepTargetHealth performs one active probe sweep: every configured
// MCP server and agent, concurrently, behind a semaphore of
// mcpFederatedFanoutConcurrency (mcp_federation.go) — reusing federation's
// own fan-out concurrency constant rather than adding a second one for
// an identical "how many outbound target requests at once" question.
// Always clears g.targetHealthSweeping on return, even on panic
// (mirrors modelRegistry.refreshProvider's own deferred-recover
// reasoning, registry.go — this goroutine has no ServeHTTP caller to
// unwind into either).
func (g *Gateway) sweepTargetHealth() {
	defer atomic.StoreInt32(&g.targetHealthSweeping, 0)
	defer func() {
		if rec := recover(); rec != nil {
			g.logf("target health: sweep panicked: %v", rec)
		}
	}()

	timeout := targetHealthProbeTimeout(g.targetTimeout)

	var wg sync.WaitGroup
	sem := make(chan struct{}, mcpFederatedFanoutConcurrency)

	for name, tc := range g.cfg.MCPServers {
		if tc == nil {
			continue
		}
		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					g.logf("target health: probe for mcp %q panicked: %v", name, rec)
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			g.probeMCPTarget(name, url, timeout)
		}(name, tc.URL)
	}
	for name, ac := range g.cfg.Agents {
		if ac == nil {
			continue
		}
		wg.Add(1)
		go func(name, url, card string) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					g.logf("target health: probe for agent %q panicked: %v", name, rec)
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			g.probeAgentTarget(name, url, card, timeout)
		}(name, ac.URL, ac.Card)
	}
	wg.Wait()
}

// probeMCPTarget performs one active probe of MCP server name at
// rawURL, bounded by timeout, and records the result with source
// "probe". A legacy HTTP+SSE-transport server (isLegacySSETransportURL,
// mcp_federation.go) gets a plain GET with its body closed unread
// (probeLegacySSE, below); every other server gets the same
// initialize/close handshake mcpBackendCall's own fallback path already
// uses (mcpBackendHandshake/mcpBackendCloseSession, mcp_federation.go)
// — a JSON-RPC error answering "initialize" is a failure carrying that
// message, exactly like a transport-level error.
func (g *Gateway) probeMCPTarget(name, rawURL string, timeout time.Duration) {
	start := time.Now()

	if isLegacySSETransportURL(rawURL) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		ok, err := g.probeLegacySSE(ctx, rawURL)
		cancel()
		g.targetHealth.record(targetKindMCP, name, rawURL, ok, err, time.Since(start), targetHealthSourceProbe)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	sessionID, err := g.mcpBackendHandshake(ctx, rawURL, mcpBackendResponseMaxBytes)
	cancel()
	ok := err == nil
	if ok && sessionID != "" {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), mcpSessionCloseTimeout)
		g.mcpBackendCloseSession(closeCtx, rawURL, sessionID)
		closeCancel()
	}
	g.targetHealth.record(targetKindMCP, name, rawURL, ok, err, time.Since(start), targetHealthSourceProbe)
}

// probeLegacySSE probes a legacy HTTP+SSE-transport MCP server (URL path
// ends "/sse", isLegacySSETransportURL) with a plain GET: ok iff no
// transport error and the response status is under 500. The body is
// closed IMMEDIATELY after headers, never read — an SSE endpoint streams
// indefinitely, so reading it would hang the probe for its entire
// timeout budget on every sweep.
func (g *Gateway) probeLegacySSE(ctx context.Context, rawURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil) //nolint:gosec // operator-configured target URL, validated at construction (validateTargetURLs, mcp_a2a.go)
	if err != nil {
		return false, err
	}
	resp, err := g.targetClient.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close() //nolint:errcheck // deliberately unread — see this function's own doc comment
	if resp.StatusCode >= http.StatusInternalServerError {
		return false, fmt.Errorf("legacy sse probe: upstream returned HTTP %d", resp.StatusCode)
	}
	return true, nil
}

// probeAgentTarget performs one active probe of agent name: a GET of
// baseURL+cardPath (AgentConfig.Card, or defaultAgentCardPath when empty
// — mcp_a2a.go), bounded by timeout. ok iff no transport error and the
// response status is under 500 — a 404 counts as reachable (e.g. an
// umbrella agent whose root has no card of its own), the identical
// "any status under 500 proves the target answered" rule handleTargetProxy's
// own passive recording applies (recordTargetProxyHealth, below). The
// body is closed without being read: an agent-card probe only needs the
// status.
func (g *Gateway) probeAgentTarget(name, baseURL, cardPath string, timeout time.Duration) {
	if cardPath == "" {
		cardPath = defaultAgentCardPath
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+cardPath, nil) //nolint:gosec // operator-configured target URL, validated at construction (validateTargetURLs, mcp_a2a.go)
	if err != nil {
		g.targetHealth.record(targetKindAgent, name, baseURL, false, err, time.Since(start), targetHealthSourceProbe)
		return
	}
	resp, err := g.targetClient.Do(req)
	if err != nil {
		g.targetHealth.record(targetKindAgent, name, baseURL, false, err, time.Since(start), targetHealthSourceProbe)
		return
	}
	_ = resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure
	ok := resp.StatusCode < http.StatusInternalServerError
	var probeErr error
	if !ok {
		probeErr = fmt.Errorf("agent probe: upstream returned HTTP %d", resp.StatusCode)
	}
	g.targetHealth.record(targetKindAgent, name, baseURL, ok, probeErr, time.Since(start), targetHealthSourceProbe)
}

// --- passive recording: handleTargetProxy (mcp_a2a.go) ---

// recordTargetProxyHealth applies handleTargetProxy's own passive
// recording rule to one proxyUpstream result (routes_passthrough.go):
// failure iff the proxy attempt itself failed for a reason other than
// the client canceling, OR the upstream answered with a 5xx; otherwise
// success — any status under 500 counts as reachable, 4xx included,
// since a 4xx still proves the target itself answered. A client cancel
// (result.clientCanceled) records nothing at all: a caller hanging up
// says nothing about the target's own health.
func (g *Gateway) recordTargetProxyHealth(kind, name, rawURL string, result proxyResult, ok bool, latency time.Duration) {
	if result.clientCanceled {
		return
	}
	failed := !ok || result.status >= http.StatusInternalServerError
	var err error
	if failed {
		err = targetProxyHealthError(ok, result.status)
	}
	g.targetHealth.record(kind, name, rawURL, !failed, err, latency, targetHealthSourceTraffic)
}

// targetProxyHealthError builds a short, bounded description for a
// failed passive observation. proxyUpstream itself already logged the
// real underlying error; this is only what targetHealthEntry.lastError
// — exposed via GET /admin/api/targets — shows an operator.
func targetProxyHealthError(ok bool, status int) error {
	if !ok {
		return errors.New("target proxy: upstream request failed")
	}
	return fmt.Errorf("target proxy: upstream returned HTTP %d", status)
}
