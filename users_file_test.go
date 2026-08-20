package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- test helpers shared with llmgateway_test.go ---

// writeUsersDoc marshals users to the {"users":[...]} shape and writes it to
// fp. When mtime is non-zero, it also forces the file's mtime to that value
// via os.Chtimes — tests use this to make an mtime change unambiguous
// regardless of filesystem timestamp resolution, instead of relying on real
// wall-clock write timing.
func writeUsersDoc(t *testing.T, fp string, users []*UserConfig, mtime time.Time) {
	t.Helper()
	doc := struct {
		Users []*UserConfig `json:"users"`
	}{Users: users}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(fp, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(fp, mtime, mtime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
}

// writeUsersFile creates a new users file under dir/name and returns its path.
func writeUsersFile(t *testing.T, dir, name string, users []*UserConfig) string {
	t.Helper()
	fp := filepath.Join(dir, name)
	writeUsersDoc(t, fp, users, time.Time{})
	return fp
}

// identifyWithKey builds a Bearer-authenticated request for key and runs it
// through a.identify.
func identifyWithKey(a *authStore, key string) (*user, *group, bool) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return a.identify(r)
}

// stubLogger is a gatewayLogger that records calls instead of writing to
// stderr, for tests that exercise maybeReload directly.
type stubLogger struct {
	infos  []string
	errors []string
	mu     sync.Mutex
}

func (s *stubLogger) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.infos = append(s.infos, fmt.Sprintf(format, args...))
}

func (s *stubLogger) errorf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, fmt.Sprintf(format, args...))
}

func (s *stubLogger) infoCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.infos)
}

func (s *stubLogger) errorCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.errors)
}

// fakeClock is an injectable, manually-advanced clock for nowFn.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newReloadableAuthStore builds an authStore with inline users, performs the
// same synchronous initial load newGateway would perform against a users
// file seeded with initialFileUsers, and wires a stub logger plus a fake
// clock for throttle control.
func newReloadableAuthStore(t *testing.T, inline, initialFileUsers []*UserConfig) (a *authStore, fp string, log *stubLogger, clock *fakeClock) {
	t.Helper()
	dir := t.TempDir()
	fp = writeUsersFile(t, dir, "users.json", initialFileUsers)

	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"eng": {}},
		Users:     &UsersConfig{Inline: inline},
	}
	a, err := newAuthStore(cfg)
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}

	uf := newUsersFile(fp)
	initial, err := uf.load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if err := a.replaceFileUsers(initial); err != nil {
		t.Fatalf("initial replaceFileUsers: %v", err)
	}

	clock = &fakeClock{now: time.Unix(1_700_000_000, 0)}
	log = &stubLogger{}
	a.usersFile = uf
	a.log = log
	a.nowFn = clock.Now
	if info, statErr := os.Stat(fp); statErr == nil {
		a.lastModTime = info.ModTime()
	}
	a.lastCheck = clock.Now()

	return a, fp, log, clock
}

// --- usersFile.load ---

func TestUsersFile_Load_ValidFile(t *testing.T) {
	dir := t.TempDir()
	fp := writeUsersFile(t, dir, "users.json", []*UserConfig{
		{Name: "alice", Group: "eng", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerDay: 100}},
	})

	uf := newUsersFile(fp)
	users, err := uf.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %d", len(users))
	}
	got := users[0]
	if got.Name != "alice" || got.Group != "eng" || got.APIKey != "sk-alice" {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.Limits == nil || got.Limits.RequestsPerDay != 100 {
		t.Fatalf("want limits.requestsPerDay=100, got %+v", got.Limits)
	}
}

func TestUsersFile_Load_MalformedJSON_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	uf := newUsersFile(fp)
	if _, err := uf.load(); err == nil {
		t.Fatal("want error for malformed JSON")
	}
}

func TestUsersFile_Load_MissingFile_ReturnsError(t *testing.T) {
	uf := newUsersFile(filepath.Join(t.TempDir(), "nope.json"))
	if _, err := uf.load(); err == nil {
		t.Fatal("want error for a missing file")
	}
}

// --- authStore.maybeReload ---

func TestAuthStore_MaybeReload_NoFileConfigured_NoOp(t *testing.T) {
	a, err := newAuthStore(testAuthCfg())
	if err != nil {
		t.Fatal(err)
	}
	a.maybeReload() // must not panic when a.usersFile is nil
	if _, _, ok := identifyWithKey(a, "sk-secret"); !ok {
		t.Fatal("want inline user still identifiable after a no-op maybeReload")
	}
}

func TestAuthStore_MaybeReload_WithinThrottleWindow_DoesNotReload(t *testing.T) {
	a, fp, _, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})

	writeUsersDoc(t, fp, []*UserConfig{{Name: "f2", Group: "eng", APIKey: "sk-f2"}}, time.Now().Add(time.Hour))

	clock.Advance(reloadEvery - time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-f1"); !ok {
		t.Fatal("want f1 still identifiable before the throttle window elapses")
	}
	if _, _, ok := identifyWithKey(a, "sk-f2"); ok {
		t.Fatal("want f2 not yet identifiable before the throttle window elapses")
	}
}

func TestAuthStore_MaybeReload_AfterThrottleWindow_PicksUpMtimeChange(t *testing.T) {
	a, fp, _, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})

	writeUsersDoc(t, fp, []*UserConfig{{Name: "f2", Group: "eng", APIKey: "sk-f2"}}, time.Now().Add(time.Hour))

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-f2"); !ok {
		t.Fatal("want f2 identifiable once the throttle window elapses and mtime changed")
	}
	if _, _, ok := identifyWithKey(a, "sk-f1"); ok {
		t.Fatal("want f1 no longer identifiable once the reload replaced the file-sourced set")
	}
}

func TestAuthStore_MaybeReload_MalformedJSON_KeepsLastGoodSetAndLogsError(t *testing.T) {
	a, fp, log, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})

	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(fp, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-f1"); !ok {
		t.Fatal("want the last good file-sourced set retained after a malformed reload")
	}
	if log.errorCount() == 0 {
		t.Fatal("want an errorf call recorded for the malformed reload")
	}
}

func TestAuthStore_MaybeReload_UnchangedMtime_DoesNotReload(t *testing.T) {
	a, fp, log, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})
	_ = fp

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-f1"); !ok {
		t.Fatal("want f1 still identifiable when the file did not change")
	}
	if log.infoCount() != 0 {
		t.Fatal("want no count-change log when the file did not change")
	}
}

// TestAuthStore_MaybeReload_BackwardMtimeMove_TriggersReload is the
// regression for the Equal-vs-After mtime check: a file restored from a
// backup, or copied with `cp -p` from an older file, can move the mtime
// backward. That must still trigger a reload — only an mtime equal to the
// last observed one may skip it.
func TestAuthStore_MaybeReload_BackwardMtimeMove_TriggersReload(t *testing.T) {
	a, fp, _, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})

	past := time.Now().Add(-time.Hour)
	writeUsersDoc(t, fp, []*UserConfig{{Name: "f2", Group: "eng", APIKey: "sk-f2"}}, past)

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-f2"); !ok {
		t.Fatal("want a backward mtime move to still trigger a reload")
	}
	if _, _, ok := identifyWithKey(a, "sk-f1"); ok {
		t.Fatal("want f1 no longer identifiable once the backward-mtime reload replaced the file-sourced set")
	}
}

func TestAuthStore_MaybeReload_UserCountChange_LogsViaLogf(t *testing.T) {
	a, fp, log, clock := newReloadableAuthStore(t, nil, []*UserConfig{{Name: "f1", Group: "eng", APIKey: "sk-f1"}})

	writeUsersDoc(t, fp, []*UserConfig{
		{Name: "f1", Group: "eng", APIKey: "sk-f1"},
		{Name: "f2", Group: "eng", APIKey: "sk-f2"},
	}, time.Now().Add(time.Hour))

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if log.infoCount() == 0 {
		t.Fatal("want a logf call recorded for the user-count change (1 -> 2)")
	}
}

// TestAuthStore_MaybeReload_FileUserSameNameAsInline_FileWins is the merge
// semantic required by Task 4: a file user overrides an inline user of the
// same name, even when their API keys differ. The inline entry's key must
// stop working and the file entry's key must resolve to the file's data.
func TestAuthStore_MaybeReload_FileUserSameNameAsInline_FileWins(t *testing.T) {
	inline := []*UserConfig{{Name: "alice", Group: "eng", APIKey: "sk-inline-alice"}}
	a, fp, _, clock := newReloadableAuthStore(t, inline, nil)

	writeUsersDoc(t, fp, []*UserConfig{{Name: "alice", Group: "eng", APIKey: "sk-file-alice"}}, time.Now().Add(time.Hour))

	clock.Advance(reloadEvery + time.Second)
	a.maybeReload()

	if _, _, ok := identifyWithKey(a, "sk-inline-alice"); ok {
		t.Fatal("want the inline alice overridden once the file defines a same-named user")
	}
	u, _, ok := identifyWithKey(a, "sk-file-alice")
	if !ok || u.name != "alice" {
		t.Fatalf("want file alice identifiable, got %v ok=%v", u, ok)
	}
}
