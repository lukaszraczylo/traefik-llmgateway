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

// anthropicMessagesPath and anthropicModelsPath are Anthropic's Messages
// and Models API endpoints, appended to the adapter's base URL.
const (
	anthropicMessagesPath = "/v1/messages"
	anthropicModelsPath   = "/v1/models"
)

// anthropicAdapter is the providerAdapter for provider type "anthropic".
type anthropicAdapter struct {
	client      *http.Client
	adapterName string
	baseURL     string
	apiKey      string
}

// newAnthropicAdapter returns an anthropicAdapter for provider name, with
// base as its already-defaulted, trailing-slash-trimmed base URL and
// apiKey as already resolved by resolveSecret. Unlike openai-type
// providers, Anthropic's API requires an API key on every request — a
// resolved-empty apiKey is a constructor error here, per ruling (c),
// rather than the keyless-upstream case openai-type providers accept.
func newAnthropicAdapter(name, base, apiKey string) (*anthropicAdapter, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("llmgateway: provider %q: anthropic requires an API key", name)
	}
	return &anthropicAdapter{
		adapterName: name,
		baseURL:     base,
		apiKey:      apiKey,
		client:      newAdapterHTTPClient(),
	}, nil
}

// name implements providerAdapter.
func (a *anthropicAdapter) name() string { return a.adapterName }

// typeName implements providerAdapter.
func (a *anthropicAdapter) typeName() string { return providerTypeAnthropic }

// base implements providerAdapter.
func (a *anthropicAdapter) base() string { return a.baseURL }

// injectAuth implements providerAdapter: Anthropic's x-api-key scheme,
// plus the anthropic-version header every request must carry. Unlike
// openai-type's injectAuth, this is never a no-op — the constructor
// already rejected an empty apiKey.
func (a *anthropicAdapter) injectAuth(r *http.Request) {
	r.Header.Set("x-api-key", a.apiKey)
	r.Header.Set("anthropic-version", anthropicAPIVersion)
}

// requestHeaders returns the headers for one upstream request: a
// Content-Type when the request carries a JSON body, plus whatever
// injectAuth adds.
func (a *anthropicAdapter) requestHeaders(hasBody bool) http.Header {
	h := http.Header{}
	if hasBody {
		h.Set("Content-Type", "application/json")
	}
	a.injectAuth(&http.Request{Header: h})
	return h
}

// chatCompletion implements providerAdapter: translates req to an
// Anthropic Messages API request via anthropicRequestFromOpenAI, then
// forwards the (possibly streamed) response back translated to OpenAI's
// shape. A translation failure — a *translateError from
// anthropicRequestFromOpenAI — is returned as-is, before any upstream
// request is made.
func (a *anthropicAdapter) chatCompletion(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	gatewayModel, _ := req["model"].(string)
	streaming, _ := req["stream"].(bool)

	body, err := anthropicRequestFromOpenAI(req)
	if err != nil {
		return usage{}, err
	}

	hdr := a.requestHeaders(true)
	if streaming {
		// Ask the upstream for an SSE response. The Content-Type check
		// below still decides which forwarding path runs.
		hdr.Set("Accept", "text/event-stream")
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+anthropicMessagesPath, hdr, body)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	if streaming && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return a.forwardStream(w, resp.Body, gatewayModel)
	}
	// Either a non-streaming request, or a streaming one whose upstream
	// ignored stream=true and answered with a normal JSON body.
	return a.forwardJSON(w, resp, gatewayModel)
}

// forwardJSON reads resp's full body (capped at maxResponseBytes),
// translates it to an OpenAI chat.completion response via
// openAIResponseFromAnthropic, and writes the translated JSON to w.
// gatewayModel becomes the translated response's "model" field, and the
// response's "created" timestamp is generated here — Anthropic's response
// carries neither.
func (a *anthropicAdapter) forwardJSON(w http.ResponseWriter, resp *http.Response, gatewayModel string) (usage, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return usage{}, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	out, u, err := openAIResponseFromAnthropic(raw, gatewayModel, time.Now().Unix())
	if err != nil {
		return usage{}, err
	}

	b, err := json.Marshal(out)
	if err != nil {
		return usage{}, fmt.Errorf("%w: marshal translated response: %w", errUpstream, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return u, nil
}

// forwardStream reads body as an upstream Anthropic text/event-stream,
// translating each event via an anthropicStreamState and forwarding every
// resulting OpenAI-shaped chunk to w through a fresh sseWriter, flushing
// per event. gatewayModel and the stream's start time become every
// chunk's "model"/"created" fields. Per ruling (b), the terminal
// "[DONE]" is written once readSSE returns without error — whether that
// is because the upstream sent message_stop (which the translator itself
// emits nothing for) or because the connection reached a clean EOF
// without one. On a translate error (the "error" event, or a genuine
// stream read error), forwardStream returns without writing "[DONE]" —
// the caller already saw the connection fail or an explicit upstream
// error chunk.
func (a *anthropicAdapter) forwardStream(w http.ResponseWriter, body io.Reader, gatewayModel string) (usage, error) {
	st := newAnthropicStreamState(gatewayModel, time.Now().Unix())
	sw := newSSEWriter(w)

	err := readSSE(body, func(ev sseEvent) error {
		chunks, terr := st.translate(ev)
		for _, c := range chunks {
			if werr := sw.writeData(c); werr != nil {
				return werr
			}
		}
		return terr
	})
	if err != nil {
		return st.usage(), fmt.Errorf("%w: stream: %w", errUpstream, err)
	}
	sw.writeDone()
	return st.usage(), nil
}

// embeddings implements providerAdapter. Anthropic's API has no
// embeddings endpoint, so this never makes an upstream request — it
// always returns a *translateError with notSupported set, which the
// caller (Task 12) maps to HTTP 501 rather than 400.
func (a *anthropicAdapter) embeddings(_ context.Context, _ http.ResponseWriter, _ map[string]any) (usage, error) {
	return usage{}, &translateError{msg: "embeddings not supported for anthropic models", notSupported: true}
}

// listModels implements providerAdapter: GET {base}/v1/models, parsing
// the same "data": [{"id": ...}] shape OpenAI's endpoint uses (Anthropic's
// 2026 Models API responds in the same shape). A non-2xx response —
// including a 404 from an older Anthropic API version that predates this
// endpoint — returns a *providerHTTPError as-is; the registry (Task 11)
// falls back to a provider's explicitly configured models on error rather
// than this adapter guessing at a fallback itself.
func (a *anthropicAdapter) listModels(ctx context.Context) ([]string, error) {
	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, a.baseURL+anthropicModelsPath, a.requestHeaders(false), nil)
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
