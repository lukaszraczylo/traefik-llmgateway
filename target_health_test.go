package traefikllmgateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- targetHealthTracker: state machine ---

func TestTargetHealthTracker_NeverRecorded_ReadsUnknown(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	assert.Equal(t, targetHealthUnknown, tr.stateOf(targetKindMCP, "alpha"))
}

func TestTargetHealthTracker_NilReceiver_Safe(t *testing.T) {
	t.Parallel()
	var tr *targetHealthTracker
	assert.NotPanics(t, func() {
		tr.record(targetKindMCP, "alpha", "", true, nil, time.Millisecond, targetHealthSourceTraffic)
	})
	assert.Equal(t, targetHealthUnknown, tr.stateOf(targetKindMCP, "alpha"))
	snap := tr.snapshot(targetKindMCP, "alpha")
	assert.Equal(t, targetHealthUnknown, snap.state)
}

func TestTargetHealthTracker_SingleSuccess_ReadsHealthy(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	tr.record(targetKindMCP, "alpha", "", true, nil, 5*time.Millisecond, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthHealthy, tr.stateOf(targetKindMCP, "alpha"))
}

// TestTargetHealthTracker_BelowThreshold_StaysHealthy proves the
// documented exception: 1 or 2 recent failures after a success still
// reads "healthy" until consecutiveFailures actually reaches the
// configured threshold (targetHealthTracker's own doc comment).
func TestTargetHealthTracker_BelowThreshold_StaysHealthy(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	tr.record(targetKindMCP, "alpha", "", true, nil, 0, targetHealthSourceTraffic)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthHealthy, tr.stateOf(targetKindMCP, "alpha"), "1 failure of threshold 3 must still read healthy")
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom again"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthHealthy, tr.stateOf(targetKindMCP, "alpha"), "2 failures of threshold 3 must still read healthy")
}

func TestTargetHealthTracker_ReachesThreshold_ReadsUnhealthy(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	for i := 0; i < 3; i++ {
		tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	}
	assert.Equal(t, targetHealthUnhealthy, tr.stateOf(targetKindMCP, "alpha"))
}

// TestTargetHealthTracker_FirstObservationFailure_ReadsUnknown proves F4:
// a target whose only observations so far are failures below
// failureThreshold reads "unknown", not "healthy" — the tracker has
// never actually seen it succeed, so "healthy" would be a false claim
// even though the failure-threshold gate has not fired. The threshold
// still protects against a transient first failure: this stays
// "unknown", it does not jump straight to "unhealthy".
func TestTargetHealthTracker_FirstObservationFailure_ReadsUnknown(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthUnknown, tr.stateOf(targetKindMCP, "alpha"), "never having succeeded must read unknown, not healthy, below threshold")

	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom again"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthUnknown, tr.stateOf(targetKindMCP, "alpha"), "still unknown after a second failure, still below threshold")
}

// TestTargetHealthTracker_SuccessThenFewFailures_ReadsHealthy re-proves
// TestTargetHealthTracker_BelowThreshold_StaysHealthy's claim in F4
// terms: once a target HAS succeeded at least once, 1-2 subsequent
// failures below threshold still read "healthy" — F4 only changes the
// NEVER-succeeded case.
func TestTargetHealthTracker_SuccessThenFewFailures_ReadsHealthy(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	tr.record(targetKindMCP, "alpha", "", true, nil, 0, targetHealthSourceTraffic)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom again"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthHealthy, tr.stateOf(targetKindMCP, "alpha"))
}

func TestTargetHealthTracker_SuccessAfterUnhealthy_RecoversToHealthy(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(2)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	require.Equal(t, targetHealthUnhealthy, tr.stateOf(targetKindMCP, "alpha"))

	tr.record(targetKindMCP, "alpha", "", true, nil, 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthHealthy, tr.stateOf(targetKindMCP, "alpha"))
	snap := tr.snapshot(targetKindMCP, "alpha")
	assert.Equal(t, 0, snap.consecutiveFailures, "a success must reset consecutiveFailures to 0")
	assert.Empty(t, snap.lastError, "a success must clear lastError")
}

func TestTargetHealthTracker_ErrorString_BoundedLength(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	long := strings.Repeat("x", targetHealthMaxErrorRunes+500)
	tr.record(targetKindMCP, "alpha", "", false, errors.New(long), 0, targetHealthSourceProbe)
	snap := tr.snapshot(targetKindMCP, "alpha")
	assert.LessOrEqual(t, len([]rune(snap.lastError)), targetHealthMaxErrorRunes)
	assert.Equal(t, strings.Repeat("x", targetHealthMaxErrorRunes), snap.lastError)
}

func TestTargetHealthTracker_ShortError_Unchanged(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	tr.record(targetKindMCP, "alpha", "", false, errors.New("dial tcp: connection refused"), 0, targetHealthSourceProbe)
	snap := tr.snapshot(targetKindMCP, "alpha")
	assert.Equal(t, "dial tcp: connection refused", snap.lastError)
}

// TestTargetHealthTracker_Record_ScrubsCredentialsFromLastError proves F1:
// record scrubs an error's embedded target URL through sanitizeProviderErr
// before storing lastError, so a target URL configured with an embedded
// credential (an "api-key" query parameter, or userinfo) never survives
// into GET /admin/api/targets.
func TestTargetHealthTracker_Record_ScrubsCredentialsFromLastError(t *testing.T) {
	t.Parallel()

	t.Run("query parameter credential", func(t *testing.T) {
		t.Parallel()
		tr := newTargetHealthTracker(3)
		rawURL := "https://svc.internal/mcp?api-key=SECRETVALUE"
		err := errors.New(`Get "` + rawURL + `": dial tcp: connection refused`)
		tr.record(targetKindMCP, "alpha", rawURL, false, err, 0, targetHealthSourceProbe)
		snap := tr.snapshot(targetKindMCP, "alpha")
		assert.NotContains(t, snap.lastError, "SECRETVALUE")
		assert.NotContains(t, snap.lastError, "api-key=")
	})

	t.Run("userinfo credential", func(t *testing.T) {
		t.Parallel()
		tr := newTargetHealthTracker(3)
		rawURL := "https://tok3n@svc.internal/mcp"
		err := errors.New(`Get "` + rawURL + `": dial tcp: connection refused`)
		tr.record(targetKindMCP, "alpha", rawURL, false, err, 0, targetHealthSourceProbe)
		snap := tr.snapshot(targetKindMCP, "alpha")
		assert.NotContains(t, snap.lastError, "tok3n")
	})
}

// TestTargetHealthTracker_Record_ScrubsBeforeTruncate proves F1: the URL
// scrub runs against err.Error() BEFORE truncateRunes bounds it to
// targetHealthMaxErrorRunes — even when the embedded URL straddles the
// truncation point, no fragment of the secret survives.
func TestTargetHealthTracker_Record_ScrubsBeforeTruncate(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	rawURL := "https://svc.internal/mcp?api-key=" + strings.Repeat("S", 60)
	prefix := strings.Repeat("p", 180)
	require.Less(t, len([]rune(prefix)), targetHealthMaxErrorRunes, "prefix must start before the truncation point")
	require.Greater(t, len([]rune(prefix))+len([]rune(rawURL)), targetHealthMaxErrorRunes, "the url must straddle the truncation point")
	errText := prefix + " " + rawURL + " connection refused"

	tr.record(targetKindMCP, "alpha", rawURL, false, errors.New(errText), 0, targetHealthSourceProbe)
	snap := tr.snapshot(targetKindMCP, "alpha")
	assert.LessOrEqual(t, len([]rune(snap.lastError)), targetHealthMaxErrorRunes)
	assert.NotContains(t, snap.lastError, "SSSS", "no fragment of the secret must survive truncation")
	assert.NotContains(t, snap.lastError, "api-key=")
}

func TestTargetHealthTracker_KeyedByKindAndName_Independent(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(1)
	tr.record(targetKindMCP, "shared", "", false, errors.New("boom"), 0, targetHealthSourceTraffic)
	assert.Equal(t, targetHealthUnhealthy, tr.stateOf(targetKindMCP, "shared"))
	assert.Equal(t, targetHealthUnknown, tr.stateOf(targetKindAgent, "shared"), "an mcp server and an agent sharing a name must track independently")
}

func TestTargetHealthTracker_Snapshot_FieldsPopulated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tr := newTargetHealthTracker(3)
	tr.nowFn = func() time.Time { return now }
	tr.record(targetKindAgent, "beta", "", true, nil, 42*time.Millisecond, targetHealthSourceProbe)

	snap := tr.snapshot(targetKindAgent, "beta")
	assert.Equal(t, targetHealthHealthy, snap.state)
	assert.Equal(t, now, snap.lastCheck)
	assert.Equal(t, 42*time.Millisecond, snap.latency)
	assert.Equal(t, targetHealthSourceProbe, snap.source)
	assert.Equal(t, 0, snap.consecutiveFailures)
}

func TestTargetHealthTracker_ConcurrentRecord_NoRace(t *testing.T) {
	t.Parallel()
	tr := newTargetHealthTracker(3)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.record(targetKindMCP, "alpha", "", i%2 == 0, errors.New("x"), time.Millisecond, targetHealthSourceTraffic)
		}(i)
	}
	wg.Wait()
	_ = tr.stateOf(targetKindMCP, "alpha")
}

// --- TargetHealthConfig validation ---

func TestValidateTargetHealthConfig_Defaults(t *testing.T) {
	t.Parallel()
	thc, err := validateTargetHealthConfig(TargetHealthConfig{})
	require.NoError(t, err)
	assert.False(t, thc.enabled)
	assert.Equal(t, defaultTargetHealthProbeInterval, thc.probeInterval)
	assert.Equal(t, defaultTargetHealthFailureThreshold, thc.failureThreshold)
}

func TestValidateTargetHealthConfig_ExplicitEnable(t *testing.T) {
	t.Parallel()
	thc, err := validateTargetHealthConfig(TargetHealthConfig{Enabled: true})
	require.NoError(t, err)
	assert.True(t, thc.enabled)
}

func TestValidateTargetHealthConfig_ProbeIntervalBounds(t *testing.T) {
	t.Parallel()
	_, err := validateTargetHealthConfig(TargetHealthConfig{ProbeInterval: "1s"})
	assert.Error(t, err, "below the 10s floor must be rejected")

	_, err = validateTargetHealthConfig(TargetHealthConfig{ProbeInterval: "2h"})
	assert.Error(t, err, "above the 1h ceiling must be rejected")

	_, err = validateTargetHealthConfig(TargetHealthConfig{ProbeInterval: "not-a-duration"})
	assert.Error(t, err)

	thc, err := validateTargetHealthConfig(TargetHealthConfig{ProbeInterval: "30s"})
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, thc.probeInterval)
}

func TestValidateTargetHealthConfig_FailureThresholdBounds(t *testing.T) {
	t.Parallel()
	_, err := validateTargetHealthConfig(TargetHealthConfig{FailureThreshold: -1})
	assert.Error(t, err)

	_, err = validateTargetHealthConfig(TargetHealthConfig{FailureThreshold: maxTargetHealthFailureThreshold + 1})
	assert.Error(t, err)

	thc, err := validateTargetHealthConfig(TargetHealthConfig{FailureThreshold: 10})
	require.NoError(t, err)
	assert.Equal(t, 10, thc.failureThreshold)
}

func TestNewGateway_TargetHealth_InvalidConfig_ConstructionError(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.TargetHealth = TargetHealthConfig{ProbeInterval: "1s"}
	_, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	assert.Error(t, err)
}

func TestNewGateway_TargetHealth_DisabledByDefault_TrackerStillConstructed(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	require.NotNil(t, gw.targetHealth, "the tracker must always be constructed regardless of Enabled — passive recording/exposure stay on unconditionally")
	assert.False(t, gw.targetHealthCfg.enabled)
}

// --- maybeSweepTargetHealth: disabled / enabled / single-flight ---

func newTargetHealthSweepGateway(t *testing.T, enabled bool, probeInterval time.Duration, mcpURL string) *Gateway {
	t.Helper()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers:  map[string]*ProviderConfig{"openai": {}},
		MCPServers: map[string]*TargetConfig{"alpha": {URL: mcpURL}},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	return &Gateway{
		cfg:             cfg,
		registry:        reg,
		targetHealth:    newTargetHealthTracker(3),
		targetHealthCfg: targetHealthConfig{enabled: enabled, probeInterval: probeInterval, failureThreshold: 3},
		targetClient:    http.DefaultClient,
		targetTimeout:   5 * time.Second,
		name:            "test",
	}
}

func TestMaybeSweepTargetHealth_Disabled_NoOutboundRequests(t *testing.T) {
	t.Parallel()
	// F6, feat/target-health review: atomic even though a disabled sweep
	// must never actually invoke this handler — a regression that DID
	// invoke it would otherwise race this counter's read below under
	// -race, obscuring the real bug (an unexpected outbound probe) behind
	// a data-race report instead of the intended assert.Equal failure.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gw := newTargetHealthSweepGateway(t, false, 10*time.Millisecond, srv.URL)
	gw.maybeSweepTargetHealth()
	// maybeSweepTargetHealth spawns a goroutine when it decides to sweep;
	// since it must return immediately when disabled, there is nothing to
	// wait on — a brief sleep would only prove absence of a signal we
	// never expect to see.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "a disabled targetHealth must never send an outbound probe")
	assert.Equal(t, targetHealthUnknown, gw.targetHealth.stateOf(targetKindMCP, "alpha"))
}

func TestMaybeSweepTargetHealth_Enabled_ProbesConfiguredServer(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"federated","result":{"protocolVersion":"2025-11-25","capabilities":{}}}`))
	}))
	defer srv.Close()

	gw := newTargetHealthSweepGateway(t, true, time.Hour, srv.URL)
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return atomic.LoadInt32(&hits) > 0 }, time.Second)
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindMCP, "alpha") == targetHealthHealthy }, time.Second)

	snap := gw.targetHealth.snapshot(targetKindMCP, "alpha")
	assert.Equal(t, targetHealthSourceProbe, snap.source)
}

func TestMaybeSweepTargetHealth_ConcurrentTriggers_SingleFlight(t *testing.T) {
	t.Parallel()
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"federated","result":{}}`))
	}))
	// F6, feat/target-health review: registered in this order so
	// t.Cleanup's LIFO order unblocks the handler (closing release)
	// BEFORE srv.Close runs — httptest.Server.Close blocks until
	// outstanding requests complete, and the assertions below can
	// t.Fatalf/t.Errorf before ever reaching the explicit close(release)
	// further down; without this a failing assertion left the handler
	// permanently blocked and srv.Close hung until the whole test
	// binary's 10-minute timeout, rather than failing fast. unblock is
	// idempotent (sync.Once) since the happy path below also calls it
	// directly, before waiting on the sweep's resulting state.
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)

	gw := newTargetHealthSweepGateway(t, true, time.Hour, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.maybeSweepTargetHealth()
		}()
	}
	wg.Wait()

	// Give the single in-flight probe time to actually reach the server,
	// then release it — regardless of how many goroutines called
	// maybeSweepTargetHealth concurrently, at most one outbound request
	// must have been sent.
	waitForCondition(t, func() bool { return atomic.LoadInt32(&hits) > 0 }, time.Second)
	unblock()
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindMCP, "alpha") != targetHealthUnknown }, time.Second)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "concurrent triggers within one interval must produce exactly one sweep")
}

// TestMaybeSweepTargetHealth_IntervalEnforcedAcrossRapidCalls proves F3:
// the interval load (top of maybeSweepTargetHealth) and the CAS that wins
// the single-flight flag are not atomic with each other. Without a
// re-check under the CAS, a caller whose own interval read predates an
// entirely separate sweep's start-and-finish could still win the CAS
// once that sweep clears the flag, and spawn a second sweep inside the
// same interval — worst at startup, when every caller's first read sees
// last == 0. A fast-completing first sweep, immediately followed by a
// burst of concurrent callers, must still yield exactly one sweep within
// probeInterval; a call after the interval has genuinely elapsed must
// start a second one.
func TestMaybeSweepTargetHealth_IntervalEnforcedAcrossRapidCalls(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"federated","result":{}}`))
	}))
	defer srv.Close()

	probeInterval := 150 * time.Millisecond
	gw := newTargetHealthSweepGateway(t, true, probeInterval, srv.URL)

	// First sweep: the probe answers immediately, so this completes fast
	// and clears targetHealthSweeping well inside probeInterval.
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return atomic.LoadInt32(&hits) > 0 }, time.Second)
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindMCP, "alpha") != targetHealthUnknown }, time.Second)

	// A burst of concurrent callers immediately after — still well inside
	// probeInterval — must not start a second sweep.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.maybeSweepTargetHealth()
		}()
	}
	wg.Wait()
	time.Sleep(30 * time.Millisecond) // let any wrongly-spawned sweep reach the server
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want exactly 1 (a burst of concurrent callers inside probeInterval must not start a second sweep)", got)
	}

	// Once probeInterval has genuinely elapsed, a new call starts a
	// second sweep.
	time.Sleep(probeInterval)
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return atomic.LoadInt32(&hits) == 2 }, time.Second)
}

// TestMaybeSweepTargetHealth_LegacySSE_ClosesBodyWithoutReading proves a
// legacy "/sse" MCP server's probe uses a plain GET and closes the body
// immediately: a test server that never closes its stream must not hang
// the probe.
func TestMaybeSweepTargetHealth_LegacySSE_ClosesBodyWithoutReading(t *testing.T) {
	t.Parallel()
	headersSent := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(headersSent)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		<-release // never closes on its own; relies on the probe not reading
	}))
	// F6, feat/target-health review: previously relied on the PROBE'S OWN
	// 5s context timeout (targetTimeout below) to unblock this handler,
	// so srv.Close (deferred) silently added a real ~5s stall to every
	// run of this test. An explicit release channel, unblocked via
	// t.Cleanup BEFORE srv.Close (LIFO — see
	// TestMaybeSweepTargetHealth_ConcurrentTriggers_SingleFlight's own
	// comment for the identical ordering reasoning), makes this test
	// fast AND immune to a failing assertion ever leaving it blocked.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers:  map[string]*ProviderConfig{"openai": {}},
		MCPServers: map[string]*TargetConfig{"alpha": {URL: srv.URL + "/sse"}},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	gw := &Gateway{
		cfg:             cfg,
		registry:        reg,
		targetHealth:    newTargetHealthTracker(3),
		targetHealthCfg: targetHealthConfig{enabled: true, probeInterval: time.Hour, failureThreshold: 3},
		targetClient:    http.DefaultClient,
		targetTimeout:   5 * time.Second,
		name:            "test",
	}
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindMCP, "alpha") != targetHealthUnknown }, 2*time.Second)
	assert.Equal(t, targetHealthHealthy, gw.targetHealth.stateOf(targetKindMCP, "alpha"))
}

func TestMaybeSweepTargetHealth_AgentReturns404_ReadsHealthy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {}},
		Agents:    map[string]*AgentConfig{"beta": {URL: srv.URL}},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	gw := &Gateway{
		cfg:             cfg,
		registry:        reg,
		targetHealth:    newTargetHealthTracker(3),
		targetHealthCfg: targetHealthConfig{enabled: true, probeInterval: time.Hour, failureThreshold: 3},
		targetClient:    http.DefaultClient,
		targetTimeout:   5 * time.Second,
		name:            "test",
	}
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindAgent, "beta") != targetHealthUnknown }, time.Second)
	assert.Equal(t, targetHealthHealthy, gw.targetHealth.stateOf(targetKindAgent, "beta"), "a 404 agent-card fetch counts as reachable")
}

func TestMaybeSweepTargetHealth_AgentReturns503_RecordsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {}},
		Agents:    map[string]*AgentConfig{"beta": {URL: srv.URL}},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	gw := &Gateway{
		cfg:             cfg,
		registry:        reg,
		targetHealth:    newTargetHealthTracker(1),
		targetHealthCfg: targetHealthConfig{enabled: true, probeInterval: time.Hour, failureThreshold: 1},
		targetClient:    http.DefaultClient,
		targetTimeout:   5 * time.Second,
		name:            "test",
	}
	gw.maybeSweepTargetHealth()
	waitForCondition(t, func() bool { return gw.targetHealth.stateOf(targetKindAgent, "beta") != targetHealthUnknown }, time.Second)
	assert.Equal(t, targetHealthUnhealthy, gw.targetHealth.stateOf(targetKindAgent, "beta"))
}

// waitForCondition polls cond until it returns true or deadline elapses,
// failing the test on timeout — used throughout this file's sweep tests
// since the sweep itself runs in a background goroutine with no
// synchronous completion signal (maybeSweepTargetHealth's own doc
// comment: no timers, no long-lived goroutines, fire-and-forget).
func waitForCondition(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}
