package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
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
func (f *fakeAdapter) httpClient() *http.Client      { return nil }
func (f *fakeAdapter) requestTimeout() time.Duration { return defaultRequestTimeout }

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

// linesCopy returns a snapshot of every line recorded so far — for a test
// that needs to assert on a specific line's CONTENT (e.g. the
// warm-fill-retry "recovered after retry" text), not merely its count.
func (r *recordingLog) linesCopy() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
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

// TestModelRegistry_ListFor_DiscoveredIDPrefixMatchesConfiguredProvider_NeverListedBare
// is the H3 regression test. "openrouter" discovers a model id that
// itself begins with another configured provider's own name — the real
// OpenRouter convention, where upstream ids look like "openai/gpt-4o".
// splitConfiguredProvider (registry.go), used by BOTH resolve and
// listFor, peels a "configuredProvider/rest" prefix off any id string
// shaped that way, with no way to tell "this happens to start with a
// provider's name" apart from "a client addressed this id with an
// explicit provider/ prefix". Listing "openai/gpt-4o" bare (attributed
// to openrouter) would hand a client an id that resolve(), given that
// same id back, routes to provider "openai" instead — the wrong
// provider, or a 404 if "openai" never heard of "gpt-4o". listFor must
// list it ONLY in its "provider/id" disambiguating form, and resolve
// must route that form back to the provider that actually owns it.
func TestModelRegistry_ListFor_DiscoveredIDPrefixMatchesConfiguredProvider_NeverListedBare(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"openrouter": newFakeAdapter("openrouter"),
		"openai":     newFakeAdapter("openai"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"openrouter": {Models: []string{"openai/gpt-4o"}},
		"openai":     {Models: []string{"gpt-4o-mini"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := allowAllGroup()
	got := reg.listFor(grp)

	var bareEntry, prefixedEntry map[string]any
	for _, entry := range got {
		switch entry["id"] {
		case "openai/gpt-4o":
			bareEntry = entry
		case "openrouter/openai/gpt-4o":
			prefixedEntry = entry
		}
	}
	if bareEntry != nil {
		t.Errorf(`listFor listed %v bare as "openai/gpt-4o" — resolve("openai/gpt-4o") would route to provider "openai", not the "openrouter" that actually owns it`, bareEntry)
	}
	if prefixedEntry == nil || prefixedEntry["owned_by"] != "openrouter" {
		t.Fatalf(`listFor = %v, want an "openrouter/openai/gpt-4o" entry owned_by "openrouter"`, got)
	}

	// resolve() must agree with the listing: the exact id listFor emitted
	// must resolve back to the provider it named as owner, carrying the
	// original discovered id (itself containing a "/") as the upstream
	// model unchanged.
	adapter, upstreamModel, canonical, err := reg.resolve("openrouter/openai/gpt-4o", grp)
	if err != nil {
		t.Fatalf("resolve(%q): %v", "openrouter/openai/gpt-4o", err)
	}
	if adapter != adapters["openrouter"] {
		t.Errorf("adapter = %v, want openrouter adapter", adapter)
	}
	if upstreamModel != "openai/gpt-4o" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "openai/gpt-4o")
	}
	if canonical != "openrouter/openai/gpt-4o" {
		t.Errorf("canonical = %q, want %q", canonical, "openrouter/openai/gpt-4o")
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
// is the regression: grp's model-glob matching (now allowsProviderModel,
// auth.go) used to strip at any first slash, so a pattern like
// "deepseek-*" would spuriously match a bare model id that
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
// generates that candidate itself now that allowsProviderModel's
// model-matching no longer strips.
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

// --- resolve: multi-group / personal-grant authorization ---

// TestModelRegistry_Resolve_MultiGroup_UnionAcrossMemberGroups proves a
// multi-group principal (effectiveGroup, auth.go) is authorized through
// EITHER member group's own grant — the union rule.
func TestModelRegistry_Resolve_MultiGroup_UnionAcrossMemberGroups(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"minimax": newFakeAdapter("minimax"),
		"gx10":    newFakeAdapter("gx10"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"minimax": {Models: []string{"m1"}},
		"gx10":    {Models: []string{"only"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	a := &group{name: "minimax-friends", providers: []string{"minimax"}}
	b := &group{name: "gx10-only", providers: []string{"gx10"}, models: []string{"gx10/only"}}
	grp := effectiveGroup([]*group{a, b}, nil)

	if _, _, _, err := reg.resolve("minimax/m1", grp); err != nil {
		t.Errorf("resolve minimax/m1: %v, want success via member group a's unrestricted-model grant", err)
	}
	if _, _, _, err := reg.resolve("gx10/only", grp); err != nil {
		t.Errorf("resolve gx10/only: %v, want success via member group b's own grant", err)
	}
}

// TestModelRegistry_Resolve_MultiGroup_CrossGrantLeakDenied is the
// feature's core safety rule: grant A {providers:[minimax], models:[]}
// (models unrestricted, but only within minimax) must never combine with
// grant B {providers:[gx10], models:[gx10/only]} to authorize
// "gx10/other" — a naive "any grant allows the provider AND any grant
// allows the model" check would wrongly allow it, since A's own empty
// Models list matches anything on its own.
func TestModelRegistry_Resolve_MultiGroup_CrossGrantLeakDenied(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"minimax": newFakeAdapter("minimax"),
		"gx10":    newFakeAdapter("gx10"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"minimax": {Models: []string{"m1"}},
		"gx10":    {Models: []string{"only", "other"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	a := &group{name: "minimax-unrestricted", providers: []string{"minimax"}}
	b := &group{name: "gx10-only", providers: []string{"gx10"}, models: []string{"gx10/only"}}
	grp := effectiveGroup([]*group{a, b}, nil)

	if _, _, _, err := reg.resolve("gx10/only", grp); err != nil {
		t.Errorf("resolve gx10/only: %v, want success", err)
	}
	if _, _, _, err := reg.resolve("gx10/other", grp); err != errModelDenied {
		t.Errorf("resolve gx10/other: err = %v, want errModelDenied (no cross-grant leak from A's unrestricted models)", err)
	}
}

// TestModelRegistry_Resolve_PersonalProvidersGrant_AddsProviderKeepsGroupIntact
// covers a personal Providers-only grant layered on top of a group: group
// friends{providers:[minimax,uni]} plus a personal grant of
// providers:[gx10] must allow gx10 in addition to minimax/uni, unchanged,
// and deny every other provider.
func TestModelRegistry_Resolve_PersonalProvidersGrant_AddsProviderKeepsGroupIntact(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"minimax": newFakeAdapter("minimax"),
		"uni":     newFakeAdapter("uni"),
		"gx10":    newFakeAdapter("gx10"),
		"other":   newFakeAdapter("other"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"minimax": {Models: []string{"m1"}},
		"uni":     {Models: []string{"u1"}},
		"gx10":    {Models: []string{"g1"}},
		"other":   {Models: []string{"o1"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	friends := &group{name: "friends", providers: []string{"minimax", "uni"}}
	grp := effectiveGroup([]*group{friends}, &grant{providers: []string{"gx10"}})

	for _, id := range []string{"minimax/m1", "uni/u1", "gx10/g1"} {
		if _, _, _, err := reg.resolve(id, grp); err != nil {
			t.Errorf("resolve %s: %v, want success", id, err)
		}
	}
	if _, _, _, err := reg.resolve("other/o1", grp); err != errModelDenied {
		t.Errorf("resolve other/o1: err = %v, want errModelDenied (personal grant only adds gx10)", err)
	}
}

// TestModelRegistry_Resolve_PersonalModelsOnlyGrant_ExactModelAllowedOthersDenied
// covers a personal Models-only grant: models:["gx10/GLM-5.3-Flash-EXL3"]
// allows exactly that id and denies "gx10/other", independent of the
// user's own group's (unrelated) model restriction.
func TestModelRegistry_Resolve_PersonalModelsOnlyGrant_ExactModelAllowedOthersDenied(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"gx10": newFakeAdapter("gx10")}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"gx10": {Models: []string{"GLM-5.3-Flash-EXL3", "other"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	base := &group{name: "base", providers: []string{"gx10"}, models: []string{"nothing-else"}}
	grp := effectiveGroup([]*group{base}, &grant{models: []string{"gx10/GLM-5.3-Flash-EXL3"}})

	if _, _, _, err := reg.resolve("gx10/GLM-5.3-Flash-EXL3", grp); err != nil {
		t.Errorf("resolve gx10/GLM-5.3-Flash-EXL3: %v, want success via personal grant", err)
	}
	if _, _, _, err := reg.resolve("gx10/other", grp); err != errModelDenied {
		t.Errorf("resolve gx10/other: err = %v, want errModelDenied", err)
	}
}

// TestModelRegistry_ListFor_MultiGroup_ShowsUnionOfMemberGroupAndPersonalGrant
// proves GET /v1/models (listFor) shows exactly the union: a member
// group's own unrestricted-model provider, and the personal grant's own
// restricted model, with everything the personal grant does not list
// absent.
func TestModelRegistry_ListFor_MultiGroup_ShowsUnionOfMemberGroupAndPersonalGrant(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"minimax": newFakeAdapter("minimax"),
		"gx10":    newFakeAdapter("gx10"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"minimax": {Models: []string{"m1"}},
		"gx10":    {Models: []string{"only", "other"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	member := &group{name: "minimax-friends", providers: []string{"minimax"}}
	grp := effectiveGroup([]*group{member}, &grant{providers: []string{"gx10"}, models: []string{"only"}})

	ids := map[string]bool{}
	for _, entry := range reg.listFor(grp) {
		ids[entry["id"].(string)] = true //nolint:forcetypeassert // modelObject always sets id to a string
	}
	if !ids["m1"] {
		t.Errorf("listFor ids = %v, want \"m1\" present (member group's unrestricted grant on minimax)", ids)
	}
	if !ids["only"] {
		t.Errorf("listFor ids = %v, want \"only\" present (personal grant)", ids)
	}
	if ids["other"] {
		t.Errorf("listFor ids = %v, want \"other\" absent (personal grant restricts gx10 to \"only\")", ids)
	}
}

// TestModelRegistry_Resolve_MultiGrant_BareWinnerPrefersGrantThatActuallyAuthorizes
// is HIGH-2's regression (review round 2): eng{providers:[beta]} alone
// already fully authorizes bare "llama" via beta (unrestricted models).
// Adding a personal grant restricted to alpha/special must NEVER take
// that away — a union-based bareWinner picks alpha first (visible via
// the personal grant's own provider entry) and then denies it (alpha's
// grant doesn't cover "llama"), even though beta, right behind it in
// sorted order, is fully authorized by eng's own grant. bareWinner (and
// listFor's identical bare-entry selection) must skip alpha and land on
// beta instead.
func TestModelRegistry_Resolve_MultiGrant_BareWinnerPrefersGrantThatActuallyAuthorizes(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"beta":  newFakeAdapter("beta"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"alpha": {Models: []string{"llama", "special"}},
		"beta":  {Models: []string{"llama"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	eng := &group{name: "eng", providers: []string{"beta"}}
	grp := effectiveGroup([]*group{eng}, &grant{providers: []string{"alpha"}, models: []string{"alpha/special"}})

	adapter, upstreamModel, canonical, err := reg.resolve("llama", grp)
	if err != nil {
		t.Fatalf("resolve(llama): %v, want success via eng's own unrestricted-model grant on beta", err)
	}
	if adapter != adapters["beta"] {
		t.Errorf("adapter = %v, want beta's adapter", adapter)
	}
	if upstreamModel != "llama" || canonical != "beta/llama" {
		t.Errorf("upstreamModel/canonical = %q/%q, want \"llama\"/\"beta/llama\"", upstreamModel, canonical)
	}

	ids := map[string]bool{}
	for _, e := range reg.listFor(grp) {
		ids[e["id"].(string)] = true
	}
	if !ids["llama"] {
		t.Errorf("listFor ids = %v, want bare \"llama\" still listed (via eng's own grant on beta)", ids)
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

// --- resolve: GROUP-AWARE collision precedence (2026-08-27 fix) ---

// TestModelRegistry_Resolve_BareIDCollision_GroupAwareWinner is the exact
// production repro from the bug report: "alpha" and "zeta" both serve
// "shared". alpha sorts first, so bareWinner used to pick it globally
// regardless of the caller — a group authorized ONLY for zeta got
// errModelDenied for a model it was actually entitled to use, purely
// because alpha (a provider the group cannot even see) happened to sort
// earlier. The fix restricts bareWinner's candidate search to providers
// grp.allowsProvider admits, falling back to the global winner only when
// none of them qualify.
// Mutation that must make this test fail: revert bareWinner to its
// pre-fix, group-blind body (`for _, name := range m.providerNames { if
// m.states[name].hasModel(id) { return name, true } }`, ignoring grp
// entirely) — resolve then picks "alpha", grp.allowsProvider("alpha") is
// false, and this returns errModelDenied instead of serving zeta. Verified:
// applying that exact revert turns this failure red (errModelDenied, want
// nil) before restoring the fix.
func TestModelRegistry_Resolve_BareIDCollision_GroupAwareWinner(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"zeta":  newFakeAdapter("zeta"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"alpha": {Models: []string{"shared"}},
		"zeta":  {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "friends", providers: []string{"zeta"}}
	adapter, upstreamModel, canonical, err := reg.resolve("shared", grp)
	if err != nil {
		t.Fatalf("resolve: %v, want success (zeta serves \"shared\" and this group may use zeta)", err)
	}
	if adapter != adapters["zeta"] {
		t.Errorf("adapter = %v, want zeta", adapter)
	}
	if upstreamModel != "shared" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "shared")
	}
	if canonical != "zeta/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "zeta/shared")
	}
}

// TestModelRegistry_Resolve_BareIDCollision_BothAllowed_MatchesGlobalWinner
// is the required non-regression: a group explicitly authorized for BOTH
// colliding providers must resolve to the exact same provider it did
// before this fix — the sorted-first one, "alpha" — with no behavior
// change at all for a caller who could already reach today's global
// winner.
// Mutation that must make this test fail: reverse bareWinner's provider
// iteration order (`for i := len(m.providerNames) - 1; i >= 0; i--`) —
// this group can reach both providers, so the now-sorted-last provider
// "zeta" would win instead of "alpha". Verified: applying that reversal
// turns this failure red (adapter = zeta, want alpha) before restoring
// the fix.
func TestModelRegistry_Resolve_BareIDCollision_BothAllowed_MatchesGlobalWinner(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"zeta":  newFakeAdapter("zeta"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"alpha": {Models: []string{"shared"}},
		"zeta":  {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "both", providers: []string{"alpha", "zeta"}}
	adapter, _, canonical, err := reg.resolve("shared", grp)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if adapter != adapters["alpha"] {
		t.Errorf("adapter = %v, want alpha (sorted-first, unchanged from today)", adapter)
	}
	if canonical != "alpha/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "alpha/shared")
	}
}

// TestModelRegistry_Resolve_BareIDCollision_NeitherAllowed_SameDenialAsToday
// covers the other required non-regression: a group authorized for
// NEITHER colliding provider must still get errModelDenied — the same
// sentinel a known-but-unauthorized model returns today — never the wrong
// errModelUnknown a "no owner found" result would produce.
// Mutation that must make this test fail: make bareWinner return ("",
// false) when grp allows none of id's owners, instead of falling back to
// the global sorted-first owner — resolve's `if !ok { return ...,
// errModelUnknown }` branch then fires, and this test's errModelDenied
// assertion fails against the wrong sentinel. Verified: that change turns
// this failure red (err = errModelUnknown, want errModelDenied) before
// restoring the fallback.
func TestModelRegistry_Resolve_BareIDCollision_NeitherAllowed_SameDenialAsToday(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"zeta":  newFakeAdapter("zeta"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"alpha": {Models: []string{"shared"}},
		"zeta":  {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "outsiders", providers: []string{"unrelated"}}
	if _, _, _, err := reg.resolve("shared", grp); err != errModelDenied {
		t.Errorf("err = %v, want errModelDenied", err)
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

// TestNewModelRegistry_NonPositiveDiscoveryInterval_ReturnsConstructorError
// is the L5 regression test: "0s" and a negative value both parse
// successfully via time.ParseDuration, so without an explicit d<=0 check
// they would silently pass through as the refresh interval. With
// interval<=0, tryBeginRefresh's own throttle gate (registry.go) never
// suppresses a same-window re-probe, so every request would spawn a new
// listModels the instant the previous one finishes — the config must be
// rejected at construction instead.
func TestNewModelRegistry_NonPositiveDiscoveryInterval_ReturnsConstructorError(t *testing.T) {
	t.Parallel()
	for _, interval := range []string{"0s", "0", "-1h", "-30s"} {
		t.Run(interval, func(t *testing.T) {
			t.Parallel()
			adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
			cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {DiscoveryInterval: interval}}}
			if _, err := newModelRegistry(adapters, cfg, func(string, ...any) {}); err == nil {
				t.Fatalf("want constructor error for discoveryInterval %q, got nil", interval)
			}
		})
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

// --- warmFill retry: one-shot background recovery for a cold-start blip
// (production incident: "openai-audio"/"anthropic" initial discovery
// occasionally exceeds warmFillTimeout on pod start) ---

// TestModelRegistry_WarmFillRetry_FailedThenSuccessful_PopulatesModelsAndClosesBreaker
// is the core recovery case: warmFill's synchronous fetch fails, tripping
// a threshold-1 breaker, and the scheduled retry succeeds — the provider
// must end up with the retried models AND a closed breaker, exactly as if
// its very first discovery attempt had simply succeeded a little late.
func TestModelRegistry_WarmFillRetry_FailedThenSuccessful_PopulatesModelsAndClosesBreaker(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	var calls int32
	fa.listModelsFn = func(context.Context) ([]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, fmt.Errorf("cold-start egress timeout")
		}
		return []string{"gpt-warm"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Discovery: true}},
		// FailureThreshold 1: the failed warm fill alone trips the breaker,
		// so a successful retry closing it again is actually exercised —
		// at the package default (3) a single failure would never open it.
		Breaker: BreakerConfig{FailureThreshold: 1, OpenDuration: "1m", MaxOpenDuration: "10m"},
	}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	infoLog := &recordingLog{}
	reg.info = infoLog.fn
	// Widened from an earlier 10ms (verify-retry.md item 4): the two
	// "before the retry has had a chance to run" checks just below run
	// synchronously, right after warmFill returns, with no synchronization
	// of their own against the scheduled timer — a heavily loaded (e.g.
	// -race) machine could in principle delay the test goroutine past a
	// 10ms window and see the retry's own result instead of the pre-retry
	// state. 200ms is generous headroom for that gap while the overall
	// waitUntil below still bounds the test's total time.
	reg.warmFillRetryDelay = 200 * time.Millisecond

	reg.warmFill(context.Background())

	if reg.discoveryHealthy("openai") {
		t.Fatal(`discoveryHealthy("openai") = true immediately after the failed warm fill (threshold 1), want false`)
	}
	if reg.states["openai"].hasModel("gpt-warm") {
		t.Fatal(`hasModel("gpt-warm") = true before the retry has had a chance to run`)
	}

	waitUntil(t, 2*time.Second, func() bool {
		return reg.states["openai"].hasModel("gpt-warm")
	})

	if !reg.discoveryHealthy("openai") {
		t.Error(`discoveryHealthy("openai") = false after a successful retry, want true — the retry must close the breaker it tripped`)
	}
	_, _, lastErr, _, _ := reg.states["openai"].snapshot()
	if lastErr != "" {
		t.Errorf("lastErr = %q after a successful retry, want empty", lastErr)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("listModels called %d times, want exactly 2 (the failed warm fill plus one retry)", got)
	}
	// Fix 1 (verify-retry.md): wait for the log line itself, not just the
	// state change that precedes it — retryWarmFill's own deferred func
	// calls finishRefresh (which flips hasModel/discoveryHealthy, above)
	// BEFORE it calls m.infof, on the SAME goroutine but with no
	// synchronization the test can observe from outside; a single read
	// immediately after the waitUntil above can race that write and see
	// no line yet.
	waitUntil(t, 2*time.Second, func() bool {
		for _, line := range infoLog.linesCopy() {
			if strings.Contains(line, "recovered after retry") {
				return true
			}
		}
		return false
	})
}

// TestModelRegistry_WarmFillRetry_RetryAlsoFails_FallsBackToExistingBreakerBehavior
// proves the retry adds no special-casing to the breaker: two consecutive
// failures (the warm fill, then its retry) against a threshold-2 breaker
// open it exactly as two ordinary consecutive discovery failures already
// would, and the existing ERROR line (not the success INFO line) is what
// logs.
func TestModelRegistry_WarmFillRetry_RetryAlsoFails_FallsBackToExistingBreakerBehavior(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	var calls int32
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("upstream still down")
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-a"}, Discovery: true}},
		Breaker:   BreakerConfig{FailureThreshold: 2, OpenDuration: "1m", MaxOpenDuration: "10m"},
	}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	infoLog := &recordingLog{}
	reg.info = infoLog.fn
	reg.warmFillRetryDelay = 10 * time.Millisecond

	reg.warmFill(context.Background())

	waitUntil(t, 2*time.Second, func() bool {
		return atomic.LoadInt32(&calls) == 2
	})
	waitUntil(t, 2*time.Second, func() bool {
		return !reg.discoveryHealthy("openai")
	})

	if reg.states["openai"].hasModel("gpt-a") == false {
		t.Error(`hasModel("gpt-a") = false, want true — the explicit model must survive two failed discovery attempts (stale-while-error)`)
	}
	_, _, lastErr, _, _ := reg.states["openai"].snapshot()
	if lastErr == "" {
		t.Error("lastErr = \"\" after the retry also failed, want the retry's own error message")
	}
	// Fix 1 (verify-retry.md): wait for the ERROR line itself rather than
	// reading it once right after the state-change waits above —
	// retryWarmFill's deferred func writes the log line AFTER finishRefresh
	// (which is what discoveryHealthy/lastErr above already observed), on
	// the SAME goroutine but with no synchronization the test can see from
	// outside; a single read can race that write and find nothing yet.
	waitUntil(t, 2*time.Second, func() bool {
		for _, line := range rl.linesCopy() {
			if strings.Contains(line, "warm-fill retry") && strings.Contains(line, "failed") {
				return true
			}
		}
		return false
	})
	if len(infoLog.linesCopy()) != 0 {
		t.Errorf("info log = %v, want no INFO line — the retry itself failed, nothing recovered", infoLog.linesCopy())
	}
}

// TestModelRegistry_WarmFillRetry_NoRetry_WhenWarmFillSucceeds proves
// scheduleWarmFillRetry is never invoked for a provider whose synchronous
// warm fill already succeeded: listModels must not be called a second
// time just because a retry window happened to elapse.
func TestModelRegistry_WarmFillRetry_NoRetry_WhenWarmFillSucceeds(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	var calls int32
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return []string{"gpt-a"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warmFillRetryDelay = 15 * time.Millisecond

	reg.warmFill(context.Background())

	// Sleep comfortably past the retry window: proving an ABSENCE of a
	// second call needs to outlast the delay that would have fired it,
	// not just poll for a positive condition.
	time.Sleep(10 * reg.warmFillRetryDelay)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("listModels called %d times after a successful warm fill, want exactly 1 (no retry scheduled)", got)
	}
}

// TestModelRegistry_WarmFillRetry_BoundedByOwnTimeout_NoGoroutineLeak
// proves the retry goroutine is bounded by its own context timeout
// (m.warmFillTimeout) rather than depending on any external shutdown
// signal — this plugin has none (llmgateway.go's Close doc comment) — so
// an upstream that never responds at all still cannot leak the goroutine
// forever: retryWarmFill's own context.WithTimeout cuts it off and
// finishRefresh still runs.
func TestModelRegistry_WarmFillRetry_BoundedByOwnTimeout_NoGoroutineLeak(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	var calls int32
	fa.listModelsFn = func(ctx context.Context) ([]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, fmt.Errorf("cold-start egress timeout")
		}
		// The retry attempt: never returns on its own, exactly like a
		// hung upstream connection — only ctx's own timeout can end it.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warmFillRetryDelay = 20 * time.Millisecond
	// Shortened from the package default (5s, registry.go's warmFillTimeout
	// const) via the injectable m.warmFillTimeout field: this test only
	// needs the retry's own context to expire, not the production budget,
	// so there is no reason to actually wait out 5s of real time here.
	reg.warmFillTimeout = 50 * time.Millisecond

	// Fix 4b (verify-retry.md): capture the goroutine count BEFORE the
	// retry's own goroutine (time.AfterFunc, fired warmFillRetryDelay from
	// now) exists at all, so the "back to baseline" poll below actually
	// proves it exited rather than merely proving something else did.
	runtime.GC()
	before := runtime.NumGoroutine()

	reg.warmFill(context.Background())

	// warmFillRetryDelay (20ms) + reg.warmFillTimeout (50ms, shortened
	// above) + slack for a loaded (e.g. -race) test machine.
	waitUntil(t, 2*time.Second, func() bool {
		_, _, lastErr, _, _ := reg.states["openai"].snapshot()
		return strings.Contains(lastErr, context.DeadlineExceeded.Error())
	})

	// The state change above (finishRefresh recording lastErr) happens
	// BEFORE retryWarmFill's goroutine actually returns and exits — its
	// deferred log call still runs after. Poll NumGoroutine back down to
	// (at most) the baseline instead of asserting immediately, so the
	// test proves the goroutine itself ends, not just that its result was
	// recorded.
	waitUntil(t, 2*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}

// TestModelRegistry_WarmFillRetry_GuardSkipsWhenRefreshInFlightOrDone proves
// fix 2/3 (verify-retry.md): retryWarmFill's tryBeginWarmFillRetry guard
// makes it skip entirely — never calling listModels a second time — both
// while a request-driven refresh (maybeRefresh/refreshProvider, via
// tryBeginRefresh) is already in flight against the SAME provider, and
// after one has already completed since the warm fill that scheduled this
// retry. Exercised by calling refreshProvider/retryWarmFill directly
// (not by racing real timers against each other), so the assertion is
// deterministic rather than timing-dependent — this is also, structurally,
// the "concurrent request-driven refresh + retry" scenario: both paths
// gate on the identical st.mu-guarded inFlight field, so at most one
// listModels call is ever in progress for this provider at a time.
func TestModelRegistry_WarmFillRetry_GuardSkipsWhenRefreshInFlightOrDone(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	inProgress := make(chan struct{})
	release := make(chan struct{})
	var calls, current, maxConcurrent int32
	fa.listModelsFn = func(context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		n := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
				break
			}
		}
		close(inProgress)
		<-release
		atomic.AddInt32(&current, -1)
		return []string{"gpt-a"}, nil
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	st := reg.states["openai"]
	warmFillTime := reg.now()

	// Simulate a request-driven refresh already claiming inFlight — exactly
	// what maybeRefresh's own tryBeginRefresh call, ahead of spawning
	// refreshProvider, already does in production — and let it block
	// mid-fetch so a concurrent retry has something real to race against.
	if !st.tryBeginRefresh(reg.now()) {
		t.Fatal("tryBeginRefresh on a fresh provider must succeed")
	}
	go reg.refreshProvider("openai", st, fa)

	select {
	case <-inProgress:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshProvider's listModels call never started")
	}

	// The retry must skip: a refresh is already inFlight.
	reg.retryWarmFill("openai", st, fa, warmFillTime)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d after the in-flight guard should have skipped the retry, want 1 (only the request-driven refresh)", got)
	}

	close(release)
	waitUntil(t, 2*time.Second, func() bool {
		return st.hasModel("gpt-a")
	})

	// The retry must ALSO skip now that the refresh has completed: its
	// lastRefresh has already moved past warmFillTime.
	reg.retryWarmFill("openai", st, fa, warmFillTime)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d after the retry should have skipped (a refresh already completed since the warm fill), want 1", got)
	}
	if got := atomic.LoadInt32(&maxConcurrent); got > 1 {
		t.Errorf("max concurrent listModels calls = %d, want at most 1 — the retry must never overlap a request-driven refresh", got)
	}
}

// TestModelRegistry_WarmFillRetry_NoRetry_WhenParentCtxCanceled proves fix
// 4a (verify-retry.md): a warm fill that fails because the PARENT ctx
// (newGateway's own construction context) was already canceled — a
// shutdown mid-construction, not a slow or broken upstream — must not
// schedule a retry at all.
func TestModelRegistry_WarmFillRetry_NoRetry_WhenParentCtxCanceled(t *testing.T) {
	t.Parallel()
	fa := newFakeAdapter("openai")
	var calls int32
	fa.listModelsFn = func(ctx context.Context) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, ctx.Err()
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warmFillRetryDelay = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent already canceled — simulates a shutdown mid-construction

	reg.warmFill(ctx)

	// Sleep comfortably past the retry window: proving an ABSENCE of a
	// scheduled retry needs to outlast the delay that would have fired
	// it, not just poll for a positive condition — same shape as
	// NoRetry_WhenWarmFillSucceeds above.
	time.Sleep(10 * reg.warmFillRetryDelay)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("listModels called %d times after a parent-ctx-canceled warm fill, want exactly 1 (no retry scheduled)", got)
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
	// clockMu guards clock: reg.nowFn runs on maybeRefresh's own
	// background refresh goroutine (registry.go), concurrently with this
	// test's own goroutine advancing the clock below a few lines down. A
	// plain `now` variable closed over by both was a genuine data race
	// (coordinator adversarial review, 2026-08-23) — the 10ms sleep below
	// gave the background goroutine a chance to finish reading it first,
	// but that is a race won by scheduling luck, not a happens-before
	// guarantee, and -race caught it in roughly 14% of full-suite runs.
	var clockMu sync.Mutex
	clock := time.Now()
	reg.nowFn = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		clock = clock.Add(d)
		clockMu.Unlock()
	}

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return atomic.LoadInt32(&calls) == 1 })

	// Still within the same interval: a second entry must not refetch.
	reg.maybeRefresh(context.Background())
	time.Sleep(10 * time.Millisecond) // give a wrongly-spawned goroutine a chance to run
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("listModels called %d times within one interval, want 1", got)
	}

	advanceClock(2 * time.Hour) // past the interval: next entry should refetch
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

// TestModelRegistry_Snapshot_ModelsStaleWhileError proves
// modelRegistry.snapshot()'s providerSnapshot.models — the admin
// dashboard's provider-model-accordion field (registry.go's
// providerState.snapshot) — reflects the same stale-while-error set
// hasModel/knownIDs already do: a refresh that fails after an earlier
// successful one leaves the previously discovered models in the
// snapshot, not an empty list. This is the existing semantics
// finishRefresh's own doc comment already documents; this test applies
// no special handling for the mid-refresh/failed case, it only asserts
// the models field is populated from whatever snapshot() already reads.
func TestModelRegistry_Snapshot_ModelsStaleWhileError(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	fa := newFakeAdapter("openai")
	fa.listModelsFn = func(context.Context) ([]string, error) {
		if fail.Load() {
			return nil, fmt.Errorf("upstream unreachable")
		}
		return []string{"m2", "m1"}, nil // deliberately unsorted at the source
	}
	adapters := map[string]providerAdapter{"openai": fa}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Discovery: true, DiscoveryInterval: "1h"}}}
	rl := &recordingLog{}
	reg, err := newModelRegistry(adapters, cfg, rl.fn)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warmFill(context.Background()) // succeeds, populates discovered={m1,m2}

	snaps := reg.snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshot = %+v, want 1 provider", snaps)
	}
	if got := snaps[0].models; len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("models after a successful discovery = %v, want sorted [m1 m2]", got)
	}

	now := time.Now().Add(2 * time.Hour)
	reg.nowFn = func() time.Time { return now }
	fail.Store(true)
	before := rl.count()

	reg.maybeRefresh(context.Background())
	waitUntil(t, time.Second, func() bool { return rl.count() > before })

	snaps = reg.snapshot()
	if got := snaps[0].models; len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Errorf("models after a failed refresh = %v, want the stale-while-error set [m1 m2] unchanged", got)
	}
	if snaps[0].lastErr == "" {
		t.Error("lastErr must be non-empty after the failed refresh, even though models stayed populated")
	}
}

// TestModelRegistry_Snapshot_DiscoveryEnabledReflectsConfig proves
// providerSnapshot.discoveryEnabled (Feature B, v0.22 — last-refresh label
// honesty) mirrors each provider's own configured Discovery flag exactly,
// read straight from providerState rather than through providerState.
// snapshot()'s mutex-guarded fields, since it never changes after
// construction.
func TestModelRegistry_Snapshot_DiscoveryEnabledReflectsConfig(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"on":  newFakeAdapter("on"),
		"off": newFakeAdapter("off"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"on":  {Discovery: true, DiscoveryInterval: "1h"},
		"off": {Models: []string{"pinned"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	byName := map[string]providerSnapshot{}
	for _, s := range reg.snapshot() {
		byName[s.name] = s
	}
	if !byName["on"].discoveryEnabled {
		t.Error(`provider "on" discoveryEnabled = false, want true`)
	}
	if byName["off"].discoveryEnabled {
		t.Error(`provider "off" discoveryEnabled = true, want false`)
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

// TestModelRegistry_ListFor_Collision_SingleVisibleProviderGetsBareID is
// the group-aware listing fix (2026-08-27): "alpha" and "beta" both serve
// "shared", but a group that may only reach "beta" sees no reason to know
// "alpha" exists at all. Before the fix, listFor always computed the bare
// id's owner from the GLOBAL sorted-first provider ("alpha" here) and
// only fell back to a "provider/id"-prefixed entry for a group that could
// not use it — so this exact group saw "beta/shared", a prefix that
// disambiguates against a provider it can never see in the first place.
// Mutation that must make this test fail: revert listFor's winner
// selection to the unconditional `winner := provs[0]` global choice (this
// commit's registry.go diff) — the entry becomes "beta/shared" again.
func TestModelRegistry_ListFor_Collision_SingleVisibleProviderGetsBareID(t *testing.T) {
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
	if got[0]["id"] != "shared" || got[0]["owned_by"] != "beta" {
		t.Errorf("entry = %v, want {id:shared, owned_by:beta} (bare, not prefixed)", got[0])
	}
}

// TestModelRegistry_ListFor_Collision_TwoVisibleProvidersKeepBothPrefixedForms
// covers the OTHER half of the same fix: when a group can see two or more
// of an id's owning providers, the collision is real FROM THAT GROUP'S OWN
// VIEW, so both "provider/id" forms stay listed (letting the group reach
// either explicitly) alongside a bare-id entry attributed to whichever of
// the group's own visible providers sorts first — "alpha" is configured
// but invisible to this group entirely and must not appear anywhere in the
// output, including as the bare id's owner.
// Mutation that must make this test fail: change listFor's prefixed-form
// gate back to the global `len(provs) < 2` (this commit's registry.go
// diff) — with alpha invisible but still counted, the gate does not
// change here (provs already has 2 entries either way), so instead mutate
// the winner used for the bare entry back to the unconditional
// `provs[0]` — the bare entry's owned_by becomes "alpha", a provider this
// group is never authorized to reach.
func TestModelRegistry_ListFor_Collision_TwoVisibleProvidersKeepBothPrefixedForms(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"gamma": newFakeAdapter("gamma"),
		"beta":  newFakeAdapter("beta"),
		"alpha": newFakeAdapter("alpha"),
	}
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"gamma": {Models: []string{"shared"}},
		"beta":  {Models: []string{"shared"}},
		"alpha": {Models: []string{"shared"}},
	}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	// alpha sorts first globally but is invisible to this group.
	grp := &group{name: "beta-and-gamma", providers: []string{"beta", "gamma"}}
	got := reg.listFor(grp)

	byID := make(map[string]map[string]any, len(got))
	for _, entry := range got {
		byID[entry["id"].(string)] = entry
	}
	if len(got) != 3 {
		t.Fatalf("listFor = %v, want exactly 3 entries (bare + 2 prefixed)", got)
	}
	if entry, ok := byID["shared"]; !ok || entry["owned_by"] != "beta" {
		t.Errorf("bare entry = %v, want owned_by beta (sorted-first of the group's OWN visible providers)", entry)
	}
	if entry, ok := byID["beta/shared"]; !ok || entry["owned_by"] != "beta" {
		t.Errorf("beta/shared entry = %v, want owned_by beta", entry)
	}
	if entry, ok := byID["gamma/shared"]; !ok || entry["owned_by"] != "gamma" {
		t.Errorf("gamma/shared entry = %v, want owned_by gamma", entry)
	}
	if _, ok := byID["alpha/shared"]; ok {
		t.Errorf("listFor = %v, must never list alpha/shared: alpha is invisible to this group", got)
	}
}

// --- modelsJSON: cached encoded /v1/models body (perf finding 3) ---

// TestModelRegistry_ModelsJSON_CachesUntilFinishRefresh proves the perf
// fix: a group's encoded body is the SAME []byte (not merely
// byte-equal — literally not re-marshaled) across repeated calls, until
// finishRefresh actually records a refresh attempt for some provider
// (modelsGen bumps), at which point the next call reflects the change.
func TestModelRegistry_ModelsJSON_CachesUntilFinishRefresh(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"gpt-test"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "all"}
	first := reg.modelsJSON(grp)
	if len(first) == 0 {
		t.Fatal("modelsJSON returned an empty body")
	}
	second := reg.modelsJSON(grp)
	if len(first) == 0 || len(second) == 0 || &first[0] != &second[0] {
		t.Error("two calls between refreshes returned different backing arrays, want the identical cached []byte")
	}
	if !strings.Contains(string(first), "gpt-test") {
		t.Errorf("body = %s, want it to contain %q", first, "gpt-test")
	}

	// A discovery refresh — even for a provider whose own discovery is
	// disabled here, driven directly via the registry's own finishRefresh
	// wrapper rather than a real background goroutine — must invalidate
	// the cache.
	reg.finishRefresh(reg.states["openai"], reg.now(), []string{"gpt-test", "gpt-new"}, nil)

	third := reg.modelsJSON(grp)
	if string(third) == string(second) {
		t.Error("modelsJSON body unchanged after finishRefresh added a new model, want it to reflect the new catalog")
	}
	if !strings.Contains(string(third), "gpt-new") {
		t.Errorf("body after refresh = %s, want it to contain the newly discovered %q", third, "gpt-new")
	}
}

// TestModelRegistry_ModelsJSON_PerGroupIsolation proves the cache never
// leaks one group's catalog to another: two groups authorized for
// disjoint providers must each get their own encoded body, keyed by
// *group pointer identity (modelsJSON's own doc comment).
func TestModelRegistry_ModelsJSON_PerGroupIsolation(t *testing.T) {
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

	grpOpenAI := &group{name: "openai-only", providers: []string{"openai"}}
	grpAnthropic := &group{name: "anthropic-only", providers: []string{"anthropic"}}

	bodyOpenAI := reg.modelsJSON(grpOpenAI)
	bodyAnthropic := reg.modelsJSON(grpAnthropic)

	if strings.Contains(string(bodyOpenAI), "claude-x") {
		t.Errorf("openai-only group's body leaked anthropic's model: %s", bodyOpenAI)
	}
	if strings.Contains(string(bodyAnthropic), "gpt-test") {
		t.Errorf("anthropic-only group's body leaked openai's model: %s", bodyAnthropic)
	}
	if !strings.Contains(string(bodyOpenAI), "gpt-test") {
		t.Errorf("openai-only group's body = %s, want it to contain gpt-test", bodyOpenAI)
	}
	if !strings.Contains(string(bodyAnthropic), "claude-x") {
		t.Errorf("anthropic-only group's body = %s, want it to contain claude-x", bodyAnthropic)
	}
}

// TestGroup_ModelsCacheKey_NoAmbiguityBetweenMemberNamesAndPersonalGrantMarkers
// is the round-3 NIT regression: the cache key's own "p:"/"m:" markers
// used to be bare string literals inside the joined \x00-separated
// signature, so a member group literally NAMED "p:" or "m:" (group names
// carry no character-set validation — effectiveGroup's own doc comment,
// auth.go) could produce a signature IDENTICAL to an entirely different
// (membership, personal grant) combination: members=[A,"p:","x","m:"]
// (no personal grant) collided byte-for-byte with members=[A] plus a
// personal grant of providers:["x"] — both joined to
// "s:A\x00p:\x00x\x00m:". Both must now get distinct keys.
func TestGroup_ModelsCacheKey_NoAmbiguityBetweenMemberNamesAndPersonalGrantMarkers(t *testing.T) {
	t.Parallel()
	a := &group{name: "A"}
	pColon := &group{name: "p:"}
	x := &group{name: "x"}
	mColon := &group{name: "m:"}

	ambiguousMembers := effectiveGroup([]*group{a, pColon, x, mColon}, nil)
	ambiguousPersonal := effectiveGroup([]*group{a}, &grant{providers: []string{"x"}})

	keyMembers := ambiguousMembers.modelsCacheKey()
	keyPersonal := ambiguousPersonal.modelsCacheKey()
	if keyMembers == keyPersonal {
		t.Errorf("modelsCacheKey collision: members=[A,p:,x,m:] (no personal) and members=[A]+personal{providers:[x]} both produced %q", keyMembers)
	}
}

// TestGroup_ModelsCacheKey_LengthPrefixPreventsSplitPointAmbiguity is the
// round-4 regression: the test above already distinguishes its two
// signatures by member COUNT alone (4 vs 1), so it stays green even if
// writeLengthPrefixed's own length prefix were removed entirely — the
// count prefix ahead of the member list would still differ. This test
// isolates the length prefix's OWN job with two adversarial pairs:
//
//   - members=["ab","c"] vs members=["a","bc"]: both concatenate to the
//     IDENTICAL "abc" once a split point moves, so with NO separator at
//     all (raw concatenation), both would produce "s2:abcN".
//   - members=["a:b","c"] vs members=["a","b:c"] (round-6 regression):
//     both concatenate to "a:b:c" — this pair specifically catches a
//     WEAKER mutation than "no separator at all": writeLengthPrefixed
//     keeping ONLY a bare ":" separator, with the length DIGITS dropped,
//     would still produce ":a:b" + ":c" = ":a:b:c" for the first and
//     ":a" + ":b:c" = ":a:b:c" for the second — identical
//     ("s2::a:b:cN" either way) — because a bare ":" is not
//     self-delimiting the way "<len>:" is; it does not tell a reader
//     where the CURRENT component ends if the component's own value can
//     itself contain ":".
//
// Only writeLengthPrefixed's own length-then-colon-then-value encoding
// (a netstring) keeps every pair apart: "2:ab1:c" is never confusable
// with "1:a2:bc", and "3:a:b1:c" is never confusable with "1:a3:b:c",
// regardless of what either component contains.
func TestGroup_ModelsCacheKey_LengthPrefixPreventsSplitPointAmbiguity(t *testing.T) {
	t.Parallel()
	splitAbC := effectiveGroup([]*group{{name: "ab"}, {name: "c"}}, nil)
	splitABc := effectiveGroup([]*group{{name: "a"}, {name: "bc"}}, nil)

	keyAbC := splitAbC.modelsCacheKey()
	keyABc := splitABc.modelsCacheKey()
	if keyAbC == keyABc {
		t.Errorf("modelsCacheKey collision: members=[ab,c] and members=[a,bc] (same count, concatenation \"abc\" either way) both produced %q", keyAbC)
	}

	splitAColonBC := effectiveGroup([]*group{{name: "a:b"}, {name: "c"}}, nil)
	splitABColonC := effectiveGroup([]*group{{name: "a"}, {name: "b:c"}}, nil)

	keyAColonBC := splitAColonBC.modelsCacheKey()
	keyABColonC := splitABColonC.modelsCacheKey()
	if keyAColonBC == keyABColonC {
		t.Errorf("modelsCacheKey collision: members=[a:b,c] and members=[a,b:c] (same count, concatenation \"a:b:c\" either way) both produced %q", keyAColonBC)
	}
}

// TestModelRegistry_ModelsJSON_MultiGroupReload_CacheStaysBounded is
// MEDIUM-3's regression (review round 2): effectiveGroup (auth.go)
// allocates a FRESH *group for every multi-group/personal-grant
// principal, so a *group-pointer-keyed modelsCache grows without bound
// across repeated, semantically IDENTICAL users-file reloads (each
// reload rebuilds every user's principal from scratch, even when nothing
// changed). Five identical reloads of the same two-member-group user
// must leave the cache no larger than the number of DISTINCT principals
// actually queried (here: one multi-group user + one single-group user =
// 2 stable cache keys), not one entry per reload.
func TestModelRegistry_ModelsJSON_MultiGroupReload_CacheStaysBounded(t *testing.T) {
	a, err := newAuthStore(&Config{Groups: map[string]*GroupConfig{"eng": {}, "ops": {}}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := newModelRegistry(map[string]providerAdapter{"alpha": newFakeAdapter("alpha")},
		&Config{Providers: map[string]*ProviderConfig{"alpha": {Models: []string{"m1"}}}}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := a.replaceFileUsers([]*UserConfig{
			{Name: "carol", Group: "eng", Groups: []string{"ops"}, APIKey: "sk-carol"},
			{Name: "dave", Group: "eng", APIKey: "sk-dave"},
		}); err != nil {
			t.Fatalf("replaceFileUsers (reload %d): %v", i, err)
		}
		for _, key := range []string{"sk-carol", "sk-dave"} {
			_, grp, ok := identifyWithKey(a, key)
			if !ok {
				t.Fatalf("identify failed for %q on reload %d", key, i)
			}
			if body := reg.modelsJSON(grp); len(body) == 0 {
				t.Fatalf("modelsJSON returned an empty body on reload %d", i)
			}
		}
	}

	if got := len(reg.modelsCache); got > 2 {
		t.Errorf("modelsCache entries after 5 identical reloads = %d, want at most 2 (one per DISTINCT principal — carol's multi-group principal must share one cache entry across reloads, not grow one per reload)", got)
	}
}

// TestModelRegistry_ModelsJSON_MatchesUncachedListFor proves modelsJSON's
// output is byte-identical to the OLD, pre-cache call site
// (json.NewEncoder(w).Encode(...), routes_unified.go before perf finding
// 3) for the same envelope over listFor's own result — the cache changes
// WHEN the body is computed, never WHAT it computes, including
// json.Encoder.Encode's own trailing '\n' that a bare json.Marshal does
// not add (review fix, Should-Fix 5 — modelsJSON appends it explicitly).
func TestModelRegistry_ModelsJSON_MatchesUncachedListFor(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	cfg := &Config{Providers: map[string]*ProviderConfig{"openai": {Models: []string{"zeta", "alpha"}}}}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := allowAllGroup()
	got := reg.modelsJSON(grp)

	var want bytes.Buffer
	if err := json.NewEncoder(&want).Encode(map[string]any{"object": "list", "data": reg.listFor(grp)}); err != nil {
		t.Fatalf("json.NewEncoder.Encode: %v", err)
	}
	if string(got) != want.String() {
		t.Errorf("modelsJSON = %q, want %q (byte-identical to the old json.Encoder.Encode call site)", got, want.String())
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

// TestModelRegistry_Resolve_Alias_BareTarget_GroupAwareWinner is the alias
// path's counterpart to TestModelRegistry_Resolve_BareIDCollision_
// GroupAwareWinner (2026-08-27 fix): resolveAliasTarget's own bareWinner
// call must apply the identical group-aware collision rule the direct
// bare-id path does, not the pre-fix, group-blind one — the brief
// explicitly calls out this call site since it is easy to fix bareWinner
// itself and forget one of its two callers.
// Mutation that must make this test fail: hardcode resolveAliasTarget's
// `m.bareWinner(target, grp)` call to pass `&group{}` (allow-all) instead
// of the real grp — resolveAliasTarget would then always prefer "alpha"
// (the global winner) regardless of what this group may use, and this
// test's zeta assertion fails. Verified: that change turns this failure
// red (adapter = alpha, want zeta; also errModelDenied surfaces once
// resolveAgainst's own provider check runs against alpha) before
// restoring the real grp parameter.
func TestModelRegistry_Resolve_Alias_BareTarget_GroupAwareWinner(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"alpha": newFakeAdapter("alpha"),
		"zeta":  newFakeAdapter("zeta"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"alpha": {Models: []string{"shared"}},
			"zeta":  {Models: []string{"shared"}},
		},
		ModelAliases: map[string]string{"aliased/x": "shared"},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}

	grp := &group{name: "friends", providers: []string{"zeta"}}
	adapter, _, canonical, err := reg.resolve("aliased/x", grp)
	if err != nil {
		t.Fatalf("resolve: %v, want success via the alias's group-visible target provider", err)
	}
	if adapter != adapters["zeta"] {
		t.Errorf("adapter = %v, want zeta", adapter)
	}
	if canonical != "zeta/shared" {
		t.Errorf("canonical = %q, want %q", canonical, "zeta/shared")
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
//
// A second scenario (review fix, second pass) shares this fixture: when
// grp cannot reach the COLLIDING provider at all (provider-denied), the
// real model's own entry is never emitted in the first place — the
// dedupe must key off entries actually LISTED, not the raw,
// authorization-blind owners map, or the id would vanish from the
// listing entirely (neither the real entry nor the alias appears) even
// though resolve still serves it through the alias.
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

	// Provider-denied variant: grp can reach only "openai" — "acme", the
	// colliding discovered model's own provider, is denied, so its raw
	// entry is never emitted at all. The alias must still be listed
	// exactly once, owned by its own target's provider ("openai") — the
	// earlier owners-keyed dedupe would have wrongly skipped it too here,
	// vanishing "shared-id" from the listing entirely.
	narrowGrp := &group{name: "openai-only", providers: []string{"openai"}}
	gotNarrow := reg.listFor(narrowGrp)
	var narrowCount int
	var ownedBy string
	for _, entry := range gotNarrow {
		if entry["id"] == "shared-id" {
			narrowCount++
			ownedBy, _ = entry["owned_by"].(string)
		}
	}
	if narrowCount != 1 {
		t.Fatalf("listFor (provider-denied collision) has %d entries for id %q, want exactly 1 (the alias)", narrowCount, "shared-id")
	}
	if ownedBy != "openai" {
		t.Errorf("owned_by = %q, want %q (the alias's own target provider)", ownedBy, "openai")
	}
}

// TestModelRegistry_ListFor_Alias_ShadowedCollisionKeepsAliasEntry proves the
// listing follows resolve's OWN precedence when a discovered model's bare id
// collides with a configured alias id: the single bare entry must be the
// ALIAS's (its target's owner and its resolved metadata), never the
// discovered model's, because every request for that id lands on the alias
// target instead (resolve's doc comment). The shadowed provider's own copy
// stays listed in "provider/id" form so it remains both visible and
// addressable.
//
// Live-fleet regression this fixes (2026-09-10): the alias
// "deepseek-v4-flash-vision-exp" -> "gx10/current" was listed as the cloud
// provider's identically-named model, owned_by that cloud provider and
// carrying NO context_window at all, while every request for the id ran
// against the local 1M-context model — so clients silently fell back to their
// own 128k default.
//
// Mutation that must make this test fail: restore listFor's old
// listed[alias]-keyed skip, which emitted the discovered model's entry and
// dropped the alias's.
func TestModelRegistry_ListFor_Alias_ShadowedCollisionKeepsAliasEntry(t *testing.T) {
	t.Parallel()
	adapters := map[string]providerAdapter{
		"acme":  newFakeAdapter("acme"),
		"local": newFakeAdapter("local"),
	}
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"acme":  {Discovery: true},
			"local": {Models: []string{"current"}},
		},
		ModelAliases: map[string]string{"shared-id": "local/current"},
		ModelMeta: map[string]*ModelMetaConfig{
			"local/current": {ContextTokens: 1048576, Free: true},
		},
	}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	// acme's discovery finds a model literally named "shared-id" — the same
	// string as the configured alias, discovered only after construction, so
	// validateModelAliases never saw it.
	reg.states["acme"].finishRefresh(reg.now(), []string{"shared-id"}, nil)

	got := reg.listFor(allowAllGroup())
	byID := make(map[string]map[string]any, len(got))
	bareCount := 0
	for _, entry := range got {
		id, _ := entry["id"].(string)
		byID[id] = entry
		if id == "shared-id" {
			bareCount++
		}
	}
	if bareCount != 1 {
		t.Fatalf("listFor has %d entries for id %q, want exactly 1: %v", bareCount, "shared-id", got)
	}

	bare := byID["shared-id"]
	if bare["owned_by"] != "local" {
		t.Errorf("owned_by = %v, want %q (the alias's own target provider, matching resolve)", bare["owned_by"], "local")
	}
	if bare["context_window"] != 1048576 {
		t.Errorf("context_window = %v, want 1048576 (the alias target's metadata)", bare["context_window"])
	}
	if _, ok := byID["acme/shared-id"]; !ok {
		t.Errorf("listFor = %v, want the shadowed discovered model listed as %q", got, "acme/shared-id")
	}
}
