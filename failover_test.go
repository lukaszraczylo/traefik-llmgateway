package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- modelRegistry.resolveWithCandidates / failoverCandidates ---

func TestModelRegistry_ResolveWithCandidates_BareCollision_ReturnsOrderedExtras(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
		"gamma": newFakeAdapter("gamma"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"beta":  {Models: []string{"shared"}},
		"alpha": {Models: []string{"shared"}},
		"gamma": {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	primary, extra, err := reg.resolveWithCandidates("shared", allowAllGroup())
	require.NoError(t, err)
	assert.Equal(t, "alpha", primary.providerName, "primary must match resolve's own sorted-first winner")

	names := make([]string, len(extra))
	for i, c := range extra {
		names[i] = c.providerName
	}
	assert.Equal(t, []string{"beta", "gamma"}, names, "extras must be every OTHER owning provider, in providerNames sorted order, primary excluded")
	for _, c := range extra {
		assert.Equal(t, "shared", c.upstreamModel)
		assert.Equal(t, c.providerName+"/shared", c.canonical)
	}
}

func TestModelRegistry_ResolveWithCandidates_ProviderPrefixed_NoExtras(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"beta":  {Models: []string{"shared"}},
		"alpha": {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	primary, extra, err := reg.resolveWithCandidates("beta/shared", allowAllGroup())
	require.NoError(t, err)
	assert.Equal(t, "beta", primary.providerName)
	assert.Nil(t, extra, "an explicit provider/model request must never gain failover candidates")
}

func TestModelRegistry_ResolveWithCandidates_AliasToProviderPrefixedTarget_NoExtras(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"beta":  {Models: []string{"shared"}},
			"alpha": {Models: []string{"shared"}},
		},
		ModelAliases: map[string]string{"aliased": "beta/shared"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	primary, extra, err := reg.resolveWithCandidates("aliased", allowAllGroup())
	require.NoError(t, err)
	assert.Equal(t, "beta", primary.providerName)
	assert.Nil(t, extra, "an alias targeting an explicit provider/model must never gain failover candidates")
}

func TestModelRegistry_ResolveWithCandidates_AliasToBareTarget_ReturnsExtras(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"beta":  {Models: []string{"shared"}},
			"alpha": {Models: []string{"shared"}},
		},
		ModelAliases: map[string]string{"aliased": "shared"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	primary, extra, err := reg.resolveWithCandidates("aliased", allowAllGroup())
	require.NoError(t, err)
	assert.Equal(t, "alpha", primary.providerName, "an alias to a bare-collision target resolves via the same sorted-first winner")
	require.Len(t, extra, 1)
	assert.Equal(t, "beta", extra[0].providerName)
}

// TestModelRegistry_ResolveWithCandidates_ExtraFiltersByProviderAuthorization
// proves a group restricted away from a would-be failover provider never
// gets it offered as a candidate, even though that provider genuinely
// serves the bare id.
func TestModelRegistry_ResolveWithCandidates_ExtraFiltersByProviderAuthorization(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"beta":  {Models: []string{"shared"}},
		"alpha": {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	grp := &group{name: "restricted", providers: []string{"alpha"}}
	primary, extra, err := reg.resolveWithCandidates("shared", grp)
	require.NoError(t, err)
	assert.Equal(t, "alpha", primary.providerName)
	assert.Empty(t, extra, "beta is denied to this group, so it must never appear as a failover candidate")
}

// TestModelRegistry_ResolveWithCandidates_ResolveError_PropagatesUnchanged
// proves feat/failover never changes resolve's own error/status contract:
// an unknown or denied model still resolves to the identical error, with
// no candidates offered.
func TestModelRegistry_ResolveWithCandidates_ResolveError_PropagatesUnchanged(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"alpha": newFakeAdapter("alpha")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"alpha": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)

	_, extra, err := reg.resolveWithCandidates("no-such-model", allowAllGroup())
	assert.ErrorIs(t, err, errModelUnknown)
	assert.Nil(t, extra)

	grp := &group{name: "denied", models: []string{"something-else"}}
	_, extra, err = reg.resolveWithCandidates("gpt-test", grp)
	assert.ErrorIs(t, err, errModelDenied)
	assert.Nil(t, extra)
}

// --- requestHealthTracker ---

func TestRequestHealthTracker_HealthyByDefault(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	assert.True(t, tr.healthy("never-seen"))
}

func TestRequestHealthTracker_NilSafe(t *testing.T) {
	t.Parallel()
	var tr *requestHealthTracker
	assert.True(t, tr.healthy("anything"), "a nil tracker must report healthy, never panic")
	assert.NotPanics(t, func() { tr.record("anything", false) })
}

// TestRequestHealthTracker_OpensAfterThreshold_ThenSkips is the direct
// regression test for "a provider marked unhealthy by REQUEST-path health
// is skipped without being called": would FAIL if record() never opened
// the breaker (a reverted feature would leave healthy() returning true
// forever).
func TestRequestHealthTracker_OpensAfterThreshold_ThenSkips(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	now := time.Now()
	tr.nowFn = func() time.Time { return now }

	for i := 0; i < requestBreakerFailureThreshold-1; i++ {
		tr.record("p", false)
		assert.True(t, tr.healthy("p"), "must stay healthy before the threshold is reached")
	}
	tr.record("p", false)
	assert.False(t, tr.healthy("p"), "must open exactly at the failure threshold")
}

func TestRequestHealthTracker_SuccessBeforeThreshold_ResetsCounter(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	for i := 0; i < requestBreakerFailureThreshold-1; i++ {
		tr.record("p", false)
	}
	tr.record("p", true)
	// The counter must have reset to zero, not merely "not yet open": one
	// more failure alone must not open it.
	tr.record("p", false)
	assert.True(t, tr.healthy("p"), "a success below threshold must reset the failure count, not just pause it")
}

// TestRequestHealthTracker_RecoversAfterCooldown_ThenCloses proves the
// breaker admits exactly one probe once its cooldown elapses, and a
// successful probe closes it.
func TestRequestHealthTracker_RecoversAfterCooldown_ThenCloses(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	now := time.Now()
	tr.nowFn = func() time.Time { return now }

	for i := 0; i < requestBreakerFailureThreshold; i++ {
		tr.record("p", false)
	}
	require.False(t, tr.healthy("p"))

	now = now.Add(requestBreakerOpenDuration - time.Millisecond)
	assert.False(t, tr.healthy("p"), "must stay unhealthy until the cooldown fully elapses")

	now = now.Add(2 * time.Millisecond)
	assert.True(t, tr.healthy("p"), "must admit exactly one probe once the cooldown elapses")

	tr.record("p", true)
	assert.True(t, tr.healthy("p"))
	// A single further failure alone must not reopen it — closeBreaker
	// resets the failure counter to zero, mirroring registry.go's own
	// discovery breaker.
	tr.record("p", false)
	assert.True(t, tr.healthy("p"))
}

// TestRequestHealthTracker_FailedProbe_ReopensWithDoubledBackoff proves a
// failed post-cooldown probe re-opens the breaker with a LONGER backoff,
// not the same one — would FAIL if reverted to a fixed-duration reopen.
func TestRequestHealthTracker_FailedProbe_ReopensWithDoubledBackoff(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	now := time.Now()
	tr.nowFn = func() time.Time { return now }

	for i := 0; i < requestBreakerFailureThreshold; i++ {
		tr.record("p", false)
	}
	now = now.Add(requestBreakerOpenDuration + time.Millisecond)
	require.True(t, tr.healthy("p"), "cooldown elapsed, must admit the probe")
	tr.record("p", false) // the probe itself fails

	assert.False(t, tr.healthy("p"), "a failed probe must reopen the breaker")
	// The new backoff is requestBreakerOpenDuration*2: advancing only
	// past the ORIGINAL window must still read unhealthy.
	now = now.Add(requestBreakerOpenDuration + time.Millisecond)
	assert.False(t, tr.healthy("p"), "a failed probe must double the backoff, not repeat the original window")

	now = now.Add(requestBreakerOpenDuration + 2*time.Millisecond)
	assert.True(t, tr.healthy("p"), "must recover once the doubled window has fully elapsed")
}

// TestRequestHealthTracker_FailureWhileOpenBeforeCooldown_IsANoOp is the
// direct regression test for adversarial-review finding F5: a failure
// arriving while the breaker is ALREADY open and its cooldown has not
// yet elapsed must change nothing — not the backoff, not openUntil. An
// earlier version fell through to the closed-state consecutiveFailures++
// branch for this case, which reset backoff to the flat base duration
// and slid openUntil forward on every such failure, so doubling never
// actually happened outside the narrow post-cooldown probe path.
func TestRequestHealthTracker_FailureWhileOpenBeforeCooldown_IsANoOp(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	now := time.Now()
	tr.nowFn = func() time.Time { return now }

	for i := 0; i < requestBreakerFailureThreshold; i++ {
		tr.record("p", false)
	}
	st := tr.stateFor("p")
	st.mu.Lock()
	openUntilAfterTrip := st.openUntil
	backoffAfterTrip := st.backoff
	st.mu.Unlock()
	require.Equal(t, requestBreakerOpenDuration, backoffAfterTrip)

	// Well before the cooldown elapses: several more failures arrive
	// (e.g. other in-flight requests against the same dead provider).
	now = now.Add(requestBreakerOpenDuration / 2)
	for i := 0; i < 5; i++ {
		require.False(t, tr.healthy("p"))
		tr.record("p", false)
	}

	st.mu.Lock()
	openUntilAfterExtraFailures := st.openUntil
	backoffAfterExtraFailures := st.backoff
	failuresAfterExtra := st.consecutiveFailures
	st.mu.Unlock()
	assert.Equal(t, openUntilAfterTrip, openUntilAfterExtraFailures, "openUntil must not slide forward from failures arriving before the cooldown elapses")
	assert.Equal(t, backoffAfterTrip, backoffAfterExtraFailures, "backoff must stay at the base duration, not reset by every failure while already open")
	assert.Equal(t, requestBreakerFailureThreshold, failuresAfterExtra, "consecutiveFailures must not keep growing once the breaker is already open")
}

func TestRequestHealthTracker_BackoffCapsAtMax(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	now := time.Now()
	tr.nowFn = func() time.Time { return now }

	for i := 0; i < requestBreakerFailureThreshold; i++ {
		tr.record("p", false)
	}
	// Fail every probe repeatedly — backoff must never exceed
	// requestBreakerMaxOpenDuration, so this loop must always terminate
	// within a bounded number of doublings.
	for i := 0; i < 20; i++ {
		now = now.Add(requestBreakerMaxOpenDuration + time.Millisecond)
		require.True(t, tr.healthy("p"))
		tr.record("p", false)
	}
	st := tr.stateFor("p")
	st.mu.Lock()
	backoff := st.backoff
	st.mu.Unlock()
	assert.LessOrEqual(t, backoff, requestBreakerMaxOpenDuration)
}

// TestRequestHealthTracker_IndependentPerProvider proves one provider's
// failures never affect another's health — the per-pod signal is keyed by
// provider name, not shared global state.
func TestRequestHealthTracker_IndependentPerProvider(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		tr.record("broken", false)
	}
	assert.False(t, tr.healthy("broken"))
	assert.True(t, tr.healthy("fine"), "an unrelated provider's health must be untouched")
}

// TestRequestHealthTracker_ConcurrentRecord_NoRace exercises record/healthy
// from many goroutines at once — the whole point of this signal is to sit
// on the live request path.
func TestRequestHealthTracker_ConcurrentRecord_NoRace(t *testing.T) {
	t.Parallel()
	tr := newRequestHealthTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.record("p", i%3 != 0)
			tr.healthy("p")
		}(i)
	}
	wg.Wait()
}

// --- failoverEligible / isProviderNotFoundError ---

func TestFailoverEligible(t *testing.T) {
	t.Parallel()
	deadlineExceededErr := fmt.Errorf("%w: %w", errUpstream, fmt.Errorf("dial tcp: %w", context.DeadlineExceeded))

	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "400 never falls through", err: &providerHTTPError{status: http.StatusBadRequest}, want: false},
		{name: "500 falls through", err: &providerHTTPError{status: http.StatusInternalServerError}, want: true},
		{name: "502 falls through", err: &providerHTTPError{status: http.StatusBadGateway}, want: true},
		{name: "429 falls through", err: &providerHTTPError{status: http.StatusTooManyRequests}, want: true},
		{name: "401 falls through", err: &providerHTTPError{status: http.StatusUnauthorized}, want: true},
		{name: "403 falls through", err: &providerHTTPError{status: http.StatusForbidden}, want: true},
		{name: "404 falls through", err: &providerHTTPError{status: http.StatusNotFound}, want: true},
		{name: "connection failure falls through", err: fmt.Errorf("%w: dial tcp: connection refused", errUpstream), want: true},
		{name: "deadline exceeded (timeout) falls through", err: deadlineExceededErr, want: true},
		{name: "client canceled does not fall through", err: fmt.Errorf("%w: %w", errUpstream, context.Canceled), want: false},
		{name: "request build failure does not fall through", err: fmt.Errorf("%w: %w: build request", errUpstream, errRequestBuildFailed), want: false},
		{name: "translateError does not fall through", err: &translateError{msg: "bad field"}, want: false},
		{name: "responseTranslationError does not fall through", err: &responseTranslationError{err: errors.New("boom")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, failoverEligible(tt.err))
		})
	}
}

func TestIsProviderNotFoundError(t *testing.T) {
	t.Parallel()
	assert.True(t, isProviderNotFoundError(&providerHTTPError{status: http.StatusNotFound}))
	assert.False(t, isProviderNotFoundError(&providerHTTPError{status: http.StatusInternalServerError}))
	assert.False(t, isProviderNotFoundError(errors.New("plain error")))
	assert.False(t, isProviderNotFoundError(nil))
}

// --- validateFailoverConfig ---

func TestValidateFailoverConfig_Defaults(t *testing.T) {
	t.Parallel()
	fc, err := validateFailoverConfig(FailoverConfig{})
	require.NoError(t, err)
	assert.False(t, fc.enabled, "nil Enabled must default to false — coordinator ruling: new behavior that can change which provider serves a request goes behind a flag defaulting off")
	assert.Equal(t, defaultFailoverMaxAttempts, fc.maxAttempts)
}

func TestValidateFailoverConfig_ExplicitEnable(t *testing.T) {
	t.Parallel()
	yes := true
	fc, err := validateFailoverConfig(FailoverConfig{Enabled: &yes})
	require.NoError(t, err)
	assert.True(t, fc.enabled)
}

func TestValidateFailoverConfig_ExplicitDisable(t *testing.T) {
	t.Parallel()
	no := false
	fc, err := validateFailoverConfig(FailoverConfig{Enabled: &no})
	require.NoError(t, err)
	assert.False(t, fc.enabled)
}

func TestValidateFailoverConfig_MaxAttemptsBounds(t *testing.T) {
	t.Parallel()
	_, err := validateFailoverConfig(FailoverConfig{MaxAttempts: -1})
	assert.Error(t, err)

	_, err = validateFailoverConfig(FailoverConfig{MaxAttempts: maxFailoverMaxAttempts + 1})
	assert.Error(t, err)

	fc, err := validateFailoverConfig(FailoverConfig{MaxAttempts: maxFailoverMaxAttempts})
	require.NoError(t, err)
	assert.Equal(t, maxFailoverMaxAttempts, fc.maxAttempts)
}

// --- Gateway.orderedFailoverCandidates ---

// testGatewayForCandidates builds a minimal *Gateway sufficient for
// orderedFailoverCandidates: a real modelRegistry (for discoveryHealthy)
// and a fresh requestHealthTracker, without going through newGateway/New.
func testGatewayForCandidates(t *testing.T, providerNames []string, fc failoverConfig) *Gateway {
	t.Helper()
	adapters := make(map[string]providerAdapter, len(providerNames))
	providers := make(map[string]*ProviderConfig, len(providerNames))
	for _, name := range providerNames {
		adapters[name] = newFakeAdapter(name)
		providers[name] = &ProviderConfig{Models: []string{"shared"}}
	}
	cfg := &Config{Providers: providers}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	return &Gateway{registry: reg, failoverHealth: newRequestHealthTracker(), failover: fc, cfg: cfg, name: "test"}
}

func candNames(cands []resolveCandidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.providerName
	}
	return out
}

func TestOrderedFailoverCandidates_Disabled_ReturnsPrimaryOnly(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta"}, failoverConfig{enabled: false, maxAttempts: 3})
	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.Equal(t, []string{"alpha"}, candNames(got), "disabled failover must never try a second provider")
}

func TestOrderedFailoverCandidates_NoExtras_ReturnsPrimaryOnly_RegardlessOfEnabled(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha"}, failoverConfig{enabled: true, maxAttempts: 3})
	primary := resolveCandidate{providerName: "alpha"}
	got := g.orderedFailoverCandidates(primary, nil)
	assert.Equal(t, []string{"alpha"}, candNames(got), "single-provider deployment must behave exactly as before regardless of the enabled switch")
}

func TestOrderedFailoverCandidates_RequestUnhealthy_HardSkipped(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta"}, failoverConfig{enabled: true, maxAttempts: 3})
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		g.failoverHealth.record("alpha", false)
	}
	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.Equal(t, []string{"beta"}, candNames(got), "a request-unhealthy provider must be excluded outright, never merely deprioritized")
}

func TestOrderedFailoverCandidates_AllRequestUnhealthy_FailsOpenToFullList(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta"}, failoverConfig{enabled: true, maxAttempts: 3})
	for _, name := range []string{"alpha", "beta"} {
		for i := 0; i < requestBreakerFailureThreshold; i++ {
			g.failoverHealth.record(name, false)
		}
	}
	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, candNames(got), "every candidate unhealthy must fail OPEN to the full list, not refuse to route at all")
}

func TestOrderedFailoverCandidates_DiscoveryUnhealthy_DeprioritizedNotDropped(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta"}, failoverConfig{enabled: true, maxAttempts: 3})
	// Force alpha's discovery breaker open directly.
	st := g.registry.states["alpha"]
	st.mu.Lock()
	st.health = breakerOpen
	st.openUntil = time.Now().Add(time.Hour)
	st.mu.Unlock()
	require.False(t, g.registry.discoveryHealthy("alpha"))

	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.Equal(t, []string{"beta", "alpha"}, candNames(got), "discovery-unhealthy must be deprioritized (moved after), never dropped from the list")
}

// TestOrderedFailoverCandidates_DoesNotCap proves orderedFailoverCandidates
// itself no longer applies maxAttempts — capFailoverCandidates does, as the
// LAST step of runMeteredCall's own sequence, after cost filtering.
func TestOrderedFailoverCandidates_DoesNotCap(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta", "gamma"}, failoverConfig{enabled: true, maxAttempts: 2})
	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}, {providerName: "gamma"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, candNames(got), "orderedFailoverCandidates must return the full health/discovery-ordered list uncapped")
}

func TestCapFailoverCandidates(t *testing.T) {
	t.Parallel()
	all := []resolveCandidate{{providerName: "alpha"}, {providerName: "beta"}, {providerName: "gamma"}}

	got := capFailoverCandidates(all, 2)
	assert.Equal(t, []string{"alpha", "beta"}, candNames(got), "maxAttempts must cap the total candidate list, primary included")

	got = capFailoverCandidates(all, 10)
	assert.Len(t, got, 3, "a cap larger than the list must not truncate anything")

	got = capFailoverCandidates(all, 0)
	assert.Len(t, got, 3, "max <= 0 must be a defensive no-op")
}

// --- resolveUnifiedPricing / candidateCostAllowed / filterCandidatesByCost ---

func TestResolveUnifiedPricing_CanonicalFirst_ThenBareFallback(t *testing.T) {
	t.Parallel()
	overrides := map[string]*ModelPricing{
		"beta/shared": {InputPerM: 1, OutputPerM: 2},
		"shared":      {InputPerM: 5, OutputPerM: 6},
	}
	p, ok := resolveUnifiedPricing("beta/shared", "shared", overrides)
	require.True(t, ok)
	assert.Equal(t, ModelPricing{InputPerM: 1, OutputPerM: 2}, p, "an override keyed on the canonical id must win over the bare fallback — the exact case unifiedCostMicros itself resolves this way")
}

func TestResolveUnifiedPricing_FallsBackToBare(t *testing.T) {
	t.Parallel()
	overrides := map[string]*ModelPricing{"shared": {InputPerM: 5, OutputPerM: 6}}
	p, ok := resolveUnifiedPricing("beta/shared", "shared", overrides)
	require.True(t, ok)
	assert.Equal(t, ModelPricing{InputPerM: 5, OutputPerM: 6}, p)
}

func TestResolveUnifiedPricing_Unknown(t *testing.T) {
	t.Parallel()
	_, ok := resolveUnifiedPricing("beta/shared", "shared", nil)
	assert.False(t, ok, "neither the canonical nor the bare id has a price — this must never be silently treated as zero")
}

func TestResolveUnifiedPricing_BuiltinTable(t *testing.T) {
	t.Parallel()
	p, ok := resolveUnifiedPricing("openai/gpt-5", "gpt-5", nil)
	require.True(t, ok, "the built-in pricing table must still resolve with no operator overrides configured")
	assert.Equal(t, builtinPricing["gpt-5"], p)
}

// TestCandidateCostAllowed is the direct regression test for the
// operator's binding ruling: table-driven across every named case,
// including the two-dimension requirement (cheap on one axis, dear on
// the other, must still be rejected).
func TestCandidateCostAllowed(t *testing.T) {
	t.Parallel()
	cheap := ModelPricing{InputPerM: 1, OutputPerM: 2}
	dear := ModelPricing{InputPerM: 3, OutputPerM: 4}
	mixedCheapInputDearOutput := ModelPricing{InputPerM: 0.5, OutputPerM: 10}
	zero := ModelPricing{}

	tests := []struct {
		name                         string
		primary, candidate           ModelPricing
		primaryKnown, candidateKnown bool
		want                         bool
	}{
		{name: "both known, candidate strictly cheaper on both", primary: dear, primaryKnown: true, candidate: cheap, candidateKnown: true, want: true},
		{name: "both known, identical price", primary: cheap, primaryKnown: true, candidate: cheap, candidateKnown: true, want: true},
		{name: "both known, candidate strictly dearer on both", primary: cheap, primaryKnown: true, candidate: dear, candidateKnown: true, want: false},
		{name: "both known, cheaper input but dearer output must still reject", primary: cheap, primaryKnown: true, candidate: mixedCheapInputDearOutput, candidateKnown: true, want: false},
		{name: "both known, dearer input but cheaper output must still reject", primary: mixedCheapInputDearOutput, primaryKnown: true, candidate: cheap, candidateKnown: true, want: false},
		{name: "primary known, candidate unknown: never allowed", primary: dear, primaryKnown: true, candidate: ModelPricing{}, candidateKnown: false, want: false},
		{name: "primary known-zero, candidate unknown: never allowed", primary: zero, primaryKnown: true, candidate: ModelPricing{}, candidateKnown: false, want: false},
		{name: "primary unknown, candidate known non-zero: never allowed", primary: ModelPricing{}, primaryKnown: false, candidate: cheap, candidateKnown: true, want: false},
		{name: "primary unknown, candidate known but only input non-zero: never allowed", primary: ModelPricing{}, primaryKnown: false, candidate: ModelPricing{InputPerM: 0.01}, candidateKnown: true, want: false},
		{name: "primary unknown, candidate known-zero: allowed", primary: ModelPricing{}, primaryKnown: false, candidate: zero, candidateKnown: true, want: true},
		{name: "both unknown: allowed", primary: ModelPricing{}, primaryKnown: false, candidate: ModelPricing{}, candidateKnown: false, want: true},
		{name: "both known-zero: allowed", primary: zero, primaryKnown: true, candidate: zero, candidateKnown: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := candidateCostAllowed(tt.primary, tt.primaryKnown, tt.candidate, tt.candidateKnown)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFilterCandidatesByCost_SkipsPricierKeepsCheaper(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta", "gamma"}, failoverConfig{enabled: true, maxAttempts: 3})
	g.cfg.Pricing = map[string]*ModelPricing{
		"alpha/shared": {InputPerM: 2, OutputPerM: 4}, // primary
		"beta/shared":  {InputPerM: 3, OutputPerM: 4}, // dearer input -> rejected
		"gamma/shared": {InputPerM: 2, OutputPerM: 4}, // equal -> allowed
	}
	candidates := []resolveCandidate{
		{providerName: "alpha", upstreamModel: "shared", canonical: "alpha/shared"},
		{providerName: "beta", upstreamModel: "shared", canonical: "beta/shared"},
		{providerName: "gamma", upstreamModel: "shared", canonical: "gamma/shared"},
	}
	got := g.filterCandidatesByCost(candidates, "shared")
	assert.Equal(t, []string{"alpha", "gamma"}, candNames(got))
}

func TestFilterCandidatesByCost_FewerThanTwoCandidates_NoOp(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha"}, failoverConfig{enabled: true, maxAttempts: 3})
	candidates := []resolveCandidate{{providerName: "alpha", upstreamModel: "shared", canonical: "alpha/shared"}}
	got := g.filterCandidatesByCost(candidates, "shared")
	assert.Equal(t, candidates, got)
}

// =====================================================================
// End-to-end HTTP tests
// =====================================================================

// twoProviderFailoverConfig builds a Config with two openai-type providers,
// "alpha" and "beta" (alpha sorted first — the bareWinner/primary
// candidate), both configured with the identical bare model id "shared",
// pointed at srvAlpha/srvBeta respectively, one group with no
// restrictions, and one user "alice" with unlimited limits.
func twoProviderFailoverConfig(srvAlpha, srvBeta *httptest.Server) *Config {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: srvAlpha.URL, APIKey: "k", Models: []string{"shared"}},
		"beta":  {Type: "openai", BaseURL: srvBeta.URL, APIKey: "k", Models: []string{"shared"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	// Explicitly opt in: FailoverConfig.Enabled now defaults to false
	// (coordinator ruling) — every test in this file that exercises actual
	// failover behavior must opt in on purpose, the same way an operator
	// would, rather than relying on a default this feature no longer ships
	// with.
	yes := true
	cfg.Failover = FailoverConfig{Enabled: &yes}
	return cfg
}

const successRespBody = `{"id":"chatcmpl-ok","object":"chat.completion","model":"shared","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`

func jsonServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func countingServer(status int, body string, calls *int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestHandleChat_Failover_FallsThroughAndServes is table-driven across
// every failure shape the brief names as a fallthrough case (5xx,
// connection failure, 429, 401, 403, 404) — proving alpha's failure is
// classified eligible AND that beta's real response reaches the client
// each time. Would FAIL if failoverEligible or the candidate loop's
// hasNext wiring were reverted (alpha's error would just be returned to
// the client instead of falling through).
func TestHandleChat_Failover_FallsThroughAndServes(t *testing.T) {
	tests := []struct {
		name        string
		alphaStatus int
		unreachable bool
	}{
		{name: "5xx", alphaStatus: http.StatusInternalServerError},
		{name: "connection_failure", unreachable: true},
		{name: "429", alphaStatus: http.StatusTooManyRequests},
		{name: "401", alphaStatus: http.StatusUnauthorized},
		{name: "403", alphaStatus: http.StatusForbidden},
		{name: "404", alphaStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var alphaURL string
			if tt.unreachable {
				// A closed listener: connections are refused immediately,
				// deterministic and fast, unlike a black-holed address.
				dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				alphaURL = dead.URL
				dead.Close()
			} else {
				alphaSrv := jsonServer(tt.alphaStatus, `{"error":{"message":"alpha down","type":"server_error"}}`)
				defer alphaSrv.Close()
				alphaURL = alphaSrv.URL
			}
			betaSrv := jsonServer(http.StatusOK, successRespBody)
			defer betaSrv.Close()

			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{
				"alpha": {Type: "openai", BaseURL: alphaURL, APIKey: "k", Models: []string{"shared"}},
				"beta":  {Type: "openai", BaseURL: betaSrv.URL, APIKey: "k", Models: []string{"shared"}},
			}
			cfg.Groups = map[string]*GroupConfig{"default": {}}
			cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
			yes := true
			cfg.Failover = FailoverConfig{Enabled: &yes}

			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			require.NoError(t, err)

			origStderr := captureStderrStart()
			body := map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			logOutput := captureStderrStop(origStderr)

			require.Equal(t, http.StatusOK, rec.Code, "must fall through to beta and serve its response, body=%s", rec.Body.String())
			assert.Equal(t, successRespBody, rec.Body.String())

			if tt.alphaStatus == http.StatusNotFound {
				assert.Contains(t, logOutput, "404", "a 404 failover must log loudly")
				assert.Contains(t, logOutput, "alpha")
				assert.Contains(t, logOutput, "beta")
			}
		})
	}
}

// TestHandleChat_Failover_400_DoesNotFallThrough is the regression test
// for the brief's one hard exception: a malformed request must never
// retry against a second provider. Would FAIL if failoverEligible treated
// 400 the same as every other status.
func TestHandleChat_Failover_400_DoesNotFallThrough(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusBadRequest, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "a 400 must be returned as-is, never retried")
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "beta must never be called after alpha's 400")
}

// TestHandleChat_Failover_MidStream_DoesNotFailOver_NoCorruption is the
// regression test for THE HARD CONSTRAINT: once anything has reached the
// client, failover must never happen and the response must never be
// corrupted with a second envelope. Would FAIL if the loop checked
// failoverEligible before sw.wroteHeader.
func TestHandleChat_Failover_MidStream_DoesNotFailOver_NoCorruption(t *testing.T) {
	const firstChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"

	var betaCalls int64
	alphaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = w.Write([]byte(firstChunk))
		fl.Flush()
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, hjErr := hj.Hijack()
		if hjErr == nil {
			_ = conn.Close()
		}
	}))
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "stream": true, "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "headers already committed before the drop")
	assert.Equal(t, firstChunk, rec.Body.String(), "no trailing envelope, no failover content appended")
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "beta must never be called once alpha has started writing")
}

// TestHandleChat_Failover_RequestUnhealthy_SkippedWithoutBeingCalled is the
// end-to-end regression test for the request-path health hard-skip gate:
// after enough consecutive failures, alpha must be excluded from routing
// entirely — its upstream server must receive no further hits — while beta
// still serves.
func TestHandleChat_Failover_RequestUnhealthy_SkippedWithoutBeingCalled(t *testing.T) {
	var alphaCalls, betaCalls int64
	alphaSrv := countingServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`, &alphaCalls)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "messages": []any{}}
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "every one of these must still fail over to beta and succeed")
	}
	require.Equal(t, int64(requestBreakerFailureThreshold), atomic.LoadInt64(&alphaCalls))

	// alpha's request-path breaker must now be open — one more request
	// must skip it entirely.
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(requestBreakerFailureThreshold), atomic.LoadInt64(&alphaCalls), "alpha must NOT receive a further hit once request-unhealthy")
	assert.Equal(t, int64(requestBreakerFailureThreshold+1), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_DiscoveryUnhealthy_StillTried proves discovery
// health is never a HARD SKIP: a provider whose DISCOVERY breaker is
// open must still be attempted, and can still serve the response, when
// beta (the only other candidate) fails. Would FAIL if discoveryHealthy
// were used to exclude a candidate outright, but does NOT independently
// prove ordering (deleting the deprioritization partition in
// orderedFailoverCandidates leaves this test passing too, since beta
// fails either way and alpha is tried regardless of position) — the
// ordering claim itself is covered only by
// TestOrderedFailoverCandidates_DiscoveryUnhealthy_DeprioritizedNotDropped
// (adversarial-review correction: this comment previously overstated
// what this test proves).
func TestHandleChat_Failover_DiscoveryUnhealthy_StillTried(t *testing.T) {
	alphaSrv := jsonServer(http.StatusOK, successRespBody) // alpha's CHAT endpoint works fine
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)

	// Force alpha's DISCOVERY breaker open directly (feat/provider-health's
	// own signal) — its chat-completions endpoint is untouched and stays
	// healthy; only discovery is marked broken, per discoveryHealthy's own
	// documented scope.
	st := gw.registry.states["alpha"]
	st.mu.Lock()
	st.health = breakerOpen
	st.openUntil = time.Now().Add(time.Hour)
	st.mu.Unlock()
	require.False(t, gw.registry.discoveryHealthy("alpha"))

	// beta is healthy by every signal but its own chat endpoint 500s, so
	// the request can only succeed if alpha is tried at some point in
	// the chain despite its open discovery breaker.
	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "alpha must still be tried and must serve, body=%s", rec.Body.String())
	assert.Equal(t, successRespBody, rec.Body.String())
}

// TestHandleChat_Failover_TenantRequestCount_IncrementsOnceAcrossFailover
// is the regression test for "never double-count the request": a
// tenant's own req/min counter must move by exactly 1 across a whole
// failover chain, never once per attempt.
func TestHandleChat_Failover_TenantRequestCount_IncrementsOnceAcrossFailover(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	now := time.Now()
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, now)
	require.True(t, ok)
	assert.Equal(t, int64(1), reqCount, "one client-facing request must count as exactly one, regardless of how many providers it took")

	totalReq, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, now)
	require.True(t, ok)
	assert.Equal(t, int64(1), totalReq)
}

// TestHandleChat_Failover_PerProviderAttemptCounters_AttributeCorrectly is
// the regression test for "rebind the attempt recorder per attempt": alpha
// must show exactly one attempt/one failure, and beta exactly one
// attempt/zero failures — never both attributed to alpha (the closure's
// original, fixed-at-construction bug this feature explicitly fixes).
func TestHandleChat_Failover_PerProviderAttemptCounters_AttributeCorrectly(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	// Deterministic: run the fire-and-forget counter write synchronously
	// (mirrors TestHandleChat_ContextDeadlineExceeded_RecordsProviderFailure).
	gw.limiter.spawn = func(f func()) { f() }

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	now := time.Now()
	alphaAttempts, _ := gw.limiter.getCounter(kindProvider, "alpha", metricProvAttempt, windowDay, now)
	alphaFails, _ := gw.limiter.getCounter(kindProvider, "alpha", metricProvFail, windowDay, now)
	betaAttempts, _ := gw.limiter.getCounter(kindProvider, "beta", metricProvAttempt, windowDay, now)
	betaFails, _ := gw.limiter.getCounter(kindProvider, "beta", metricProvFail, windowDay, now)

	assert.Equal(t, int64(1), alphaAttempts, "alpha attempts")
	assert.Equal(t, int64(1), alphaFails, "alpha fails — a reverted 'rebind per attempt' bug would misattribute this to whichever provider the closure was first bound to")
	assert.Equal(t, int64(1), betaAttempts, "beta attempts")
	assert.Equal(t, int64(0), betaFails, "beta fails")
}

// TestHandleChat_Failover_CachedUnderWinningProviderKey is the regression
// test for cache-key correctness: a response produced by the FAILOVER
// provider (beta) must be retrievable via a cache key built with BETA's
// own name, not alpha's — would FAIL if cacheKeyStr were still computed
// once, before the loop, from the primary candidate alone.
func TestHandleChat_Failover_CachedUnderWinningProviderKey(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	redisLn := newBehavioralRedisServer(t)
	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: 1 << 20}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)

	reqBody := map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", reqBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "miss", rec.Header().Get("X-Llmgw-Cache"))
	require.Equal(t, int64(1), atomic.LoadInt64(&betaCalls))

	// Reconstruct the exact key runMeteredCall would compute for BETA's
	// own candidate — upstreamModel is "shared" (the bare id every
	// candidate shares in this fixture), requestedModel is the client's
	// own "shared" string, endpoint is cacheEndpointChat, and req is the
	// decoded body with "model" rewritten (exactly what the gateway
	// hashes) — see cacheKey's own doc comment, cache.go.
	hashed := map[string]any{"model": "shared", "messages": reqBody["messages"]}
	wantKey := cacheKey("beta", "shared", "shared", cacheEndpointChat, hashed)
	cached, hit := gw.cache.lookup(wantKey)
	require.True(t, hit, "the response must be stored under beta's own cache key")
	assert.Contains(t, string(cached.Body), `"id":"chatcmpl-ok"`)

	// A second, identical request must now be served from cache without
	// beta's real backend being hit again.
	req2 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", reqBody)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, int64(1), atomic.LoadInt64(&betaCalls), "the second request must hit cache under beta's key, not call beta's backend again")
}

// --- cost guard (operator ruling): never fail over to a pricier candidate ---

// costGuardConfig builds a two-provider (alpha, beta) config exactly like
// twoProviderFailoverConfig but WITHOUT setting cfg.Pricing, so each test
// configures its own overrides for its own case.
func costGuardConfig(srvAlpha, srvBeta *httptest.Server) *Config {
	cfg := twoProviderFailoverConfig(srvAlpha, srvBeta)
	cfg.Providers["alpha"].Models = []string{"shared"}
	cfg.Providers["beta"].Models = []string{"shared"}
	return cfg
}

// driveCostGuardRequest posts one chat-completion request for "shared"
// through h and returns the recorder and everything logged to stderr
// while it ran.
func driveCostGuardRequest(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, string) {
	t.Helper()
	origStderr := captureStderrStart()
	body := map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, captureStderrStop(origStderr)
}

// TestHandleChat_Failover_CostGuard_SkipsPricierCandidate is the direct
// regression test for the operator's requirement: a candidate priced
// higher than the primary must never serve. Would FAIL if the cost guard
// were reverted (mutation-tested in this branch's own history).
func TestHandleChat_Failover_CostGuard_SkipsPricierCandidate(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"alpha/shared": {InputPerM: 1, OutputPerM: 2},
		"beta/shared":  {InputPerM: 1, OutputPerM: 3}, // dearer on output only — still must be rejected
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, logOutput := driveCostGuardRequest(t, h)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "beta is dearer, so alpha's own failure must be returned as-is, body=%s", rec.Body.String())
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "beta must never be called once the cost guard rejects it")
	assert.Contains(t, logOutput, "alpha")
	assert.Contains(t, logOutput, "beta")
	assert.Contains(t, logOutput, "shared")
}

// TestHandleChat_Failover_CostGuard_AllowsCheaperOrEqualCandidate is the
// mirror positive case: a candidate priced at or below the primary must
// still serve normally.
func TestHandleChat_Failover_CostGuard_AllowsCheaperOrEqualCandidate(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"alpha/shared": {InputPerM: 2, OutputPerM: 4},
		"beta/shared":  {InputPerM: 2, OutputPerM: 4}, // exactly equal — allowed
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusOK, rec.Code, "beta is equally priced, so failover must still serve it, body=%s", rec.Body.String())
	assert.Equal(t, successRespBody, rec.Body.String())
}

// TestHandleChat_Failover_CostGuard_PrimaryKnown_CandidateUnknown_Skips
// covers the operator's first unknown-pricing ruling: primary priced,
// candidate unpriced — never allowed, since an unknown price cannot be
// proven not-more-expensive.
func TestHandleChat_Failover_CostGuard_PrimaryKnown_CandidateUnknown_Skips(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"alpha/shared": {InputPerM: 1, OutputPerM: 2},
		// beta has no override, and "shared"/"beta/shared" is not in the
		// built-in table either — its price is genuinely unknown.
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_CostGuard_PrimaryUnknown_CandidateKnownNonZero_Skips
// covers the operator's second unknown-pricing ruling: moving from
// unbilled to billed is itself a spend increase.
func TestHandleChat_Failover_CostGuard_PrimaryUnknown_CandidateKnownNonZero_Skips(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"beta/shared": {InputPerM: 1, OutputPerM: 2}, // alpha has no override at all: unknown
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_CostGuard_PrimaryUnknown_CandidateKnownZero_Allows
// covers the ruling's own stated exception: a candidate that is known,
// explicit free can never be more expensive than an unknown amount.
func TestHandleChat_Failover_CostGuard_PrimaryUnknown_CandidateKnownZero_Allows(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"beta/shared": {InputPerM: 0, OutputPerM: 0}, // explicit, known-zero override
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusOK, rec.Code, "a known-zero candidate can never be more expensive than an unknown primary, body=%s", rec.Body.String())
}

// TestHandleChat_Failover_CostGuard_BothUnknown_Allows and
// TestHandleChat_Failover_CostGuard_BothKnownZero_Allows cover the
// ruling's "equal, so allowed" cases.
func TestHandleChat_Failover_CostGuard_BothUnknown_Allows(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv) // no cfg.Pricing at all
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusOK, rec.Code, "both unknown must be treated as equal, body=%s", rec.Body.String())
}

func TestHandleChat_Failover_CostGuard_BothKnownZero_Allows(t *testing.T) {
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := costGuardConfig(alphaSrv, betaSrv)
	cfg.Pricing = map[string]*ModelPricing{
		"alpha/shared": {InputPerM: 0, OutputPerM: 0},
		"beta/shared":  {InputPerM: 0, OutputPerM: 0},
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	rec, _ := driveCostGuardRequest(t, h)
	assert.Equal(t, http.StatusOK, rec.Code, "both known-zero must be treated as equal, body=%s", rec.Body.String())
}

// TestHandleChat_Failover_CostGuard_PerProviderPricingOverride_RealShape is
// the requested production-shape case: the SAME bare model id
// (a real, built-in-priced model — "gpt-5") served by two providers,
// where alpha has NO override (resolves via the built-in table) and beta
// has an operator-set per-provider override making it dearer on the
// canonical "beta/gpt-5" key. Proves the guard resolves pricing through
// the exact canonical-then-bare path unifiedCostMicros itself uses, not
// a parallel, simplified notion of price — the whole point of ruling 1.
func TestHandleChat_Failover_CostGuard_PerProviderPricingOverride_RealShape(t *testing.T) {
	const realModel = "gpt-5"
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: alphaSrv.URL, APIKey: "k", Models: []string{realModel}},
		"beta":  {Type: "openai", BaseURL: betaSrv.URL, APIKey: "k", Models: []string{realModel}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	yes := true
	cfg.Failover = FailoverConfig{Enabled: &yes}
	// Only beta gets a per-provider override — alpha resolves gpt-5's
	// price from builtinPricing instead, exactly the real production
	// shape a per-provider Pricing override produces.
	builtin := builtinPricing[realModel]
	cfg.Pricing = map[string]*ModelPricing{
		"beta/" + realModel: {InputPerM: builtin.InputPerM * 10, OutputPerM: builtin.OutputPerM * 10},
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	origStderr := captureStderrStart()
	body := map[string]any{"model": realModel, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	logOutput := captureStderrStop(origStderr)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "beta's per-provider override makes it 10x alpha's built-in price, so it must be rejected, body=%s", rec.Body.String())
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls))
	assert.Contains(t, logOutput, "alpha")
	assert.Contains(t, logOutput, "beta")
	assert.Contains(t, logOutput, realModel)
}

// --- adversarial-review regressions (F1-F4, F6, F7) ---

// TestHandleChat_Failover_F1_StreamOptionsNotLeakedAcrossCandidates is the
// direct regression test for adversarial-review finding F1: an
// openai-type adapter mutates req in place (chatCompletion forces
// stream_options.include_usage=true onto it BEFORE it ever calls the
// upstream, provider_openai.go), so reusing one shared req map across
// candidates let a later candidate read back an earlier candidate's own
// forced injection and misreport clientAskedUsage=true to a client that
// never asked for streamed usage — leaking the gateway's own
// deliberately-suppressed usage-only SSE chunk. alpha fails before ever
// reaching the network (its own mutation to its OWN req copy still
// happens first, exactly like the real bug), beta serves a real SSE
// stream containing a usage-only chunk the client never asked for.
func TestHandleChat_Failover_F1_StreamOptionsNotLeakedAcrossCandidates(t *testing.T) {
	const contentChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	const usageOnlyChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"
	const doneMarker = "data: [DONE]\n\n"

	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		require.True(t, ok)
		for _, chunk := range []string{contentChunk, usageOnlyChunk, doneMarker} {
			_, _ = w.Write([]byte(chunk))
			fl.Flush()
		}
	}))
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	// The client explicitly does NOT set stream_options at all — it
	// never asked for usage in the stream.
	body := map[string]any{"model": "shared", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rw := newRecordingWriter()
	h.ServeHTTP(rw, req)

	require.Equal(t, http.StatusOK, rw.status)
	assert.Contains(t, rw.buf.String(), `"content":"Hi"`, "the real content chunk must still be forwarded")
	assert.NotContains(t, rw.buf.String(), `"prompt_tokens":5`, "a client that never asked for stream usage must never receive it, even though ALPHA's own (failed, discarded) attempt forced include_usage=true on ITS OWN copy of the request")
}

// TestHandleChat_Failover_F2_StaleSSEHeadersDoNotSurviveToPlainResponse is
// the direct regression test for adversarial-review finding F2:
// newSSEWriter (sse.go) stages Content-Type/Cache-Control/
// X-Accel-Buffering on sw.Header() the moment a streaming attempt starts,
// before any byte is written. alpha enters that streaming path (a real
// 200 + text/event-stream response) but its connection is severed before
// any complete SSE event is dispatched — an error, satisfying THE HARD
// CONSTRAINT (nothing was actually WRITTEN to the client) — so failover
// proceeds to beta, which answers a plain, non-streaming JSON body.
func TestHandleChat_Failover_F2_StaleSSEHeadersDoNotSurviveToPlainResponse(t *testing.T) {
	alphaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, hjErr := hj.Hijack()
		if hjErr == nil {
			// Closing a hijacked chunked-encoding connection with no
			// terminating chunk produces a genuine "unexpected EOF" on
			// the client's read side (verified empirically), not a
			// clean end-of-stream — readSSE returns that as a real
			// error without ever dispatching an event, so
			// sw.writeData/sw.WriteHeader is never reached even though
			// newSSEWriter already staged its three headers.
			_ = conn.Close()
		}
	}))
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, successRespBody, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"), "beta's own Content-Type must win, not alpha's stale text/event-stream")
	assert.Empty(t, rec.Header().Get("Cache-Control"), "alpha's staged SSE header must never survive into beta's plain JSON response")
	assert.Empty(t, rec.Header().Get("X-Accel-Buffering"), "alpha's staged SSE header must never survive into beta's plain JSON response")
}

// TestHandleChat_Failover_F3_401IsAFailure_OpensRequestHealthBreaker is the
// direct regression test for adversarial-review finding F3: a provider
// answering 401 to every request (a dead API key — exactly the "masking
// a broken provider key" risk this feature exists to reduce) must
// eventually be skipped, not retried forever. Would FAIL under the
// reverted isTransient-based classification, since isTransient returns
// false for any non-429 4xx.
func TestHandleChat_Failover_F3_401IsAFailure_OpensRequestHealthBreaker(t *testing.T) {
	var alphaCalls, betaCalls int64
	alphaSrv := countingServer(http.StatusUnauthorized, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, &alphaCalls)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "messages": []any{}}
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "every one of these must still fail over to beta and succeed")
	}
	require.Equal(t, int64(requestBreakerFailureThreshold), atomic.LoadInt64(&alphaCalls))

	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(requestBreakerFailureThreshold), atomic.LoadInt64(&alphaCalls), "alpha must be skipped after 3 consecutive 401s, not called a 4th time")
	assert.Equal(t, int64(requestBreakerFailureThreshold+1), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_F4_SharedDeadlineExhausted_NeverMisattributedToNextCandidate
// is the direct regression test for adversarial-review finding F4: alpha
// burns the ENTIRE shared request context sleeping past its deadline;
// beta must never be dialed at all (an attempt after the deadline is
// already spent would fail near-instantly for a reason that has nothing
// to do with beta's own health), never be recorded as having failed, and
// the client-facing error must name alpha, not beta.
func TestHandleChat_Failover_F4_SharedDeadlineExhausted_NeverMisattributedToNextCandidate(t *testing.T) {
	alphaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // well past the request's own 50ms deadline below
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successRespBody))
	}))
	defer alphaSrv.Close()
	var betaCalls int64
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	gw.limiter.spawn = func(f func()) { f() }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")

	origStderr := captureStderrStart()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	logOutput := captureStderrStop(origStderr)

	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "beta must never be dialed once the shared deadline is already spent")
	assert.True(t, gw.failoverHealth.healthy("beta"), "beta must never be marked unhealthy for a timeout that was alpha's own fault")

	now := time.Now()
	betaAttempts, _ := gw.limiter.getCounter(kindProvider, "beta", metricProvAttempt, windowDay, now)
	assert.Equal(t, int64(0), betaAttempts, "beta's own dashboard attempt counter must not move either")

	assert.Contains(t, logOutput, "alpha", "the error must name the provider that actually failed")
}

// TestHandleChat_Failover_F6_RetryAttemptsDoNotOpenBreakerOnOneRequest is
// the direct regression test for adversarial-review finding F6: with
// retry.enabled and attempts:3, ONE client request against a
// permanently-failing provider must count as ONE outcome for the
// request-health gate, not one per raw retry attempt — otherwise a
// single request alone could reach requestBreakerFailureThreshold (3)
// purely from ITS OWN internal retries.
func TestHandleChat_Failover_F6_RetryAttemptsDoNotOpenBreakerOnOneRequest(t *testing.T) {
	var alphaCalls int64
	alphaSrv := countingServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`, &alphaCalls)
	defer alphaSrv.Close()
	betaSrv := jsonServer(http.StatusOK, successRespBody)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	cfg.Retry = RetryConfig{Enabled: true, Attempts: 3, Backoff: "1ms"}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "must still fail over to beta after alpha's own retries are exhausted")

	// retry.attempts=3 means 4 raw HTTP attempts against alpha for this
	// ONE client request (retryPolicy's own doc comment, retry.go).
	require.Equal(t, int64(4), atomic.LoadInt64(&alphaCalls))
	// The request-health gate must still read this as exactly ONE
	// failure, not four — well below requestBreakerFailureThreshold (3).
	assert.True(t, gw.failoverHealth.healthy("alpha"), "one client request's internal retries must count as one outcome for the request-health gate, not one per raw attempt")
}

// TestHandleChat_Failover_F7_CachedFailoverResponse_NeverHitsDeadPrimary is
// the direct regression test for adversarial-review finding F7: even
// while alpha stays permanently broken, a SECOND identical request must
// be served from beta's cache entry WITHOUT alpha's dead backend being
// hit again on that second request — proving cache lookups run for
// every candidate BEFORE any of them is attempted, not interleaved with
// each candidate's own turn.
func TestHandleChat_Failover_F7_CachedFailoverResponse_NeverHitsDeadPrimary(t *testing.T) {
	var alphaCalls, betaCalls int64
	alphaSrv := countingServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`, &alphaCalls)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	redisLn := newBehavioralRedisServer(t)
	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: 1 << 20}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	reqBody := map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req1 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", reqBody)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, int64(1), atomic.LoadInt64(&alphaCalls))
	require.Equal(t, int64(1), atomic.LoadInt64(&betaCalls))

	req2 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", reqBody)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "hit", rec2.Header().Get("X-Llmgw-Cache"))
	assert.Equal(t, int64(1), atomic.LoadInt64(&alphaCalls), "alpha's dead backend must NOT be hit again just to discover beta's cache entry")
	assert.Equal(t, int64(1), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_SingleProvider_UpstreamError_BehavesExactlyAsBefore
// pins the "single-provider deployment behaves exactly as before"
// requirement explicitly for this branch, with failover left at its
// (now disabled) default: with no second candidate to try, an upstream
// error must be returned to the client unchanged, identical to v0.1
// behavior, regardless of the enabled switch either way.
func TestHandleChat_Failover_SingleProvider_UpstreamError_BehavesExactlyAsBefore(t *testing.T) {
	srv := jsonServer(http.StatusTooManyRequests, `{"error":{"message":"rate limited upstream","type":"rate_limit_error"}}`)
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "gpt-test", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "a single-provider deployment must still surface the upstream error unchanged — there is nothing to fail over to")
	var out struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "upstream_error", out.Error.Type)
}

// TestHandleChat_Failover_DefaultConfig_ByteIdenticalToPreFailover is the
// regression test for the coordinator's default-off ruling: a Config that
// never mentions "failover" at all — the shape of every config written
// before this feature existed, and the shape most operators keep using
// after upgrading without opting in — must behave byte-identically to
// having no failover code at all. Would FAIL if validateFailoverConfig's
// default reverted to enabled.
func TestHandleChat_Failover_DefaultConfig_ByteIdenticalToPreFailover(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: alphaSrv.URL, APIKey: "k", Models: []string{"shared"}},
		"beta":  {Type: "openai", BaseURL: betaSrv.URL, APIKey: "k", Models: []string{"shared"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	// cfg.Failover is deliberately left untouched — the zero value, what
	// every config predating this feature has.

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "an unconfigured Failover block must leave alpha's own 500 unchanged")
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "beta must never be called when failover was never opted into")
}

// TestHandleChat_Failover_ExplicitlyDisabled_5xxNeverFallsThrough proves
// the on/off switch actually gates the behavior end to end.
func TestHandleChat_Failover_ExplicitlyDisabled_5xxNeverFallsThrough(t *testing.T) {
	var betaCalls int64
	alphaSrv := jsonServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	cfg := twoProviderFailoverConfig(alphaSrv, betaSrv)
	no := false
	cfg.Failover = FailoverConfig{Enabled: &no}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "with failover disabled, alpha's own 500 must be returned as-is, wrapped in the upstream-error envelope")
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls))
}

// TestHandleChat_Failover_ContextDeadlineExceeded_ClassifiedEligible_
// AndRecordsHealthFailure proves timeout classification end to end
// through the SAME mechanism recordProviderAttempt already uses
// (isDeadlineExceeded) — see this test file's package-level note on why a
// full "alpha times out, beta still serves" HTTP scenario cannot be
// demonstrated: every candidate in one logical request shares ONE
// request context, so once that context's own deadline is what makes
// alpha's failure a genuine timeout, zero budget remains for any
// subsequent candidate. This proves the half that IS achievable and
// correct: alpha's attempt is recorded as a request-health failure
// exactly like a plain connection error would be.
func TestHandleChat_Failover_ContextDeadlineExceeded_RecordsRequestHealthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successRespBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"shared"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.True(t, gw.failoverHealth.healthy("alpha"), "one deadline-exceeded attempt alone must not yet open the breaker (below threshold)")

	st := gw.failoverHealth.stateFor("alpha")
	st.mu.Lock()
	failures := st.consecutiveFailures
	st.mu.Unlock()
	assert.Equal(t, 1, failures, "the deadline-exceeded attempt must be classified as a request-health failure, not silently ignored")
}

// capturedStderr holds the state captureStderrStart needs to hand back to
// captureStderrStop — the same os.Pipe swap
// TestHandleChat_MidStreamUpstreamDrop_NoTrailingEnvelope already uses in
// routes_unified_test.go, factored out here so the table-driven fallthrough
// test above can capture the 404 loud-log assertion per sub-test.
type capturedStderr struct {
	orig *os.File
	pr   *os.File
	pw   *os.File
}

// captureStderrStart redirects os.Stderr to an in-memory pipe. Callers
// must not run this concurrently with another goroutine writing to
// os.Stderr in the SAME process (the same constraint the existing
// mid-stream-drop test already accepts) — every caller in this file runs
// its captured request synchronously, sequentially, never inside
// t.Parallel().
func captureStderrStart() capturedStderr {
	orig := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		panic(err) // os.Pipe failing is not a condition any test here can meaningfully recover from
	}
	os.Stderr = pw
	return capturedStderr{orig: orig, pr: pr, pw: pw}
}

// captureStderrStop restores os.Stderr and returns everything written to
// it since the matching captureStderrStart call.
func captureStderrStop(c capturedStderr) string {
	os.Stderr = c.orig
	_ = c.pw.Close()
	out, _ := io.ReadAll(c.pr)
	return string(out)
}
