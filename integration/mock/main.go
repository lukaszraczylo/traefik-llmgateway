// Command mock is the integration suite's fake upstream. It runs in two
// distinct modes, selected by flags:
//
//   - Server mode (-mode openai|anthropic|gemini|tool): serves the minimal
//     subset of one upstream's wire API the corresponding plugin adapter
//     calls, using canned fixtures shaped like the plugin's own unit-test
//     fakes (see provider_openai_test.go, provider_anthropic_test.go,
//     provider_gemini_test.go). "tool" mode serves a bare SSE endpoint,
//     standing in for an MCP target the gateway's target proxy forwards to
//     verbatim.
//   - Probe mode (-probe): a raw-TCP client that dials a running gateway
//     directly (inside the compose network, never through the Docker
//     Desktop host port-proxy — that layer buffers and hides true
//     inter-chunk timing), issues one streaming chat completion by hand,
//     and reports via its exit code whether the response arrived as
//     genuinely incremental SSE chunks rather than one buffered write.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// streamChunkGap is the pause between successive SSE writes in every mode's
// streaming handler. The probe (see runProbe) requires at least 3 gaps of
// 100ms or more between arrivals; 150ms per gap leaves comfortable margin
// against scheduler jitter while keeping the suite fast.
const streamChunkGap = 150 * time.Millisecond

func main() {
	mode := flag.String("mode", "", "server mode: openai, anthropic, gemini, or tool")
	addr := flag.String("addr", ":8080", "listen address for server mode")
	probe := flag.Bool("probe", false, "run as a raw-TCP streaming probe instead of a server")
	target := flag.String("target", "traefik1:80", "probe mode: host:port of the gateway to dial")
	apiKey := flag.String("apikey", "sk-int-alice", "probe mode: gateway API key to present")
	model := flag.String("model", "openai/gpt-mock", "probe mode: model id to request")
	flag.Parse()

	if *probe {
		os.Exit(runProbe(*target, *apiKey, *model))
	}

	mux, err := newServerMux(*mode)
	if err != nil {
		log.Fatalf("mock: %v", err)
	}
	log.Printf("mock: mode=%s listening on %s", *mode, *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil { //nolint:gosec // integration-only fixture server, no need for timeouts
		log.Fatalf("mock: listen: %v", err)
	}
}

// newServerMux returns the handler set for one server mode.
func newServerMux(mode string) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	switch mode {
	case "openai":
		mux.HandleFunc("/v1/models", handleOpenAIModels)
		mux.HandleFunc("/v1/chat/completions", handleOpenAIChat)
		// flaky/v1/chat/completions backs the retry integration test (task
		// 5): the plugin's "flaky" provider points its baseUrl at this
		// mode's own address plus "/flaky", so a.baseURL+"/v1/chat/
		// completions" (provider_openai.go) resolves to exactly this path.
		// flaky/reset is reachable the same way, through the "flaky"
		// provider's native passthrough route (adapter.base()+"/"+rest,
		// routes_passthrough.go) — the test calls it directly, never this
		// mock process's own network address.
		mux.HandleFunc("/flaky/v1/chat/completions", handleFlakyOpenAIChat)
		mux.HandleFunc("/flaky/reset", handleFlakyReset)
		mux.HandleFunc("/v1/images/generations", handleOpenAIImages)
		mux.HandleFunc("/v1/audio/speech", handleOpenAIAudioSpeech)
		mux.HandleFunc("/v1/audio/transcriptions", handleOpenAIAudioTranscriptions)
	case "anthropic":
		mux.HandleFunc("/v1/models", handleAnthropicModels)
		mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	case "gemini":
		mux.HandleFunc("/v1beta/models", handleGeminiModels)
		mux.HandleFunc("/v1beta/models/", handleGeminiGenerate)
	case "tool":
		mux.HandleFunc("/sse", handleToolSSE)
	default:
		return nil, fmt.Errorf("unknown -mode %q (want openai, anthropic, gemini, or tool)", mode)
	}
	return mux, nil
}

// decodeJSONBody reads and parses r's body as a generic JSON object. It
// returns an empty, non-nil map on any read/parse error — every handler
// below only reads a couple of optional fields out of it, so a malformed
// body degrades to default fixture values rather than a 400 the tests
// don't care about.
func decodeJSONBody(r *http.Request) map[string]any {
	var body map[string]any
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return map[string]any{}
	}
	return body
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// sseFlusher wraps the response writer for one SSE handler, setting the
// standard streaming headers up front and flushing after every write so a
// downstream proxy (the gateway, Traefik) observes each event as it is
// produced rather than buffered until the handler returns.
type sseFlusher struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEFlusher(w http.ResponseWriter) sseFlusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return sseFlusher{w: w, f: f}
}

func (s sseFlusher) write(event, data string) {
	if event != "" {
		_, _ = fmt.Fprintf(s.w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if s.f != nil {
		s.f.Flush()
	}
}

// --- openai mode -----------------------------------------------------------

func handleOpenAIModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "gpt-mock", "object": "model", "owned_by": "mock-openai"},
		},
	})
}

// openaiStreamDeltas are the content fragments the streaming handler emits
// as separate SSE frames, one streamChunkGap apart, mirroring the shape of
// provider_openai_test.go's streamFrames fixture.
var openaiStreamDeltas = []string{"Hel", "lo", " wor", "ld", "!"}

func handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Mock-Received-Auth", r.Header.Get("Authorization"))
	body := decodeJSONBody(r)
	model, _ := body["model"].(string)
	if model == "" {
		model = "gpt-mock"
	}
	stream, _ := body["stream"].(bool)

	if !stream {
		writeJSON(w, map[string]any{
			"id":     "chatcmpl-mock",
			"object": "chat.completion",
			"model":  model,
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "mock openai response"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
		return
	}

	sw := newSSEFlusher(w)
	for i, delta := range openaiStreamDeltas {
		deltaObj := map[string]any{"content": delta}
		if i == 0 {
			deltaObj["role"] = "assistant"
		}
		frame := map[string]any{
			"id":      "chatcmpl-mock",
			"choices": []map[string]any{{"index": 0, "delta": deltaObj}},
		}
		b, _ := json.Marshal(frame)
		sw.write("", string(b))
		time.Sleep(streamChunkGap)
	}
	usage, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"choices": []map[string]any{},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
	sw.write("", string(usage))
	time.Sleep(streamChunkGap)
	sw.write("", "[DONE]")
}

// flakyHits counts every request handleFlakyOpenAIChat has served since
// the last reset (handleFlakyReset, below), or since process start if
// never reset. It is package-level state deliberately, not reset per
// request: the retry integration test relies on exactly the FIRST
// request counted this way failing, and every one after succeeding.
// TestRetryFlakyRecovers calls handleFlakyReset (via the gateway's own
// "flaky" passthrough provider) at its own start, so a re-run against an
// already-up stack (`make integration-keep`, no mockopenai restart)
// reproduces the same forced-503-then-success scenario every time — the
// same idempotence flushRedis gives the rate-limit counters via
// TestCrossReplicaLimits.
var flakyHits int32

// handleFlakyReset resets flakyHits to 0, letting the retry integration
// test reproduce its forced-503-then-success scenario on demand rather
// than only on this process's true first request. POST only — a GET (or
// any other method) must never mutate state; it answers 405 instead.
func handleFlakyReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	atomic.StoreInt32(&flakyHits, 0)
	writeJSON(w, map[string]any{"reset": true})
}

// handleFlakyOpenAIChat answers the first request since the last reset
// (handleFlakyReset, or process start if never reset) with HTTP 503 (a
// transient failure retryPolicy.isTransient classifies as retryable,
// retry.go) and every request after that with a normal chat completion
// response carrying an extra top-level
// "mock_attempt" field set to this handler's own call count — the
// unified route's openai-type adapter forwards a JSON response body
// verbatim (forwardJSON, provider_openai.go), so this field survives
// straight through to the client and lets the retry integration test
// prove the mock was hit exactly twice without needing to inspect
// response headers forwardJSON never copies.
func handleFlakyOpenAIChat(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&flakyHits, 1)
	if n == 1 {
		http.Error(w, `{"error":{"message":"mock flaky failure","type":"server_error"}}`, http.StatusServiceUnavailable)
		return
	}

	body := decodeJSONBody(r)
	model, _ := body["model"].(string)
	if model == "" {
		model = "flaky-mock"
	}
	writeJSON(w, map[string]any{
		"id":           "chatcmpl-flaky",
		"object":       "chat.completion",
		"model":        model,
		"mock_attempt": n,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "mock flaky response"},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
}

// handleOpenAIImages answers POST /v1/images/generations with a canned
// OpenAI images.generations response — the images integration test's
// openai-type case is a native forward (routes_media.go), so whatever
// this returns reaches the client unmodified.
func handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	_ = decodeJSONBody(r) // request shape isn't asserted on; draining it is enough
	writeJSON(w, map[string]any{
		"created": 1755600000,
		"data": []map[string]any{
			{"b64_json": "bW9jay1vcGVuYWktaW1hZ2UtYnl0ZXM=", "revised_prompt": "mock revised prompt"},
		},
	})
}

// mockAudioBytes is the fixed binary payload handleOpenAIAudioSpeech
// returns — deliberately not valid MP3 data, since nothing in the
// gateway's audioSpeech path (a pure byte-for-byte stream,
// provider_openai.go) ever parses it; only its content-type and byte
// equality on round trip matter to the audio integration test.
var mockAudioBytes = []byte("MOCK-AUDIO-BYTES-0123456789-DISTINCTIVE-PAYLOAD")

// handleOpenAIAudioSpeech answers POST /v1/audio/speech with mockAudioBytes
// under Content-Type audio/mpeg, mirroring a real TTS endpoint's binary
// response.
func handleOpenAIAudioSpeech(w http.ResponseWriter, r *http.Request) {
	_ = decodeJSONBody(r)
	w.Header().Set("Content-Type", "audio/mpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mockAudioBytes)
}

// handleOpenAIAudioTranscriptions answers POST /v1/audio/transcriptions by
// parsing the multipart body Traefik forwarded and echoing back the "model"
// form field it finds — this is the review wave's highest-value Yaegi
// coverage gap closed: the audio-transcriptions integration test sends a
// provider-prefixed model ("openai/gpt-mock") and asserts this handler
// receives the bare, rewritten form ("gpt-mock"), proving
// rewriteMultipartModel (routes_media.go) actually rebuilds the multipart
// body under real Traefik+Yaegi, not just in the compiled unit tests. A
// second, already-bare request exercises the byte-identical replay path
// instead, so this same field still reads back "gpt-mock" with no rebuild.
func handleOpenAIAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: maxMemory is set (1MiB) and the whole handler is behind readCapped's 10MiB gateway-side cap (routes_media.go) before this mock ever sees the body; test-only fixture server, not production code
		http.Error(w, `{"error":{"message":"invalid multipart body","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"text":           "mock transcription",
		"model_received": r.FormValue("model"),
	})
}

// --- anthropic mode ----------------------------------------------------------

func handleAnthropicModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"data": []map[string]any{{"id": "claude-mock"}},
	})
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Mock-Received-Auth", r.Header.Get("x-api-key"))
	body := decodeJSONBody(r)
	model, _ := body["model"].(string)
	if model == "" {
		model = "claude-mock"
	}
	stream, _ := body["stream"].(bool)

	if !stream {
		writeJSON(w, map[string]any{
			"id":   "msg_mock",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "mock anthropic response"},
			},
			"model":       model,
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 4},
		})
		return
	}

	sw := newSSEFlusher(w)
	frames := []struct{ event, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_mock","usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mock "}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"anthropic "}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"response"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	for _, f := range frames {
		sw.write(f.event, f.data)
		time.Sleep(20 * time.Millisecond)
	}
}

// --- gemini mode -------------------------------------------------------------

func handleGeminiModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"models": []map[string]any{{"name": "models/gemini-mock"}},
	})
}

func handleGeminiGenerate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Mock-Received-Auth", r.Header.Get("x-goog-api-key"))
	rest := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	_, action, _ := strings.Cut(rest, ":")

	switch action {
	case "generateContent":
		writeJSON(w, map[string]any{
			"candidates": []map[string]any{
				{
					"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "mock gemini response"}}},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{"promptTokenCount": 10, "candidatesTokenCount": 4},
			"responseId":    "resp_mock",
		})
	case "streamGenerateContent":
		sw := newSSEFlusher(w)
		frames := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"mock "}]}}],"responseId":"resp_mock"}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"gemini "}]}}],"responseId":"resp_mock"}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"response"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}`,
		}
		for _, f := range frames {
			sw.write("", f) // Gemini's SSE stream carries no "event:" field.
			time.Sleep(20 * time.Millisecond)
		}
	case "predict":
		// Backs the images integration test's gemini case: the gateway's
		// geminiAdapter.imagesGeneration (provider_gemini.go) POSTs to
		// {base}/v1beta/models/{model}:predict and translates this
		// Imagen-shaped response into OpenAI's images.generations shape
		// (translate_gemini_images.go) before it ever reaches the client.
		writeJSON(w, map[string]any{
			"predictions": []map[string]any{
				{"bytesBase64Encoded": "bW9jay1nZW1pbmktaW1hZ2UtYnl0ZXM="},
			},
		})
	default:
		http.NotFound(w, r)
	}
}

// --- tool mode (MCP target proxy test) ---------------------------------------

func handleToolSSE(w http.ResponseWriter, _ *http.Request) {
	sw := newSSEFlusher(w)
	for i, msg := range []string{"hello", "from", "mocktool"} {
		b, _ := json.Marshal(map[string]any{"seq": i, "msg": msg})
		sw.write("", string(b))
		time.Sleep(20 * time.Millisecond)
	}
}

// --- probe mode ----------------------------------------------------------

// runProbe dials target over a plain net.Conn (never through an http.Client,
// and never through the Docker Desktop host port-proxy — the caller must
// pass an in-compose-network address such as "traefik1:80"), hand-writes one
// streaming chat completion request, and times each raw read that carries
// SSE "data:" bytes. It returns 0 iff at least 3 distinct inter-arrival gaps
// of 100ms or more were observed, proving the response arrived as
// incremental chunks rather than one buffered write; 1 otherwise.
func runProbe(target, apiKey, model string) int {
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: dial %s: %v\n", target, err)
		return 1
	}
	defer func() { _ = conn.Close() }()

	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)
	host, _, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		host = target
	}
	req := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: Bearer " + apiKey + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n" + body

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "probe: set deadline: %v\n", err)
		return 1
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		fmt.Fprintf(os.Stderr, "probe: write request: %v\n", err)
		return 1
	}

	var arrivals []time.Time
	var headerBuf []byte
	buf := make([]byte, 4096)
	headerDone := false
	for {
		n, readErr := conn.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !headerDone {
				// The "\r\n\r\n" header/body terminator is not guaranteed to
				// land inside a single Read: accumulate every read into a
				// rolling buffer and search that, not just the latest
				// chunk, so a terminator split across two reads is still
				// found instead of silently discarding both reads' bytes.
				headerBuf = append(headerBuf, chunk...)
				idx := bytes.Index(headerBuf, []byte("\r\n\r\n"))
				if idx < 0 {
					if readErr != nil {
						break
					}
					continue
				}
				headerDone = true
				chunk = headerBuf[idx+4:]
				headerBuf = nil
			}
			if len(chunk) > 0 && bytes.Contains(chunk, []byte("data:")) {
				arrivals = append(arrivals, time.Now())
			}
		}
		if readErr != nil {
			break
		}
	}

	gaps := 0
	for i := 1; i < len(arrivals); i++ {
		if d := arrivals[i].Sub(arrivals[i-1]); d >= 100*time.Millisecond {
			gaps++
		}
	}
	fmt.Printf("probe: target=%s model=%s arrivals=%d gaps>=100ms=%d\n", target, model, len(arrivals), gaps)
	if gaps >= 3 {
		return 0
	}
	return 1
}
