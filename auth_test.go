package traefikllmgateway

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
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
	if !grp.allowsModel("openai/gpt-5-mini") {
		t.Error("prefixed form should match")
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
