package traefikllmgateway

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestIdentify is the brief's Step-1 table: bearer auth resolves the right
// user and group, and the group's model glob matches both the bare id and
// the provider-prefixed id.
func TestIdentify(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {Models: []string{"gpt-5*"}}},
		Users:     &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "eng", APIKey: "sk-secret"}}},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	u, grp, ok := a.identify(r)
	if !ok || u.name != "a" || grp.name != "eng" {
		t.Fatalf("identify failed: %v %v %v", u, grp, ok)
	}
	if !grp.allowsModel("gpt-5-mini") {
		t.Error("glob should match")
	}
	// allowsModel is an exact glob against the given string only — it no
	// longer strips a "provider/" prefix itself (fix(registry) ruling 3): a
	// pattern like "gpt-5*" reaching a provider-prefixed request like
	// "openai/gpt-5-mini" is now the caller's job (modelRegistry.
	// resolveAgainst / listFor build the bare-suffix candidate themselves),
	// covered at the registry level in registry_test.go, not here.
	if grp.allowsModel("openai/gpt-5-mini") {
		t.Error("allowsModel must not match a provider-prefixed id on its own; prefix-stripping now lives in the caller")
	}
	if grp.allowsModel("claude-4") {
		t.Error("should not match")
	}
}

func testAuthCfg() *Config {
	return &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users:     &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "eng", APIKey: "sk-secret"}}},
	}
}

func TestIdentify_XAPIKeyHeader_OK(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-api-key", "sk-secret")
	u, grp, ok := a.identify(r)
	if !ok || u.name != "a" || grp.name != "eng" {
		t.Fatalf("identify via x-api-key failed: %v %v %v", u, grp, ok)
	}
}

func TestIdentify_BearerCaseInsensitivePrefix(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", prefix+" sk-secret")
		if _, _, ok := a.identify(r); !ok {
			t.Errorf("prefix %q: want identify to succeed", prefix)
		}
	}
}

func TestIdentify_BearerWinsOverXAPIKey(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}, "ops": {}},
		Users: &UsersConfig{Inline: []*UserConfig{
			{Name: "bearer-user", Group: "eng", APIKey: "sk-bearer"},
			{Name: "header-user", Group: "ops", APIKey: "sk-header"},
		}},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-bearer")
	r.Header.Set("x-api-key", "sk-header")
	u, _, ok := a.identify(r)
	if !ok || u.name != "bearer-user" {
		t.Fatalf("want bearer to win, got %v ok=%v", u, ok)
	}
}

func TestIdentify_WrongKeyFails(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-wrong")
	if _, _, ok := a.identify(r); ok {
		t.Fatal("want identify to fail for wrong key")
	}
}

func TestIdentify_MissingHeaderFails(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if _, _, ok := a.identify(r); ok {
		t.Fatal("want identify to fail with no auth header")
	}
}

func TestNewAuthStore_UnknownGroup_ReturnsError(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users:     &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "nonexistent", APIKey: "sk-secret"}}},
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want error for user referencing unknown group")
	}
}

func TestNewAuthStore_DuplicateAPIKey_ReturnsError(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users: &UsersConfig{Inline: []*UserConfig{
			{Name: "a", Group: "eng", APIKey: "sk-same"},
			{Name: "b", Group: "eng", APIKey: "sk-same"},
		}},
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want error for duplicate API key across users")
	}
}

func TestNewAuthStore_EmptyAPIKeyAfterResolution_ReturnsError(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users:     &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "eng", APIKey: ""}}},
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want error for user with empty API key after resolution")
	}
}

// TestNewAuthStore_NilGroupConfig_ReturnsError covers a Groups map entry
// whose value is a nil *GroupConfig (a caller can construct this in Go, and
// Traefik's own YAML-to-struct decoding can leave a map value nil for an
// empty mapping entry) — newAuthStore must reject it as a constructor error
// rather than panic dereferencing gc.Limits on a nil gc.
func TestNewAuthStore_NilGroupConfig_ReturnsError(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": nil},
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want error for a nil GroupConfig value, got nil")
	}
}

func TestNewAuthStore_NoUsers_OK(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-api-key", "anything")
	if _, _, ok := a.identify(r); ok {
		t.Fatal("want identify to fail when no users are configured")
	}
}

// --- GroupConfig.CacheTTL: constructor validation (item B) ---

// TestNewAuthStore_GroupCacheTTL_ConstructorErrors is the constructor-error
// table for GroupConfig.CacheTTL: a malformed duration, a zero duration, a
// negative duration, and CacheTTL set while the global cache block is not
// configured (nothing to inherit ttl from, the same reasoning
// newAuthStore already applies to Cache:true) must all fail construction.
func TestNewAuthStore_GroupCacheTTL_ConstructorErrors(t *testing.T) {
	tests := []struct {
		name        string
		cacheTTL    string
		globalCache bool
	}{
		{name: "malformed duration", cacheTTL: "not-a-duration", globalCache: true},
		{name: "zero duration", cacheTTL: "0s", globalCache: true},
		{name: "negative duration", cacheTTL: "-5s", globalCache: true},
		{name: "set without global cache enabled", cacheTTL: "5m", globalCache: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
				Groups:    map[string]*GroupConfig{"default": {CacheTTL: tt.cacheTTL}},
				Cache:     CacheConfig{Enabled: tt.globalCache},
			}
			if _, err := newAuthStore(cfg); err == nil {
				t.Fatalf("newAuthStore: want a constructor error for cacheTTL=%q (globalCache=%v)", tt.cacheTTL, tt.globalCache)
			}
		})
	}
}

// TestNewAuthStore_GroupCacheTTL_EmptyInheritsZero proves an omitted
// CacheTTL resolves to group.cacheTTL == 0 — effectiveTTL's (cache.go)
// signal to inherit the global cache TTL rather than override it.
func TestNewAuthStore_GroupCacheTTL_EmptyInheritsZero(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"default": {}},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}
	if got := a.groups["default"].cacheTTL; got != 0 {
		t.Errorf("groups[default].cacheTTL = %v, want 0 (inherit)", got)
	}
}

// TestNewAuthStore_GroupCacheTTL_ValidOverride_ResolvedOnGroup proves a
// well-formed, positive CacheTTL with the global cache block configured
// both constructs cleanly and resolves onto the group struct exactly,
// ready for effectiveTTL to read.
func TestNewAuthStore_GroupCacheTTL_ValidOverride_ResolvedOnGroup(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"default": {CacheTTL: "30s"}},
		Cache:     CacheConfig{Enabled: true},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}
	if got := a.groups["default"].cacheTTL; got != 30*time.Second {
		t.Errorf("groups[default].cacheTTL = %v, want 30s", got)
	}
}

func TestGroup_AllowsProvider_EmptyListAllowsAll(t *testing.T) {
	grp := &group{name: "eng"}
	if !grp.allowsProvider("openai") {
		t.Fatal("empty providers list should allow all")
	}
}

func TestGroup_AllowsProvider_Glob(t *testing.T) {
	grp := &group{name: "eng", providers: []string{"open*"}}
	if !grp.allowsProvider("openai") {
		t.Error("want glob match")
	}
	if grp.allowsProvider("anthropic") {
		t.Error("want no match")
	}
}

func TestGroup_AllowsMCP_Glob(t *testing.T) {
	grp := &group{name: "eng", mcpServers: []string{"search-*"}}
	if !grp.allowsMCP("search-web") {
		t.Error("want glob match")
	}
	if grp.allowsMCP("db-admin") {
		t.Error("want no match")
	}
}

func TestGroup_AllowsAgent_Glob(t *testing.T) {
	grp := &group{name: "eng", agents: []string{"triage"}}
	if !grp.allowsAgent("triage") {
		t.Error("want exact match")
	}
	if grp.allowsAgent("other") {
		t.Error("want no match")
	}
}

func TestGroup_AllowsModel_EmptyListAllowsAnything(t *testing.T) {
	grp := &group{name: "eng"}
	if !grp.allowsModel("anything-goes") {
		t.Fatal("empty models list should allow anything")
	}
}

// TestGroup_AllowsPassthroughPath_EmptyListAllowsAll pins the zero-config
// default for GroupConfig.PassthroughPaths (security+performance audit,
// 2026-08-22): a group with no PassthroughPaths configured — every group
// that existed before this field did — must keep reaching every native
// passthrough path, exactly like allowsProvider/allowsModel/allowsMCP/
// allowsAgent's own empty-means-all defaults.
func TestGroup_AllowsPassthroughPath_EmptyListAllowsAll(t *testing.T) {
	grp := &group{name: "eng"}
	if !grp.allowsPassthroughPath("v1/files") {
		t.Fatal("empty passthroughPaths list should allow all")
	}
}

func TestGroup_AllowsPassthroughPath_Glob(t *testing.T) {
	grp := &group{name: "eng", passthroughPaths: []string{"v1/chat/completions"}}
	if !grp.allowsPassthroughPath("v1/chat/completions") {
		t.Error("want exact match")
	}
	if grp.allowsPassthroughPath("v1/files") {
		t.Error("want no match for a path outside the allowlist")
	}
}

// TestGroup_AllowsPassthroughPath_RejectsEncodedSlashBypass is the
// security review finding 3 regression test (round 3, 2026-08-22): a
// glob of ["v1/*"] (one wildcard segment) must not be defeated by
// encoding the "/" that would otherwise split the candidate into a
// second segment. Before this fix, matchesGlob ran against the escaped
// form directly, so path.Match("v1/*", "v1/fine_tuning%2Fjobs") reported
// true — the escaped rest looks like exactly two segments to path.Match,
// same as the decoded intent, but MEASURED to actually still match
// because "%2Fjobs" contains no literal "/" for the glob to split on
// differently — while an upstream that itself decodes %2F would read the
// same rest as "v1/fine_tuning/jobs", a THREE-segment path "v1/*" was
// never meant to authorize.
func TestGroup_AllowsPassthroughPath_RejectsEncodedSlashBypass(t *testing.T) {
	grp := &group{name: "eng", passthroughPaths: []string{"v1/*"}}
	if grp.allowsPassthroughPath("v1/fine_tuning%2Fjobs") {
		t.Error("want no match: the decoded form is three segments (v1/fine_tuning/jobs), which \"v1/*\" does not authorize")
	}
	// Sanity: the decoded-equivalent form is independently denied too,
	// proving the glob genuinely does not authorize the deeper path — not
	// just that encoding happens to break matching by accident.
	if grp.allowsPassthroughPath("v1/fine_tuning/jobs") {
		t.Error("want no match for the literal three-segment form either")
	}
	// A genuinely single-segment encoded value must still match: "v1/*"
	// authorizes any ONE segment after "v1", encoded or not.
	if !grp.allowsPassthroughPath("v1/chat%2Dcompletions") {
		t.Error("want match: a single encoded segment (no literal slash once decoded) is still one segment")
	}
}

// TestGroup_AllowsPassthroughPath_MalformedEncoding_DeniesWhenRestricted
// mirrors hasTraversalSegment's own "fails to unescape at all is
// rejected too" rule (routes_passthrough.go) — applied here to the
// allowlist match itself, for a group that actually restricts paths.
func TestGroup_AllowsPassthroughPath_MalformedEncoding_DeniesWhenRestricted(t *testing.T) {
	grp := &group{name: "eng", passthroughPaths: []string{"v1/*"}}
	if grp.allowsPassthroughPath("v1/%zz") {
		t.Error("want no match: malformed percent-encoding must fail closed for a restricted group")
	}
}

// TestGroup_AllowsPassthroughPath_MalformedEncoding_StillAllowsWhenUnrestricted
// pins the zero-config default-preserving requirement precisely at the
// boundary this fix touches: an EMPTY PassthroughPaths list (the fast
// path, checked before any decoding) must keep allowing a request whose
// rest fails to percent-decode — exactly as before this finding, since
// hasTraversalSegment (unconditional, in handlePassthrough) is still the
// gate for that case, unchanged by this fix.
func TestGroup_AllowsPassthroughPath_MalformedEncoding_StillAllowsWhenUnrestricted(t *testing.T) {
	grp := &group{name: "eng"} // no PassthroughPaths configured
	if !grp.allowsPassthroughPath("v1/%zz") {
		t.Error("want match: an unrestricted group's fast path must never decode rest at all")
	}
}

func TestAuthStore_ReplaceFileUsers_AddsAndSwapsUsers(t *testing.T) {
	a, err := newAuthStore(testAuthCfg()) // inline user "a" / sk-secret in group eng
	if err != nil {
		t.Fatal(err)
	}

	if err := a.replaceFileUsers([]*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-file1"}}); err != nil {
		t.Fatalf("replaceFileUsers: %v", err)
	}

	inlineReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	inlineReq.Header.Set("Authorization", "Bearer sk-secret")
	if _, _, ok := a.identify(inlineReq); !ok {
		t.Fatal("want inline user to remain identifiable after replaceFileUsers")
	}

	fileReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	fileReq.Header.Set("Authorization", "Bearer sk-file1")
	u, _, ok := a.identify(fileReq)
	if !ok || u.name != "f1" {
		t.Fatalf("want file user f1 identifiable, got %v ok=%v", u, ok)
	}

	// Second call swaps the file-sourced set wholesale: f1 must disappear,
	// f2 must appear, inline user must still work.
	if err := a.replaceFileUsers([]*UserConfig{{Name: "f2", Group: "eng", APIKey: "sk-file2"}}); err != nil {
		t.Fatalf("replaceFileUsers (second): %v", err)
	}
	if _, _, ok := a.identify(fileReq); ok {
		t.Fatal("want f1 to no longer be identifiable after being replaced")
	}
	if _, _, ok := a.identify(inlineReq); !ok {
		t.Fatal("want inline user to remain identifiable after second replaceFileUsers")
	}
	f2Req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	f2Req.Header.Set("Authorization", "Bearer sk-file2")
	if _, _, ok := a.identify(f2Req); !ok {
		t.Fatal("want file user f2 identifiable after second replaceFileUsers")
	}
}

func TestAuthStore_ReplaceFileUsers_DuplicateWithInline_ReturnsErrorAndLeavesStoreUnchanged(t *testing.T) {
	a, err := newAuthStore(testAuthCfg()) // inline user "a" / sk-secret
	if err != nil {
		t.Fatal(err)
	}
	err = a.replaceFileUsers([]*UserConfig{{Name: "dup", Group: "eng", APIKey: "sk-secret"}})
	if err == nil {
		t.Fatal("want error for file user colliding with inline user's API key")
	}

	// Store must still be usable and unchanged by the rejected replace.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	u, _, ok := a.identify(r)
	if !ok || u.name != "a" {
		t.Fatalf("want original inline user still resolvable after rejected replace, got %v ok=%v", u, ok)
	}
}

func TestAuthStore_ReplaceFileUsers_UnknownGroup_ReturnsError(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	err = a.replaceFileUsers([]*UserConfig{{Name: "f1", Group: "nonexistent", APIKey: "sk-file1"}})
	if err == nil {
		t.Fatal("want error for file user referencing unknown group")
	}
}

// TestAuthStore_ReplaceFileUsers_NilElement_ReturnsErrorNotPanic is the
// regression for a nil *UserConfig slice element reaching buildEntry. This
// path is reachable at runtime: json.Unmarshal of a `null` array element
// into []*UserConfig yields a nil entry, and the config-reload path (Task 4)
// that calls replaceFileUsers is NOT wrapped by recoverPanic — that guard
// only covers ServeHTTP. An unguarded nil dereference here would take down
// the whole process instead of failing one reload.
func TestAuthStore_ReplaceFileUsers_NilElement_ReturnsErrorNotPanic(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.replaceFileUsers([]*UserConfig{nil}); err == nil {
		t.Fatal("want error for a nil user config element, got nil")
	}

	// The store must still be usable — the rejected replace must not have
	// partially mutated it.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	if _, _, ok := a.identify(r); !ok {
		t.Fatal("want inline user still resolvable after a rejected nil-element replace")
	}
}

// TestNewAuthStore_NilInlineElement_ReturnsErrorNotPanic covers the same
// nil-element hazard reachable from newAuthStore's inline-user loop, which
// runs at plugin construction time — also outside recoverPanic's coverage.
func TestNewAuthStore_NilInlineElement_ReturnsErrorNotPanic(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users:     &UsersConfig{Inline: []*UserConfig{nil}},
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want error for a nil inline user config element, got nil")
	}
}

// TestAuthStore_ReplaceFileUsers_ConcurrentWritersAndReaders exercises
// replaceFileUsers and identify from many goroutines simultaneously under
// -race. Each writer swaps in a single, uniquely-keyed file user, so
// whichever writer's rebuild is applied last, the store must always hold
// exactly the inline users plus one file user's worth of entries — never a
// partial mix from two overlapping rebuilds, and never a state where the
// always-present inline user briefly disappears.
func TestAuthStore_ReplaceFileUsers_ConcurrentWritersAndReaders(t *testing.T) {
	a, err := newAuthStore(testAuthCfg()) // inline user "a" / sk-secret, group eng
	if err != nil {
		t.Fatal(err)
	}

	const writers = 20
	const readers = 10
	const readsPerReader = 50

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			uc := &UserConfig{Name: fmt.Sprintf("f%d", i), Group: "eng", APIKey: fmt.Sprintf("sk-file-%d", i)}
			if err := a.replaceFileUsers([]*UserConfig{uc}); err != nil {
				t.Errorf("replaceFileUsers(%d): %v", i, err)
			}
		}(i)
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.Header.Set("Authorization", "Bearer sk-secret")
			for j := 0; j < readsPerReader; j++ {
				if _, _, ok := a.identify(r); !ok {
					t.Error("inline user must remain identifiable during concurrent file-user swaps")
					return
				}
			}
		}()
	}
	wg.Wait()

	a.mu.RLock()
	got := len(a.byDigest)
	a.mu.RUnlock()
	want := len(a.inline) + 1 // inline users plus exactly one winning file-user batch
	if got != want {
		t.Fatalf("want exactly one file-user batch to survive concurrent swaps (size %d), got size %d — a rebuild was lost or merged with another", want, got)
	}
}

// TestAuthStore_Identify_NotBlockedByBuildMu is the regression for the
// Task-4 lock split: replaceFileUsers's writer-serialization lock (buildMu)
// must be independent from the reader lock (mu) that identify uses. The
// prior implementation held mu for the whole rebuild, which would stall
// every identify call for as long as a reload's build phase (including
// resolveSecret's potentially slow file-backed reads) took. Holding buildMu
// externally here simulates "a rebuild's build phase is in progress" — a
// correct identify must not need buildMu at all.
func TestAuthStore_Identify_NotBlockedByBuildMu(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	a.buildMu.Lock()
	defer a.buildMu.Unlock()

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")

	done := make(chan bool, 1)
	go func() {
		_, _, ok := a.identify(r)
		done <- ok
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("want identify to succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("identify blocked while buildMu is held — reader lock must not depend on the writer-serialization lock")
	}
}
