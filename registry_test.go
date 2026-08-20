package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAdapter is a providerAdapter test double whose listModels behavior is
// scripted per test via listModelsFn. Its other methods are never exercised
// by registry.go, so they return harmless zero values.
type fakeAdapter struct {
	listModelsFn func(context.Context) ([]string, error)
	adapterName  string
}

func newFakeAdapter(name string) *fakeAdapter {
	return &fakeAdapter{adapterName: name}
}

func (f *fakeAdapter) name() string             { return f.adapterName }
func (f *fakeAdapter) typeName() string         { return "fake" }
func (f *fakeAdapter) base() string             { return "" }
func (f *fakeAdapter) injectAuth(*http.Request) {}
func (f *fakeAdapter) chatCompletion(context.Context, http.ResponseWriter, map[string]any) (usage, error) {
	return usage{}, nil
}
func (f *fakeAdapter) embeddings(context.Context, http.ResponseWriter, map[string]any) (usage, error) {
	return usage{}, nil
}
func (f *fakeAdapter) imagesGeneration(context.Context, http.ResponseWriter, map[string]any) (usage, error) {
	return usage{}, nil
}
func (f *fakeAdapter) audioSpeech(context.Context, http.ResponseWriter, []byte, string) (usage, error) {
	return usage{}, nil
}
func (f *fakeAdapter) audioTranscription(context.Context, http.ResponseWriter, []byte, string) (usage, error) {
	return usage{}, nil
}
func (f *fakeAdapter) listModels(ctx context.Context) ([]string, error) {
	if f.listModelsFn == nil {
		return nil, nil
	}
	return f.listModelsFn(ctx)
}
func (f *fakeAdapter) httpClient() *http.Client { return nil }

// recordingLog is a thread-safe log func for tests, capturing formatted
// lines instead of writing to stderr.
type recordingLog struct {
	lines []string
	mu    sync.Mutex
}

func (r *recordingLog) fn(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLog) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// waitUntil polls cond every millisecond until it returns true, failing the
// test if timeout elapses first. Used to synchronize against background
// refresh goroutines without a fixed sleep.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

// allowAllGroup has no provider/model restrictions.
func allowAllGroup() *group { return &group{name: "all"} }

// --- resolve: prefix vs bare-id disambiguation (ruling a) ---

func TestModelRegistry_Resolve_ProviderPrefixed_ResolvesDirect(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	adapter, upstreamModel, canonical, err := reg.resolve("openai/gpt-test", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["openai"] {
		t.Errorf("adapter = %v, want openai adapter", adapter)
	}
	if upstreamModel != "gpt-test" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "gpt-test")
	}
	if canonical != "openai/gpt-test" {
		t.Errorf("canonical = %q, want %q", canonical, "openai/gpt-test")
	}
}

// TestModelRegistry_Resolve_BareIDContainingSlash_UnconfiguredPrefix_ResolvesAsBareID
// is the real-case regression from ruling (a): an upstream model id
// ("uni/deepseek-v4-flash-0731") that itself contains a slash must resolve
// as a bare id against whichever configured provider lists it, because
// "uni" itself is not a configured provider name.
func TestModelRegistry_Resolve_BareIDContainingSlash_UnconfiguredPrefix_ResolvesAsBareID(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openai": newFakeAdapter("openai"),
		"acme":   newFakeAdapter("acme"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"openai": {Models: []string{"gpt-test"}},
		"acme":   {Models: []string{"uni/deepseek-v4-flash-0731"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	const id = "uni/deepseek-v4-flash-0731"
	adapter, upstreamModel, canonical, err := reg.resolve(id, allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["acme"] {
		t.Errorf("adapter = %v, want acme adapter", adapter)
	}
	if upstreamModel != id {
		t.Errorf("upstreamModel = %q, want unchanged %q", upstreamModel, id)
	}
	if canonical != "acme/"+id {
		t.Errorf("canonical = %q, want %q", canonical, "acme/"+id)
	}
}

func TestModelRegistry_Resolve_UnknownModel_ReturnsErrModelUnknown(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	if _, _, _, err := reg.resolve("does-not-exist", allowAllGroup()); err != errModelUnknown {
		t.Errorf("err = %v, want errModelUnknown", err)
	}
}

func TestModelRegistry_Resolve_EmptyProviderModelSet_ResolvesNothing(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {}}} // no explicit models, discovery off
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	if _, _, _, err := reg.resolve("openai/anything", allowAllGroup()); err != errModelUnknown {
		t.Errorf("err = %v, want errModelUnknown", err)
	}
	if _, _, _, err := reg.resolve("anything", allowAllGroup()); err != errModelUnknown {
		t.Errorf("err = %v, want errModelUnknown", err)
	}
}

// --- resolve: authorization pairing (ruling c) ---

func TestModelRegistry_Resolve_KnownModel_ProviderDenied_ReturnsErrModelDenied(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openai":    newFakeAdapter("openai"),
		"anthropic": newFakeAdapter("anthropic"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"openai":    {Models: []string{"gpt-test"}},
		"anthropic": {Models: []string{"claude-x"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	// providers:[openai] restricts the provider; models:[claude-*] would
	// match claude-x on its own — the provider gate must still deny it.
	grp := &group{name: "openai-only", providers: []string{"openai"}, models: []string{"claude-*"}}
	if _, _, _, err := reg.resolve("anthropic/claude-x", grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied", err)
	}
}

func TestModelRegistry_Resolve_KnownModel_ModelGlobDenied_ReturnsErrModelDenied(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "narrow", providers: []string{"openai"}, models: []string{"gpt-4*"}}
	if _, _, _, err := reg.resolve("openai/gpt-test", grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied", err)
	}
}

func TestModelRegistry_Resolve_AllowedByBothProviderAndModel_Succeeds(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "ok", providers: []string{"openai"}, models: []string{"gpt-*"}}
	if _, _, _, err := reg.resolve("openai/gpt-test", grp); err != nil {
		t.Errorf("resolve: %v, want success", err)
	}
}

// --- resolve: exact-glob model authorization, no spurious prefix-strip
// (fix(registry) ruling 3) ---

// TestModelRegistry_Resolve_BareIDWithSlash_ModelGlobMustNotMatchViaSpuriousStrip
// is the regression: allowsModel used to strip at any first slash, so a
// pattern like "deepseek-*" would spuriously match a bare model id that
// merely contains a slash — an upstream's own "uni/deepseek-v4-flash-0731"
// naming, where "uni" is not a configured provider and there is no
// legitimate prefix to strip at all. resolveAgainst must not invent that
// candidate for a request that was never provider-prefixed in the first
// place.
func TestModelRegistry_Resolve_BareIDWithSlash_ModelGlobMustNotMatchViaSpuriousStrip(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"acme": newFakeAdapter("acme")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"acme": {Models: []string{"uni/deepseek-v4-flash-0731"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	const id = "uni/deepseek-v4-flash-0731"
	grp := &group{name: "narrow", models: []string{"deepseek-*"}}
	if _, _, _, err := reg.resolve(id, grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied (deepseek-* must not spuriously match via a bare-suffix strip)", err)
	}

	// Positive control: a pattern that legitimately targets the full bare
	// id, slash included, still matches via plain exact-glob.
	grp.models = []string{"uni/deepseek*"}
	if _, _, _, err := reg.resolve(id, grp); err != nil {
		t.Errorf("resolve with a full-string-matching pattern: %v, want success", err)
	}
}

// TestModelRegistry_Resolve_ProviderPrefixed_ModelGlobMatchesViaBareCandidate
// is the positive half of the same ruling: a provider-prefixed request's
// bare suffix is still checked against the model glob — resolveAgainst
// generates that candidate itself now that allowsModel no longer strips.
func TestModelRegistry_Resolve_ProviderPrefixed_ModelGlobMatchesViaBareCandidate(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "narrow", models: []string{"gpt-*"}}
	if _, _, _, err := reg.resolve("openai/gpt-test", grp); err != nil {
		t.Errorf("resolve: %v, want success (gpt-* must match via the bare-suffix candidate)", err)
	}
}

// --- resolve: collision precedence (ruling g) ---

func TestModelRegistry_Resolve_BareIDCollision_PicksSortedFirstProvider(t *testing.T) {
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
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	adapter, _, canonical, err := reg.resolve("shared", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["alpha"] {
		t.Errorf("adapter = %v, want alpha (sorted-first)", adapter)
	}
	if canonical != "alpha/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "alpha/shared")
	}
}

func TestModelRegistry_Resolve_ProviderPrefixed_AlwaysReachesLosingProvider(t *testing.T) {
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
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	adapter, _, canonical, err := reg.resolve("beta/shared", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["beta"] {
		t.Errorf("adapter = %v, want beta (explicit form bypasses collision winner)", adapter)
	}
	if canonical != "beta/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "beta/shared")
	}
}

// --- newModelRegistry construction ---

func TestNewModelRegistry_InvalidDiscoveryInterval_ReturnsConstructorError(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {DiscoveryInterval: "not-a-duration"}}}
	if _, err := newModelRegistry(adapters, cfg, func(string, ...any) {}); err == nil {
		t.Fatal("want constructor error for an invalid discoveryInterval")
	}
}

func TestNewModelRegistry_EmptyDiscoveryInterval_DefaultsOneHour(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	if got := reg.states["openai"].interval; got != defaultDiscoveryInterval {
		t.Errorf("interval = %v, want %v", got, defaultDiscoveryInterval)
	}
}

// --- warmFill: synchronous initial discovery (ruling e) ---

func TestModelRegistry_WarmFill_MergesExplicitAndDiscovered(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) { return []string{"gpt-b", "gpt-a"}, nil }
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-a"}, Discovery: true}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.warmFill(context.Background())

	got := reg.states["openai"].knownIDs()
	want := []string{"gpt-a", "gpt-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("knownIDs = %v, want %v", got, want)
	}
}

func TestModelRegistry_WarmFill_DiscoveryError_NonFatal_LogsAndLeavesExplicitModels(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) { return nil, fmt.Errorf("upstream down") }
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-a"}, Discovery: true}}}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.warmFill(context.Background())

	if !reg.states["openai"].hasModel("gpt-a") {
		t.Error("want explicit model gpt-a still known after a failed initial discovery fetch")
	}
	if rl.count() == 0 {
		t.Error("want the discovery failure logged")
	}
}

func TestModelRegistry_WarmFill_NonDiscoveryProvider_NeverCallsListModels(t *testing.T) {
	t.Parallel()
	var calls int32
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-a"}}}} // Discovery left false
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.warmFill(context.Background())

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("listModels called %d times, want 0 for a non-discovery provider", got)
	}
}

// TestModelRegistry_WarmFill_EachProviderGetsFullBudget_NotSharedAcrossProviders
// is the regression for llmgateway.go's old outer 5s wrap around the whole
// warmFill call (fix(registry) ruling 2): that outer wrap made
// warmFillTimeout a total budget shared across every discovery-enabled
// provider instead of a fresh one per provider, so a slow first provider
// ate into a later provider's deadline and could leave it resolving
// nothing for the whole first interval. warmFill derives each provider's
// timeout via context.WithTimeout(ctx, warmFillTimeout) fresh per
// iteration; this proves that yields close to the full budget for a later
// provider even after an earlier one took real wall-clock time, as long as
// ctx itself (as llmgateway.go now passes it) carries no deadline of its
// own.
func TestModelRegistry_WarmFill_EachProviderGetsFullBudget_NotSharedAcrossProviders(t *testing.T) {
	t.Parallel()
	slow := newFakeAdapter("a-slow")
	slow.listModelsFn = func(context.Context) ([]string, error) {
		time.Sleep(1200 * time.Millisecond) // wall-clock cost, not budget shared with b-fast
		return []string{"slow-model"}, nil
	}
	var fastRemaining time.Duration
	fast := newFakeAdapter("b-fast")
	fast.listModelsFn = func(ctx context.Context) ([]string, error) {
		if dl, ok := ctx.Deadline(); ok {
			fastRemaining = time.Until(dl)
		}
		return []string{"fast-model"}, nil
	}
	adapters := map[string]providerAdapter{"a-slow": slow, "b-fast": fast}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		// Sorted provider-name order drives warmFill's iteration: "a-slow"
		// runs before "b-fast" for this test to be meaningful.
		"a-slow": {Discovery: true},
		"b-fast": {Discovery: true},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	// Mirrors llmgateway.go's fixed call site: an unbounded parent ctx, not
	// one pre-wrapped to warmFillTimeout.
	reg.warmFill(context.Background())

	if fastRemaining < 4*time.Second {
		t.Errorf("b-fast's received context deadline was %v from expiry, want >4s (its 5s warm-fill budget must not be reduced by a-slow's 1.2s elapsed time)", fastRemaining)
	}
}

// --- maybeRefresh: throttling, in-flight guard, stale-while-error ---

func TestModelRegistry_MaybeRefresh_Throttled_OnePerInterval(t *testing.T) {
	t.Parallel()
	var calls int32
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return []string{"m1"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true, DiscoveryInterval: "1h"}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	now := time.Now()
	reg.nowFn = func() time.Time { return now }

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return atomic.LoadInt32(&calls) == 1 })

	// Still within the same interval: a second entry must not refetch.
	reg.maybeRefresh(context.Background())
	time.Sleep(10 * time.Millisecond) // give a wrongly-spawned goroutine a chance to run
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("listModels called %d times within one interval, want 1", got)
	}

	now = now.Add(2 * time.Hour) // past the interval: next entry should refetch
	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return atomic.LoadInt32(&calls) == 2 })
}

// TestModelRegistry_MaybeRefresh_InFlightGuard_BlocksConcurrentFetch proves
// the guard is the inFlight flag, not just the interval check: the first
// fetch is held open (lastRefresh never advances while it's in flight), so
// the interval check alone would spuriously allow a second concurrent
// fetch. Only the inFlight flag prevents it.
func TestModelRegistry_MaybeRefresh_InFlightGuard_BlocksConcurrentFetch(t *testing.T) {
	t.Parallel()
	var calls int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-release
		return []string{"m1"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true, DiscoveryInterval: "1h"}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	now := time.Now()
	reg.nowFn = func() time.Time { return now }

	reg.maybeRefresh(context.Background()) // spawns the in-flight fetch, synchronously marks inFlight
	reg.maybeRefresh(context.Background()) // must be a no-op: inFlight already true

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh never started")
	}
	select {
	case <-started:
		t.Fatal("a second concurrent fetch started while the first was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	waitUntil(t, time.Second, func() bool { return atomic.LoadInt32(&calls) == 1 && reg.states["openai"].hasModel("m1") })
}

// TestModelRegistry_RefreshProvider_AdapterPanics_RecoversAndReleasesInFlight
// is the regression for the missing recover() in refreshProvider (fix
// (registry) ruling 1): refreshProvider's "go" statement is the plugin's
// only background goroutine, running off any request's stack. An
// unrecovered panic there would crash the whole Traefik process, not just
// fail one refresh — there is no ServeHTTP caller above it to catch it.
// Reaching the end of this test at all is part of the proof: if recover()
// were missing, the panicking listModels call below would have already
// crashed the test binary.
func TestModelRegistry_RefreshProvider_AdapterPanics_RecoversAndReleasesInFlight(t *testing.T) {
	t.Parallel()
	var calls int32
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			panic("boom: adapter blew up")
		}
		return []string{"m1"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true, DiscoveryInterval: "1h"}}}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	now := time.Now()
	reg.nowFn = func() time.Time { return now }

	reg.maybeRefresh(context.Background())
	// The log write happens strictly after finishRefresh inside
	// refreshProvider's deferred recover handler (both under the same
	// goroutine, in that order), so waiting for it also proves inFlight was
	// released and lastRefresh advanced — not merely that the panic
	// occurred. recordingLog's own mutex gives this a proper happens-before
	// edge with the write, so this is race-safe without touching
	// providerState's fields directly.
	waitUntil(t, time.Second, func() bool { return rl.count() > 0 })
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("listModels called %d times, want exactly 1 before the panic", got)
	}

	// The guard was released, not left stuck true: a second attempt after
	// the interval elapses retries rather than being throttled forever.
	now = now.Add(2 * time.Hour)
	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return reg.states["openai"].hasModel("m1") })
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("listModels called %d times after the retry window, want 2", got)
	}
}

func TestModelRegistry_MaybeRefresh_DiscoveryError_KeepsPreviousDiscoveredSet(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		if fail.Load() {
			return nil, fmt.Errorf("upstream unreachable")
		}
		return []string{"m1"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true, DiscoveryInterval: "1h"}}}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warmFill(context.Background()) // succeeds, populates discovered={m1}
	if !reg.states["openai"].hasModel("m1") {
		t.Fatal("warmFill did not populate m1")
	}

	now := time.Now().Add(2 * time.Hour)
	reg.nowFn = func() time.Time { return now }
	fail.Store(true)
	before := rl.count()

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return rl.count() > before })

	if !reg.states["openai"].hasModel("m1") {
		t.Error("want the previous discovered set kept after a failed refresh (stale-while-error)")
	}
}

// --- listFor: group filtering, sorting, collision presentation (ruling h) ---

func TestModelRegistry_ListFor_GroupFiltered(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openai":    newFakeAdapter("openai"),
		"anthropic": newFakeAdapter("anthropic"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"openai":    {Models: []string{"gpt-test"}},
		"anthropic": {Models: []string{"claude-x"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "openai-only", providers: []string{"openai"}}
	got := reg.listFor(grp)
	if len(got) != 1 {
		t.Fatalf("listFor = %v, want exactly 1 entry", got)
	}
	if got[0]["id"] != "gpt-test" || got[0]["owned_by"] != "openai" || got[0]["object"] != "model" {
		t.Errorf("entry = %v, want {id:gpt-test, owned_by:openai, object:model}", got[0])
	}
}

func TestModelRegistry_ListFor_SortedByID(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"zeta", "alpha", "mu"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	got := reg.listFor(allowAllGroup())
	if len(got) != 3 {
		t.Fatalf("listFor = %v, want 3 entries", got)
	}
	ids := []string{got[0]["id"].(string), got[1]["id"].(string), got[2]["id"].(string)}
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids = %v, want %v", ids, want)
			break
		}
	}
}

func TestModelRegistry_ListFor_Collision_ListsBareWinnerPlusAllPrefixedForms(t *testing.T) {
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
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	got := reg.listFor(allowAllGroup())
	byID := make(map[string]string, len(got))
	for _, e := range got {
		byID[e["id"].(string)] = e["owned_by"].(string)
	}
	want := map[string]string{"shared": "alpha", "alpha/shared": "alpha", "beta/shared": "beta"}
	if len(byID) != len(want) {
		t.Fatalf("listFor = %v, want exactly %v", byID, want)
	}
	for id, ownedBy := range want {
		if byID[id] != ownedBy {
			t.Errorf("entry %q owned_by = %q, want %q", id, byID[id], ownedBy)
		}
	}
}

// TestModelRegistry_ListFor_Collision_LogsWarningOnceRegardlessOfCallCount
// covers ruling (g)'s "log warn once per colliding id": repeated listFor
// calls over the same collision must not re-emit the warning.
func TestModelRegistry_ListFor_Collision_LogsWarningOnceRegardlessOfCallCount(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"beta":  {Models: []string{"shared"}},
		"alpha": {Models: []string{"shared"}},
	}}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.listFor(allowAllGroup())
	reg.listFor(allowAllGroup())
	reg.listFor(allowAllGroup())

	if got := rl.count(); got != 1 {
		t.Errorf("collision log emitted %d times across 3 listFor calls, want exactly 1", got)
	}
}

func TestModelRegistry_ListFor_Collision_LosingProviderStillReachableViaPrefixedForm(t *testing.T) {
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
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "beta-only", providers: []string{"beta"}}
	got := reg.listFor(grp)
	if len(got) != 1 {
		t.Fatalf("listFor = %v, want exactly 1 entry", got)
	}
	if got[0]["id"] != "beta/shared" || got[0]["owned_by"] != "beta" {
		t.Errorf("entry = %v, want {id:beta/shared, owned_by:beta}", got[0])
	}
}

// --- end-to-end: GET /v1/models through Gateway.ServeHTTP ---

func TestGateway_ServeHTTP_GetV1Models_ReturnsGroupFilteredList(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai":    {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}},
		"anthropic": {Type: "anthropic", APIKey: "k", Models: []string{"claude-x"}},
	}
	cfg.Groups = map[string]*GroupConfig{"openai-only": {Providers: []string{"openai"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "openai-only", APIKey: "sk-alice"}}}

	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want %q", body.Object, "list")
	}
	if len(body.Data) != 1 || body.Data[0]["id"] != "gpt-test" || body.Data[0]["owned_by"] != "openai" {
		t.Errorf("data = %v, want exactly the openai gpt-test entry", body.Data)
	}
}

func TestGateway_ServeHTTP_GetV1Models_Unauthenticated_Returns401(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}

	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Type == "" {
		t.Error("want a non-empty error.type in the 401 envelope")
	}
}

// TestNewGateway_WarmFill_PopulatesDiscoveredModels_BeforeAnyRequest proves
// newGateway's synchronous first-fill wiring: a discovery-enabled
// provider's models are known immediately after New returns, with no
// request ever sent.
func TestNewGateway_WarmFill_PopulatesDiscoveredModels_BeforeAnyRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"disco-model"}]}`))
	}))
	t.Cleanup(srv.Close)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "k", BaseURL: srv.URL, Discovery: true},
	}
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	if !gw.registry.states["openai"].hasModel("disco-model") {
		t.Fatal("want disco-model known immediately after New returns (synchronous warm fill)")
	}
}

// --- model aliases (spec §5, v0.2) ---

// newAliasTestRegistry builds a registry with one "openai" provider
// (explicit model "gpt-test") and the given aliases, failing the test on
// any construction error. log defaults to a no-op when nil.
func newAliasTestRegistry(t *testing.T, aliases map[string]string, log func(string, ...any)) *modelRegistry {
	t.Helper()
	if log == nil {
		log = func(string, ...any) {}
	}
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}},
		ModelAliases: aliases,
	}
	reg, err := newModelRegistry(adapters, cfg, log)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	return reg
}

// TestNewModelRegistry_ModelAliases_ValidationTable covers every row of
// spec §5's validation table, plus a positive control.
func TestNewModelRegistry_ModelAliases_ValidationTable(t *testing.T) {
	t.Parallel()
	baseProviders := map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}

	tests := []struct {
		aliases     map[string]string
		name        string
		wantErrText string // substring expected in the error; "" means construction must succeed
	}{
		{
			name:        "empty alias id",
			aliases:     map[string]string{"": "gpt-test"},
			wantErrText: "invalid",
		},
		{
			name:        "control character in alias id",
			aliases:     map[string]string{"bad\x7falias": "gpt-test"},
			wantErrText: "invalid",
		},
		{
			name:        "alias prefix shadows a configured provider",
			aliases:     map[string]string{"openai/special": "gpt-test"},
			wantErrText: "shadowed",
		},
		{
			name:        "alias collides with an explicit model id",
			aliases:     map[string]string{"gpt-test": "openai/gpt-test"},
			wantErrText: "collides",
		},
		{
			name:        "alias chain: target is itself an alias",
			aliases:     map[string]string{"aliased/a": "aliased/b", "aliased/b": "openai/gpt-test"},
			wantErrText: "itself an alias",
		},
		{
			name:        "empty target",
			aliases:     map[string]string{"aliased/x": ""},
			wantErrText: "empty target",
		},
		{
			name:    "valid: bare target, no prefix collision",
			aliases: map[string]string{"aliased/coding": "gpt-test"},
		},
		{
			name:    "valid: provider-prefixed target, unconfigured prefix on the alias itself",
			aliases: map[string]string{"team/coding": "openai/gpt-test"},
		},
		{
			name:    "valid: target not yet known (lazy resolution, allowed at construction)",
			aliases: map[string]string{"aliased/future": "openai/not-yet-discovered"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Providers: baseProviders, ModelAliases: tt.aliases}
			_, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
			if tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("newModelRegistry: %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("newModelRegistry: want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErrText) {
				t.Errorf("err = %v, want substring %q", err, tt.wantErrText)
			}
		})
	}
}

// TestModelRegistry_Resolve_Alias_SlashLikeID_NotMistakenForProviderPrefix
// is the resolve-precedence regression from spec §5: an alias id
// containing "/" whose prefix names NO configured provider ("team" is not
// configured here — a prefix that IS configured is rejected at
// construction, so this is the only shape available to prove the alias
// map is checked, and wins, ahead of splitConfiguredProvider/bareWinner)
// must still resolve through the alias table, not fall through to
// errModelUnknown.
func TestModelRegistry_Resolve_Alias_SlashLikeID_NotMistakenForProviderPrefix(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"team/coding": "openai/gpt-test"}, nil)

	adapter, upstreamModel, canonical, err := reg.resolve("team/coding", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != reg.adapters["openai"] {
		t.Errorf("adapter = %v, want openai adapter", adapter)
	}
	if upstreamModel != "gpt-test" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "gpt-test")
	}
	if canonical != "openai/gpt-test" {
		t.Errorf("canonical = %q, want %q", canonical, "openai/gpt-test")
	}
}

// TestModelRegistry_Resolve_Alias_BareTarget_ResolvesViaCollisionWinner
// proves resolveAliasTarget's bareWinner branch (a target with no
// provider prefix) applies the identical sorted-first collision rule a
// direct bare-id request already gets (ruling g).
func TestModelRegistry_Resolve_Alias_BareTarget_ResolvesViaCollisionWinner(t *testing.T) {
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
		ModelAliases: map[string]string{"aliased/x": "shared"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	adapter, _, canonical, err := reg.resolve("aliased/x", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["alpha"] {
		t.Errorf("adapter = %v, want alpha (sorted-first collision winner)", adapter)
	}
	if canonical != "alpha/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "alpha/shared")
	}
}

// --- resolve: alias authorization (spec §5's "alias name OR target,
// either grants") ---

func TestModelRegistry_Resolve_Alias_AuthorizedViaAliasNameOnly(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	// Matches the alias name; matches neither "openai/gpt-test" nor its
	// bare suffix "gpt-test".
	grp := &group{name: "narrow", models: []string{"aliased/coding"}}
	if _, _, _, err := reg.resolve("aliased/coding", grp); err != nil {
		t.Errorf("resolve: %v, want success (authorized via the alias name alone)", err)
	}
}

func TestModelRegistry_Resolve_Alias_AuthorizedViaTargetOnly(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	// Matches the target's bare suffix; does not match the alias name.
	grp := &group{name: "narrow", models: []string{"gpt-*"}}
	if _, _, _, err := reg.resolve("aliased/coding", grp); err != nil {
		t.Errorf("resolve: %v, want success (authorized via the resolved target alone)", err)
	}
}

func TestModelRegistry_Resolve_Alias_DeniedWhenNeitherAliasNorTargetMatch(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	grp := &group{name: "narrow", models: []string{"claude-*"}}
	if _, _, _, err := reg.resolve("aliased/coding", grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied", err)
	}
}

func TestModelRegistry_Resolve_Alias_ProviderDenied_OverridesModelAuthz(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	// The model glob would allow it via the alias name, but the group's
	// providers list excludes "openai" entirely — allowsProvider still
	// gates the resolved target's provider (spec §5's "allowsProvider
	// still applies to the target's provider").
	grp := &group{name: "no-openai", providers: []string{"anthropic"}, models: []string{"aliased/coding"}}
	if _, _, _, err := reg.resolve("aliased/coding", grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied (provider gate must still apply)", err)
	}
}

// TestModelRegistry_Resolve_Alias_UnresolvedTarget_ReturnsAliasTargetError
// is spec §5's lazy-resolution 404: a target naming no currently-known
// model returns *aliasTargetError, whose message names both the alias and
// the missing target.
func TestModelRegistry_Resolve_Alias_UnresolvedTarget_ReturnsAliasTargetError(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/future": "openai/not-yet-discovered"}, nil)

	_, _, _, err := reg.resolve("aliased/future", allowAllGroup())
	var aerr *aliasTargetError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %v (%T), want *aliasTargetError", err, err)
	}
	if !strings.Contains(aerr.Error(), "aliased/future") {
		t.Errorf("error message = %q, want it to name the alias %q", aerr.Error(), "aliased/future")
	}
	if !strings.Contains(aerr.Error(), "openai/not-yet-discovered") {
		t.Errorf("error message = %q, want it to name the target %q", aerr.Error(), "openai/not-yet-discovered")
	}
}

// TestModelRegistry_Resolve_Alias_TargetBecomesKnown_ResolvesAfterDiscovery
// proves the lazy side of "resolve lazily": a target that was unresolvable
// at construction succeeds once its provider's discovered set catches up
// (no re-validation needed — discovery mutates providerState directly).
func TestModelRegistry_Resolve_Alias_TargetBecomesKnown_ResolvesAfterDiscovery(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/future": "openai/not-yet-discovered"}, nil)

	if _, _, _, err := reg.resolve("aliased/future", allowAllGroup()); err == nil {
		t.Fatal("resolve before discovery: want an error, got nil")
	}

	reg.states["openai"].finishRefresh(reg.now(), []string{"not-yet-discovered"}, nil)

	adapter, upstreamModel, canonical, err := reg.resolve("aliased/future", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve after discovery: %v, want success", err)
	}
	if adapter != reg.adapters["openai"] || upstreamModel != "not-yet-discovered" || canonical != "openai/not-yet-discovered" {
		t.Errorf("resolve after discovery = (%v, %q, %q), want (openai adapter, %q, %q)", adapter, upstreamModel, canonical, "not-yet-discovered", "openai/not-yet-discovered")
	}
}

// TestModelRegistry_Resolve_Alias_PrecedesDiscoveredCollision_WarnsOnce
// covers the documented precedence edge (registry.go's resolve doc
// comment): an alias id that also happens to name a model discovered
// AFTER construction (impossible for an explicit model — that is a
// construction error) still resolves to the ALIAS's own target, and the
// gateway logs exactly one warning about the shadowed id, regardless of
// how many times resolve is called for it.
func TestModelRegistry_Resolve_Alias_PrecedesDiscoveredCollision_WarnsOnce(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openai": newFakeAdapter("openai"),
		"acme":   newFakeAdapter("acme"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"openai": {Models: []string{"gpt-test"}},
			"acme":   {Discovery: true},
		},
		ModelAliases: map[string]string{"shared-id": "openai/gpt-test"},
	}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	// acme's discovery finds a model literally named "shared-id" — the
	// same string as the configured alias, discovered only after
	// construction, so validateModelAliases never saw it.
	reg.states["acme"].finishRefresh(reg.now(), []string{"shared-id"}, nil)

	adapter, _, canonical, err := reg.resolve("shared-id", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["openai"] {
		t.Errorf("adapter = %v, want openai (the alias, not acme's discovered model, must win)", adapter)
	}
	if canonical != "openai/gpt-test" {
		t.Errorf("canonical = %q, want %q", canonical, "openai/gpt-test")
	}

	if _, _, _, err := reg.resolve("shared-id", allowAllGroup()); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := rl.count(); got != 1 {
		t.Errorf("collision-shadow log emitted %d times across 2 resolve calls, want exactly 1", got)
	}
}

// --- listFor: alias visibility (spec §5) ---

func TestModelRegistry_ListFor_Alias_ListedWhenAuthorized_OwnedByTargetProvider(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	got := reg.listFor(allowAllGroup())
	var found bool
	for _, entry := range got {
		if entry["id"] == "aliased/coding" {
			found = true
			if entry["owned_by"] != "openai" {
				t.Errorf("aliased/coding owned_by = %v, want %q", entry["owned_by"], "openai")
			}
		}
	}
	if !found {
		t.Errorf("listFor = %v, want an entry for the alias %q", got, "aliased/coding")
	}
}

func TestModelRegistry_ListFor_Alias_UnresolvedTarget_Omitted(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/future": "openai/not-yet-discovered"}, nil)

	got := reg.listFor(allowAllGroup())
	for _, entry := range got {
		if entry["id"] == "aliased/future" {
			t.Errorf("listFor lists %q whose target does not resolve yet, want it omitted", "aliased/future")
		}
	}
}

func TestModelRegistry_ListFor_Alias_NotAuthorized_Omitted(t *testing.T) {
	t.Parallel()
	reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)

	grp := &group{name: "narrow", models: []string{"claude-*"}}
	got := reg.listFor(grp)
	for _, entry := range got {
		if entry["id"] == "aliased/coding" {
			t.Errorf("listFor lists %q for a group authorized for neither the alias nor its target", "aliased/coding")
		}
	}
}

// TestModelRegistry_ListFor_Alias_DedupedAgainstDiscoveredCollision is the
// review fix for listFor's alias-listing loop: when a provider's
// discovery fetch finds a model whose bare id is identical to a
// configured alias id (impossible for an EXPLICIT model —
// validateModelAliases already rejects that at construction, but
// discovery is dynamic and runs after that check), the listing must show
// exactly ONE entry for that id — not one from the real discovered model
// and a second, duplicate one from the alias loop. resolve's own
// precedence (registry.go's resolve doc comment) must still have the
// alias win at request time regardless of which entry the listing shows.
func TestModelRegistry_ListFor_Alias_DedupedAgainstDiscoveredCollision(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openai": newFakeAdapter("openai"),
		"acme":   newFakeAdapter("acme"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"openai": {Models: []string{"gpt-test"}},
			"acme":   {Discovery: true},
		},
		ModelAliases: map[string]string{"shared-id": "openai/gpt-test"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	// acme's discovery finds a model literally named "shared-id" — the
	// same string as the configured alias, discovered only after
	// construction, so validateModelAliases never saw it.
	reg.states["acme"].finishRefresh(reg.now(), []string{"shared-id"}, nil)

	got := reg.listFor(allowAllGroup())
	count := 0
	for _, entry := range got {
		if entry["id"] == "shared-id" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("listFor has %d entries for id %q, want exactly 1 (deduped)", count, "shared-id")
	}

	// resolve must still prefer the alias, per its documented precedence,
	// regardless of which single entry the listing showed above.
	adapter, _, canonical, err := reg.resolve("shared-id", allowAllGroup())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["openai"] {
		t.Errorf("adapter = %v, want openai (the alias, not acme's discovered model, must still win resolution)", adapter)
	}
	if canonical != "openai/gpt-test" {
		t.Errorf("canonical = %q, want %q", canonical, "openai/gpt-test")
	}
}
