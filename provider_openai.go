package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openaiAdapter is the providerAdapter for provider type "openai": OpenAI
// itself, and any OpenAI-compatible upstream (the operator's own gateway
// included — a real deployment target for this adapter is keyless).
type openaiAdapter struct {
	client *http.Client
	// retry is nil for every adapter built by a bare newOpenAIAdapter call
	// (every v0.1 test, and any caller that never wires one) — nil-safe,
	// per retryPolicy.do's own contract, so leaving it unset is exactly
	// v0.1's one-attempt behavior. buildAdapters (providers.go) sets it
	// from cfg.Retry for production use.
	retry       *retryPolicy
	adapterName string
	baseURL     string
	apiKey      string
	// metadataPath is ProviderConfig.MetadataPath (feature v0.23), set by
	// buildAdapters (providers.go) after construction — empty (the
	// default) makes fetchModelMetadata a no-op, so every existing
	// newOpenAIAdapter call site (every test, and any caller that never
	// sets this) keeps behaving exactly as before this feature existed.
	metadataPath string
	// timeout is this adapter's resolved per-request timeout
	// (resolveProviderTimeout, timeout.go) — UNLIKE retry above, it is NOT
	// left at its zero value by a bare newOpenAIAdapter call: a
	// zero-duration timeout would make watchdogBody's watchdog fire
	// immediately (time.AfterFunc(0, ...) runs at the next scheduler
	// tick), the opposite of "no timeout configured". newOpenAIAdapter
	// sets it to defaultRequestTimeout; buildAdapters (providers.go)
	// overwrites it with the real, config-resolved value for production
	// use, the same way it overwrites retry/metadataPath above.
	timeout time.Duration
}

// newOpenAIAdapter returns an openaiAdapter for provider name, with base as
// its already-defaulted, trailing-slash-trimmed base URL and apiKey as
// already resolved by resolveSecret (empty means keyless). It builds one
// shared *http.Client, reused for every request this adapter makes, with
// its Transport.ResponseHeaderTimeout set to defaultRequestTimeout — see
// the timeout field's own doc comment for why this constructor cannot
// leave timeout at its zero value the way it leaves retry at nil.
func newOpenAIAdapter(name, base, apiKey string) *openaiAdapter {
	return &openaiAdapter{
		adapterName: name,
		baseURL:     base,
		apiKey:      apiKey,
		client:      newAdapterHTTPClient(defaultRequestTimeout),
		timeout:     defaultRequestTimeout,
	}
}

// name implements providerAdapter.
func (a *openaiAdapter) name() string { return a.adapterName }

// typeName implements providerAdapter.
func (a *openaiAdapter) typeName() string { return providerTypeOpenAI }

// base implements providerAdapter.
func (a *openaiAdapter) base() string { return a.baseURL }

// httpClient implements providerAdapter.
func (a *openaiAdapter) httpClient() *http.Client { return a.client }

// requestTimeout implements providerAdapter.
func (a *openaiAdapter) requestTimeout() time.Duration { return a.timeout }

// injectAuth implements providerAdapter: OpenAI's bearer-token scheme. A
// no-op when a.apiKey is empty, per the keyless-upstream ruling — a request
// against a keyless upstream must carry no Authorization header at all,
// not one with an empty token.
func (a *openaiAdapter) injectAuth(r *http.Request) {
	if a.apiKey == "" {
		return
	}
	r.Header.Set("Authorization", "Bearer "+a.apiKey)
}

// requestHeaders returns the headers for one upstream request: a
// Content-Type when the request carries a JSON body, plus whatever
// injectAuth adds. Built through injectAuth (rather than setting
// Authorization directly) so this adapter's outgoing requests exercise the
// same auth path the providerAdapter interface exposes to callers.
func (a *openaiAdapter) requestHeaders(hasBody bool) http.Header {
	h := http.Header{}
	if hasBody {
		h.Set("Content-Type", "application/json")
	}
	a.injectAuth(&http.Request{Header: h})
	return h
}

// chatStreamUsage is the "usage" object OpenAI's streaming chat completion
// chunks carry: null on every content chunk, populated only on the final,
// choices-empty chunk when the client (or this adapter, see chatCompletion)
// requested it via stream_options.include_usage.
type chatStreamUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// chatStreamChunk is the subset of one SSE chunk's JSON payload
// chatCompletion's streaming path needs: whether it carries content
// (Choices non-empty) and, on the final chunk, its usage totals.
type chatStreamChunk struct {
	Usage   *chatStreamUsage  `json:"usage"`
	Choices []json.RawMessage `json:"choices"`
}

// chatCompletion implements providerAdapter.
//
// When req["stream"] is true, chatCompletion always forces
// stream_options.include_usage=true on the outgoing request — a client
// sending include_usage=false must not be able to suppress upstream usage
// reporting and escape token/cost budgets (controller ruling C1). It merges
// that into a copy of the client's own stream_options map when present, so
// the caller's map is never mutated in place. clientAskedUsage records
// whether the client itself asked for usage (via isTruthy on the client's
// own include_usage value, never the forced upstream one), so the
// streaming response can still suppress the resulting usage-only chunk
// from a client that never asked for it, while always capturing the usage
// for accounting.
func (a *openaiAdapter) chatCompletion(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	// gatewayAliasKey must never reach the real provider: an openai-type
	// adapter forwards req verbatim as the upstream wire body, so a
	// leftover alias entry would arrive as an unrecognized request field.
	// This adapter's own response is a verbatim passthrough of whatever
	// the upstream returns, so — unlike anthropic/gemini — there is no
	// alias to echo back into it (documented in gatewayAliasKey's doc
	// comment, routes_unified.go).
	delete(req, gatewayAliasKey)
	streaming, _ := req["stream"].(bool)
	clientAskedUsage := false
	if streaming {
		merged := map[string]any{}
		if existing, ok := req["stream_options"].(map[string]any); ok {
			for k, v := range existing {
				merged[k] = v
			}
			clientAskedUsage = isTruthy(existing["include_usage"])
		}
		merged["include_usage"] = true
		req["stream_options"] = merged
	}

	hdr := a.requestHeaders(true)
	if streaming {
		// Ask the upstream for an SSE response. The Content-Type check below
		// still decides which forwarding path runs — an upstream that
		// ignores stream=true may answer with JSON regardless of this
		// header.
		hdr.Set("Accept", "text/event-stream")
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+"/v1/chat/completions", hdr, req, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	if streaming && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return a.forwardStream(w, resp.Body, clientAskedUsage)
	}
	// Either a non-streaming request, or a streaming one whose upstream
	// ignored stream=true and answered with a normal JSON body: forward it
	// through the same verbatim passthrough non-streaming responses use,
	// instead of emitting an empty SSE stream for a body that was never SSE.
	return a.forwardJSON(w, resp)
}

// embeddings implements providerAdapter. Embeddings responses never
// stream, so it is forwardJSON end to end, same as chatCompletion's
// non-streaming path.
func (a *openaiAdapter) embeddings(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	delete(req, gatewayAliasKey) // see chatCompletion's identical delete for why
	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+"/v1/embeddings", a.requestHeaders(true), req, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}
	return a.forwardJSON(w, resp)
}

// chatUsagePayload extracts the fields forwardJSON needs from a
// non-streaming response body: OpenAI's chat completion and embeddings
// responses both carry a top-level "usage" object, embeddings' simply
// omitting completion_tokens (it decodes as its zero value).
type chatUsagePayload struct {
	Usage chatStreamUsage `json:"usage"`
}

// forwardJSON reads resp's full body (capped at maxResponseBytes),
// extracts its usage for accounting, and writes resp's Content-Type,
// status code, and body verbatim to w — the non-streaming passthrough both
// chatCompletion and embeddings use. A body that fails to decode as JSON
// still passes through unchanged; forwardJSON only fails on a read error,
// since a malformed-but-readable body is the upstream's problem to answer
// for, not a reason to swallow the response.
func (a *openaiAdapter) forwardJSON(w http.ResponseWriter, resp *http.Response) (usage, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return usage{}, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	var payload chatUsagePayload
	_ = json.Unmarshal(body, &payload) // best-effort usage extraction; body still forwards verbatim below

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	return usage{prompt: payload.Usage.PromptTokens, completion: payload.Usage.CompletionTokens}, nil
}

// sseDoneMarker is the exact data payload readSSE hands forwardStream for
// the OpenAI streaming convention's terminal event.
const sseDoneMarker = "[DONE]"

// forwardStream reads body as an upstream text/event-stream and forwards
// each event to w via a fresh sseWriter, flushing per event. The final
// usage-only chunk (empty "choices", non-null "usage") is captured into
// the returned usage either way, but only forwarded to the client when
// clientAskedUsage is true — otherwise it is dropped, since the client
// never asked for it and chatCompletion only requested it upstream for
// accounting. The "[DONE]" sentinel is always forwarded. On a mid-stream
// read error (upstream drops the connection, client context is canceled),
// forwardStream still returns whatever usage it had already captured
// alongside the error — a usage chunk that arrived before the break must
// not be discarded and billed as zero.
func (a *openaiAdapter) forwardStream(w http.ResponseWriter, body io.Reader, clientAskedUsage bool) (usage, error) {
	sw := newSSEWriter(w)
	var u usage

	err := readSSE(body, func(ev sseEvent) error {
		if string(ev.data) == sseDoneMarker {
			sw.writeDone()
			return nil
		}

		var chunk chatStreamChunk
		hasUsage := json.Unmarshal(ev.data, &chunk) == nil && chunk.Usage != nil
		if hasUsage {
			u.prompt = chunk.Usage.PromptTokens
			u.completion = chunk.Usage.CompletionTokens
		}

		isUsageOnlyChunk := hasUsage && len(chunk.Choices) == 0
		if isUsageOnlyChunk && !clientAskedUsage {
			return nil
		}
		return sw.writeData(ev.data)
	})
	if err != nil {
		// u carries whatever usage was already captured before the stream
		// broke — a usage chunk that arrived, then a dropped connection,
		// must not report as zero and get treated as an unbilled request.
		return u, fmt.Errorf("%w: stream: %w", errUpstream, err)
	}
	return u, nil
}

// imagesGeneration implements providerAdapter: a native forward to
// {base}/v1/images/generations. req has already had its "model" field
// rewritten to the provider's own model id by the caller (routes_media.go),
// matching chatCompletion/embeddings' contract. Images are never
// cost-accounted (spec §3, v0.2), so this always returns a zero usage,
// regardless of what the upstream response reports.
func (a *openaiAdapter) imagesGeneration(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	// gatewayAliasKey must never reach the real provider, mirroring
	// chatCompletion's identical delete above: routes_media.go never sets
	// this key itself for a media route (its doc comment), but this
	// adapter forwards req verbatim as the upstream wire body, so a
	// client independently sending a literal "__alias" field of its own
	// would otherwise leak straight through as an unrecognized request
	// field. Unconditional, not conditional on whether the gateway set
	// it — the same defensive posture chatCompletion/embeddings already
	// take.
	delete(req, gatewayAliasKey)
	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+"/v1/images/generations", a.requestHeaders(true), req, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}
	return usage{}, a.forwardMediaBody(w, resp)
}

// audioSpeech implements providerAdapter: a native forward to
// {base}/v1/audio/speech. body is the caller's already re-marshaled JSON
// request (routes_media.go rewrites "model" to the provider's own model id
// before calling in). The response is binary audio, streamed to w via a
// flushWriter as it arrives rather than buffered in full: once this loop
// starts copying bytes, the response has already committed to the client
// and can no longer be retried — matching spec §1's "retry only while ZERO
// response bytes have reached the client" (the retry itself lives entirely
// inside upstreamRawBytes, above this point).
func (a *openaiAdapter) audioSpeech(ctx context.Context, w http.ResponseWriter, body []byte, contentType string) (usage, error) {
	hdr := a.requestHeaders(false)
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	resp, err := upstreamRawBytes(ctx, a.client, http.MethodPost, a.baseURL+"/v1/audio/speech", hdr, body, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	fw := newFlushWriter(w)
	if _, err := io.Copy(fw, io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
		return usage{}, fmt.Errorf("%w: stream audio response: %w", errUpstream, err)
	}
	return usage{}, nil
}

// audioTranscription implements providerAdapter: a native forward to
// {base}/v1/audio/transcriptions. body is the multipart request
// routes_media.go hands in — the client's original bytes when its "model"
// field already named the resolved upstream model id, or a rewritten
// body (a fresh boundary, every other part copied verbatim) when it did
// not, per rewriteMultipartModel's controller ruling — and contentType
// matches whichever body this call received, so the upstream always sees
// a Content-Type whose boundary parameter is consistent with what
// actually follows it. The response (JSON or plain text, depending on the
// client's requested response_format) is copied through unchanged, the
// same non-streaming path images.generations uses.
func (a *openaiAdapter) audioTranscription(ctx context.Context, w http.ResponseWriter, body []byte, contentType string) (usage, error) {
	hdr := a.requestHeaders(false)
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	resp, err := upstreamRawBytes(ctx, a.client, http.MethodPost, a.baseURL+"/v1/audio/transcriptions", hdr, body, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}
	return usage{}, a.forwardMediaBody(w, resp)
}

// forwardMediaBody reads resp's full body (capped at maxResponseBytes) and
// writes resp's Content-Type, status code, and body verbatim to w — the
// copy-through path images.generations and audio-transcriptions share.
// Unlike forwardJSON, it never extracts usage: images/audio are never
// cost-accounted (spec §3, v0.2), so there is nothing to parse the body
// for.
func (a *openaiAdapter) forwardMediaBody(w http.ResponseWriter, resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return nil
}

// modelsPayload is the "data": [{"id": ...}, ...] shape OpenAI's
// /v1/models endpoint returns.
type modelsPayload struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// listModels implements providerAdapter.
func (a *openaiAdapter) listModels(ctx context.Context) ([]string, error) {
	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, a.baseURL+"/v1/models", a.requestHeaders(false), nil, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newProviderHTTPError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	var payload modelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode models response: %w", errUpstream, err)
	}

	ids := make([]string, len(payload.Data))
	for i, d := range payload.Data {
		ids[i] = d.ID
	}
	return ids, nil
}

// lmStudioModelsPayload is the wire shape of LM Studio's own, native
// models endpoint (ProviderConfig.MetadataPath — a real deployment sets
// it to "/api/v0/models"), verified live against a running LM Studio
// instance (2026-08): {"data":[{"id":...,"max_context_length":262144,
// "loaded_context_length":4096}, ...]}. Distinct from modelsPayload
// above, which is OpenAI's own /v1/models shape (LM Studio also serves
// that, unchanged, but it carries no per-model context metadata) — this
// is LM Studio's separate, additional endpoint this feature (v0.23)
// opts into reading.
type lmStudioModelsPayload struct {
	Data []struct {
		ID                  string `json:"id"`
		MaxContextLength    int    `json:"max_context_length"`
		LoadedContextLength int    `json:"loaded_context_length"`
	} `json:"data"`
}

// fetchModelMetadata implements modelMetadataFetcher (feature v0.23,
// registry.go): when a.metadataPath is configured, fetches it and
// returns each reported model id's discovery-captured context length.
// It prefers loaded_context_length when the upstream reports one greater
// than zero — the context actually usable right now, which a runtime can
// configure smaller than the model's own maximum under VRAM-constrained
// settings — and falls back to max_context_length otherwise. a.
// metadataPath left empty (the default; every provider except an
// operator's opt-in) is a fast no-op: (nil, nil), never an error, so
// registry.go's captureModelMetadata can invoke this unconditionally on
// every openai-type adapter without a config check of its own. A
// non-2xx response or a decode failure returns an error — the caller
// treats it as non-fatal, per this feature's own "absent/failed = no
// metadata captured" ruling.
func (a *openaiAdapter) fetchModelMetadata(ctx context.Context) (map[string]int, error) {
	if a.metadataPath == "" {
		return nil, nil
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, a.baseURL+a.metadataPath, a.requestHeaders(false), nil, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newProviderHTTPError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read metadata response body: %w", errUpstream, err)
	}

	var payload lmStudioModelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode metadata response: %w", errUpstream, err)
	}

	out := make(map[string]int, len(payload.Data))
	for _, d := range payload.Data {
		ctxLen := d.MaxContextLength
		if d.LoadedContextLength > 0 {
			ctxLen = d.LoadedContextLength
		}
		if ctxLen > 0 {
			out[d.ID] = ctxLen
		}
	}
	return out, nil
}
