package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
func (f *fakeAdapter) listModels(ctx context.Context) ([]string, error) {
	if f.listModelsFn == nil {
		return nil, nil
	}
	return f.listModelsFn(ctx)
}

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
