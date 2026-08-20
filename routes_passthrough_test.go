package traefikllmgateway

import (
	"bytes"
	"compress/gzip"
	"context"
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
