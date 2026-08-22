package traefikllmgateway

import (
	"context"
	"testing"
	"time"
)

// fakeMetaAdapter wraps fakeAdapter, additionally implementing
// modelMetadataFetcher (feature v0.23) — a test double for an
// openai-type adapter configured with metadataPath, exercising
// warmFill/refreshProvider's captureModelMetadata wiring without a real
// HTTP fetch. Every other fakeAdapter-consuming test is unaffected: only
// tests that explicitly construct a fakeMetaAdapter get this extra
// capability.
type fakeMetaAdapter struct {
	*fakeAdapter
	fetchModelMetadataFn func(context.Context) (map[string]int, error)
}

func newFakeMetaAdapter(name string) *fakeMetaAdapter {
	return &fakeMetaAdapter{fakeAdapter: newFakeAdapter(name)}
}

func (f *fakeMetaAdapter) fetchModelMetadata(ctx context.Context) (map[string]int, error) {
	if f.fetchModelMetadataFn == nil {
		return nil, nil
	}
	return f.fetchModelMetadataFn(ctx)
}

// TestModelRegistry_WarmFill_CapturesModelMetadata proves warmFill's
// captureModelMetadata wiring: a discovery-enabled provider whose
// adapter implements modelMetadataFetcher has its returned per-model
// context lengths recorded on providerState, reachable through
// resolveMetaFor.
func TestModelRegistry_WarmFill_CapturesModelMetadata(t *testing.T) {
	t.Parallel()
	fa := newFakeMetaAdapter("lmstudio")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		return []string{"qwen2.5-0.5b"}, nil
	}
	fa.fetchModelMetadataFn = func(context.Context) (map[string]int, error) {
		return map[string]int{"qwen2.5-0.5b": 32768}, nil
	}
	adapters := map[string]providerAdapter{"lmstudio": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"lmstudio": {Discovery: true, MetadataPath: "/api/v0/models"}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.warmFill(context.Background())

	ctxVal, ok := reg.states["lmstudio"].contextFor("qwen2.5-0.5b")
	if !ok || ctxVal != 32768 {
		t.Errorf("contextFor(qwen2.5-0.5b) = (%d, %v), want (32768, true)", ctxVal, ok)
	}

	meta := reg.resolveMetaFor("lmstudio", "qwen2.5-0.5b")
	if !meta.ContextKnown || meta.ContextTokens != 32768 {
		t.Errorf("resolveMetaFor = %+v, want ContextKnown=true ContextTokens=32768", meta)
	}
}

// TestModelRegistry_WarmFill_MetadataFetchErrorNonFatal proves a failed
// metadata capture is non-fatal to discovery itself (feature v0.23's own
// "absent/failed = no metadata captured" ruling): listModels still
// succeeds and populates the provider's known model set, one log line
// records the metadata failure, and contextFor simply reports unknown.
func TestModelRegistry_WarmFill_MetadataFetchErrorNonFatal(t *testing.T) {
	t.Parallel()
	fa := newFakeMetaAdapter("lmstudio")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		return []string{"m1"}, nil
	}
	fa.fetchModelMetadataFn = func(context.Context) (map[string]int, error) {
		return nil, errUpstream
	}
	adapters := map[string]providerAdapter{"lmstudio": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"lmstudio": {Discovery: true, MetadataPath: "/api/v0/models"}}}
	log := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, log.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.warmFill(context.Background())

	if !reg.states["lmstudio"].hasModel("m1") {
		t.Error("discovery's own model set must still populate despite a metadata capture failure")
	}
	if _, ok := reg.states["lmstudio"].contextFor("m1"); ok {
		t.Error("contextFor must report unknown after a failed metadata capture")
	}
	if log.count() == 0 {
		t.Error("want at least one log line recording the metadata capture failure")
	}
}

// TestModelRegistry_MaybeRefresh_CapturesModelMetadata proves the SAME
// wiring runs on the background refresh path (refreshProvider), not just
// the synchronous warmFill — captureModelMetadata is shared between both.
func TestModelRegistry_MaybeRefresh_CapturesModelMetadata(t *testing.T) {
	t.Parallel()
	fa := newFakeMetaAdapter("lmstudio")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		return []string{"m1"}, nil
	}
	fa.fetchModelMetadataFn = func(context.Context) (map[string]int, error) {
		return map[string]int{"m1": 8192}, nil
	}
	adapters := map[string]providerAdapter{"lmstudio": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"lmstudio": {Discovery: true, DiscoveryInterval: "1h", MetadataPath: "/api/v0/models"}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool {
		_, ok := reg.states["lmstudio"].contextFor("m1")
		return ok
	})
	ctxVal, _ := reg.states["lmstudio"].contextFor("m1")
	if ctxVal != 8192 {
		t.Errorf("contextFor(m1) = %d, want 8192", ctxVal)
	}
}

// TestModelRegistry_ListFor_ExposesContextAndPricingFields proves
// listFor's modelObject output carries "context_window" and "pricing"
// when resolveMetaFor knows them (via a modelMeta config override here),
// and omits both entirely for a model nothing knows anything about.
func TestModelRegistry_ListFor_ExposesContextAndPricingFields(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test", "totally-unknown"}}},
		ModelMeta: map[string]*ModelMetaConfig{
			"gpt-test": {ContextTokens: 128000, InputCostPerMTokMicroUSD: 1_250_000, OutputCostPerMTokMicroUSD: 10_000_000},
		},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	got := reg.listFor(allowAllGroup())

	var known, unknown map[string]any
	for _, entry := range got {
		switch entry["id"] {
		case "gpt-test":
			known = entry
		case "totally-unknown":
			unknown = entry
		}
	}
	if known == nil || unknown == nil {
		t.Fatalf("listFor missing expected entries: %v", got)
	}

	if got := known["context_window"]; got != 128000 {
		t.Errorf(`known["context_window"] = %v, want 128000`, got)
	}
	pricing, ok := known["pricing"].(map[string]any)
	if !ok {
		t.Fatalf(`known["pricing"] = %v, want a map`, known["pricing"])
	}
	if pricing["input_per_mtok_usd"] != 1.25 || pricing["output_per_mtok_usd"] != 10.0 {
		t.Errorf("pricing = %v, want {input_per_mtok_usd:1.25 output_per_mtok_usd:10}", pricing)
	}

	if _, present := unknown["context_window"]; present {
		t.Errorf(`unknown["context_window"] present (%v), want omitted entirely`, unknown["context_window"])
	}
	if _, present := unknown["pricing"]; present {
		t.Errorf(`unknown["pricing"] present (%v), want omitted entirely`, unknown["pricing"])
	}
}

// TestModelRegistry_ListFor_Alias_InheritsTargetMetadata proves an
// alias's listFor entry carries its target's resolved metadata
// (modelObject via resolveMetaForAlias), and that an alias-level
// modelMeta entry overrides it.
func TestModelRegistry_ListFor_Alias_InheritsTargetMetadata(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}

	t.Run("inherits target metadata with no alias override", func(t *testing.T) {
		cfg := &Config{
			Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}},
			ModelAliases: map[string]string{"aliased/coding": "openai/gpt-test"},
			ModelMeta: map[string]*ModelMetaConfig{
				"openai/gpt-test": {ContextTokens: 128000, Free: true},
			},
		}
		reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
		if err != nil {
			t.Fatalf("newModelRegistry: %v", err)
		}
		got := reg.listFor(allowAllGroup())
		var alias map[string]any
		for _, entry := range got {
			if entry["id"] == "aliased/coding" {
				alias = entry
			}
		}
		if alias == nil {
			t.Fatalf("listFor missing alias entry: %v", got)
		}
		if alias["context_window"] != 128000 {
			t.Errorf(`alias["context_window"] = %v, want 128000 (inherited)`, alias["context_window"])
		}
		pricing, ok := alias["pricing"].(map[string]any)
		if !ok || pricing["input_per_mtok_usd"] != 0.0 {
			t.Errorf(`alias["pricing"] = %v, want a free (zero) pricing object (inherited)`, alias["pricing"])
		}
	})

	t.Run("alias-level override wins over inherited context", func(t *testing.T) {
		cfg := &Config{
			Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}},
			ModelAliases: map[string]string{"aliased/coding": "openai/gpt-test"},
			ModelMeta: map[string]*ModelMetaConfig{
				"openai/gpt-test": {ContextTokens: 128000},
				"aliased/coding":  {ContextTokens: 999},
			},
		}
		reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
		if err != nil {
			t.Fatalf("newModelRegistry: %v", err)
		}
		got := reg.listFor(allowAllGroup())
		var alias map[string]any
		for _, entry := range got {
			if entry["id"] == "aliased/coding" {
				alias = entry
			}
		}
		if alias == nil {
			t.Fatalf("listFor missing alias entry: %v", got)
		}
		if alias["context_window"] != 999 {
			t.Errorf(`alias["context_window"] = %v, want 999 (alias override)`, alias["context_window"])
		}
	})
}

// TestModelRegistry_ResolveMetaForAliasName covers the admin dashboard's
// unrestricted alias-metadata resolution directly: an unconfigured
// alias, one whose target has not resolved yet, and a real, resolvable
// one.
func TestModelRegistry_ResolveMetaForAliasName(t *testing.T) {
	t.Parallel()

	t.Run("unconfigured alias reports everything unknown", func(t *testing.T) {
		reg := newAliasTestRegistry(t, map[string]string{"aliased/coding": "openai/gpt-test"}, nil)
		got := reg.resolveMetaForAliasName("not-an-alias")
		if got.ContextKnown || got.CostKnown {
			t.Errorf("got %+v, want everything unknown", got)
		}
	})

	t.Run("unresolved target reports everything unknown", func(t *testing.T) {
		reg := newAliasTestRegistry(t, map[string]string{"aliased/future": "openai/not-yet-discovered"}, nil)
		got := reg.resolveMetaForAliasName("aliased/future")
		if got.ContextKnown || got.CostKnown {
			t.Errorf("got %+v, want everything unknown", got)
		}
	})

	t.Run("resolvable alias ignores group authorization entirely", func(t *testing.T) {
		adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
		cfg := &Config{
			Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}},
			ModelAliases: map[string]string{"aliased/coding": "openai/gpt-test"},
			ModelMeta:    map[string]*ModelMetaConfig{"openai/gpt-test": {ContextTokens: 55555}},
		}
		reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
		if err != nil {
			t.Fatalf("newModelRegistry: %v", err)
		}
		got := reg.resolveMetaForAliasName("aliased/coding")
		if !got.ContextKnown || got.ContextTokens != 55555 {
			t.Errorf("got %+v, want ContextKnown=true ContextTokens=55555", got)
		}
	})
}

// TestModelRegistry_WarmFill_MetadataGetsFreshTimeoutBudget is the
// review fix (SHOULD-4) regression: the metadata fetch must get its OWN
// context.WithTimeout budget, computed AFTER listModels finishes — not a
// reuse of listModels' own, already-partially-spent context. A
// listModels call that takes real, non-trivial time must not leave the
// metadata fetch with a shrunken remaining budget.
func TestModelRegistry_WarmFill_MetadataGetsFreshTimeoutBudget(t *testing.T) {
	t.Parallel()
	const simulatedListModelsDelay = 800 * time.Millisecond
	const slack = 400 * time.Millisecond // generous vs. the 800ms delay — see the assertion's own comment

	var metadataCtxDeadline time.Time
	var sawDeadline bool

	fa := newFakeMetaAdapter("lmstudio")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		time.Sleep(simulatedListModelsDelay)
		return []string{"m1"}, nil
	}
	fa.fetchModelMetadataFn = func(ctx context.Context) (map[string]int, error) {
		metadataCtxDeadline, sawDeadline = ctx.Deadline()
		return map[string]int{"m1": 4096}, nil
	}
	adapters := map[string]providerAdapter{"lmstudio": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"lmstudio": {Discovery: true, MetadataPath: "/api/v0/models"}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	before := time.Now()
	reg.warmFill(context.Background())
	if !sawDeadline {
		t.Fatal("fetchModelMetadataFn never ran, or its context carried no deadline")
	}

	// Old (buggy) behavior would reuse listModels' own context, whose
	// warmFillTimeout budget started BEFORE the simulated delay — its
	// deadline would land ~800ms EARLIER than a freshly computed one.
	// The fixed behavior computes a new deadline AFTER listModels
	// returns, so it lands close to (start-of-warmFill + delay +
	// warmFillTimeout), not (start-of-warmFill + warmFillTimeout).
	wantEarliest := before.Add(warmFillTimeout - slack)
	if metadataCtxDeadline.Before(wantEarliest) {
		t.Errorf("metadata fetch context deadline = %v, want at or after %v — it must get a fresh warmFillTimeout budget, not listModels' spent one", metadataCtxDeadline, wantEarliest)
	}
}

// TestModelRegistry_RefreshProvider_MetadataPanicDoesNotFailDiscovery is
// the review fix (SHOULD-5) regression: a panic inside fetchModelMetadata
// must never discard a listModels call that already succeeded.
// captureModelMetadata's own recover (registry.go) must catch it before
// it ever reaches refreshProvider's outer recover, which would otherwise
// overwrite the (already-nil, already-correct) err variable and cause
// finishRefresh to treat a genuinely successful discovery as failed.
func TestModelRegistry_RefreshProvider_MetadataPanicDoesNotFailDiscovery(t *testing.T) {
	t.Parallel()
	fa := newFakeMetaAdapter("lmstudio")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		return []string{"m1", "m2"}, nil
	}
	fa.fetchModelMetadataFn = func(context.Context) (map[string]int, error) {
		panic("simulated bug in fetchModelMetadata")
	}
	adapters := map[string]providerAdapter{"lmstudio": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"lmstudio": {Discovery: true, DiscoveryInterval: "1h", MetadataPath: "/api/v0/models"}}}
	log := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, log.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return reg.states["lmstudio"].hasModel("m1") })

	if !reg.states["lmstudio"].hasModel("m2") {
		t.Error("a panic in metadata capture must not discard the successful discovery result")
	}
	_, _, lastErr, _, _ := reg.states["lmstudio"].snapshot()
	if lastErr != "" {
		t.Errorf("lastErr = %q, want empty — discovery itself succeeded; only metadata capture panicked", lastErr)
	}
	if log.count() == 0 {
		t.Error("want at least one log line recording the metadata-capture panic")
	}
}
