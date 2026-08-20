package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openaiAdapter is the providerAdapter for provider type "openai": OpenAI
// itself, and any OpenAI-compatible upstream (the operator's own gateway
// included — a real deployment target for this adapter is keyless).
type openaiAdapter struct {
	client      *http.Client
	adapterName string
	baseURL     string
	apiKey      string
}

// newOpenAIAdapter returns an openaiAdapter for provider name, with base as
// its already-defaulted, trailing-slash-trimmed base URL and apiKey as
// already resolved by resolveSecret (empty means keyless). It builds one
// shared *http.Client, reused for every request this adapter makes.
func newOpenAIAdapter(name, base, apiKey string) *openaiAdapter {
	return &openaiAdapter{
		adapterName: name,
		baseURL:     base,
		apiKey:      apiKey,
		client:      newAdapterHTTPClient(),
	}
}

// name implements providerAdapter.
func (a *openaiAdapter) name() string { return a.adapterName }

// typeName implements providerAdapter.
func (a *openaiAdapter) typeName() string { return providerTypeOpenAI }

// base implements providerAdapter.
func (a *openaiAdapter) base() string { return a.baseURL }

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
// When req["stream"] is true, chatCompletion injects
// stream_options.include_usage=true into the upstream request only when
// the client did not already set stream_options itself — remembering
// whether the client asked, so the streaming response can suppress the
// resulting usage-only chunk from a client that never asked for it while
// still capturing its usage for accounting.
func (a *openaiAdapter) chatCompletion(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	streaming, _ := req["stream"].(bool)
	clientAskedUsage := false
	if streaming {
		if _, present := req["stream_options"]; present {
			clientAskedUsage = true
		} else {
			req["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	hdr := a.requestHeaders(true)
	if streaming {
		// Ask the upstream for an SSE response. The Content-Type check below
		// still decides which forwarding path runs — an upstream that
		// ignores stream=true may answer with JSON regardless of this
		// header.
		hdr.Set("Accept", "text/event-stream")
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+"/v1/chat/completions", hdr, req)
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
	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+"/v1/embeddings", a.requestHeaders(true), req)
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

// modelsPayload is the "data": [{"id": ...}, ...] shape OpenAI's
// /v1/models endpoint returns.
type modelsPayload struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// listModels implements providerAdapter.
func (a *openaiAdapter) listModels(ctx context.Context) ([]string, error) {
	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, a.baseURL+"/v1/models", a.requestHeaders(false), nil)
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
