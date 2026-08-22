package traefikllmgateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- unit tests: passthroughRoute ---

func TestPassthroughRoute(t *testing.T) {
	tests := []struct {
		path         string
		wantProvider string
		wantRest     string
		wantOK       bool
	}{
		{"/openai/v1/audio/speech", "openai", "v1/audio/speech", true},
		{"/gemini/v1beta/models/gemini-pro:generateContent", "gemini", "v1beta/models/gemini-pro:generateContent", true},
		{"/anthropic", "anthropic", "", true},
		{"/anthropic/", "anthropic", "", true},
		{"/", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			gotProvider, gotRest, gotOK := passthroughRoute(tt.path)
			if gotOK != tt.wantOK || gotProvider != tt.wantProvider || gotRest != tt.wantRest {
				t.Errorf("passthroughRoute(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.path, gotProvider, gotRest, gotOK, tt.wantProvider, tt.wantRest, tt.wantOK)
			}
		})
	}
}

// --- unit tests: extractPassthroughUsage ---

func TestExtractPassthroughUsage(t *testing.T) {
	tests := []struct {
		name         string
		typeName     string
		providerName string
		body         string
		wantModel    string
		wantUsage    usage
		wantErr      bool
	}{
		{
			name:         "openai shape",
			typeName:     providerTypeOpenAI,
			providerName: "openai",
			body:         `{"model":"gpt-native","usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			wantUsage:    usage{prompt: 7, completion: 3},
			wantModel:    "gpt-native",
		},
		{
			name:         "anthropic shape",
			typeName:     providerTypeAnthropic,
			providerName: "anthropic",
			body:         `{"model":"claude-native","usage":{"input_tokens":11,"output_tokens":4}}`,
			wantUsage:    usage{prompt: 11, completion: 4},
			wantModel:    "claude-native",
		},
		{
			name:         "gemini shape, no model field",
			typeName:     providerTypeGemini,
			providerName: "gemini",
			body:         `{"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":6}}`,
			wantUsage:    usage{prompt: 20, completion: 6},
			wantModel:    "unknown/gemini",
		},
		{
			name:         "malformed body",
			typeName:     providerTypeOpenAI,
			providerName: "openai",
			body:         `not json`,
			wantUsage:    usage{},
			wantModel:    "unknown/openai",
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUsage, gotModel, gotErr := extractPassthroughUsage(tt.typeName, tt.providerName, []byte(tt.body))
			if gotUsage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", gotUsage, tt.wantUsage)
			}
			if gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
			}
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("err = %v, want err present = %v", gotErr, tt.wantErr)
			}
		})
	}
}

// --- unit tests: hasTraversalSegment ---

func TestHasTraversalSegment(t *testing.T) {
	tests := []struct {
		rest string
		want bool
	}{
		{"v1/audio/speech", false},
		{"v1beta/models/gemini-pro:generateContent", false},
		{"a%2Fb", false},
		{"../secret", true},
		{"v1/../secret", true},
		{"..%2f..%2fsecret", true},
		{"..%2F..%2Fsecret", true},
		{"%zz", true}, // invalid percent-encoding
	}
	for _, tt := range tests {
		t.Run(tt.rest, func(t *testing.T) {
			if got := hasTraversalSegment(tt.rest); got != tt.want {
				t.Errorf("hasTraversalSegment(%q) = %v, want %v", tt.rest, got, tt.want)
			}
		})
	}
}

// --- end-to-end: native passthrough through Gateway.ServeHTTP ---

// TestHandlePassthrough_ClientKeySwappedForProviderKey proves the client's
// own gateway credential never reaches the upstream provider, and the
// provider's own configured key is injected in its place.
func TestHandlePassthrough_ClientKeySwappedForProviderKey(t *testing.T) {
	var gotAuth, gotAPIKeyHeader string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-provider-real"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/audio/speech", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "upstream-ok" {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}
	if strings.Contains(gotAuth, "sk-alice") {
		t.Errorf("upstream saw the client's gateway key in Authorization: %q", gotAuth)
	}
	if gotAuth != "Bearer sk-provider-real" {
		t.Errorf("Authorization = %q, want the provider's own key injected", gotAuth)
	}
	if gotAPIKeyHeader != "" {
		t.Errorf("x-api-key = %q, want empty (openai adapter never sets it)", gotAPIKeyHeader)
	}
	if string(gotBody) != `{"foo":"bar"}` {
		t.Errorf("upstream body = %q, want the client body preserved", gotBody)
	}
}

// TestHandlePassthrough_OpenAI_ClientAPIKeyHeaderStripped proves the
// client's gateway credential, presented via x-api-key rather than
// Authorization, never reaches an OpenAI passthrough upstream. openai's
// injectAuth only ever sets Authorization, so if gatewayCredentialHeaders'
// X-Api-Key entry were ever dropped from the strip set, this exact header
// would pass straight through copyHeadersExcept to the upstream (I1: the
// sibling test above authenticates via Authorization and never sends
// x-api-key at all, so it never exercised this strip entry; the anthropic
// coverage sends x-api-key but targets a provider whose own injectAuth
// always overwrites it regardless of whether the strip ran).
func TestHandlePassthrough_OpenAI_ClientAPIKeyHeaderStripped(t *testing.T) {
	var gotAPIKeyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-provider-real"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "gateway-key-must-not-leak"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/audio/speech", strings.NewReader(`{"foo":"bar"}`))
	// Gateway auth presented via x-api-key only (no Authorization header) —
	// presentedKey falls back to x-api-key when Authorization is absent.
	req.Header.Set("x-api-key", "gateway-key-must-not-leak")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotAPIKeyHeader != "" {
		t.Errorf("upstream saw x-api-key = %q, want empty (openai adapter only ever sets Authorization; the client's gateway credential must be stripped)", gotAPIKeyHeader)
	}
}

// syncFlushWriter is an http.ResponseWriter/http.Flusher test double that
// signals writeCh (non-blocking) after every Write, so a test can prove
// two upstream chunks reached the client as two distinct Write calls
// instead of one coalesced Read — a plain httptest.Recorder or the
// unified routes' recordingWriter can't rule out coalescing, since
// nothing stops both upstream flushes landing in a single client-side
// Read before either type's Write is ever inspected. Guarded by mu since
// the handler goroutine writes concurrently with the test goroutine's
// reads of buf/writes/flushes.
type syncFlushWriter struct {
	hdr     http.Header
	writeCh chan struct{}
	buf     bytes.Buffer
	mu      sync.Mutex
	status  int
	writes  int
	flushes int
}

func newSyncFlushWriter() *syncFlushWriter {
	return &syncFlushWriter{hdr: http.Header{}, writeCh: make(chan struct{}, 8)}
}

func (s *syncFlushWriter) Header() http.Header { return s.hdr }

func (s *syncFlushWriter) WriteHeader(status int) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

func (s *syncFlushWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, _ := s.buf.Write(b)
	s.writes++
	s.mu.Unlock()
	select {
	case s.writeCh <- struct{}{}:
	default:
	}
	return n, nil
}

func (s *syncFlushWriter) Flush() {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
}

func (s *syncFlushWriter) snapshot() (status, writes, flushes int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.writes, s.flushes, s.buf.String()
}

// TestHandlePassthrough_StreamingSSE_FlushesIncrementally proves a raw
// SSE passthrough is forwarded to the client chunk by chunk, flushing
// after each one, instead of buffered until the response completes. The
// fake upstream blocks between its two chunks on continueCh, which the
// test only closes after observing (via writeCh) that the first chunk
// already reached the client as its own Write — this rules out the two
// chunks getting coalesced into a single client-side Read/Write, which a
// plain "write and flush twice with no synchronization" fake upstream
// cannot rule out.
func TestHandlePassthrough_StreamingSSE_FlushesIncrementally(t *testing.T) {
	continueCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = w.Write([]byte("data: chunk-1\n\n"))
		fl.Flush()
		<-continueCh
		_, _ = w.Write([]byte("data: chunk-2\n\n"))
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/native-stream", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	sw := newSyncFlushWriter()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(sw, req)
		close(done)
	}()

	select {
	case <-sw.writeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first chunk to reach the client")
	}
	_, writesAfterFirst, _, bodyAfterFirst := sw.snapshot()
	if writesAfterFirst != 1 {
		t.Fatalf("writes after first chunk = %d, want exactly 1 (chunks must not coalesce)", writesAfterFirst)
	}
	if !strings.Contains(bodyAfterFirst, "chunk-1") || strings.Contains(bodyAfterFirst, "chunk-2") {
		t.Fatalf("body after first chunk = %q, want chunk-1 only", bodyAfterFirst)
	}

	close(continueCh)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ServeHTTP to finish")
	}

	status, writes, flushes, body := sw.snapshot()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if writes < 2 {
		t.Fatalf("want >=2 writes (one per upstream chunk), got %d", writes)
	}
	if flushes < writes {
		t.Fatalf("want a flush per write (incremental delivery), got %d writes and %d flushes", writes, flushes)
	}
	if !strings.Contains(body, "chunk-1") || !strings.Contains(body, "chunk-2") {
		t.Errorf("body missing streamed chunks, got %q", body)
	}
}

// TestHandlePassthrough_HopByHopHeadersStripped proves hop-by-hop headers
// are stripped in both directions, while ordinary headers pass through
// untouched.
func TestHandlePassthrough_HopByHopHeadersStripped(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Connection", "close") // hop-by-hop; must not reach the client
		w.Header().Set("X-Custom", "keep-me") // ordinary header; must reach the client
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Te", "trailers")
	req.Header.Set("X-Client-Custom", "keep-me-too")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotHeaders.Get("Te") != "" {
		t.Errorf("upstream saw hop-by-hop Te header: %q", gotHeaders.Get("Te"))
	}
	if gotHeaders.Get("X-Client-Custom") != "keep-me-too" {
		t.Errorf("upstream did not see ordinary header X-Client-Custom")
	}
	if rec.Header().Get("Connection") != "" {
		t.Errorf("client saw hop-by-hop Connection header from upstream response: %q", rec.Header().Get("Connection"))
	}
	if rec.Header().Get("X-Custom") != "keep-me" {
		t.Errorf("client did not see ordinary response header X-Custom")
	}
}

// TestHandlePassthrough_NonStreamJSON_AccountsUsage proves a non-streaming
// application/json response's usage is extracted and accounted against
// the caller's own limiter counters.
func TestHandlePassthrough_NonStreamJSON_AccountsUsage(t *testing.T) {
	const respBody = `{"model":"gpt-native-x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	// SHOULD-5 (v0.22 review round): production spawns recordProviderAttempt's
	// store write in its own goroutine, off this route's TTFB path — this
	// test asserts on the resulting counter immediately after ServeHTTP
	// returns, so it overrides spawn to run synchronously instead (the
	// deterministic-test half of that dependency-injection field; see
	// limiter.spawn's own doc comment, limits.go).
	gw.limiter.spawn = func(f func()) { f() }

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 7 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 7", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 3 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 3", tokOut, ok)
	}

	// withTotalScope (handlePassthrough, routes_passthrough.go) must have
	// appended the synthetic total scope too (v0.2 data-layer task).
	totalTokIn, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricTokIn, windowDay, time.Now())
	if !ok || totalTokIn != 7 {
		t.Errorf("total tokin/day counter = %d (ok=%v), want 7", totalTokIn, ok)
	}
	totalTokOut, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricTokOut, windowDay, time.Now())
	if !ok || totalTokOut != 3 {
		t.Errorf("total tokout/day counter = %d (ok=%v), want 3", totalTokOut, ok)
	}

	// Feature A (v0.22): handlePassthrough wraps r's context with an
	// attemptRecorder before calling g.proxyUpstream — provider-level
	// only (model="": the upstream model here lives in the response body,
	// read only after this attempt already resolved, per
	// recordProviderAttempt's own doc comment, limits.go).
	attempts, ok := gw.limiter.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, time.Now())
	if !ok || attempts != 1 {
		t.Errorf("provider attempts/day = %d (ok=%v), want 1", attempts, ok)
	}
	fails, _ := gw.limiter.getCounter(kindProvider, "openai", metricProvFail, windowDay, time.Now())
	if fails != 0 {
		t.Errorf("provider fails/day = %d, want 0", fails)
	}
}

// TestHandlePassthrough_DeadUpstream_RecordsProviderFailure proves a
// connection-level failure (proxyUpstream's client.Do returning a network
// error) still reports one provider-level attempt AND one failure —
// Feature A (v0.22): passthrough makes no retry.go attempt at all, so
// this exercises proxyUpstream's own attemptRecorderFromContext call
// directly, not retryPolicy.do's.
func TestHandlePassthrough_DeadUpstream_RecordsProviderFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // closed before any request: every dial fails

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: deadURL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	// SHOULD-5 (v0.22 review round): see the identical override in
	// TestHandlePassthrough_NonStreamJSON_AccountsUsage above.
	gw.limiter.spawn = func(f func()) { f() }

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}

	attempts, ok := gw.limiter.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, time.Now())
	if !ok || attempts != 1 {
		t.Errorf("provider attempts/day = %d (ok=%v), want 1", attempts, ok)
	}
	fails, ok := gw.limiter.getCounter(kindProvider, "openai", metricProvFail, windowDay, time.Now())
	if !ok || fails != 1 {
		t.Errorf("provider fails/day = %d (ok=%v), want 1", fails, ok)
	}
}

// TestHandlePassthrough_GroupDeniesProvider_Returns403 covers a provider
// the group's providers allowlist does not include.
func TestHandlePassthrough_GroupDeniesProvider_Returns403(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai":    {Type: "openai", APIKey: "k"},
		"anthropic": {Type: "anthropic", APIKey: "k"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {Providers: []string{"anthropic"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

// TestServeHTTP_UnknownProviderPrefix_Returns404 covers a first path
// segment that names no configured provider: the request must fall
// through to the ordinary unknown-route 404, never reaching auth.
func TestServeHTTP_UnknownProviderPrefix_Returns404(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/notaprovider/v1/thing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlePassthrough_UpgradeHeader_Returns501 covers a websocket
// upgrade request against a passthrough route, which this gateway cannot
// proxy.
func TestHandlePassthrough_UpgradeHeader_Returns501(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/realtime", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlePassthrough_DeadUpstream_Returns502 covers a configured
// provider whose upstream is unreachable.
func TestHandlePassthrough_DeadUpstream_Returns502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // upstream is now unreachable

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: deadURL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlePassthrough_AcceptEncodingStripped_AccountsUsageThroughGzip
// proves a client's own "Accept-Encoding: gzip" (sent by every mainstream
// OpenAI SDK, Node fetch, and curl --compressed) never reaches the
// upstream verbatim: stripping it lets Go's Transport negotiate and
// transparently decompress gzip itself, so the accounting JSON parse
// still sees plaintext instead of silently failing on raw gzip bytes.
func TestHandlePassthrough_AcceptEncodingStripped_AccountsUsageThroughGzip(t *testing.T) {
	const respBody = `{"model":"gpt-native-x","usage":{"prompt_tokens":7,"completion_tokens":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(respBody))
			_ = gz.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want decompressed upstream body %q", rec.Body.String(), respBody)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 7 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 7 — usage must still be accounted through gzip", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 3 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 3 — usage must still be accounted through gzip", tokOut, ok)
	}
}

// TestHandlePassthrough_TraversalPath_Returns400 covers both a literal
// and a percent-encoded ".." segment in rest; neither must ever reach the
// upstream.
func TestHandlePassthrough_TraversalPath_Returns400(t *testing.T) {
	upstreamCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, path := range []string{
		"/openai/../secret",
		"/openai/..%2f..%2fsecret",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a rejected traversal path")
	}
}

// TestHandlePassthrough_EncodedSlashSegment_PreservedAtUpstream proves a
// legitimately percent-encoded "/" within one rest segment (e.g. a
// resource id that itself contains a slash) reaches the upstream in its
// original encoded form, not silently decoded into an extra path
// separator.
func TestHandlePassthrough_EncodedSlashSegment_PreservedAtUpstream(t *testing.T) {
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/files/a%2Fb", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotRequestURI, "a%2Fb") {
		t.Errorf("upstream RequestURI = %q, want the encoded \"a%%2Fb\" preserved, not decoded", gotRequestURI)
	}
}

// TestHandlePassthrough_OversizedJSONBody_ClientGetsFullBody_RequestOnlyAccounting
// proves a JSON response body larger than maxAccountingTeeBytes still
// reaches the client in full, while the accounting parse is skipped —
// only the request itself was already accounted, by checkAndCount.
func TestHandlePassthrough_OversizedJSONBody_ClientGetsFullBody_RequestOnlyAccounting(t *testing.T) {
	// Pad well past maxAccountingTeeBytes (4MiB) with an oversized field
	// ahead of "usage", so the tee's cap is hit before "usage" is reached.
	padding := strings.Repeat("x", maxAccountingTeeBytes+1024)
	respBody := `{"model":"gpt-native-x","padding":"` + padding + `","usage":{"prompt_tokens":7,"completion_tokens":3}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != respBody {
		t.Errorf("client body = %d bytes, want the full %d bytes (client copy must never truncate)", rec.Body.Len(), len(respBody))
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 0 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 0 — usage accounting must be skipped over the tee cap", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 0 — usage accounting must be skipped over the tee cap", tokOut, ok)
	}
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1 — the request itself is still accounted", reqCount, ok)
	}
}

// TestHandlePassthrough_QueryStringForwarded proves the client's query
// string reaches the upstream via RawQuery, unmodified.
func TestHandlePassthrough_QueryStringForwarded(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing?a=1&b=2", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotQuery != "a=1&b=2" {
		t.Errorf("upstream RawQuery = %q, want %q", gotQuery, "a=1&b=2")
	}
}

// TestHandlePassthrough_AnthropicClientAPIKeyStripped_ProviderKeyInjected
// proves an anthropic passthrough client's own x-api-key — the gateway's
// own auth header, which happens to share anthropic's own upstream
// credential header name — never reaches the upstream: the provider's own
// key is injected in its place.
func TestHandlePassthrough_AnthropicClientAPIKeyStripped_ProviderKeyInjected(t *testing.T) {
	var gotAPIKeyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: "anthropic", BaseURL: srv.URL, APIKey: "sk-provider-real"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "client-key-should-not-leak"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "client-key-should-not-leak")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotAPIKeyHeader != "sk-provider-real" {
		t.Errorf("x-api-key = %q, want the provider's own key injected", gotAPIKeyHeader)
	}
}

// TestHandlePassthrough_ContextDeadlineExceeded_RecordsProviderFailure is
// the passthrough half of the route-level regression the review's
// blocker demanded (v0.22, round 2) — a real context deadline against a
// real sleeping httptest upstream. Passthrough was NEVER actually broken
// by the multi-%w bug (proxyUpstream's client.Do error reaches
// attemptRecorderFromContext bare, never re-wrapped by upstreamBytes —
// see recordProviderAttempt's own call site here, routes_passthrough.go),
// but the review round proved that by observation, not a route-level
// assertion; this pins it so a future refactor cannot silently regress
// it the same way the unified route regressed.
func TestHandlePassthrough_ContextDeadlineExceeded_RecordsProviderFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond) // well past the request's own 60ms deadline below
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	gw.limiter.spawn = func(f func()) { f() }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	now := time.Now()
	attempts, _ := gw.limiter.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, now)
	if attempts != 1 {
		t.Errorf("provider attempts/day = %d, want 1", attempts)
	}
	fails, _ := gw.limiter.getCounter(kindProvider, "openai", metricProvFail, windowDay, now)
	if fails != 1 {
		t.Errorf("provider fails/day = %d, want 1 — a real context-deadline timeout must count as a provider-health failure (SHOULD-1)", fails)
	}
}

// --- security+performance audit, 2026-08-22: model enforcement, per-
// provider toggle, per-group path allowlist, dangerous-header stripping ---

// errReadCloser is an io.ReadCloser whose Read always fails, used to
// exercise peekPassthroughModel's own body-read-failure path.
type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

// TestHandlePassthrough_ModelAllowed_Proxied proves a passthrough JSON
// body whose "model" field matches the group's model glob is proxied
// normally.
func TestHandlePassthrough_ModelAllowed_Proxied(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"gpt-allowed"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"gpt-allowed"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called for an allowed model")
	}
}

// TestHandlePassthrough_ModelDenied_Returns403 closes the cheap-to-
// expensive-model bypass: a passthrough JSON body naming a model the
// group's glob does not match must never reach the upstream.
func TestHandlePassthrough_ModelDenied_Returns403(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"gpt-allowed"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/fine-tuning/jobs", strings.NewReader(`{"model":"gpt-expensive"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a denied model")
	}
}

// TestHandlePassthrough_ModelAllowed_ProviderPrefixedForm proves model
// enforcement checks both the bare model id and the "provider/model" form
// — the identical dual-candidate matcher the unified route's own
// resolveAgainst applies (registry.go) — so a group glob written against
// the prefixed form still authorizes a native passthrough request naming
// the bare upstream id.
func TestHandlePassthrough_ModelAllowed_ProviderPrefixedForm(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"openai/gpt-x"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"gpt-x"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: the group's provider-prefixed glob authorizes the bare upstream id")
	}
}

// TestHandlePassthrough_NoInspectableModel_FallsBackToProviderOnly covers
// every "no inspectable model" shape handlePassthrough's own doc comment
// documents: a non-JSON Content-Type (a multipart upload — parakeet-mlx's
// transcription passthrough shape). Even though the group's Models glob
// would deny the literal string used here if it were checked, the request
// must still succeed — falling back to provider-only authorization,
// unchanged from before model enforcement existed.
func TestHandlePassthrough_NoInspectableModel_FallsBackToProviderOnly(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"gpt-allowed"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/audio/transcriptions", strings.NewReader("--boundary\r\nfake multipart body\r\n--boundary--"))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (falls back to provider-only auth), body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: a non-JSON body has no inspectable model, so model enforcement must not block it")
	}
}

// TestHandlePassthrough_ModelBodyReadFailure_Returns400 covers
// peekPassthroughModel's own body-read-failure path, distinct from a body
// that merely fails to decode as JSON (which falls back to provider-only,
// not an error).
func TestHandlePassthrough_ModelBodyReadFailure_Returns400(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", nil)
	req.Body = errReadCloser{err: errors.New("boom: connection reset")}
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called when the request body cannot be read")
	}
}

// TestHandlePassthrough_ProviderPassthroughDisabled_Returns404 is the
// per-provider toggle's off case: ProviderConfig.Passthrough=false makes
// the native passthrough route behave exactly like an unconfigured
// provider — the ordinary unknown-route 404, without even reaching auth
// (no Authorization header is sent here, mirroring
// TestServeHTTP_UnknownProviderPrefix_Returns404's own proof style).
func TestHandlePassthrough_ProviderPassthroughDisabled_Returns404(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	disabled := false
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Passthrough: &disabled},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called when the provider's passthrough is disabled")
	}
}

// TestHandlePassthrough_ProviderPassthroughExplicitTrue_StillEnabled
// proves Passthrough=true (not just the nil default) keeps native
// passthrough reachable — the toggle's other explicit value, not only its
// absence.
func TestHandlePassthrough_ProviderPassthroughExplicitTrue_StillEnabled(t *testing.T) {
	enabled := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Passthrough: &enabled},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlePassthrough_PassthroughPaths_Restrictive proves a non-empty
// GroupConfig.PassthroughPaths restricts which rest path the group may
// address, while still allowing the listed one.
func TestHandlePassthrough_PassthroughPaths_Restrictive(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {PassthroughPaths: []string{"v1/chat/completions"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deniedReq := httptest.NewRequest(http.MethodGet, "/openai/v1/files", nil)
	deniedReq.Header.Set("Authorization", "Bearer sk-alice")
	deniedRec := httptest.NewRecorder()
	h.ServeHTTP(deniedRec, deniedReq)
	if deniedRec.Code != http.StatusForbidden {
		t.Fatalf("status for v1/files = %d, want 403, body=%s", deniedRec.Code, deniedRec.Body.String())
	}

	allowedReq := httptest.NewRequest(http.MethodGet, "/openai/v1/chat/completions", nil)
	allowedReq.Header.Set("Authorization", "Bearer sk-alice")
	allowedRec := httptest.NewRecorder()
	h.ServeHTTP(allowedRec, allowedReq)
	if allowedRec.Code != http.StatusOK {
		t.Fatalf("status for v1/chat/completions = %d, want 200, body=%s", allowedRec.Code, allowedRec.Body.String())
	}

	if len(gotPaths) != 1 || gotPaths[0] != "/v1/chat/completions" {
		t.Errorf("upstream paths reached = %v, want exactly one call for the allowlisted path", gotPaths)
	}
}

// TestHandlePassthrough_DangerousHeadersStripped proves every
// client-supplied identity/forwarding header, plus the provider-billing-
// retargeting headers, are stripped before the request reaches the
// upstream provider — additive to the existing hop-by-hop/gateway-
// credential/Accept-Encoding strips.
func TestHandlePassthrough_DangerousHeadersStripped(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Forwarded", `for=203.0.113.1`)
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	req.Header.Set("X-Forwarded-User", "spoofed-admin")
	req.Header.Set("X-Auth-Request-Email", "spoofed@example.com")
	req.Header.Set("X-Remote-User", "spoofed-admin")
	req.Header.Set("X-Remote-Groups", "admin")
	req.Header.Set("Cookie", "session=stolen")
	req.Header.Set("OpenAI-Organization", "org-not-mine")
	req.Header.Set("OpenAI-Project", "proj-not-mine")
	req.Header.Set("Anthropic-Beta", "some-beta-flag")
	req.Header.Set("X-Client-Custom", "keep-me")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, hdr := range []string{
		"Forwarded", "X-Forwarded-For", "X-Forwarded-User", "X-Auth-Request-Email",
		"X-Remote-User", "X-Remote-Groups", "Cookie",
		"Openai-Organization", "Openai-Project", "Anthropic-Beta",
	} {
		if v := got.Get(hdr); v != "" {
			t.Errorf("upstream saw %s = %q, want stripped", hdr, v)
		}
	}
	if got.Get("X-Client-Custom") != "keep-me" {
		t.Error("upstream did not see ordinary header X-Client-Custom — strip must not be a full allowlist inversion")
	}
}

// TestHandlePassthrough_ZeroConfig_NewFieldsUnset_BehavesIdenticallyToBefore
// is the GATE's own explicit requirement: a config setting NONE of this
// round's new fields (ProviderConfig.Passthrough, GroupConfig.
// PassthroughPaths) — every config that existed before this round — must
// behave exactly as it did before: the request is proxied, the client's
// gateway key is swapped for the provider's own, and the body reaches the
// upstream unmodified. Mirrors TestHandlePassthrough_
// ClientKeySwappedForProviderKey's own config shape deliberately, to
// pin the same outcome this round must not have changed.
func TestHandlePassthrough_ZeroConfig_NewFieldsUnset_BehavesIdenticallyToBefore(t *testing.T) {
	var gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer srv.Close()

	// Deliberately no Passthrough, no PassthroughPaths, no Models
	// restriction anywhere in this config.
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-provider-real"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/anything/at/all", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "upstream-ok" {
		t.Errorf("body = %q, want verbatim upstream body", rec.Body.String())
	}
	if gotAuth != "Bearer sk-provider-real" {
		t.Errorf("Authorization = %q, want the provider's own key injected, unchanged from before this round", gotAuth)
	}
	if string(gotBody) != `{"foo":"bar"}` {
		t.Errorf("upstream body = %q, want the client body preserved unchanged", gotBody)
	}
}
