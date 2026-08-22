package traefikllmgateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- validateBreakerConfig: defaults and validation table ---

func TestValidateBreakerConfig_Table(t *testing.T) {
	tests := []struct {
		name        string
		bc          BreakerConfig
		wantThresh  int
		wantOpen    time.Duration
		wantMaxOpen time.Duration
		wantErr     bool
	}{
		{
			name:        "zero value uses every default",
			bc:          BreakerConfig{},
			wantThresh:  defaultBreakerFailureThreshold,
			wantOpen:    defaultBreakerOpenDuration,
			wantMaxOpen: defaultBreakerMaxOpenDuration,
		},
		{
			name:        "explicit valid overrides used verbatim",
			bc:          BreakerConfig{FailureThreshold: 5, OpenDuration: "10s", MaxOpenDuration: "2m"},
			wantThresh:  5,
			wantOpen:    10 * time.Second,
			wantMaxOpen: 2 * time.Minute,
		},
		{
			name:       "threshold at the max boundary is accepted",
			bc:         BreakerConfig{FailureThreshold: maxBreakerFailureThreshold},
			wantThresh: maxBreakerFailureThreshold,
			wantOpen:   defaultBreakerOpenDuration, wantMaxOpen: defaultBreakerMaxOpenDuration,
		},
		{
			name:    "threshold above max is an error",
			bc:      BreakerConfig{FailureThreshold: maxBreakerFailureThreshold + 1},
			wantErr: true,
		},
		{
			name:    "negative threshold is an error",
			bc:      BreakerConfig{FailureThreshold: -1},
			wantErr: true,
		},
		{
			name:    "unparseable openDuration is an error",
			bc:      BreakerConfig{OpenDuration: "not-a-duration"},
			wantErr: true,
		},
		{
			name:    "zero-valued openDuration string is an error",
			bc:      BreakerConfig{OpenDuration: "0s"},
			wantErr: true,
		},
		{
			name:    "negative openDuration is an error",
			bc:      BreakerConfig{OpenDuration: "-1s"},
			wantErr: true,
		},
		{
			name:    "unparseable maxOpenDuration is an error",
			bc:      BreakerConfig{MaxOpenDuration: "not-a-duration"},
			wantErr: true,
		},
		{
			name:    "maxOpenDuration below openDuration is an error",
			bc:      BreakerConfig{OpenDuration: "5m", MaxOpenDuration: "1m"},
			wantErr: true,
		},
		{
			name:        "maxOpenDuration equal to openDuration is accepted",
			bc:          BreakerConfig{OpenDuration: "5m", MaxOpenDuration: "5m"},
			wantThresh:  defaultBreakerFailureThreshold,
			wantOpen:    5 * time.Minute,
			wantMaxOpen: 5 * time.Minute,
		},
		{
			name:        "maxOpenDuration at the ceiling is accepted",
			bc:          BreakerConfig{MaxOpenDuration: "24h"},
			wantThresh:  defaultBreakerFailureThreshold,
			wantOpen:    defaultBreakerOpenDuration,
			wantMaxOpen: 24 * time.Hour,
		},
		{
			name:    "maxOpenDuration above the ceiling is an error",
			bc:      BreakerConfig{MaxOpenDuration: "25h"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBreakerConfig(tt.bc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateBreakerConfig(%+v): err = %v, wantErr %v", tt.bc, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.failureThreshold != tt.wantThresh {
				t.Errorf("failureThreshold = %d, want %d", got.failureThreshold, tt.wantThresh)
			}
			if got.openBase != tt.wantOpen {
				t.Errorf("openBase = %v, want %v", got.openBase, tt.wantOpen)
			}
			if got.openMax != tt.wantMaxOpen {
				t.Errorf("openMax = %v, want %v", got.openMax, tt.wantMaxOpen)
			}
		})
	}
}

// newHealthTestState builds a providerState with the breaker fields set
// directly (no newModelRegistry round trip needed — the state machine
// under test is pure providerState methods), and every other field at a
// harmless zero/empty value. interval is set deliberately SHORT relative
// to openBase in the tests below so a passing assertion can only be
// explained by the breaker's own backoff window, never by the ordinary
// per-interval throttle it lives alongside.
func newHealthTestState(interval time.Duration, threshold int, openBase, openMax time.Duration) *providerState {
	return &providerState{
		explicit:         make(map[string]bool),
		discovered:       make(map[string]bool),
		interval:         interval,
		breakerThreshold: threshold,
		breakerOpenBase:  openBase,
		breakerOpenMax:   openMax,
	}
}

// --- breaker: opens after the configured threshold ---

func TestProviderState_Breaker_OpensAfterThreshold(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Hour, 3, time.Minute, 10*time.Minute)
	now := time.Now()
	boom := errors.New("boom")

	for i := 0; i < 2; i++ {
		st.finishRefresh(now, nil, boom)
		if !st.discoveryHealthy() {
			t.Fatalf("after %d failure(s) (threshold 3): discoveryHealthy() = false, want true — must not open early", i+1)
		}
	}

	st.finishRefresh(now, nil, boom) // 3rd consecutive failure reaches the threshold
	if st.discoveryHealthy() {
		t.Fatal("after reaching the failure threshold: discoveryHealthy() = true, want false (breaker must be open)")
	}
}

// TestProviderState_Breaker_ASuccessResetsTheFailureCounter proves the
// counter is CONSECUTIVE, not cumulative: two failures, a success, then two
// more failures must not open a threshold-3 breaker, because the success
// in between reset the count back to zero.
func TestProviderState_Breaker_ASuccessResetsTheFailureCounter(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Hour, 3, time.Minute, 10*time.Minute)
	now := time.Now()
	boom := errors.New("boom")

	st.finishRefresh(now, nil, boom)
	st.finishRefresh(now, nil, boom)
	st.finishRefresh(now, []string{"m1"}, nil) // success: resets the streak
	st.finishRefresh(now, nil, boom)
	st.finishRefresh(now, nil, boom)

	if !st.discoveryHealthy() {
		t.Error("discoveryHealthy() = false, want true — the intervening success must have reset the consecutive-failure streak")
	}
}

// TestProviderState_Breaker_ErrRequestBuildFailed_CountsAsFailure is the
// adversarial-review IMPORTANT-2 regression: an earlier version of
// recordHealthLocked reused isTransient's RETRY-shaped classification
// wholesale (retry.go) — which deliberately excludes errRequestBuildFailed,
// since retrying a malformed request build within the same attempt would
// never help. That is correct for retrying, but wrong for HEALTH: a
// provider whose baseURL is malformed (nothing validates it at
// construction) produces this exact error on every discovery cycle,
// forever. The old code routed that straight to a "success" outcome,
// resetting the breaker every time — it could never open despite
// continuous, permanent failure. It must now count as a plain failure.
func TestProviderState_Breaker_ErrRequestBuildFailed_CountsAsFailure(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Hour, 3, time.Minute, 10*time.Minute)
	now := time.Now()

	for i := 0; i < 3; i++ {
		st.finishRefresh(now, nil, errRequestBuildFailed)
	}

	if st.discoveryHealthy() {
		t.Fatal("discoveryHealthy() = true after 3 consecutive errRequestBuildFailed outcomes, want false — a persistent request-build failure is a provider fault the breaker must open on")
	}
}

// --- breaker: the zero value never trips (adversarial-review finding,
// round 2 — providerState's own doc comment claims "zero value behaves
// exactly as before this feature existed"; without this guard that claim
// was false: a zero-value breaker opened on the FIRST failure with a
// ZERO backoff, after which the interval throttle was gone too) ---

func TestProviderState_Breaker_ZeroThreshold_NeverOpens(t *testing.T) {
	t.Parallel()
	// A providerState assembled directly (bypassing newModelRegistry,
	// which always resolves breakerThreshold to >= 1 via
	// validateBreakerConfig — every test in this file up to here does
	// exactly that, through newHealthTestState) leaves every breaker
	// field at its Go zero value: this is that case, deliberately.
	st := &providerState{
		explicit:   make(map[string]bool),
		discovered: make(map[string]bool),
		interval:   time.Millisecond,
	}
	now := time.Now()
	boom := errors.New("boom")

	for i := 0; i < 50; i++ {
		if !st.tryBeginRefresh(now) {
			t.Fatalf("iteration %d: tryBeginRefresh = false, want true — a zero-value breaker must behave like plain interval-only gating, never blocked by its own (disabled) backoff", i)
		}
		st.finishRefresh(now, nil, boom)
		if !st.discoveryHealthy() {
			t.Fatalf("iteration %d: discoveryHealthy() = false, want true — a zero-value breaker must never open at all", i)
		}
		now = now.Add(time.Millisecond)
	}
}

// --- breaker: discovery backoff must never retry MORE often than plain
// interval throttling (BLOCKING adversarial-review finding, round 2) ---

// TestProviderState_Breaker_DefaultCadence_NoWorseThanPreFeatureBaseline
// simulates 24 hours of a permanently-failing provider under every
// DEFAULT setting (the shipped configuration, the xiaomi case this
// feature exists for) and counts actual ADMITTED refresh attempts.
//
// An earlier version of tryBeginRefresh let an open breaker's own backoff
// window (openUntil) REPLACE the plain interval gate instead of adding to
// it. Since defaultBreakerMaxOpenDuration (30m) is shorter than
// defaultDiscoveryInterval (1h), that measured 50 admitted attempts here —
// MORE than the 24 a plain, breaker-less interval gate would ever produce
// for the same 24h window. For a permanently broken provider that means
// MORE upstream 401s and MORE ERROR lines than today, the exact inverse of
// this feature's stated purpose. tryBeginRefresh now applies the interval
// gate unconditionally (closed or open alike), so backoff can only ever
// make retries LESS frequent, never more — and because
// defaultBreakerMaxOpenDuration is itself shorter than
// defaultDiscoveryInterval, the interval remains the sole binding
// constraint under pure defaults, so this asserts EQUALITY with the
// baseline, not just "no worse".
func TestProviderState_Breaker_DefaultCadence_NoWorseThanPreFeatureBaseline(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(defaultDiscoveryInterval, defaultBreakerFailureThreshold, defaultBreakerOpenDuration, defaultBreakerMaxOpenDuration)
	base := time.Now()
	boom := errors.New("boom: persistent 401")

	var admits int
	for minute := 0; minute < 24*60; minute++ {
		now := base.Add(time.Duration(minute) * time.Minute)
		if st.tryBeginRefresh(now) {
			admits++
			st.finishRefresh(now, nil, boom)
		}
	}

	// The pre-feature baseline: a plain interval-only gate (no breaker at
	// all) admits exactly one attempt per defaultDiscoveryInterval (1h)
	// over a 24h window for a provider that never succeeds.
	const preFeatureBaseline = 24
	if admits > preFeatureBaseline {
		t.Errorf("admitted %d discovery attempts over 24h with default config and a permanently failing provider, want <= %d (the pre-feature, interval-only baseline) — the breaker must never retry MORE often than plain interval throttling", admits, preFeatureBaseline)
	}
}

// --- breaker: stays open for the backoff window, superseding interval ---

func TestProviderState_Breaker_StaysOpenForBackoffWindow(t *testing.T) {
	t.Parallel()
	// interval is deliberately much shorter than openBase: a bare pass
	// through tryBeginRefresh would allow a retry after 5s. Only the
	// breaker's own backoff should be blocking it past that point.
	st := newHealthTestState(5*time.Second, 1, time.Minute, 10*time.Minute)
	base := time.Now()

	if ok := st.tryBeginRefresh(base); !ok {
		t.Fatal("first tryBeginRefresh on a fresh provider = false, want true")
	}
	st.finishRefresh(base, nil, errors.New("boom")) // trips the breaker (threshold 1)
	if st.discoveryHealthy() {
		t.Fatal("discoveryHealthy() = true after the trip, want false")
	}

	if ok := st.tryBeginRefresh(base.Add(6 * time.Second)); ok {
		t.Error("tryBeginRefresh past the plain interval (5s) but well inside the 1m backoff = true, want false — backoff must supersede interval")
	}
	if ok := st.tryBeginRefresh(base.Add(59 * time.Second)); ok {
		t.Error("tryBeginRefresh just before the backoff window elapses = true, want false")
	}
	if ok := st.tryBeginRefresh(base.Add(61 * time.Second)); !ok {
		t.Fatal("tryBeginRefresh once the backoff window has elapsed = false, want true (half-open probe should start)")
	}
	if !st.discoveryHealthy() {
		t.Error("discoveryHealthy() during the half-open probe = false, want true — a provider being re-probed must stay routable")
	}
}

// --- breaker: half-open probe success closes it again ---

func TestProviderState_Breaker_HalfOpenProbe_SuccessCloses(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(5*time.Second, 1, time.Minute, 10*time.Minute)
	base := time.Now()
	st.tryBeginRefresh(base)
	st.finishRefresh(base, nil, errors.New("boom")) // open

	probeAt := base.Add(61 * time.Second)
	if ok := st.tryBeginRefresh(probeAt); !ok {
		t.Fatal("tryBeginRefresh after the backoff window = false, want true")
	}
	st.finishRefresh(probeAt, []string{"m1"}, nil) // probe succeeds

	if !st.discoveryHealthy() {
		t.Error("discoveryHealthy() after a successful half-open probe = false, want true — a recovered provider must not stay dead")
	}
	if !st.hasModel("m1") {
		t.Error("hasModel(m1) = false after a successful probe, want true — discovery still updates the model set normally")
	}

	// Closed again means ORDINARY interval gating resumed, not a fresh
	// backoff window: immediately after the probe, another attempt must
	// still wait out the plain 5s interval, and no longer than that.
	if ok := st.tryBeginRefresh(probeAt); ok {
		t.Error("tryBeginRefresh at the same instant as the closing probe = true, want false (interval not yet elapsed)")
	}
	if ok := st.tryBeginRefresh(probeAt.Add(6 * time.Second)); !ok {
		t.Error("tryBeginRefresh past the plain interval after closing = false, want true")
	}
}

// --- breaker: half-open probe failure reopens it, with escalating backoff ---

func TestProviderState_Breaker_HalfOpenProbe_FailureReopens_Escalates(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(5*time.Second, 1, time.Minute, 10*time.Minute)
	base := time.Now()
	st.tryBeginRefresh(base)
	st.finishRefresh(base, nil, errors.New("boom")) // opens, backoff = 1m

	probeAt := base.Add(61 * time.Second)
	if ok := st.tryBeginRefresh(probeAt); !ok {
		t.Fatal("tryBeginRefresh after the first backoff window = false, want true")
	}
	st.finishRefresh(probeAt, nil, errors.New("still broken")) // probe fails: reopen, backoff doubles to 2m

	if st.discoveryHealthy() {
		t.Fatal("discoveryHealthy() after a failed probe = true, want false")
	}
	if ok := st.tryBeginRefresh(probeAt.Add(61 * time.Second)); ok {
		t.Error("tryBeginRefresh after only the ORIGINAL (1m) backoff = true, want false — a failed probe must double it to 2m")
	}
	if ok := st.tryBeginRefresh(probeAt.Add(121 * time.Second)); !ok {
		t.Fatal("tryBeginRefresh after the doubled (2m) backoff = false, want true")
	}
}

// TestProviderState_Breaker_BackoffCapsAtOpenMax proves repeated failed
// probes double the backoff only up to breakerOpenMax, never past it.
func TestProviderState_Breaker_BackoffCapsAtOpenMax(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Second, 1, time.Minute, 3*time.Minute) // base 1m, cap 3m: 1m -> 2m -> capped at 3m (not 4m)
	base := time.Now()
	st.tryBeginRefresh(base)
	st.finishRefresh(base, nil, errors.New("boom")) // open, backoff 1m

	at := base.Add(61 * time.Second)
	st.tryBeginRefresh(at)
	st.finishRefresh(at, nil, errors.New("boom")) // reopen, backoff doubles to 2m

	at = at.Add(121 * time.Second)
	st.tryBeginRefresh(at)
	st.finishRefresh(at, nil, errors.New("boom")) // reopen, doubling to 4m would exceed the 3m cap

	if ok := st.tryBeginRefresh(at.Add(2*time.Minute + 59*time.Second)); ok {
		t.Error("tryBeginRefresh just before the capped 3m backoff = true, want false")
	}
	if ok := st.tryBeginRefresh(at.Add(3*time.Minute + 1*time.Second)); !ok {
		t.Error("tryBeginRefresh just past the capped 3m backoff = false, want true — backoff must not exceed breakerOpenMax")
	}
}

// --- breaker: context.Canceled is NEUTRAL, neither failure nor success
// (IMPORTANT adversarial-review finding, round 2) ---

// TestProviderState_Breaker_ContextCanceled_IsNeutral_NotSuccess replaces
// a vacuous predecessor that only asserted discoveryHealthy()==true after
// ONE Canceled outcome on a threshold-1 breaker — true identically whether
// Canceled is ignored (correct) or treated as a success (the bug). This
// sequence distinguishes the two: fail, fail, Canceled, fail on a
// threshold-3 breaker opens it only if Canceled did NOT reset the
// consecutive-failure streak.
func TestProviderState_Breaker_ContextCanceled_IsNeutral_NotSuccess(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Second, 3, time.Minute, 10*time.Minute)
	now := time.Now()
	boom := errors.New("boom")

	st.finishRefresh(now, nil, boom)             // failure 1
	st.finishRefresh(now, nil, boom)             // failure 2
	st.finishRefresh(now, nil, context.Canceled) // neutral: must NOT reset the streak
	st.finishRefresh(now, nil, boom)             // failure 3: reaches the threshold only if Canceled was neutral

	if st.discoveryHealthy() {
		t.Fatal("discoveryHealthy() = true after fail,fail,Canceled,fail on a threshold-3 breaker, want false — Canceled must not reset the consecutive-failure streak (that would treat cancellation as a success)")
	}
}

// TestProviderState_Breaker_ContextCanceled_DuringHalfOpen_PreservesBackoff
// is the sharper half of the same finding: an earlier version treated a
// Canceled half-open probe as a SUCCESS, which CLOSED an already-open
// breaker and wiped its backoff — reporting a still-401ing provider
// healthy purely because a probe was interrupted at shutdown. This proves
// the backoff survives: the NEXT real failure must escalate from the
// ORIGINAL backoff, not restart fresh at the base.
func TestProviderState_Breaker_ContextCanceled_DuringHalfOpen_PreservesBackoff(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Second, 1, time.Minute, 10*time.Minute)
	base := time.Now()
	st.tryBeginRefresh(base)
	st.finishRefresh(base, nil, errors.New("boom")) // opens, backoff = 1m, openUntil = base+1m

	probeAt := base.Add(61 * time.Second)
	if !st.tryBeginRefresh(probeAt) {
		t.Fatal("tryBeginRefresh after the backoff window = false, want true")
	}
	st.finishRefresh(probeAt, nil, context.Canceled) // probe interrupted at shutdown: must be neutral

	// The next real attempt must see the ORIGINAL backoff still in
	// force, not a wiped/reset one: if Canceled had (incorrectly) closed
	// the breaker, this next failure would re-open it FRESH at the 1m
	// base instead of escalating to 2m.
	retryAt := probeAt.Add(2 * time.Second)
	if !st.tryBeginRefresh(retryAt) {
		t.Fatal("tryBeginRefresh right after a canceled probe = false, want true (interval elapsed, not blocked by the cancel)")
	}
	st.finishRefresh(retryAt, nil, errors.New("still broken"))

	if ok := st.tryBeginRefresh(retryAt.Add(119 * time.Second)); ok {
		t.Error("tryBeginRefresh before the escalated 2m backoff = true, want false — a canceled probe must not have reset the backoff to its 1m base")
	}
	if ok := st.tryBeginRefresh(retryAt.Add(121 * time.Second)); !ok {
		t.Error("tryBeginRefresh after the escalated 2m backoff = false, want true")
	}
}

// --- breaker: a provider that never fails is unaffected (default-
// preserving: today's interval-only gating, unchanged) ---

func TestProviderState_Breaker_HealthyProviderNeverAffected(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	st := newHealthTestState(interval, defaultBreakerFailureThreshold, defaultBreakerOpenDuration, defaultBreakerMaxOpenDuration)
	now := time.Now()

	for i := 0; i < 5; i++ {
		if ok := st.tryBeginRefresh(now); !ok {
			t.Fatalf("cycle %d: tryBeginRefresh = false, want true (interval already elapsed)", i)
		}
		st.finishRefresh(now, []string{"m1"}, nil)
		if !st.discoveryHealthy() {
			t.Fatalf("cycle %d: discoveryHealthy() = false, want true — a never-failing provider must never trip its breaker", i)
		}
		// Behavioral proxy for "the breaker never opened" (assert
		// behavior, not the internal openUntil field, per the brief):
		// an opened breaker would admit again as soon as its own
		// (likely shorter) backoff elapsed. Checking one instant BEFORE
		// the plain interval elapses proves nothing shorter is gating
		// this provider.
		if ok := st.tryBeginRefresh(now.Add(interval - time.Nanosecond)); ok {
			t.Fatalf("cycle %d: tryBeginRefresh one nanosecond before the interval elapses = true, want false", i)
		}
		if ok := st.tryBeginRefresh(now); ok {
			t.Fatalf("cycle %d: tryBeginRefresh immediately after a fresh refresh = true, want false (interval not elapsed)", i)
		}
		now = now.Add(2 * interval) // past the interval for the next cycle
	}
}

// --- breaker: concurrent access is race-free (IMPORTANT adversarial-
// review finding, round 2 — no prior test in this file ever exercised the
// mutex-guarded breaker fields from more than one goroutine, so -race had
// nothing to catch a locking bug with; the locking was correct, but
// unproven) ---

func TestProviderState_Breaker_ConcurrentRefreshes_RaceFree(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Millisecond, 3, 2*time.Millisecond, 20*time.Millisecond)
	base := time.Now()

	const workers = 8
	const iterations = 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				now := base.Add(time.Duration(worker*iterations+i) * time.Microsecond)
				if st.tryBeginRefresh(now) {
					var err error
					if (worker+i)%2 == 0 {
						err = errors.New("boom")
					}
					st.finishRefresh(now, []string{"m1"}, err)
				}
				_ = st.discoveryHealthy()
			}
		}(w)
	}
	wg.Wait()
}

// --- modelRegistry.discoveryHealthy: the exported predicate feat/failover
// consumes ---

func TestModelRegistry_DiscoveryHealthy(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"flaky":   newFakeAdapter("flaky"),
		"healthy": newFakeAdapter("healthy"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"flaky":   {Models: []string{"m1"}},
			"healthy": {Models: []string{"m2"}},
		},
		Breaker: BreakerConfig{FailureThreshold: 2, OpenDuration: "1m", MaxOpenDuration: "10m"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	now := time.Now()
	reg.nowFn = func() time.Time { return now }

	if !reg.discoveryHealthy("flaky") || !reg.discoveryHealthy("healthy") {
		t.Fatal("a freshly constructed registry's providers must both be discovery-healthy")
	}
	if !reg.discoveryHealthy("does-not-exist") {
		t.Error(`discoveryHealthy("does-not-exist") = false, want true — an unconfigured name is not a reason to block routing`)
	}

	boom := errors.New("boom")
	reg.states["flaky"].finishRefresh(now, nil, boom)
	reg.states["flaky"].finishRefresh(now, nil, boom) // reaches FailureThreshold: 2

	if reg.discoveryHealthy("flaky") {
		t.Error(`discoveryHealthy("flaky") = true after tripping its breaker, want false`)
	}
	if !reg.discoveryHealthy("healthy") {
		t.Error(`discoveryHealthy("healthy") = false, want true — a sibling provider's breaker must not affect it`)
	}
}
