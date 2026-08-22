package traefikllmgateway

import (
	"context"
	"errors"
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
		if !st.usable() {
			t.Fatalf("after %d failure(s) (threshold 3): usable() = false, want true — must not open early", i+1)
		}
	}

	st.finishRefresh(now, nil, boom) // 3rd consecutive failure reaches the threshold
	if st.usable() {
		t.Fatal("after reaching the failure threshold: usable() = true, want false (breaker must be open)")
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

	if !st.usable() {
		t.Error("usable() = false, want true — the intervening success must have reset the consecutive-failure streak")
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
	if st.usable() {
		t.Fatal("usable() = true after the trip, want false")
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
	if !st.usable() {
		t.Error("usable() during the half-open probe = false, want true — a provider being re-probed must stay routable")
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

	if !st.usable() {
		t.Error("usable() after a successful half-open probe = false, want true — a recovered provider must not stay dead")
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

	if st.usable() {
		t.Fatal("usable() after a failed probe = true, want false")
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

// --- breaker: a context.Canceled discovery error is not the provider's
// fault, and must not count toward the failure threshold (reuses limits.go's
// isTransient/isDeadlineExceeded classification, which draws this same
// line for request-path attempts) ---

func TestProviderState_Breaker_ContextCanceled_NeverCountsAsFailure(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Hour, 1, time.Minute, 10*time.Minute)
	now := time.Now()

	st.finishRefresh(now, nil, context.Canceled)
	if !st.usable() {
		t.Error("usable() = false after a context.Canceled discovery error, want true — a canceled attempt is not a provider fault")
	}
}

// --- breaker: a provider that never fails is unaffected (default-
// preserving: today's interval-only gating, unchanged) ---

func TestProviderState_Breaker_HealthyProviderNeverAffected(t *testing.T) {
	t.Parallel()
	st := newHealthTestState(time.Hour, defaultBreakerFailureThreshold, defaultBreakerOpenDuration, defaultBreakerMaxOpenDuration)
	now := time.Now()

	for i := 0; i < 5; i++ {
		if ok := st.tryBeginRefresh(now); !ok {
			t.Fatalf("cycle %d: tryBeginRefresh = false, want true (interval already elapsed)", i)
		}
		st.finishRefresh(now, []string{"m1"}, nil)
		if !st.usable() {
			t.Fatalf("cycle %d: usable() = false, want true — a never-failing provider must never trip its breaker", i)
		}
		if !st.openUntil.IsZero() {
			t.Fatalf("cycle %d: openUntil = %v, want zero — breaker must never have opened", i, st.openUntil)
		}
		// Immediately after: interval-gated exactly like before this
		// feature existed, no breaker-induced extra wait.
		if ok := st.tryBeginRefresh(now); ok {
			t.Fatalf("cycle %d: tryBeginRefresh immediately after a fresh refresh = true, want false (interval not elapsed)", i)
		}
		now = now.Add(2 * time.Hour) // past the interval for the next cycle
	}
}

// --- modelRegistry.providerUsable: the exported predicate feat/failover
// consumes ---

func TestModelRegistry_ProviderUsable(t *testing.T) {
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

	if !reg.providerUsable("flaky") || !reg.providerUsable("healthy") {
		t.Fatal("a freshly constructed registry's providers must both be usable")
	}
	if !reg.providerUsable("does-not-exist") {
		t.Error(`providerUsable("does-not-exist") = false, want true — an unconfigured name is not a reason to block routing`)
	}

	boom := errors.New("boom")
	reg.states["flaky"].finishRefresh(now, nil, boom)
	reg.states["flaky"].finishRefresh(now, nil, boom) // reaches FailureThreshold: 2

	if reg.providerUsable("flaky") {
		t.Error(`providerUsable("flaky") = true after tripping its breaker, want false`)
	}
	if !reg.providerUsable("healthy") {
		t.Error(`providerUsable("healthy") = false, want true — a sibling provider's breaker must not affect it`)
	}
}
