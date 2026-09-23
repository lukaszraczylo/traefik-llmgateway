package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	client *http.Client
	// retry is nil for every adapter built by a bare newAnthropicAdapter
	// call (every v0.1 test, and any caller that never wires one) —
	// nil-safe, per retryPolicy.do's own contract, so leaving it unset is
	// exactly v0.1's one-attempt behavior. buildAdapters (providers.go)
	// sets it from cfg.Retry for production use.
	retry       *retryPolicy
	adapterName string
	baseURL     string
	apiKey      string
	// timeout is this adapter's resolved per-request timeout —
	// resolveProviderTimeout, timeout.go; see openaiAdapter.timeout's own
	// doc comment (provider_openai.go) for why this is set to
	// defaultRequestTimeout at construction rather than left at its zero
	// value the way retry is.
	timeout time.Duration
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
		client:      newAdapterHTTPClient(defaultRequestTimeout),
		timeout:     defaultRequestTimeout,
	}, nil
}

// name implements providerAdapter.
func (a *anthropicAdapter) name() string { return a.adapterName }

// typeName implements providerAdapter.
func (a *anthropicAdapter) typeName() string { return providerTypeAnthropic }

// base implements providerAdapter.
func (a *anthropicAdapter) base() string { return a.baseURL }

// httpClient implements providerAdapter.
func (a *anthropicAdapter) httpClient() *http.Client { return a.client }

// requestTimeout implements providerAdapter.
func (a *anthropicAdapter) requestTimeout() time.Duration { return a.timeout }

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
	// gatewayAliasKey (ruling a, ALIAS ECHO), when present, is the exact id
	// the client requested — echoed into the translated response's "model"
	// field below instead of the bare upstream id, then deleted so it
	// cannot leak into the upstream request body.
	if alias, ok := req[gatewayAliasKey].(string); ok && alias != "" {
		gatewayModel = alias
	}
	delete(req, gatewayAliasKey)
	streaming, _ := req["stream"].(bool)
	// clientAskedUsage (M1 fix): stream_options.include_usage is an
	// OpenAI-wire field with no Anthropic equivalent, so
	// anthropicRequestFromOpenAI never forwards it upstream — it must be
	// read here, before translation, so forwardStream knows whether to
	// synthesize the final usage-only chunk OpenAI-compatible clients
	// expect.
	clientAskedUsage := false
	if streaming {
		if so, ok := req["stream_options"].(map[string]any); ok {
			clientAskedUsage = isTruthy(so["include_usage"])
		}
	}

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

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, a.baseURL+anthropicMessagesPath, hdr, body, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	if streaming && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return a.forwardStream(w, resp.Body, gatewayModel, clientAskedUsage)
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
//
// clientAskedUsage (M1 fix), when true, makes forwardStream write one
// extra chunk — st.usageChunk(), empty "choices" plus the accumulated
// usage — right before "[DONE]", the same final shape an OpenAI-type
// upstream sends when stream_options.include_usage is set
// (provider_openai.go's forwardStream). Anthropic's own stream never
// carries such a chunk, so this adapter synthesizes it instead of
// relying on anything upstream.
func (a *anthropicAdapter) forwardStream(w http.ResponseWriter, body io.Reader, gatewayModel string, clientAskedUsage bool) (usage, error) {
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
	if clientAskedUsage {
		if werr := sw.writeData(st.usageChunk()); werr != nil {
			return st.usage(), fmt.Errorf("%w: stream: %w", errUpstream, werr)
		}
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

// imagesGeneration implements providerAdapter: Anthropic has no images API
// (spec §3, v0.2), so this never makes an upstream request.
func (a *anthropicAdapter) imagesGeneration(_ context.Context, _ http.ResponseWriter, _ map[string]any) (usage, error) {
	return usage{}, &translateError{msg: "image generation not supported for anthropic models", notSupported: true}
}

// audioSpeech implements providerAdapter: Anthropic has no
// OpenAI-compatible text-to-speech API (spec §3, v0.2), so this never
// makes an upstream request.
func (a *anthropicAdapter) audioSpeech(_ context.Context, _ http.ResponseWriter, _ []byte, _ string) (usage, error) {
	return usage{}, &translateError{msg: "audio speech not supported for anthropic models", notSupported: true}
}

// audioTranscription implements providerAdapter: Anthropic has no
// OpenAI-compatible speech-to-text API (spec §3, v0.2), so this never
// makes an upstream request.
func (a *anthropicAdapter) audioTranscription(_ context.Context, _ http.ResponseWriter, _ []byte, _ string) (usage, error) {
	return usage{}, &translateError{msg: "audio transcription not supported for anthropic models", notSupported: true}
}

// anthropicModelsPayload is Anthropic's /v1/models response shape: the
// same {"data":[{"id"}]} list modelsPayload (provider_openai.go) reads,
// plus the cursor-pagination fields (M7 fix) Anthropic's Models API adds
// on top of it — has_more/last_id, paired with the after_id query
// parameter the next page's request carries.
type anthropicModelsPayload struct {
	LastID string `json:"last_id"`
	Data   []struct {
		ID string `json:"id"`
	} `json:"data"`
	HasMore bool `json:"has_more"`
}

// anthropicListModelsPageCap bounds how many pages listModels follows
// (M7 fix): a misbehaving upstream that never reports has_more:false, or
// whose last_id never advances, would otherwise page forever. 20 pages
// at anthropicListModelsPageSize covers far more models than any real
// catalog without risking an unbounded discovery loop against a single
// provider.
const anthropicListModelsPageCap = 20

// anthropicListModelsPageSize is the `limit` sent on every /v1/models
// page: Anthropic's documented maximum (default 20, range 1-1000). At the
// default page size a full catalog took several sequential requests, which
// could overrun registry.go's warmFillTimeout at pod start; one request
// at the maximum returns the whole catalog.
const anthropicListModelsPageSize = 1000

// listModels implements providerAdapter: GET {base}/v1/models, following
// Anthropic's has_more/last_id cursor pagination (M7 fix) via the
// after_id query parameter, up to anthropicListModelsPageCap pages —
// without it, models past the first page never entered the registry and
// silently 404ed unless explicitly listed in config. A non-2xx response
// on any page — including a 404 from an older Anthropic API version that
// predates this endpoint — returns a *providerHTTPError as-is; the
// registry (Task 11) falls back to a provider's explicitly configured
// models on error rather than this adapter guessing at a fallback
// itself.
func (a *anthropicAdapter) listModels(ctx context.Context) ([]string, error) {
	var ids []string
	afterID := ""
	for page := 0; page < anthropicListModelsPageCap; page++ {
		pageIDs, hasMore, lastID, err := a.listModelsPage(ctx, afterID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, pageIDs...)
		if !hasMore || lastID == "" {
			break
		}
		afterID = lastID
	}
	return ids, nil
}

// listModelsPage fetches one page of Anthropic's /v1/models, starting
// after afterID (empty for the first page), and returns that page's
// model ids alongside has_more/last_id for listModels' own pagination
// loop above.
func (a *anthropicAdapter) listModelsPage(ctx context.Context, afterID string) (ids []string, hasMore bool, lastID string, err error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(anthropicListModelsPageSize))
	if afterID != "" {
		query.Set("after_id", afterID)
	}
	endpoint := a.baseURL + anthropicModelsPath + "?" + query.Encode()
	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, endpoint, a.requestHeaders(false), nil, a.retry, a.timeout, a.adapterName)
	if err != nil {
		return nil, false, "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, "", newProviderHTTPError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, false, "", fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	var payload anthropicModelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, "", fmt.Errorf("%w: decode models response: %w", errUpstream, err)
	}

	ids = make([]string, len(payload.Data))
	for i, d := range payload.Data {
		ids[i] = d.ID
	}
	return ids, payload.HasMore, payload.LastID, nil
}
