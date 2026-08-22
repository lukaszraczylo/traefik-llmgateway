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
	assert.True(t, fc.enabled, "nil Enabled must default to true — failover is a hot standby with no config change")
	assert.Equal(t, defaultFailoverMaxAttempts, fc.maxAttempts)
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
	reg, err := newModelRegistry(adapters, &Config{Providers: providers}, func(string, ...any) {})
	require.NoError(t, err)
	return &Gateway{registry: reg, failoverHealth: newRequestHealthTracker(), failover: fc}
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

func TestOrderedFailoverCandidates_MaxAttemptsCapsList(t *testing.T) {
	t.Parallel()
	g := testGatewayForCandidates(t, []string{"alpha", "beta", "gamma"}, failoverConfig{enabled: true, maxAttempts: 2})
	primary := resolveCandidate{providerName: "alpha"}
	extra := []resolveCandidate{{providerName: "beta"}, {providerName: "gamma"}}
	got := g.orderedFailoverCandidates(primary, extra)
	assert.Len(t, got, 2, "maxAttempts must cap the total candidate list, primary included")
	assert.Equal(t, []string{"alpha", "beta"}, candNames(got))
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
// health is an ordering hint, never a hard skip: a provider whose
// DISCOVERY breaker is open must still be attempted (and can still serve
// the response) when it is the only one that actually works. Would FAIL
// if discoveryHealthy were (mis-)used as a skip gate.
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

	// beta is healthy by every signal but its own chat endpoint 500s —
	// ordering puts beta (discovery-healthy) FIRST, so this proves the
	// request actually reaches alpha SECOND, not that alpha happened to
	// be tried first anyway.
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

// TestHandleChat_Failover_SingleProvider_UpstreamError_BehavesExactlyAsBefore
// pins the "single-provider deployment behaves exactly as before"
// requirement explicitly for this branch, with failover left at its
// (enabled) default: with no second candidate to try, an upstream error
// must be returned to the client unchanged, identical to v0.1 behavior.
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
