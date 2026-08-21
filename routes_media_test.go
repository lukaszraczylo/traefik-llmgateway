package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMultipartTranscriptionRequest builds a POST /v1/audio/transcriptions
// request with a "file" part and, unless omitModel is true, a "model"
// part carrying model. modelFirst controls part order — false writes the
// file part first and the model part last, covering spec §3's "model
// field arriving after other parts" case.
func newMultipartTranscriptionRequest(t *testing.T, apiKey, model string, modelFirst, omitModel bool) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	writeModel := func() {
		fw, err := mw.CreateFormField("model")
		if err != nil {
			t.Fatalf("CreateFormField(model): %v", err)
		}
		if _, err := fw.Write([]byte(model)); err != nil {
			t.Fatalf("write model field: %v", err)
		}
	}
	writeFile := func() {
		fw, err := mw.CreateFormFile("file", "audio.wav")
		if err != nil {
			t.Fatalf("CreateFormFile(file): %v", err)
		}
		if _, err := fw.Write([]byte("FAKEAUDIOBYTES")); err != nil {
			t.Fatalf("write file field: %v", err)
		}
	}

	if modelFirst && !omitModel {
		writeModel()
	}
	writeFile()
	if !modelFirst && !omitModel {
		writeModel()
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req
}

// newMediaTestConfig returns a base Config wired with an "openai" provider
// pointed at openaiURL, a permissive "default" group, and one inline user
// "alice" — the shared fixture every routes_media.go handler test starts
// from, customized further per test as needed.
func newMediaTestConfig(openaiURL string, models ...string) *Config {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: openaiURL, APIKey: "sk-up", Models: models},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	// Limits is a non-nil (if all-zero, i.e. unlimited) *LimitsConfig —
	// matching routes_unified_test.go's own happy-path fixture. Since the
	// v0.21 accounting fix, buildLimitScopes (routes_media.go/
	// routes_unified.go) always builds a "user" scope for alice regardless
	// of whether Limits is set at all; this fixture keeps a non-nil value
	// only for parity with the other unified-route fixtures, not because it
	// is required for checkAndCount to have a scope to increment.
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	return cfg
}

// newMediaTestGateway constructs a *Gateway from cfg via New, failing the
// test on any construction error.
func newMediaTestGateway(t *testing.T, cfg *Config) *Gateway {
	t.Helper()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	return gw
}

// ---- routing: ServeHTTP recognizes all three media paths ahead of auth ----

// TestServeHTTP_MediaRoutes_RequireAuth proves llmgateway.go's dispatch
// recognizes all three media paths (spec §3) before falling through to
// the unknown-route 404: an unauthenticated request to each gets 401, not
// 404.
func TestServeHTTP_MediaRoutes_RequireAuth(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	gw := newMediaTestGateway(t, cfg)

	for _, path := range []string{imagesGenerationsPath, audioSpeechPath, audioTranscriptionsPath} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (proves routing reached auth, not a 404 fallthrough), body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---- images.generations: openai native forward ----

func TestHandleImagesGenerations_OpenAI_HappyPath_NativeForward(t *testing.T) {
	const respBody = `{"created":1734000000,"data":[{"b64_json":"xyz"}]}`
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path = %q, want /v1/images/generations", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "img-test"))

	body := map[string]any{"model": "img-test", "prompt": "a cat", "n": 1}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}
	if _, leaked := gotBody[gatewayAliasKey]; leaked {
		t.Errorf("upstream request body leaked %q: %v", gatewayAliasKey, gotBody)
	}
	if gotBody["model"] != "img-test" {
		t.Errorf("upstream request model = %v, want %q", gotBody["model"], "img-test")
	}

	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1", reqCount, ok)
	}
	if tokIn, okIn := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now()); okIn && tokIn != 0 {
		t.Errorf("user tokin/day counter = %d, want 0 (images are never token-accounted)", tokIn)
	}
	if tokOut, okOut := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now()); okOut && tokOut != 0 {
		t.Errorf("user tokout/day counter = %d, want 0 (images are never token-accounted)", tokOut)
	}

	// withTotalScope (resolveMediaRequest, routes_media.go) must have
	// appended the synthetic total scope too (v0.2 data-layer task).
	totalReq, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, time.Now())
	if !ok || totalReq != 1 {
		t.Errorf("total request/min counter = %d (ok=%v), want 1", totalReq, ok)
	}

	// Feature A (v0.22): handleImagesGenerations wraps the request context
	// with an attemptRecorder before calling adapter.imagesGeneration
	// (routes_media.go) — a successful 200 upstream attempt must land as
	// one attempt, zero failures, at both provider and (provider, model)
	// scope.
	attempts, ok := gw.limiter.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, time.Now())
	if !ok || attempts != 1 {
		t.Errorf("provider attempts/day = %d (ok=%v), want 1", attempts, ok)
	}
	fails, _ := gw.limiter.getCounter(kindProvider, "openai", metricProvFail, windowDay, time.Now())
	if fails != 0 {
		t.Errorf("provider fails/day = %d, want 0", fails)
	}
	modelAttempts, ok := gw.limiter.getCounter(kindProviderModel, "openai/img-test", metricProvAttempt, windowDay, time.Now())
	if !ok || modelAttempts != 1 {
		t.Errorf("model attempts/day = %d (ok=%v), want 1", modelAttempts, ok)
	}
}

// TestHandleImagesGenerations_ModelAlias_ResolvesToTargetUpstreamModel
// proves an operator-defined modelAliases entry (spec §5, v0.2) routes
// /v1/images/generations exactly like a bare or provider-prefixed id: the
// upstream call carries the target's own upstream model id, never the
// alias.
func TestHandleImagesGenerations_ModelAlias_ResolvesToTargetUpstreamModel(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"xyz"}]}`))
	}))
	defer srv.Close()

	cfg := newMediaTestConfig(srv.URL, "img-test")
	cfg.ModelAliases = map[string]string{"aliased/image": "openai/img-test"}
	gw := newMediaTestGateway(t, cfg)

	body := map[string]any{"model": "aliased/image", "prompt": "a cat", "n": 1}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotBody["model"] != "img-test" {
		t.Errorf("upstream request model = %v, want the alias target's bare upstream id %q", gotBody["model"], "img-test")
	}
}

// TestHandleImagesGenerations_StripsClientSuppliedAliasKey proves a
// client independently including a literal "__alias" field in its own
// JSON body (never one the gateway itself injects for a media route —
// routes_media.go never sets gatewayAliasKey here) never reaches the
// upstream provider: openaiAdapter.imagesGeneration deletes it
// unconditionally, mirroring chatCompletion's identical delete.
func TestHandleImagesGenerations_StripsClientSuppliedAliasKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "img-test"))

	body := map[string]any{"model": "img-test", "prompt": "a cat", gatewayAliasKey: "sneaky-client-value"}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if _, leaked := gotBody[gatewayAliasKey]; leaked {
		t.Errorf("upstream request body leaked client-supplied %q: %v", gatewayAliasKey, gotBody)
	}
}

func TestHandleImagesGenerations_MissingModel_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "img-test"))

	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", map[string]any{"prompt": "a cat"})
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleImagesGenerations_InvalidJSON_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "img-test"))

	req := httptest.NewRequest(http.MethodPost, imagesGenerationsPath, strings.NewReader(`{not json`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleImagesGenerations_RetriedOnTransientFailure proves retry
// (spec §1) covers the images endpoint end to end through the full
// Gateway/ServeHTTP pipeline: the upstream fails once with 503, then
// succeeds, and the client sees only the successful response.
func TestHandleImagesGenerations_RetriedOnTransientFailure(t *testing.T) {
	const respBody = `{"created":1,"data":[{"b64_json":"xyz"}]}`
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := newMediaTestConfig(srv.URL, "img-test")
	cfg.Retry = RetryConfig{Enabled: true, Attempts: 1, Backoff: "1ms"}
	gw := newMediaTestGateway(t, cfg)

	body := map[string]any{"model": "img-test", "prompt": "a cat"}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), respBody)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one retry)", got)
	}
}

// TestHandleImagesGenerations_Gemini_TranslatesToImagen exercises the
// full gemini translation chain (translate_gemini_images.go) through
// ServeHTTP: request translated to Imagen's :predict shape, response
// translated back to OpenAI's images.generations shape.
func TestHandleImagesGenerations_Gemini_TranslatesToImagen(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"predictions":[{"bytesBase64Encoded":"b64data"}]}`))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"gemini": {Type: "gemini", BaseURL: srv.URL, APIKey: "sk-gem", Models: []string{"imagen-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	gw := newMediaTestGateway(t, cfg)

	body := map[string]any{"model": "imagen-test", "prompt": "a cat", "size": "1024x1024"}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1beta/models/imagen-test:predict" {
		t.Errorf("upstream path = %q, want /v1beta/models/imagen-test:predict", gotPath)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %v, want 1 entry", out["data"])
	}
	entry, _ := data[0].(map[string]any)
	if entry["b64_json"] != "b64data" {
		t.Errorf("b64_json = %v, want %q", entry["b64_json"], "b64data")
	}
}

// ---- audio.speech: openai native forward, binary streaming ----

// TestHandleAudioSpeech_OpenAI_StreamsBinary_WithFlushes proves the binary
// audio response is streamed to the client chunk by chunk, flushing after
// each one, rather than buffered until the response completes — mirroring
// TestHandleChat_Streaming_FlushesIncrementally's assertions
// (routes_unified_test.go) for the SSE case. Unlike that SSE test, a raw
// binary stream carries no application-level event framing for readSSE's
// line-based scanner to dispatch on, so a short sleep between the fake
// upstream's writes is needed here to force the client's io.Copy to see
// each chunk as its own Read — without it, chunks written back-to-back
// with no delay can legitimately coalesce into a single Read on a fast
// loopback connection, which would make this test flaky rather than prove
// anything about audioSpeech's own streaming behavior.
func TestHandleAudioSpeech_OpenAI_StreamsBinary_WithFlushes(t *testing.T) {
	chunks := []string{"RIFF-chunk-one-", "RIFF-chunk-two-", "RIFF-chunk-three"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		for i, c := range chunks {
			_, _ = w.Write([]byte(c))
			fl.Flush()
			if i < len(chunks)-1 {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "tts-test"))

	body := map[string]any{"model": "tts-test", "input": "hello", "voice": "alloy"}
	req := newUnifiedRequest(t, http.MethodPost, audioSpeechPath, "sk-alice", body)
	rw := newRecordingWriter()
	gw.ServeHTTP(rw, req)

	if rw.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.status)
	}
	writes, flushes := 0, 0
	for _, e := range rw.events {
		switch e {
		case "write":
			writes++
		case "flush":
			flushes++
		}
	}
	if writes < len(chunks) {
		t.Fatalf("want >=%d writes, got %d (events=%v)", len(chunks), writes, rw.events)
	}
	if flushes < writes {
		t.Fatalf("want a flush per write (incremental delivery), got %d writes and %d flushes", writes, flushes)
	}
	if want := strings.Join(chunks, ""); rw.buf.String() != want {
		t.Errorf("body = %q, want %q", rw.buf.String(), want)
	}
	if ct := rw.hdr.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
}

// TestHandleAudioSpeech_StripsClientSuppliedAliasKey mirrors
// TestHandleImagesGenerations_StripsClientSuppliedAliasKey: a client
// independently including a literal "__alias" field in its own JSON body
// (never one the gateway itself injects for a media route —
// routes_media.go never sets gatewayAliasKey here) must never reach the
// upstream provider. handleAudioSpeech re-marshals req into the []byte
// body it hands openaiAdapter.audioSpeech, so the delete has to happen
// in routes_media.go itself rather than in the adapter (audioSpeech's
// interface method takes []byte, not a map, unlike imagesGeneration).
func TestHandleAudioSpeech_StripsClientSuppliedAliasKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-audio-bytes"))
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "tts-test"))

	body := map[string]any{"model": "tts-test", "input": "hello", gatewayAliasKey: "sneaky-client-value"}
	req := newUnifiedRequest(t, http.MethodPost, audioSpeechPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if _, leaked := gotBody[gatewayAliasKey]; leaked {
		t.Errorf("upstream request body leaked client-supplied %q: %v", gatewayAliasKey, gotBody)
	}
}

func TestHandleAudioSpeech_MissingModel_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "tts-test"))

	req := newUnifiedRequest(t, http.MethodPost, audioSpeechPath, "sk-alice", map[string]any{"input": "hello"})
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// ---- audio.transcriptions: openai native forward, multipart replay ----

// buildMultipartTranscription returns the raw bytes of a multipart/
// form-data body carrying a "model" field (after the "file" part, per
// spec §3's "model field arriving after other parts" case) and its
// Content-Type header value.
func buildMultipartTranscription(t *testing.T, model string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err = fw.Write([]byte("FAKEAUDIOBYTES")); err != nil {
		t.Fatalf("write file field: %v", err)
	}
	mfw, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatalf("CreateFormField(model): %v", err)
	}
	if _, err = mfw.Write([]byte(model)); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// TestHandleAudioTranscriptions_OpenAI_HappyPath_MultipartReplayed
// exercises the bare-id fast path: the client's own "model" field
// ("whisper-test") already equals the resolved upstream model id, so
// handleAudioTranscriptions replays the original multipart bytes
// byte-for-byte unchanged rather than calling rewriteMultipartModel — see
// TestHandleAudioTranscriptions_PrefixedAlias_RewritesModelFieldOnly for
// the alias case that does rewrite.
func TestHandleAudioTranscriptions_OpenAI_HappyPath_MultipartReplayed(t *testing.T) {
	const respBody = `{"text":"hello world"}`
	var gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("upstream path = %q, want /v1/audio/transcriptions", r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "whisper-test"))

	// The "model" field is placed after the "file" part in the built body
	// (buildMultipartTranscription), covering spec §3's "model field
	// arriving after other parts" case end to end.
	origBody, contentType := buildMultipartTranscription(t, "whisper-test")
	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, bytes.NewReader(origBody))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer sk-alice")

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}
	if gotContentType != contentType {
		t.Errorf("upstream Content-Type = %q, want %q (forwarded verbatim)", gotContentType, contentType)
	}
	if !bytes.Equal(gotBody, origBody) {
		t.Errorf("upstream body was rewritten; want the exact original multipart bytes replayed unchanged")
	}
}

func TestHandleAudioTranscriptions_ModelFieldMissing_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	req := newMultipartTranscriptionRequest(t, "sk-alice", "", true, true)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAudioTranscriptions_InvalidContentType_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "application/json") // not multipart at all
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "multipart/form-data") {
		t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "multipart/form-data")
	}
}

// TestHandleAudioTranscriptions_ContentTypeNotFormData_400 covers the
// case a plain "not multipart at all" Content-Type does not: a
// syntactically valid multipart Content-Type, with a real boundary
// parameter, whose subtype is not "form-data". Before the folded review
// fix, this fell through the boundary check (a boundary was present) and
// produced a misleading "model is required" 400 instead of naming the
// actual problem.
func TestHandleAudioTranscriptions_ContentTypeNotFormData_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, strings.NewReader("--xyz--\r\n"))
	req.Header.Set("Content-Type", "multipart/mixed; boundary=xyz")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "multipart/form-data") {
		t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "multipart/form-data")
	}
}

// TestHandleAudioTranscriptions_MalformedTail_400 proves a syntactically
// valid Content-Type and boundary, but a body whose closing boundary
// marker is truncated, is reported as "malformed multipart body" — a
// review-sweep fix (2026-08-20) distinct from "model is required": before
// it, any error extractMultipartModel returned that was not
// errDuplicateModelField collapsed into the same misleading
// "model is required" 400, even though the body never got far enough to
// evaluate whether a "model" part was present at all.
func TestHandleAudioTranscriptions_MalformedTail_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatalf("CreateFormField(model): %v", err)
	}
	if _, err := fw.Write([]byte("whisper-test")); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	contentType := mw.FormDataContentType()

	// Cut the last 20 bytes off — well within the closing "--boundary--"
	// marker for Go's own (60-character) generated boundary — so reading
	// the "model" part's content runs past the truncated tail without ever
	// finding a delimiter, instead of cleanly reaching io.EOF.
	truncated := buf.Bytes()[:buf.Len()-20]

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, bytes.NewReader(truncated))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "malformed multipart body") {
		t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "malformed multipart body")
	}
	if strings.Contains(rec.Body.String(), "model is required") {
		t.Errorf("body = %q, must not report the misleading %q for a body that never parsed far enough to know", rec.Body.String(), "model is required")
	}
}

// TestHandleAudioTranscriptions_DuplicateModelField_400 proves a
// multipart body carrying two "model" parts is rejected outright (400)
// rather than silently resolving to whichever one extractMultipartModel
// happened to see first.
func TestHandleAudioTranscriptions_DuplicateModelField_400(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, v := range []string{"whisper-test", "whisper-other"} {
		fw, err := mw.CreateFormField("model")
		if err != nil {
			t.Fatalf("CreateFormField: %v", err)
		}
		if _, err := fw.Write([]byte(v)); err != nil {
			t.Fatalf("write model field: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate model field") {
		t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "duplicate model field")
	}
}

// TestHandleAudioTranscriptions_PrefixedAlias_RewritesModelFieldOnly
// proves the controller ruling (2026-08-20, amends spec §3): a
// provider-prefixed model id ("openai/whisper-test") must never reach
// the real upstream verbatim. rewriteMultipartModel rebuilds the
// multipart body with the bare upstream id in the "model" field, leaving
// every other part (here, the audio file) byte-for-byte unchanged.
func TestHandleAudioTranscriptions_PrefixedAlias_RewritesModelFieldOnly(t *testing.T) {
	const fileContent = "FAKEAUDIOBYTES-DISTINCTIVE-PAYLOAD"
	var gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	gw := newMediaTestGateway(t, newMediaTestConfig(srv.URL, "whisper-test"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ffw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err = ffw.Write([]byte(fileContent)); err != nil {
		t.Fatalf("write file field: %v", err)
	}
	mfw, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatalf("CreateFormField(model): %v", err)
	}
	// "openai/whisper-test" is a provider-prefixed alias resolving to the
	// same upstream model the bare form registers under.
	if _, err = mfw.Write([]byte("openai/whisper-test")); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	_, params, err := mime.ParseMediaType(gotContentType)
	if err != nil {
		t.Fatalf("upstream Content-Type %q did not parse: %v", gotContentType, err)
	}
	mr := multipart.NewReader(bytes.NewReader(gotBody), params["boundary"])
	var gotModel string
	var gotFile []byte
	var sawModel, sawFile bool
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF { //nolint:errorlint // multipart.Reader.NextPart's own contract returns io.EOF bare
			break
		}
		if perr != nil {
			t.Fatalf("parse upstream multipart body: %v", perr)
		}
		switch part.FormName() {
		case "model":
			b, _ := io.ReadAll(part)
			gotModel = string(b)
			sawModel = true
		case "file":
			b, _ := io.ReadAll(part)
			gotFile = b
			sawFile = true
		}
		_ = part.Close()
	}

	if !sawModel || gotModel != "whisper-test" {
		t.Errorf("upstream model field = %q (present=%v), want the bare upstream id %q", gotModel, sawModel, "whisper-test")
	}
	if !sawFile || string(gotFile) != fileContent {
		t.Errorf("upstream file part = %q (present=%v), want unchanged %q", gotFile, sawFile, fileContent)
	}
}

// TestHandleAudioTranscriptions_ModelAlias_RewritesModelFieldToTargetUpstreamID
// mirrors TestHandleAudioTranscriptions_PrefixedAlias_RewritesModelFieldOnly
// above, but through an operator-defined modelAliases entry (spec §5,
// v0.2) rather than a bare provider-prefixed id: the client's "model"
// form field ("aliased/whisper") differs from the resolved upstream model
// id ("whisper-test"), so rewriteMultipartModel (routes_media.go) must
// rebuild the multipart body — every part copied verbatim except "model",
// which becomes the target's own upstream id. The alias must never reach
// the real provider, the same invariant every other unified route
// already enforces.
func TestHandleAudioTranscriptions_ModelAlias_RewritesModelFieldToTargetUpstreamID(t *testing.T) {
	const fileContent = "FAKEAUDIOBYTES-DISTINCTIVE-PAYLOAD"
	var gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	cfg := newMediaTestConfig(srv.URL, "whisper-test")
	cfg.ModelAliases = map[string]string{"aliased/whisper": "openai/whisper-test"}
	gw := newMediaTestGateway(t, cfg)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ffw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err = ffw.Write([]byte(fileContent)); err != nil {
		t.Fatalf("write file field: %v", err)
	}
	mfw, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatalf("CreateFormField(model): %v", err)
	}
	if _, err = mfw.Write([]byte("aliased/whisper")); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	_, params, err := mime.ParseMediaType(gotContentType)
	if err != nil {
		t.Fatalf("upstream Content-Type %q did not parse: %v", gotContentType, err)
	}
	mr := multipart.NewReader(bytes.NewReader(gotBody), params["boundary"])
	var gotModel string
	var gotFile []byte
	var sawModel, sawFile bool
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF { //nolint:errorlint // multipart.Reader.NextPart's own contract returns io.EOF bare
			break
		}
		if perr != nil {
			t.Fatalf("parse upstream multipart body: %v", perr)
		}
		switch part.FormName() {
		case "model":
			b, _ := io.ReadAll(part)
			gotModel = string(b)
			sawModel = true
		case "file":
			b, _ := io.ReadAll(part)
			gotFile = b
			sawFile = true
		}
		_ = part.Close()
	}

	if !sawModel || gotModel != "whisper-test" {
		t.Errorf("upstream model field = %q (present=%v), want the alias target's bare upstream id %q", gotModel, sawModel, "whisper-test")
	}
	if !sawFile || string(gotFile) != fileContent {
		t.Errorf("upstream file part = %q (present=%v), want unchanged %q", gotFile, sawFile, fileContent)
	}
}

// TestHandleAudioTranscriptions_OversizeBody_413 proves a multipart body
// above maxRequestBytes is rejected with 413 (spec §3), the one media
// route with an explicit oversize check — unlike images/speech's shared
// JSON decode path, which silently truncates instead (routes_media.go's
// readCapped doc comment explains why).
func TestHandleAudioTranscriptions_OversizeBody_413(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "whisper-test"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormField("model")
	if err != nil {
		t.Fatalf("CreateFormField: %v", err)
	}
	_, _ = fw.Write([]byte("whisper-test"))
	ffw, err := mw.CreateFormFile("file", "big.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = ffw.Write(bytes.Repeat([]byte("A"), maxRequestBytes+1024))
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, audioTranscriptionsPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rec.Code, rec.Body.String())
	}
}

// ---- unit coverage for the new pure helpers ----

func TestReadCapped(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		body, oversize, err := readCapped(strings.NewReader("hello"), 10)
		if err != nil || oversize || string(body) != "hello" {
			t.Errorf("readCapped = (%q, %v, %v), want (\"hello\", false, nil)", body, oversize, err)
		}
	})
	t.Run("exactly at limit", func(t *testing.T) {
		body, oversize, err := readCapped(strings.NewReader("0123456789"), 10)
		if err != nil || oversize || string(body) != "0123456789" {
			t.Errorf("readCapped = (%q, %v, %v), want (\"0123456789\", false, nil)", body, oversize, err)
		}
	})
	t.Run("over limit", func(t *testing.T) {
		body, oversize, err := readCapped(strings.NewReader("01234567890"), 10)
		if err != nil || !oversize || len(body) != 10 {
			t.Errorf("readCapped = (len=%d, %v, %v), want (len=10, true, nil)", len(body), oversize, err)
		}
	})
}

func TestExtractMultipartModel(t *testing.T) {
	build := func(fields []struct{ name, value string }, includeFile bool) (body []byte, boundary string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, f := range fields {
			fw, err := mw.CreateFormField(f.name)
			if err != nil {
				t.Fatalf("CreateFormField: %v", err)
			}
			_, _ = fw.Write([]byte(f.value))
		}
		if includeFile {
			fw, err := mw.CreateFormFile("file", "x.wav")
			if err != nil {
				t.Fatalf("CreateFormFile: %v", err)
			}
			_, _ = fw.Write([]byte("bytes"))
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		return buf.Bytes(), mw.Boundary()
	}

	t.Run("model field present", func(t *testing.T) {
		body, boundary := build([]struct{ name, value string }{{"model", "whisper-1"}}, true)
		got, err := extractMultipartModel(body, boundary)
		if err != nil || got != "whisper-1" {
			t.Errorf("extractMultipartModel = (%q, %v), want (\"whisper-1\", nil)", got, err)
		}
	})
	t.Run("model field missing", func(t *testing.T) {
		body, boundary := build(nil, true)
		got, err := extractMultipartModel(body, boundary)
		if err != nil || got != "" {
			t.Errorf("extractMultipartModel = (%q, %v), want (\"\", nil)", got, err)
		}
	})
	t.Run("no parts at all", func(t *testing.T) {
		body, boundary := build(nil, false)
		got, err := extractMultipartModel(body, boundary)
		if err != nil || got != "" {
			t.Errorf("extractMultipartModel = (%q, %v), want (\"\", nil)", got, err)
		}
	})
}

// ---- gemini: audio always 501 ----

func newGeminiOnlyConfig(baseURL string, models ...string) *Config {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"gemini": {Type: "gemini", BaseURL: baseURL, APIKey: "sk-gem", Models: models},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	return cfg
}

func TestHandleAudioSpeech_Gemini_Returns501(t *testing.T) {
	gw := newMediaTestGateway(t, newGeminiOnlyConfig("http://127.0.0.1:1", "gem-audio"))

	req := newUnifiedRequest(t, http.MethodPost, audioSpeechPath, "sk-alice", map[string]any{"model": "gem-audio", "input": "hi"})
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAudioTranscriptions_Gemini_Returns501(t *testing.T) {
	gw := newMediaTestGateway(t, newGeminiOnlyConfig("http://127.0.0.1:1", "gem-audio"))

	req := newMultipartTranscriptionRequest(t, "sk-alice", "gem-audio", true, false)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
}

// ---- anthropic: all three endpoints 501, never dial upstream ----

// TestAnthropicMediaEndpoints_Return501_NeverCallUpstream proves anthropic's
// three media adapter methods (provider_anthropic.go) return their
// *translateError{notSupported:true} before making any upstream HTTP
// request: BaseURL points at a port nothing listens on, so a 502
// (upstream connection error) instead of 501 would mean the adapter tried
// to dial anyway.
func TestAnthropicMediaEndpoints_Return501_NeverCallUpstream(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: "anthropic", BaseURL: "http://127.0.0.1:1", APIKey: "sk-ant", Models: []string{"claude-media"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	gw := newMediaTestGateway(t, cfg)

	t.Run("images.generations", func(t *testing.T) {
		req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", map[string]any{"model": "claude-media", "prompt": "a cat"})
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("audio.speech", func(t *testing.T) {
		req := newUnifiedRequest(t, http.MethodPost, audioSpeechPath, "sk-alice", map[string]any{"model": "claude-media", "input": "hi"})
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("audio.transcriptions", func(t *testing.T) {
		req := newMultipartTranscriptionRequest(t, "sk-alice", "claude-media", true, false)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
		}
	})
}

// ---- resolveMediaRequest: the shared resolve/limit failure branches ----
//
// Every other media-route test above drives resolveMediaRequest's success
// path only; these two cover its two failure returns (routes_media.go) —
// a model no configured provider knows, and a limit violation — using
// images.generations as the representative caller, since all three media
// routes share the same resolveMediaRequest call.

// TestHandleImagesGenerations_UnknownModel_404 covers resolveMediaRequest's
// registry.resolve error branch: a model id no configured provider knows at
// all.
func TestHandleImagesGenerations_UnknownModel_404(t *testing.T) {
	gw := newMediaTestGateway(t, newMediaTestConfig("http://127.0.0.1:1", "img-test"))

	body := map[string]any{"model": "no-such-model", "prompt": "a cat"}
	req := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// TestHandleImagesGenerations_LimitExceeded_429WithRetryAfter covers
// resolveMediaRequest's checkAndCount violation branch: a second request
// past a one-per-minute limit is refused before the adapter is ever called.
func TestHandleImagesGenerations_LimitExceeded_429WithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":1734000000,"data":[{"b64_json":"xyz"}]}`))
	}))
	defer srv.Close()

	cfg := newMediaTestConfig(srv.URL, "img-test")
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 1}},
	}}
	gw := newMediaTestGateway(t, cfg)

	body := map[string]any{"model": "img-test", "prompt": "a cat"}
	req1 := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec1 := httptest.NewRecorder()
	gw.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code, "first request body=%s", rec1.Body.String())

	req2 := newUnifiedRequest(t, http.MethodPost, imagesGenerationsPath, "sk-alice", body)
	rec2 := httptest.NewRecorder()
	gw.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusTooManyRequests, rec2.Code, "body=%s", rec2.Body.String())
	assert.NotEmpty(t, rec2.Header().Get("Retry-After"))
}
