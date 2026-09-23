package traefikllmgateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
			// F2 fix (BLOCKING, 2026-08-23 review): reverting
			// extractPassthroughUsage's providerTypeAnthropic case back
			// to input_tokens alone (dropping the cache_creation_input_
			// tokens/cache_read_input_tokens terms) must fail this case —
			// this is the production /{provider}/... native passthrough
			// route, and the previous version silently under-billed a
			// cache-heavy tenant by orders of magnitude with zero test
			// coverage catching it.
			name:         "anthropic shape with prompt-cache tokens billed (F2)",
			typeName:     providerTypeAnthropic,
			providerName: "anthropic",
			body:         `{"model":"claude-native","usage":{"input_tokens":4,"cache_creation_input_tokens":180000,"cache_read_input_tokens":20000,"output_tokens":9}}`,
			wantUsage:    usage{prompt: 200004, completion: 9},
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

// --- unit tests: scanTopLevelModel (review fix, 2026-08-22, round 2) ---

func TestScanTopLevelModel(t *testing.T) {
	tests := []struct {
		name      string
		head      string
		wantModel string
		wantFound bool
	}{
		{
			name:      "model is the first key",
			head:      `{"model":"gpt-4","temperature":0.7}`,
			wantModel: "gpt-4",
			wantFound: true,
		},
		{
			name:      "model after a nested object",
			head:      `{"metadata":{"a":1,"b":[1,2,3]},"model":"gpt-4"}`,
			wantModel: "gpt-4",
			wantFound: true,
		},
		{
			name:      "model after a nested array of objects",
			head:      `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"there"}],"model":"gpt-4"}`,
			wantModel: "gpt-4",
			wantFound: true,
		},
		{
			name:      "model found even though the document is truncated AFTER it",
			head:      `{"model":"gpt-4","messages":[{"role":"user","content":"this array is never closed`,
			wantModel: "gpt-4",
			wantFound: true,
		},
		{
			name:      "truncated BEFORE model appears: not found",
			head:      `{"messages":[{"role":"user","content":"a long message that eats the whole peek window and mo`,
			wantFound: false,
		},
		{
			name:      "empty object: not found",
			head:      `{}`,
			wantFound: false,
		},
		{
			name:      "not a JSON object at the top level: not found",
			head:      `["model","gpt-4"]`,
			wantFound: false,
		},
		{
			name:      "model value is not a string: not found",
			head:      `{"model":4}`,
			wantFound: false,
		},
		{
			name:      "model value is an empty string: not found",
			head:      `{"model":""}`,
			wantFound: false,
		},
		{
			name:      "empty head: not found",
			head:      ``,
			wantFound: false,
		},
		{
			name:      "malformed JSON: not found",
			head:      `not json at all`,
			wantFound: false,
		},
		{
			name:      "deeply nested value ahead of model, correctly skipped",
			head:      `{"a":{"b":{"c":[1,[2,3],{"d":4}]}},"model":"gpt-4"}`,
			wantModel: "gpt-4",
			wantFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotModel, gotFound, _ := scanTopLevelModel([]byte(tt.head))
			if gotFound != tt.wantFound {
				t.Errorf("found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotFound && gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
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

		// Round 3 (security review, 2026-08-22): path-parameter form
		// ("..;/x", Tomcat/Spring's RFC 3986 §3.3 convention) — the
		// segment's identity is everything before its first ";".
		{"..;/x", true},
		{"v1/foo;bar/x", false}, // a genuine path parameter on a non-".." segment is not itself traversal
		{"a;../x", false},       // ";../" strips to "a", not "..", on the FIRST segment — the traversal is in a later, untouched segment here, so this specific rest has no ".." segment at all

		// Round 3: backslash normalization ("..%5c..%5cx" decodes to
		// "..\..\x" — a Windows/.NET-style separator).
		{"..%5c..%5cx", true},
		{"..%5C..%5Cx", true},
		{"v1%5cfiles", false}, // a single encoded backslash segment with no ".." component

		// Round 3: double-encoding ("%252e%252e/x" decodes ONCE to
		// "%2e%2e/x" — still containing "%", refused rather than decoded
		// a second time).
		{"%252e%252e/x", true},
		{"%252E%252E/x", true},
		// A false positive, KNOWN and accepted (SHOULD-5, round 3
		// review, corrected doc comment): "%25" is the correct, single
		// encoding of a literal "%", indistinguishable after one decode
		// pass from a genuine double-encoding — this rejects a
		// completely benign request naming a literal "%" in its path.
		{"a%25b", true},
		{"v1/100%25done", true}, // decodes once to "v1/100%done" — a real resource path, still rejected

		// Existing correct behavior must be unchanged: a legitimate
		// single-encoded "/" within one segment, literal "..", and a
		// literal "." segment (dropped from finding 4's scope, round 3
		// review — "v1/x/./y" is a legal, non-traversal path and must
		// not 400).
		{"a%2Fb%2Fc", false},
		{"v1/x/./y", false},
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

// TestHandlePassthrough_DangerousResponseHeadersStripped is the security
// review finding 5 (round 3, 2026-08-22) regression test: an upstream
// setting Set-Cookie, Access-Control-*, Strict-Transport-Security, or
// Content-Security-Policy on its response must never have those reach
// the client — an upstream (or an in-cluster MCP/A2A target) must never
// get to plant a cookie or dictate a security/CORS policy on this
// gateway's own origin. An ordinary response header, and the two headers
// this repo's own code actually reads off a response (Content-Type,
// Retry-After), must still pass through unchanged.
func TestHandlePassthrough_DangerousResponseHeadersStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=stolen; Path=/")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("Content-Security-Policy", "default-src 'evil.example'")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Upstream-Custom", "keep-me")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, hdr := range []string{
		"Set-Cookie", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
		"Strict-Transport-Security", "Content-Security-Policy",
	} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("client saw dangerous response header %s = %q, want stripped", hdr, v)
		}
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Error("client must still see Content-Type — the strip is a deny-list, not an allowlist")
	}
	if rec.Header().Get("Retry-After") != "30" {
		t.Error("client must still see Retry-After — the strip is a deny-list, not an allowlist")
	}
	if rec.Header().Get("X-Upstream-Custom") != "keep-me" {
		t.Error("client must still see an ordinary upstream response header — the strip must not be a full allowlist inversion")
	}
}

// TestHandlePassthrough_AccountIdentityResponseHeadersStripped is the
// review-routes finding 17 (2026-09 review) regression test: an upstream
// echoing its own account-identifying headers — OpenAI's
// Openai-Organization/Openai-Project, Anthropic's
// Anthropic-Organization-Id — must never have those reach the client. The
// request side already treats these as sensitive
// (providerCredentialRetargetHeaders strips a tenant's own attempt to SET
// them); this proves the matching response-side strip, so the operator's
// real upstream org/project ids never round-trip back out to the tenant
// either. An ordinary response header must still pass through unchanged.
func TestHandlePassthrough_AccountIdentityResponseHeadersStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Openai-Organization", "org-secret123")
		w.Header().Set("Openai-Project", "proj-secret456")
		w.Header().Set("Anthropic-Organization-Id", "org-secret789")
		w.Header().Set("X-Upstream-Custom", "keep-me")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, hdr := range []string{"Openai-Organization", "Openai-Project", "Anthropic-Organization-Id"} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("client saw account-identity response header %s = %q, want stripped", hdr, v)
		}
	}
	if rec.Header().Get("X-Upstream-Custom") != "keep-me" {
		t.Error("client must still see an ordinary upstream response header — the strip must not be a full allowlist inversion")
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
		// Round 3 (security review, 2026-08-22): path-parameter form,
		// backslash normalization, and double-encoding, driven through
		// the full ServeHTTP dispatch rather than hasTraversalSegment
		// directly.
		"/openai/..;/secret",
		"/openai/..%5c..%5csecret",
		"/openai/%252e%252e/secret",
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

// TestHandlePassthrough_OversizedJSONBody_ClientGetsFullBody_EstimatedAccounting
// proves a JSON response body larger than maxAccountingTeeBytes still
// reaches the client in full, while the accounting PARSE is skipped (the
// tee never captured the "usage" field, which sits past the cap) — but
// (finding 2 fix, review-routes.md, supersedes this test's own former
// "request only" name and assertions) the request is no longer billed
// zero usage just because its response happened to be too large to
// parse: it falls back to the same request-body-size estimate a
// streamed response with no usable usage now also gets.
func TestHandlePassthrough_OversizedJSONBody_ClientGetsFullBody_EstimatedAccounting(t *testing.T) {
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

	// The request body here is `{}` (2 bytes): ceil(2/4) = 1 estimated
	// prompt token, 0 completion (finding 2's estimate has no basis for a
	// completion-side number, matching the unified route's own identical
	// estimate, routes_unified.go).
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 1 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 1 (estimated from the 2-byte request body)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 0", tokOut, ok)
	}
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1 — the request itself is still accounted", reqCount, ok)
	}
}

// TestCappedBodyReader is the direct unit test of cappedBodyReader
// (finding 8 fix, review-routes.md), mirroring TestReadCapped's own
// under/exactly-at/over-limit style (routes_media_test.go): a body
// exactly at the limit still completes normally (the underlying
// reader's own clean EOF ends it), and a body over the limit errors
// instead of silently truncating.
func TestCappedBodyReader(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		c := &cappedBodyReader{r: strings.NewReader("hello"), limit: 10}
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "hello" {
			t.Errorf("ReadAll = (%q, %v), want (\"hello\", nil)", got, err)
		}
		if c.bytesRead() != 5 {
			t.Errorf("bytesRead() = %d, want 5", c.bytesRead())
		}
	})
	t.Run("exactly at limit", func(t *testing.T) {
		c := &cappedBodyReader{r: strings.NewReader("0123456789"), limit: 10}
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "0123456789" {
			t.Errorf("ReadAll = (%q, %v), want (\"0123456789\", nil)", got, err)
		}
		if c.bytesRead() != 10 {
			t.Errorf("bytesRead() = %d, want 10", c.bytesRead())
		}
	})
	t.Run("over limit", func(t *testing.T) {
		c := &cappedBodyReader{r: strings.NewReader("01234567890"), limit: 10}
		_, err := io.ReadAll(c)
		if !errors.Is(err, errPassthroughBodyTooLarge) {
			t.Errorf("ReadAll err = %v, want errPassthroughBodyTooLarge", err)
		}
	})
}

// TestHandlePassthrough_BodyOverLimit_KnownContentLength_Returns413 proves
// finding 8 (review-routes.md): a request whose declared Content-Length
// already exceeds maxPassthroughBytes is rejected up front with 413,
// before the upstream is ever called.
func TestHandlePassthrough_BodyOverLimit_KnownContentLength_Returns413(t *testing.T) {
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

	body := strings.Repeat("a", maxPassthroughBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a body already over the cap via a known Content-Length")
	}
}

// TestHandlePassthrough_BodyOverLimit_ChunkedContentLength_Returns413
// proves finding 8's other half: an unknown/understated Content-Length
// (chunked transfer, or a client that lies) is caught mid-copy by
// cappedBodyReader instead of silently truncating — this exercises the
// SAME real maxPassthroughBytes cap the production code path uses,
// bypassing the upfront ContentLength check by setting it to -1.
func TestHandlePassthrough_BodyOverLimit_ChunkedContentLength_Returns413(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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

	body := strings.Repeat("a", maxPassthroughBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.ContentLength = -1 // simulate chunked transfer / unknown length, bypassing the upfront check
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (cappedBodyReader must catch this mid-copy), body=%s", rec.Code, rec.Body.String())
	}
}

// TestExtractPassthroughSSEUsage covers finding 2's own documented
// provider shapes directly: OpenAI's final-chunk "usage" object,
// Anthropic's message_start + message_delta pair (folding in prompt-
// cache counters), Gemini's deliberately-unhandled convention, and a
// malformed/empty stream — each falls back to the "unknown/"+provider
// model sentinel and zero usage when nothing usable was found.
func TestExtractPassthroughSSEUsage(t *testing.T) {
	tests := []struct {
		name         string
		typeName     string
		providerName string
		raw          string
		wantModel    string
		wantUsage    usage
	}{
		{
			name:         "openai: usage on final chunk before DONE",
			typeName:     providerTypeOpenAI,
			providerName: "openai",
			raw: "data: {\"id\":\"c1\",\"model\":\"gpt-native\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"id\":\"c1\",\"model\":\"gpt-native\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
			wantUsage: usage{prompt: 7, completion: 3},
			wantModel: "gpt-native",
		},
		{
			name:         "openai: no usage chunk at all",
			typeName:     providerTypeOpenAI,
			providerName: "openai",
			raw:          "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantUsage:    usage{},
			wantModel:    "unknown/openai",
		},
		{
			name:         "anthropic: message_start + message_delta",
			typeName:     providerTypeAnthropic,
			providerName: "anthropic",
			raw: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-native\",\"usage\":{\"input_tokens\":20,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\n" +
				"event: message_delta\ndata: {\"delta\":{},\"usage\":{\"output_tokens\":11}}\n\n",
			wantUsage: usage{prompt: 28, completion: 11},
			wantModel: "claude-native",
		},
		{
			name:         "anthropic: message_start only, no message_delta yet",
			typeName:     providerTypeAnthropic,
			providerName: "anthropic",
			raw:          "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-native\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n",
			wantUsage:    usage{prompt: 9, completion: 0},
			wantModel:    "claude-native",
		},
		{
			name:         "gemini: deliberately unhandled, falls back to unknown",
			typeName:     providerTypeGemini,
			providerName: "gemini",
			raw:          "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n",
			wantUsage:    usage{},
			wantModel:    "unknown/gemini",
		},
		{
			name:         "malformed data ignored",
			typeName:     providerTypeOpenAI,
			providerName: "openai",
			raw:          "data: not json at all\n\n",
			wantUsage:    usage{},
			wantModel:    "unknown/openai",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUsage, gotModel := extractPassthroughSSEUsage(tt.typeName, tt.providerName, []byte(tt.raw), []byte(tt.raw))
			if gotUsage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", gotUsage, tt.wantUsage)
			}
			if gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
			}
		})
	}
}

// TestHandlePassthrough_SSEStream_OpenAIUsageChunk_Accounted is the
// end-to-end regression for finding 2 (review-routes.md): a native SSE
// passthrough response carrying OpenAI's own final-chunk "usage" object
// is teed, parsed, and billed with the REPORTED usage — not an estimate
// — while the client still receives the stream byte-for-byte unchanged.
func TestHandlePassthrough_SSEStream_OpenAIUsageChunk_Accounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":9}}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"gpt-native","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("body = %q, want the stream to reach the client unchanged, including [DONE]", rec.Body.String())
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 42 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 42 (the reported usage, not an estimate)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 9 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 9", tokOut, ok)
	}
}

// TestHandlePassthrough_SSEStream_AnthropicUsageEvents_Accounted is
// finding 2's Anthropic-shaped counterpart: message_start.message.usage
// (including its prompt-cache counters) plus message_delta.usage.
// output_tokens together are billed, matching extractPassthroughUsage's
// own non-streaming Anthropic accounting exactly.
func TestHandlePassthrough_SSEStream_AnthropicUsageEvents_Accounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-native\",\"usage\":{\"input_tokens\":20,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":11}}\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"anthropic": {Type: "anthropic", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
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

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"claude-native","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 28 { // 20 input + 5 cache_creation + 3 cache_read
		t.Errorf("user tokin/day counter = %d (ok=%v), want 28 (input + cache tokens folded in)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 11 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 11 (from message_delta)", tokOut, ok)
	}
}

// TestHandlePassthrough_SSEStream_OverFourMiB_UsageStillAccounted is the
// verify-core round-4 regression test (finding 6): before the head+tail
// sseAccountingBuffer fix, a single front-loaded 4MiB
// cappedAccountingBuffer teed the whole SSE stream, so a stream over
// 4MiB lost its usage event entirely the moment that event landed past
// the cap — exactly what a long completion (well within reach of a
// reasoning model) does. This stream is ~6.8MB, deliberately mirroring
// scratchpad/vcopy/zz_verify2_test.go's TestVerify_PassthroughLongSSE_
// UsageLost repro, with its real usage (prompt=100, completion=25000) on
// the final SSE event — long past both the old 4MiB cap and comfortably
// past the new sseAccountingTailBytes (64KiB) cap too, proving the tail
// window, not just a larger head, is what recovers it.
func TestHandlePassthrough_SSEStream_OverFourMiB_UsageStillAccounted(t *testing.T) {
	chunk := "data: {\"id\":\"c1\",\"model\":\"gpt-native\",\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 200) + "\"}}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		for i := 0; i < 25000; i++ { // ~6.8MB, well over the old 4MiB cap
			_, _ = io.WriteString(w, chunk)
		}
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"gpt-native\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":25000}}\n\ndata: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"gpt-native","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 100 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 100 (the reported usage, not lost past the old 4MiB cap)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 25000 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 25000", tokOut, ok)
	}
}

// TestHandlePassthrough_SSEStream_NoUsageReported_EstimatedFallback
// proves finding 2's fallback: a clean SSE stream whose provider never
// sent a usage object falls back to the request-body-size estimate,
// instead of being billed zero.
func TestHandlePassthrough_SSEStream_NoUsageReported_EstimatedFallback(t *testing.T) {
	const reqBody = `{"model":"gpt-native","messages":[{"role":"user","content":"a body long enough that its estimate is clearly nonzero"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	wantEstimate := int64(math.Ceil(float64(len(reqBody)) / 4))
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != wantEstimate {
		t.Errorf("user tokin/day counter = %d (ok=%v), want %d (estimated from request body size)", tokIn, ok, wantEstimate)
	}
}

// TestHandlePassthrough_JSONResponse_AbortedMidCopy_EstimatedFallback
// proves finding 2's other fallback: a non-streaming JSON response that
// aborts mid-copy (the upstream connection resets partway through the
// body) still reaches the client with exactly the partial bytes it
// managed to write, and the request falls back to the request-body-size
// estimate instead of being billed zero because the partial body could
// not be parsed as usage.
func TestHandlePassthrough_JSONResponse_AbortedMidCopy_EstimatedFallback(t *testing.T) {
	const reqBody = `{"model":"gpt-native","messages":[{"role":"user","content":"abort mid copy test body"}]}`
	const partial = `{"model":"gpt-native","choices":[{"delta"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = w.Write([]byte(partial))
		fl.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Hijack")
		}
		conn, _, hjErr := hj.Hijack()
		if hjErr == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the drop)", rec.Code)
	}
	if rec.Body.String() != partial {
		t.Errorf("client body = %q, want exactly the partial bytes written before the drop", rec.Body.String())
	}

	wantEstimate := int64(math.Ceil(float64(len(reqBody)) / 4))
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != wantEstimate {
		t.Errorf("user tokin/day counter = %d (ok=%v), want %d (estimated: the truncated body could not be parsed)", tokIn, ok, wantEstimate)
	}
}

// TestHandlePassthrough_NonStreamJSON_NoUsageField_LargeBody_ZeroAccounted
// is the negative counterpart to the estimate fallback: a COMPLETE 200
// application/json response that simply carries no "usage" object at all
// (the shape of an OpenAI /v1/files upload response, or any other
// non-chat endpoint) must account exactly zero prompt/completion tokens,
// never the request-body-size estimate — the request body here is a
// large-ish 200KB payload specifically so a body-size estimate, if the
// fallback wrongly fired, would be unmistakably nonzero (canEstimate in
// handlePassthrough requires is2xx AND (isSSE || tee.truncated ||
// copyAborted); a complete, untruncated JSON body meets none of those,
// so this must stay a true accounting no-op).
func TestHandlePassthrough_NonStreamJSON_NoUsageField_LargeBody_ZeroAccounted(t *testing.T) {
	const respBody = `{"id":"file-abc123","object":"file","bytes":204800,"created_at":1699999999,"filename":"upload.bin","purpose":"fine-tune"}`
	reqBody := strings.Repeat("a", 200*1024) // 200KB, large enough that a wrongly-fired estimate would be unmistakably nonzero

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/files", strings.NewReader(reqBody))
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
	if !ok || tokIn != 0 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want exactly 0 (2xx JSON with no usage field must never estimate)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want exactly 0", tokOut, ok)
	}
}

// TestHandlePassthrough_UpstreamJSONError429_ZeroAccounted proves a
// non-2xx JSON error body (the upstream billed nothing for a rejected
// request) is never charged the request-body-size estimate either: the
// upstream is not is2xx, so handlePassthrough's canEstimate gate must
// stay false regardless of the fact that the body is JSON with no usage.
func TestHandlePassthrough_UpstreamJSONError429_ZeroAccounted(t *testing.T) {
	const reqBody = `{"model":"gpt-native","messages":[{"role":"user","content":"a request that gets rate limited"}]}`
	const respBody = `{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 0 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want exactly 0 (a 429 must never be charged the request-size estimate)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want exactly 0", tokOut, ok)
	}
}

// TestHandlePassthrough_UpstreamNonJSONError500_ZeroAccounted covers the
// OTHER early-return branch in handlePassthrough (result.status == 0 ||
// (!result.isJSON && !result.isSSE)): a 500 whose Content-Type is
// neither application/json nor text/event-stream — a plain-text upstream
// error, which this gateway never parses for usage at all — must also
// account exactly zero tokens, never an estimate. This is deliberately
// NOT an SSE stream (contrast with the SSE-no-usage estimated-fallback
// test), and deliberately NOT the JSON-error shape already covered by
// the 429 test above, so it exercises the isJSON==false && isSSE==false
// short-circuit specifically.
func TestHandlePassthrough_UpstreamNonJSONError500_ZeroAccounted(t *testing.T) {
	const reqBody = `{"model":"gpt-native","messages":[{"role":"user","content":"a request that hits a plain-text 500"}]}`
	const respBody = "internal server error"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 0 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want exactly 0 (non-JSON, non-SSE response must never be accounted)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 0 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want exactly 0", tokOut, ok)
	}
}

// TestHandlePassthrough_UnpricedModelCostBudget_Returns402 covers
// finding 6 (review-routes.md): a passthrough request for a model with
// no configured price is refused (402) exactly like the unified route's
// own F-1 gate, but ONLY when (a) a cost budget actually applies to the
// caller and (b) the model is determinable from the request body at all
// — a binary content type (whose "model" this gateway never even tries
// to read) is let through unpriced, since there is no basis to refuse a
// value the gateway never had.
func TestHandlePassthrough_UnpricedModelCostBudget_Returns402(t *testing.T) {
	tests := []struct {
		name                        string
		body                        string
		contentType                 string
		costBudget                  float64
		wantCode                    int
		allowUnpricedWithCostBudget bool
		wantUpstreamCalled          bool
		wantEvent                   bool // item 5, this round: the 402 refusal must reach the events feed
	}{
		{
			name:               "unpriced model, cost budget configured: refused",
			body:               `{"model":"totally-unpriced-model","messages":[]}`,
			contentType:        "application/json",
			costBudget:         5,
			wantCode:           http.StatusPaymentRequired,
			wantUpstreamCalled: false,
			wantEvent:          true,
		},
		{
			name:               "unpriced model, no cost budget: passes",
			body:               `{"model":"totally-unpriced-model","messages":[]}`,
			contentType:        "application/json",
			costBudget:         0,
			wantCode:           http.StatusOK,
			wantUpstreamCalled: true,
		},
		{
			name:                        "unpriced model, cost budget configured but AllowUnpricedWithCostBudget set: passes",
			body:                        `{"model":"totally-unpriced-model","messages":[]}`,
			contentType:                 "application/json",
			costBudget:                  5,
			allowUnpricedWithCostBudget: true,
			wantCode:                    http.StatusOK,
			wantUpstreamCalled:          true,
		},
		{
			name:               "priced (builtin) model, cost budget configured: passes",
			body:               `{"model":"gpt-5","messages":[]}`,
			contentType:        "application/json",
			costBudget:         5,
			wantCode:           http.StatusOK,
			wantUpstreamCalled: true,
		},
		{
			name:               "unpriced model, cost budget configured, but model unreadable (binary content type): passes",
			body:               "raw-audio-bytes-not-json",
			contentType:        "audio/mpeg",
			costBudget:         5,
			wantCode:           http.StatusOK,
			wantUpstreamCalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalled bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cfg := CreateConfig()
			cfg.AllowUnpricedWithCostBudget = tt.allowUnpricedWithCostBudget
			cfg.Providers = map[string]*ProviderConfig{
				"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
			}
			cfg.Groups = map[string]*GroupConfig{"default": {}}
			limits := &LimitsConfig{}
			if tt.costBudget > 0 {
				limits.CostPerDayUSD = tt.costBudget
			}
			cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: limits}}}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer sk-alice")
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if upstreamCalled != tt.wantUpstreamCalled {
				t.Errorf("upstreamCalled = %v, want %v", upstreamCalled, tt.wantUpstreamCalled)
			}
			if tt.wantEvent {
				events := h.(*Gateway).events.ring.snapshot(eventRingCap)
				if len(events) == 0 {
					t.Fatal("want an event recorded for the 402 refusal, got none")
				}
				ev := events[0] // newest first
				if ev.Kind != eventKindUnpriced {
					t.Errorf("Kind = %q, want %q", ev.Kind, eventKindUnpriced)
				}
				if ev.Status != http.StatusPaymentRequired {
					t.Errorf("Status = %d, want %d", ev.Status, http.StatusPaymentRequired)
				}
				if ev.Route != routePassthrough {
					t.Errorf("Route = %q, want %q", ev.Route, routePassthrough)
				}
			}
		})
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

// TestHandlePassthrough_UnrestrictedGroup_SkipsModelPeekEntirely proves
// ruling item 1 (review fix, 2026-08-22, round 2): a group with an EMPTY
// Models list — the live cluster's "home" group and every group that has
// not opted into model restriction — never even reads the request body
// for model enforcement, regardless of Content-Type or body shape. This
// is the correct "no inspectable model, and nothing to enforce anyway"
// case; TestHandlePassthrough_RestrictedGroup_BinaryContentType_Returns403
// below covers the opposite: the SAME binary body against a group that
// DOES restrict models.
func TestHandlePassthrough_UnrestrictedGroup_SkipsModelPeekEntirely(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}} // no Models restriction
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
		t.Fatalf("status = %d, want 200 (no model restriction, no peek at all), body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: an unrestricted group's request body is never read for model enforcement")
	}
}

// TestHandlePassthrough_MultiGrant_UnrestrictedGrantProvider_SkipsModelPeek
// proves handlePassthrough's hasModelRestrictionForProviderPath gate for
// a multi-grant (group + personal grant) principal: a provider reachable
// only through a grant that carries NO model restriction never even
// reads the request body — mirrors TestHandlePassthrough_
// UnrestrictedGroup_SkipsModelPeekEntirely above, but for a principal
// that also carries a personal grant restricting a DIFFERENT provider
// (proved not to interfere, by the companion test right below).
func TestHandlePassthrough_MultiGrant_UnrestrictedGrantProvider_SkipsModelPeek(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"open": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"base": {Providers: []string{"open"}}} // no Models restriction
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "base", APIKey: "sk-alice", Providers: []string{"closed"}, Models: []string{"closed/allowed"}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/open/v1/audio/transcriptions", strings.NewReader("--boundary\r\nfake multipart body\r\n--boundary--"))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (base group's own grant is unrestricted on \"open\"), body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: the grant that allows \"open\" carries no model restriction")
	}
}

// TestHandlePassthrough_MultiGrant_RestrictedPersonalGrantProvider_EnforcesModel
// is the opposite case for the SAME principal as the test above: the
// personal grant restricts "closed" to exactly one model, so a
// passthrough request against "closed" must peek and enforce it.
func TestHandlePassthrough_MultiGrant_RestrictedPersonalGrantProvider_EnforcesModel(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"open":   {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
		"closed": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"base": {Providers: []string{"open"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "base", APIKey: "sk-alice", Providers: []string{"closed"}, Models: []string{"closed/allowed"}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deniedReq := httptest.NewRequest(http.MethodPost, "/closed/v1/native-endpoint", strings.NewReader(`{"model":"closed/denied"}`))
	deniedReq.Header.Set("Authorization", "Bearer sk-alice")
	deniedReq.Header.Set("Content-Type", "application/json")
	deniedRec := httptest.NewRecorder()
	h.ServeHTTP(deniedRec, deniedReq)
	if deniedRec.Code != http.StatusForbidden {
		t.Fatalf("denied model: status = %d, want 403, body=%s", deniedRec.Code, deniedRec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a model the personal grant does not list")
	}

	allowedReq := httptest.NewRequest(http.MethodPost, "/closed/v1/native-endpoint", strings.NewReader(`{"model":"closed/allowed"}`))
	allowedReq.Header.Set("Authorization", "Bearer sk-alice")
	allowedReq.Header.Set("Content-Type", "application/json")
	allowedRec := httptest.NewRecorder()
	h.ServeHTTP(allowedRec, allowedReq)
	if allowedRec.Code != http.StatusOK {
		t.Fatalf("allowed model: status = %d, want 200, body=%s", allowedRec.Code, allowedRec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called for the personal grant's own allowed model")
	}
}

// TestHandlePassthrough_MultiGroup_PassthroughPathsNeverUnionAcrossProviders
// is HIGH-1's regression (review round 2): passthroughPaths must NOT be
// unioned across member groups the way providers/models/mcpServers/agents
// are — group "a" {providers:[alpha]} carries no path restriction at all,
// group "b" {providers:[beta], passthroughPaths:[v1/chat/completions]}
// restricts beta narrowly. A user in BOTH groups must still be denied
// "/beta/v1/files": pairing "a"'s own unrestricted paths with "b"'s own
// provider would let joining group "a" silently strip group "b"'s own
// path restriction, even though NEITHER group alone ever authorized that
// combination. "/beta/v1/chat/completions" (b's own allowed path) and
// "/alpha/v1/files" (a's own unrestricted provider) must both still work.
func TestHandlePassthrough_MultiGroup_PassthroughPathsNeverUnionAcrossProviders(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
		"beta":  {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{
		"a": {Providers: []string{"alpha"}},
		"b": {Providers: []string{"beta"}, PassthroughPaths: []string{"v1/chat/completions"}},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "multi", Group: "a", Groups: []string{"b"}, APIKey: "sk-multi"},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	post := func(path string) int {
		lastPath = ""
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer sk-multi")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("/beta/v1/files"); code != http.StatusForbidden {
		t.Errorf("POST /beta/v1/files: status = %d, want 403 (group b's own passthroughPaths restriction must not be stripped by joining group a)", code)
	}
	if lastPath != "" {
		t.Error("upstream must never be called for a path denied by the coupled provider+path check")
	}
	if code := post("/beta/v1/chat/completions"); code != http.StatusOK {
		t.Errorf("POST /beta/v1/chat/completions: status = %d, want 200 (group b's own allowed path)", code)
	}
	if code := post("/alpha/v1/files"); code != http.StatusOK {
		t.Errorf("POST /alpha/v1/files: status = %d, want 200 (group a's own unrestricted provider)", code)
	}
}

// TestHandlePassthrough_MultiGroup_ModelAndPathMustComeFromSameGrant is
// the round-3 HIGH regression (review round 3, repro P1): group
// a{providers:[beta], models:[m1]} restricts models but not paths;
// group b{providers:[beta], passthroughPaths:[v1/chat/completions]}
// restricts paths but not models. Round 2's own fix coupled provider+
// path per grant and provider+model per grant SEPARATELY — but nothing
// required BOTH to come from the SAME grant, so a multi-grant user could
// satisfy the path check via b's own unrestricted-model grant and the
// (irrelevant) model check via a's own unrestricted-path grant,
// authorizing a combination NEITHER grant alone ever granted. Every
// single-grant user (a-only, b-only) must still be denied on their own
// terms too (sanity controls, unaffected by this fix).
func TestHandlePassthrough_MultiGroup_ModelAndPathMustComeFromSameGrant(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"beta": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{
		"a": {Providers: []string{"beta"}, Models: []string{"m1"}},
		"b": {Providers: []string{"beta"}, PassthroughPaths: []string{"v1/chat/completions"}},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "ua", Group: "a", APIKey: "sk-a"},
		{Name: "ub", Group: "b", APIKey: "sk-b"},
		{Name: "uab", Group: "a", Groups: []string{"b"}, APIKey: "sk-ab"},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	post := func(key string) int {
		upstreamCalled = false
		req := httptest.NewRequest(http.MethodPost, "/beta/v1/files", strings.NewReader(`{"model":"m2"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if upstreamCalled {
			t.Errorf("key %q: upstream must never be called for a denied combination", key)
		}
		return rec.Code
	}

	if code := post("sk-a"); code != http.StatusForbidden {
		t.Errorf("a-only: status = %d, want 403 (a has no path restriction, but model m2 is not m1)", code)
	}
	if code := post("sk-b"); code != http.StatusForbidden {
		t.Errorf("b-only: status = %d, want 403 (b has no model restriction, but v1/files is not v1/chat/completions)", code)
	}
	if code := post("sk-ab"); code != http.StatusForbidden {
		t.Errorf("a+b: status = %d, want 403 — model m2 must not be authorized by combining a's own unrestricted path with b's own unrestricted model", code)
	}
}

// TestHandlePassthrough_MultiGroup_PersonalGrantModelAndPathFromSameGrant
// is the round-3 HIGH regression's personal-grant half (review round 3,
// repro P2): primary "home"{providers:[alpha]} carries no path
// restriction; member "friends"{providers:[beta], passthroughPaths:
// [v1/chat/completions]} restricts beta's own paths; carol's personal
// grant adds providers:[beta],models:[beta/m1]. Her request must be
// checked as ONE unit — the path it borrows is home's (the primary
// group's) own list, and the model it allows is its own — home's own
// unrestricted paths must never combine with friends' own unrestricted
// models to authorize a request the personal grant itself denies.
func TestHandlePassthrough_MultiGroup_PersonalGrantModelAndPathFromSameGrant(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
		"beta":  {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{
		"home":    {Providers: []string{"alpha"}},
		"friends": {Providers: []string{"beta"}, PassthroughPaths: []string{"v1/chat/completions"}},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "carol", Group: "home", Groups: []string{"friends"}, APIKey: "sk-carol", Providers: []string{"beta"}, Models: []string{"beta/m1"}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/beta/v1/files", strings.NewReader(`{"model":"beta/other"}`))
	req.Header.Set("Authorization", "Bearer sk-carol")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the personal grant's own Models list denies %q; home's own unrestricted paths and friends' own unrestricted models must not combine to authorize it)", rec.Code, "beta/other")
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a model the personal grant denies")
	}
}

// TestHandlePassthrough_MultiGroup_ModelAndPathFromSameGrant_PositiveCases
// are the round-3 HIGH fix's own positive controls, using the SAME
// fixtures as the two regression tests above: a request whose model AND
// path both come from ONE grant must still succeed.
func TestHandlePassthrough_MultiGroup_ModelAndPathFromSameGrant_PositiveCases(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
		"beta":  {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{
		"a":       {Providers: []string{"beta"}, Models: []string{"m1"}},
		"b":       {Providers: []string{"beta"}, PassthroughPaths: []string{"v1/chat/completions"}},
		"home":    {Providers: []string{"alpha"}},
		"friends": {Providers: []string{"beta"}, PassthroughPaths: []string{"v1/chat/completions"}},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "ub", Group: "b", APIKey: "sk-b"},
		{Name: "uab", Group: "a", Groups: []string{"b"}, APIKey: "sk-ab"},
		{Name: "carol", Group: "home", Groups: []string{"friends"}, APIKey: "sk-carol", Providers: []string{"beta"}, Models: []string{"beta/m1"}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	post := func(key, path, body string) int {
		upstreamCalled = false
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// b-only: b's own grant allows beta with no model restriction, and
	// b's own allowed path — must succeed regardless of model.
	if code := post("sk-b", "/beta/v1/chat/completions", `{"model":"anything"}`); code != http.StatusOK {
		t.Errorf("b-only on v1/chat/completions: status = %d, want 200", code)
	}
	if !upstreamCalled {
		t.Error("b-only: upstream must be called")
	}

	// a+b: model m1 on a path a allows (a has no path restriction) — a's
	// OWN grant covers both model and path together.
	if code := post("sk-ab", "/beta/v1/files", `{"model":"m1"}`); code != http.StatusOK {
		t.Errorf("a+b, model m1 on a's own unrestricted path: status = %d, want 200", code)
	}
	if !upstreamCalled {
		t.Error("a+b: upstream must be called")
	}

	// carol: personal grant's own model (beta/m1) on a path the PRIMARY
	// group (home) allows (home has no path restriction).
	if code := post("sk-carol", "/beta/v1/anything-home-allows", `{"model":"beta/m1"}`); code != http.StatusOK {
		t.Errorf("personal grant, model beta/m1 on home's own unrestricted path: status = %d, want 200", code)
	}
	if !upstreamCalled {
		t.Error("carol: upstream must be called")
	}
}

// TestHandlePassthrough_RestrictedGroup_BinaryContentType_Returns403 is
// ruling item 4's fail-closed case for a genuinely binary body: a
// RESTRICTED group's multipart/audio/image/video upload carries no
// inspectable "model" field by nature, and peekPassthroughModel does not
// even read it (isPassthroughBinaryContentType's skip-list) — but because
// the group opted into model restriction, "not found" still denies,
// rather than silently falling back to provider-only the way an
// unrestricted group would.
func TestHandlePassthrough_RestrictedGroup_BinaryContentType_Returns403(t *testing.T) {
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

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (restricted group, no model determinable), body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called: a restricted group with no determinable model must fail closed")
	}
}

// TestHandlePassthrough_ContentTypeBypass_Returns403 is the MUST-FIX
// regression test: the round-1 gate skipped the body read (and so the
// whole model check) for ANY Content-Type not containing
// "application/json" — entirely client-controlled. The reviewer measured
// {"model":"EXPENSIVE"} reaching the upstream with a 200 via
// "Content-Type: text/plain". This table drives the identical body
// through text/plain, an ABSENT Content-Type, and
// application/x-www-form-urlencoded — none of them binary, so all three
// must now be inspected and denied.
func TestHandlePassthrough_ContentTypeBypass_Returns403(t *testing.T) {
	contentTypes := []string{"text/plain", "", "application/x-www-form-urlencoded"}
	for _, ct := range contentTypes {
		t.Run("Content-Type="+ct, func(t *testing.T) {
			var upstreamCalled bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
			cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"gpt-cheap"}}}
			cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/openai/v1/fine-tuning/jobs", strings.NewReader(`{"model":"EXPENSIVE"}`))
			req.Header.Set("Authorization", "Bearer sk-alice")
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — the model check must not be skippable via Content-Type, body=%s", rec.Code, rec.Body.String())
			}
			if upstreamCalled {
				t.Error("upstream must never be called: EXPENSIVE is not in the group's Models glob")
			}
		})
	}
}

// TestHandlePassthrough_MatcherParity_AlreadyPrefixedBody_Allowed is the
// matcher-parity regression test (review fix, 2026-08-22, round 2): a
// body whose "model" field already carries the "provider/model" form
// (some client tooling always sends canonical ids) must be authorized
// against a BARE group glob, not false-403'd by a naive
// providerName+"/"+model concatenation producing "openai/openai/x".
func TestHandlePassthrough_MatcherParity_AlreadyPrefixedBody_Allowed(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"x"}}} // bare glob
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"openai/x"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the already-prefixed body must resolve against the bare glob, body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: splitConfiguredProvider must strip the client's own \"openai/\" prefix before matching, not double it")
	}
}

// TestHandlePassthrough_ModelBodyReadFailure_Returns400 covers
// peekPassthroughModel's own body-read-failure path, distinct from a body
// with no determinable model (which fails closed with 403, not 400). The
// group must have a Models restriction — otherwise the peek never runs
// at all (ruling item 1) and this failure is never reached.
func TestHandlePassthrough_ModelBodyReadFailure_Returns400(t *testing.T) {
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

// TestHandlePassthrough_BodyIntegrity_ByteIdentical_LargeBody proves
// peekPassthroughModel's io.MultiReader restoration (review fix,
// 2026-08-22, round 2) forwards the EXACT original body to the upstream,
// even when the body is larger than maxModelPeekBytes (64KiB) — so the
// remainder streams from the real, unread r.Body rather than a second
// buffered copy. Exercises the "read exactly the cap, more remains" path
// io.ReadFull's nil-error branch takes.
func TestHandlePassthrough_BodyIntegrity_ByteIdentical_LargeBody(t *testing.T) {
	padding := strings.Repeat("x", maxModelPeekBytes*2) // forces the body well past the 64KiB peek cap
	body := `{"model":"gpt-allowed","padding":"` + padding + `"}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	// Deliberately UNRESTRICTED (no Models list): this test's own purpose is
	// byte-for-byte forwarding integrity for a large body, independent of
	// model enforcement — a RESTRICTED group's own behavior for a body
	// this large is covered separately (round-3 fix, security review
	// 2026-08-22: TestHandlePassthrough_PaddedDuplicateModelBeyondPeekWindow_Returns403
	// and TestHandlePassthrough_RestrictedGroup_LargeBodyNeverClosesWindow_Returns403
	// below), and now deliberately DENIES a body this large that never
	// closes within the peek window — the opposite of what this test
	// checks. Using an unrestricted group here keeps this test's own
	// single concern (byte integrity) unaffected by that unrelated fix.
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if string(gotBody) != body {
		t.Errorf("upstream body length = %d, want %d bytes byte-identical to the original", len(gotBody), len(body))
	}
}

// TestHandlePassthrough_BodyIntegrity_ChunkedContentLength proves the
// same byte-identical forwarding when the request arrives with
// ContentLength < 0 (chunked transfer / unknown length — httptest's own
// stand-in for what a real chunked client connection produces), the
// variant the reviewer specifically asked for alongside the large-body
// case above.
func TestHandlePassthrough_BodyIntegrity_ChunkedContentLength(t *testing.T) {
	body := `{"model":"gpt-allowed","messages":[{"role":"user","content":"hi"}]}`

	var gotBody []byte
	var gotContentLength int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.ContentLength = -1 // simulate chunked transfer / unknown length
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if string(gotBody) != body {
		t.Errorf("upstream body = %q, want %q (byte-identical)", gotBody, body)
	}
	_ = gotContentLength // upstream's own negotiated length; not asserted, proxyUpstream already re-derives it from the reader
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

// TestHandlePassthrough_PassthroughPaths_EncodedSlashBypass_Returns403 is
// the end-to-end regression test for security review finding 3, round 3,
// 2026-08-22: a group restricted to PassthroughPaths: ["v1/*"] must deny
// "/openai/v1/fine_tuning%2Fjobs" (an encoded "/" hiding a second
// segment from the escaped-form matcher) exactly as it would deny the
// literal, decoded three-segment path. Before this fix, path.Match
// evaluated the escaped rest directly and reported a match, letting the
// request reach the upstream — which, if it decodes %2F itself, would
// have read the same bytes as "v1/fine_tuning/jobs", a path this group's
// glob was never meant to authorize.
func TestHandlePassthrough_PassthroughPaths_EncodedSlashBypass_Returns403(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {PassthroughPaths: []string{"v1/*"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/fine_tuning%2Fjobs", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the encoded slash must not defeat the \"v1/*\" allowlist), body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called: the decoded rest is a three-segment path \"v1/*\" does not authorize")
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

// TestScanTopLevelModel_DuplicateModelKeyFailsClosed pins the fail-closed
// rule for a duplicate top-level "model" key (security audit, 2026-08-22,
// round 3). RFC 8259 permits duplicate names and every mainstream upstream
// parser resolves them LAST-wins, while this gateway forwards a passthrough
// body byte-for-byte — so reporting the FIRST occurrence (the behaviour
// before this test existed) let a model-restricted caller authorize against
// "allowed" and execute "expensive". scanTopLevelModel refuses to answer
// instead, matching extractMultipartModel's errDuplicateModelField rule.
func TestScanTopLevelModel_DuplicateModelKeyFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantModel  string
		wantFound  bool
		wantClosed bool
	}{
		{"single model", `{"model":"allowed","messages":[]}`, "allowed", true, true},
		{"model after other keys", `{"stream":true,"model":"allowed"}`, "allowed", true, true},
		{"model found then body truncated", `{"model":"allowed","messages":[{"role":"user"`, "allowed", true, false},
		{"duplicate model", `{"model":"allowed","model":"expensive","messages":[]}`, "", false, false},
		{"duplicate across a nested object", `{"model":"allowed","opts":{"a":1},"model":"expensive"}`, "", false, false},
		{"duplicate with identical values", `{"model":"same","model":"same"}`, "", false, false},
		{"no model", `{"messages":[]}`, "", false, true},
		{"nested model only", `{"opts":{"model":"x"}}`, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotFound, gotClosed := scanTopLevelModel([]byte(tc.body))
			if gotModel != tc.wantModel || gotFound != tc.wantFound || gotClosed != tc.wantClosed {
				t.Fatalf("scanTopLevelModel(%s) = (%q, %v, %v), want (%q, %v, %v)",
					tc.body, gotModel, gotFound, gotClosed, tc.wantModel, tc.wantFound, tc.wantClosed)
			}
		})
	}
}

// TestScanTopLevelModel_ClosedTracksWindowTruncation is the round-3
// regression test (coordinator ruling, 2026-08-22): closed must be false
// whenever the scanned window ends before the top-level object's closing
// '}' is actually consumed — even when a valid "model" was already found —
// and true whenever it genuinely was.
func TestScanTopLevelModel_ClosedTracksWindowTruncation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantClosed bool
	}{
		{"cut mid-string value of a later key", `{"model":"cheap","padding":"AAAA`, false},
		{"cut exactly after a complete key:value pair, no closing brace", `{"model":"cheap","padding":"x"`, false},
		{"cut inside a nested array", `{"model":"cheap","messages":[{"role":"user"`, false},
		{"complete, closes properly", `{"model":"cheap","messages":[]}`, true},
		{"complete with trailing whitespace inside cap", `{"model":"cheap"}   `, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotClosed := scanTopLevelModel([]byte(tc.body))
			if gotClosed != tc.wantClosed {
				t.Errorf("scanTopLevelModel(%s) closed = %v, want %v", tc.body, gotClosed, tc.wantClosed)
			}
		})
	}
}

// --- security review finding 2, round 3, 2026-08-22: padded-duplicate
// model beyond the peek window ---

// TestHandlePassthrough_PaddedDuplicateModelBeyondPeekWindow_Returns403
// is the coordinator's own demonstrated exploit: a restricted group's
// glob authorizes "cheap" but not "expensive"; the body's SECOND "model"
// key sits past maxModelPeekBytes, hidden behind padding the peek never
// reaches. Round 2 alone (fail-closed only on a duplicate WITHIN the
// window) let this straight through, reporting "cheap" and forwarding
// the whole body — where every mainstream upstream JSON parser resolves
// duplicate keys last-wins and would have executed "expensive". This
// must now be denied with 403 before the upstream is ever called.
func TestHandlePassthrough_PaddedDuplicateModelBeyondPeekWindow_Returns403(t *testing.T) {
	var upstreamCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"cheap"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	padding := strings.Repeat("A", maxModelPeekBytes) // pushes the second "model" key past the peek window
	body := `{"model":"cheap","padding":"` + padding + `","model":"expensive"}`

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a duplicate model beyond the peek window must fail closed), body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called: the padded-duplicate exploit must be denied before forwarding")
	}
}

// TestHandlePassthrough_RestrictedGroup_LargeBodyNeverClosesWindow_Returns403
// covers the general (non-duplicate) case the coordinator ruling actually
// implements: ANY restricted-group body that never closes its top-level
// object within maxModelPeekBytes is denied, not just one carrying a
// provably duplicate key — because a later duplicate cannot be ruled out
// either way once the window is exhausted mid-object.
func TestHandlePassthrough_RestrictedGroup_LargeBodyNeverClosesWindow_Returns403(t *testing.T) {
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

	padding := strings.Repeat("x", maxModelPeekBytes*2) // single, genuine "model" — but far past the peek window
	body := `{"model":"gpt-allowed","padding":"` + padding + `"}`

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail closed: the object never closes within the peek window, so a later duplicate cannot be ruled out), body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Error("upstream must never be called for a restricted group whose body exceeds the peek window without closing")
	}
}

// TestHandlePassthrough_UnrestrictedGroup_PaddedDuplicateModel_NeverPeeked
// is the GATE's own explicit requirement: an UNRESTRICTED group
// (grp.hasModelRestriction() false) must be COMPLETELY unaffected by
// this fix — the exact padded-duplicate body from the exploit test above
// must still be forwarded untouched, because peekPassthroughModel is
// never even called for such a group (unchanged since round 2).
func TestHandlePassthrough_UnrestrictedGroup_PaddedDuplicateModel_NeverPeeked(t *testing.T) {
	var upstreamCalled bool
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}} // no Models restriction
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	padding := strings.Repeat("A", maxModelPeekBytes)
	body := `{"model":"cheap","padding":"` + padding + `","model":"expensive"}`

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unrestricted group must never be affected by model-peek fixes, body=%s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Error("upstream must be called: an unrestricted group never peeks the body at all")
	}
	if string(gotBody) != body {
		t.Error("upstream body must be forwarded byte-for-byte unchanged for an unrestricted group")
	}
}

// TestSSEAccountingBuffer_TailBoundedAndExact: many small writes keep the
// tail buffer under 2x its cap, and tailBytes always returns exactly the
// last sseAccountingTailBytes written.
func TestSSEAccountingBuffer_TailBoundedAndExact(t *testing.T) {
	b := &sseAccountingBuffer{}
	var all []byte
	chunk := make([]byte, 58)
	for i := 0; i < 20000; i++ {
		for j := range chunk {
			chunk[j] = byte((i + j) % 256)
		}
		if _, err := b.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
		all = append(all, chunk...)
		if len(b.tail) >= 2*sseAccountingTailBytes {
			t.Fatalf("tail grew to %d bytes, want < %d", len(b.tail), 2*sseAccountingTailBytes)
		}
	}
	want := all[len(all)-sseAccountingTailBytes:]
	if !bytes.Equal(b.tailBytes(), want) {
		t.Fatal("tailBytes() != last sseAccountingTailBytes written")
	}
	if !bytes.Equal(b.head.Bytes(), all[:sseAccountingHeadBytes]) {
		t.Fatal("head != first sseAccountingHeadBytes written")
	}
}

// --- admin-redesign WP-A step 5: passthrough's own provider-level
// latency threading (never a model bucket: the upstream model lives in
// the response body, known only after the attempt already resolved) ---

// TestHandlePassthrough_Latency_ProviderBucketWrittenWhenStatsLatencyOn
// proves handlePassthrough's own accountWith call carries the completed
// request's latSample/hasLat through accountExtras, writing exactly one
// provider-level latency duration bucket when admin.stats.latency is on.
func TestHandlePassthrough_Latency_ProviderBucketWrittenWhenStatsLatencyOn(t *testing.T) {
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
	cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{Latency: true}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	now := time.Now()
	var total int64
	for i := 0; i <= len(latencyBucketBounds); i++ {
		if v, ok := gw.limiter.getCounter(kindProvider, "openai", latencyDurationMetric(i), windowDay, now); ok {
			total += v
		}
	}
	if total != 1 {
		t.Errorf("provider-level latency duration buckets summed = %d, want 1", total)
	}
}

// TestHandlePassthrough_ModelLatencyAndUModelSuppressed_ProviderHistogramsOnly
// is P13 (admin dashboard redesign verify round; plan §2(c): passthrough
// gets "provider histograms only"): the model scope routes_passthrough.go
// always attaches for accounting (base req/tokin/tokout/cost) must NOT
// also drive the opt-in per-model latency or user x model (umodel)
// counter families — canonical here is providerName + "/" + an upstream-
// ECHOED model id, unbounded cardinality this gateway does not control,
// and neither family is ever read back for it anyway (both rank off the
// configured catalog, never an arbitrary passthrough id). Provider-level
// latency (the previous test) and the base model-scope req counter are
// BOTH still written — only the two opt-in, per-model-keyed families are
// suppressed.
func TestHandlePassthrough_ModelLatencyAndUModelSuppressed_ProviderHistogramsOnly(t *testing.T) {
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
	cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{Latency: true, UserModel: true}}
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

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	now := time.Now()
	canonical := "openai/gpt-native-x"

	var modelLatTotal int64
	for i := 0; i <= len(latencyBucketBounds); i++ {
		if v, ok := gw.limiter.getCounter(kindModel, canonical, latencyDurationMetric(i), windowDay, now); ok {
			modelLatTotal += v
		}
	}
	if modelLatTotal != 0 {
		t.Errorf("model-level latency duration buckets summed = %d, want 0 (provider histograms only)", modelLatTotal)
	}

	umodelID := userModelScopeID("alice", canonical)
	if v, ok := gw.limiter.getCounter(kindUserModel, umodelID, metricReq, windowDay, now); !ok || v != 0 {
		t.Errorf("umodel req = %d (ok=%v), want 0 (passthrough must not write umodel)", v, ok)
	}

	// The base model-scope req counter (unaffected by noModelHistograms —
	// driven by the per-scope loop, not the latency/umodel blocks) must
	// still be written.
	if v, ok := gw.limiter.getCounter(kindModel, canonical, metricReq, windowDay, now); !ok || v != 1 {
		t.Errorf("model req = %d (ok=%v), want 1 (base model-scope counters are unaffected)", v, ok)
	}
}
