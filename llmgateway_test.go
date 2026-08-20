package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCreateConfig_ReturnsEmptyConfig(t *testing.T) {
	cfg := CreateConfig()
	if cfg == nil {
		t.Fatal("CreateConfig returned nil")
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("want zero providers, got %d", len(cfg.Providers))
	}
}

func TestNew_NoProviders_ReturnsError(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, CreateConfig(), "llmgw")
	if err == nil {
		t.Fatal("want error for config with no providers, got nil")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

func TestNew_NilConfig_ReturnsError(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, nil, "llmgw")
	if err == nil {
		t.Fatal("want error for nil config, got nil")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

func TestGateway_PassthroughUnknown_ForwardsToNext(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.PassthroughUnknown = true
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("want next handler called when PassthroughUnknown is true")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("want %d from next handler, got %d", http.StatusTeapot, rec.Code)
	}
}

func TestWriteOAIError_WritesEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOAIError(rec, http.StatusBadRequest, "invalid_request_error", "bad request")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want status %d, got %d", http.StatusBadRequest, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %q", ct)
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "bad request" {
		t.Fatalf("want message %q, got %q", "bad request", body.Error.Message)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("want type %q, got %q", "invalid_request_error", body.Error.Type)
	}
	wantCode := strconv.Itoa(http.StatusBadRequest)
	if body.Error.Code != wantCode {
		t.Fatalf("want code %q (JSON string), got %q", wantCode, body.Error.Code)
	}
}

func TestServeHTTP_PanicRecovery_Returns500Envelope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	func() {
		defer recoverPanic(sw, gw)
		panic("boom")
	}()

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want %d, got %d", http.StatusInternalServerError, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %q", ct)
	}
	if !strings.Contains(buf.String(), "llmgw[llmgw] ERROR ") {
		t.Fatalf("want stderr to contain error log prefix, got %q", buf.String())
	}
}

func TestGateway_Logf_WritesInfoPrefix(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "mygw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	gw.logf("hello %s", "world")

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	want := "llmgw[mygw] INFO hello world"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("want stderr to contain %q, got %q", want, buf.String())
	}
}

func TestGateway_Errorf_WritesErrorPrefix(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), next, cfg, "mygw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	gw.errorf("failed: %s", "reason")

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	want := "llmgw[mygw] ERROR failed: reason"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("want stderr to contain %q, got %q", want, buf.String())
	}
}

func TestStatusTrackingWriter_WriteHeaderSetsFlag(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	if sw.wroteHeader {
		t.Fatal("want wroteHeader false before any write")
	}
	sw.WriteHeader(http.StatusAccepted)
	if !sw.wroteHeader {
		t.Fatal("want wroteHeader true after WriteHeader")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want delegated status %d, got %d", http.StatusAccepted, rec.Code)
	}
}

func TestStatusTrackingWriter_WriteSetsFlag(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	if _, err := sw.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !sw.wroteHeader {
		t.Fatal("want wroteHeader true after an implicit-header Write")
	}
	if rec.Body.String() != "hi" {
		t.Fatalf("want delegated body %q, got %q", "hi", rec.Body.String())
	}
}

func TestStatusTrackingWriter_FlushDelegates(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	sw.Flush()
	if !rec.Flushed {
		t.Fatal("want Flush to delegate to the underlying http.Flusher")
	}
}

// TestServeHTTP_PanicAfterHeadersCommitted_DoesNotOverwriteResponse is the
// regression test for the recoverPanic header-guard: a handler that panics
// after it has already written a status code and body bytes (e.g.
// mid-stream) must not get a second, conflicting error envelope appended.
func TestServeHTTP_PanicAfterHeadersCommitted_DoesNotOverwriteResponse(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("partial")); err != nil {
			t.Fatalf("next handler Write: %v", err)
		}
		panic("boom mid-stream")
	})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.PassthroughUnknown = true
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	origStderr := os.Stderr
	pr, pw, _ := os.Pipe()
	os.Stderr = pw
	defer func() { os.Stderr = origStderr }()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	h.ServeHTTP(rec, req)

	_ = pw.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var logBuf bytes.Buffer
	if _, err := io.Copy(&logBuf, pr); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("want status left at %d (already committed by next), got %d", http.StatusOK, rec.Code)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Fatalf("want body left as %q (no error envelope appended), got %q", "partial", got)
	}
	if !strings.Contains(logBuf.String(), "llmgw[llmgw] ERROR ") {
		t.Fatalf("want panic still logged even though no envelope was written, got %q", logBuf.String())
	}
}

// TestNewGateway_UsersFile_InitialLoadFailure_ReturnsConstructorError is the
// fail-fast case: a malformed users file must fail plugin construction, not
// leave the plugin running with an empty file-sourced user set.
func TestNewGateway_UsersFile_InitialLoadFailure_ReturnsConstructorError(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Users = &UsersConfig{File: fp}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err == nil {
		t.Fatal("want constructor error for a malformed initial users file")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

// TestNewGateway_UsersFile_InitialLoadMissingFile_ReturnsConstructorError
// covers the other fail-fast path: a configured file that does not exist.
func TestNewGateway_UsersFile_InitialLoadMissingFile_ReturnsConstructorError(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Users = &UsersConfig{File: filepath.Join(t.TempDir(), "nope.json")}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err == nil {
		t.Fatal("want constructor error for a missing initial users file")
	}
	if h != nil {
		t.Fatal("want nil handler on error")
	}
}

// TestNewGateway_UsersFile_ValidFile_LoadsUsersSynchronously verifies the
// synchronous initial load: a file-sourced user must be identifiable
// immediately after New returns, with no reload wait.
func TestNewGateway_UsersFile_ValidFile_LoadsUsersSynchronously(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "users.json")
	writeUsersDoc(t, fp, []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}, time.Time{})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{File: fp}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	if _, _, ok := identifyWithKey(gw.auth, "sk-alice"); !ok {
		t.Fatal("want file-sourced user identifiable immediately after construction")
	}
}

// TestServeHTTP_NoUsersFile_MaybeReloadIsNoOp verifies that entry-time
// maybeReload is a cheap no-op — and, crucially, does not panic — when no
// users file is configured.
func TestServeHTTP_NoUsersFile_MaybeReloadIsNoOp(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.PassthroughUnknown = true
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("want next handler called")
	}
}

// TestServeHTTP_TriggersUsersFileReload is the wire-up regression: ServeHTTP
// must call g.auth.maybeReload() at entry, not just newGateway's initial
// load. It drives a real request through ServeHTTP (never calling
// maybeReload directly) and expects the file change to have been picked up.
func TestServeHTTP_TriggersUsersFileReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "users.json")
	writeUsersDoc(t, fp, []*UserConfig{{Name: "f1", Group: "default", APIKey: "sk-f1"}}, time.Time{})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{File: fp}
	cfg.PassthroughUnknown = true
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	// Force the throttle window open and give the file an unambiguous
	// future mtime, then drive one request through ServeHTTP.
	clock := &fakeClock{now: gw.auth.nowFn().Add(reloadEvery + time.Second)}
	gw.auth.nowFn = clock.Now
	writeUsersDoc(t, fp, []*UserConfig{{Name: "f2", Group: "default", APIKey: "sk-f2"}}, time.Now().Add(time.Hour))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	h.ServeHTTP(rec, req)

	if _, _, ok := identifyWithKey(gw.auth, "sk-f2"); !ok {
		t.Fatal("want ServeHTTP's entry-time maybeReload call to have picked up the file change")
	}
}

// TestNewGateway_RejectsInvalidGroupLimits is carried-item (a): a negative
// limit value in a group's LimitsConfig must fail plugin construction, not
// silently behave as unlimited.
func TestNewGateway_RejectsInvalidGroupLimits(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Limits: &LimitsConfig{RequestsPerDay: -1}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for a negative group limit")
	}
}

// TestNewGateway_RejectsInvalidUserLimits mirrors the group case for an
// inline user's own limits override.
func TestNewGateway_RejectsInvalidUserLimits(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "a", Group: "default", APIKey: "sk-a", Limits: &LimitsConfig{CostPerMonthUSD: -1}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for a negative user limit")
	}
}

// TestNewGateway_RejectsInvalidFileUserLimits covers the file-sourced user
// path (attachUsersFile -> replaceFileUsers -> buildEntry), the other
// caller of LimitsConfig.validate besides newAuthStore's inline loop.
func TestNewGateway_RejectsInvalidFileUserLimits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "users.json")
	writeUsersDoc(t, fp, []*UserConfig{
		{Name: "f", Group: "default", APIKey: "sk-f", Limits: &LimitsConfig{TokensPerDay: -1}},
	}, time.Time{})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{File: fp}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for a negative file-sourced user limit")
	}
}

// TestNewGateway_NoRedis_UsesMemoryLimiterFailOpenDefaultTrue is
// carried-item (d)'s "else" branch: no Redis configured wires
// newLimiter(nil, true) — a nil store (memoryStore fallback only) and
// failOpen defaulted true.
func TestNewGateway_NoRedis_UsesMemoryLimiterFailOpenDefaultTrue(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	if gw.limiter == nil {
		t.Fatal("want a non-nil limiter")
	}
	if gw.limiter.store != nil {
		t.Errorf("gw.limiter.store = %#v, want nil (no Redis configured)", gw.limiter.store)
	}
	if !gw.limiter.failOpen {
		t.Error("gw.limiter.failOpen = false, want true (default)")
	}
}

// TestNewGateway_RedisConfigured_WiresRedisStoreAndFailOpen is
// carried-item (d)'s main branch: a configured Redis block wires a
// redisStore (backed by a respClient) into the limiter, and an explicit
// FailOpen=false is honored rather than overridden by the default.
func TestNewGateway_RedisConfigured_WiresRedisStoreAndFailOpen(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	failOpen := false
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:0", FailOpen: &failOpen}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	if gw.limiter == nil {
		t.Fatal("want a non-nil limiter")
	}
	if _, ok := gw.limiter.store.(*redisStore); !ok {
		t.Errorf("gw.limiter.store = %#v (%T), want *redisStore", gw.limiter.store, gw.limiter.store)
	}
	if gw.limiter.failOpen {
		t.Error("gw.limiter.failOpen = true, want false (explicit config)")
	}
}

// TestNewGateway_RedisFailOpenNilDefaultsTrue asserts a configured Redis
// block with FailOpen left nil defaults to true, matching the no-Redis
// default.
func TestNewGateway_RedisFailOpenNilDefaultsTrue(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:0"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	if !gw.limiter.failOpen {
		t.Error("gw.limiter.failOpen = false, want true (FailOpen left nil)")
	}
}

// TestNewGateway_RedisConfigured_EmptyAddress_ReturnsConstructorError
// asserts a Redis block with no address fails construction immediately,
// rather than deferring the error to the first lazy-connect attempt on
// the request path.
func TestNewGateway_RedisConfigured_EmptyAddress_ReturnsConstructorError(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Redis = &RedisConfig{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for a Redis block with an empty address")
	}
}

// TestNewGateway_RedisConfigured_NegativeDB_ReturnsConstructorError is
// review item 5: a negative Redis.DB must fail construction immediately,
// same as an empty address, rather than reaching SELECT -1 on the wire.
func TestNewGateway_RedisConfigured_NegativeDB_ReturnsConstructorError(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:0", DB: -1}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for a negative Redis db")
	}
}

// TestNewGateway_RedisPassword_ResolvedViaSecret is an end-to-end proof
// that newGateway resolves Config.Redis.Password through resolveSecret
// (env:/file:/literal) before handing it to respClient: an "env:" password
// must arrive at the fake server as its resolved value, not the literal
// "env:..." string.
func TestNewGateway_RedisPassword_ResolvedViaSecret(t *testing.T) {
	t.Setenv("LLMGW_TEST_REDIS_PASSWORD", "resolved-pw")

	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"AUTH", "resolved-pw"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$-1\r\n")},
	})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Redis = &RedisConfig{Address: ln.Addr().String(), Password: "env:LLMGW_TEST_REDIS_PASSWORD"} // #nosec G101 -- not a credential, a resolveSecret "env:" reference to an env var name
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	store, ok := gw.limiter.store.(*redisStore)
	if !ok {
		t.Fatalf("gw.limiter.store = %#v (%T), want *redisStore", gw.limiter.store, gw.limiter.store)
	}
	if _, err := store.get("k"); err != nil {
		t.Fatalf("store.get: %v (want the resolved password to authenticate against the fake server)", err)
	}
}

// TestNewGateway_RedisPassword_UnresolvableSecret_ReturnsConstructorError
// asserts an "env:" password referencing an unset variable fails
// construction with resolveSecret's error, rather than deferring to a
// confusing AUTH failure at request time.
func TestNewGateway_RedisPassword_UnresolvableSecret_ReturnsConstructorError(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:0", Password: "env:LLMGW_TEST_REDIS_PASSWORD_UNSET"} // #nosec G101 -- not a credential, a resolveSecret "env:" reference to an env var name
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if _, err := New(context.Background(), next, cfg, "llmgw"); err == nil {
		t.Fatal("want constructor error for an unresolvable Redis password secret")
	}
}

// TestNewGateway_SetsPricingWarnFn is carried-item (d)'s pricing wiring:
// New must call setPricingWarnFn so an unpriced model's warning reaches
// the gateway's own log, not pricing.go's default no-op.
func TestNewGateway_SetsPricingWarnFn(t *testing.T) {
	warnedModelsMu.Lock()
	prevWarned, prevCapNotified := warnedModels, warnCapNotified
	warnedModels = map[string]bool{}
	warnCapNotified = false
	warnedModelsMu.Unlock()
	t.Cleanup(func() {
		warnedModelsMu.Lock()
		warnedModels, warnCapNotified = prevWarned, prevCapNotified
		warnedModelsMu.Unlock()
		setPricingWarnFn(func(string) {})
	})

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "mygw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = h.(*Gateway)

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	costMicros("totally-unpriced-model-for-newgateway-test", usage{prompt: 1}, nil)

	_ = w.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if !strings.Contains(buf.String(), "llmgw[mygw]") {
		t.Fatalf("want the pricing warning routed through the gateway's own logf, got %q", buf.String())
	}
}
