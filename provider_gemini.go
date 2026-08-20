package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// geminiAdapter is the providerAdapter for provider type "gemini".
type geminiAdapter struct {
	client      *http.Client
	adapterName string
	baseURL     string
	apiKey      string
}

// newGeminiAdapter returns a geminiAdapter for provider name, with base as
// its already-defaulted, trailing-slash-trimmed base URL and apiKey as
// already resolved by resolveSecret. Like anthropic and unlike openai-type
// providers, Gemini's API requires an API key on every request — a
// resolved-empty apiKey is a constructor error here, mirroring ruling (c).
func newGeminiAdapter(name, base, apiKey string) (*geminiAdapter, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("llmgateway: provider %q: gemini requires an API key", name)
	}
	return &geminiAdapter{
		adapterName: name,
		baseURL:     base,
		apiKey:      apiKey,
		client:      newAdapterHTTPClient(),
	}, nil
}

// name implements providerAdapter.
func (a *geminiAdapter) name() string { return a.adapterName }

// typeName implements providerAdapter.
func (a *geminiAdapter) typeName() string { return providerTypeGemini }

// base implements providerAdapter.
func (a *geminiAdapter) base() string { return a.baseURL }

// httpClient implements providerAdapter.
func (a *geminiAdapter) httpClient() *http.Client { return a.client }

// injectAuth implements providerAdapter: Gemini's x-goog-api-key header
// scheme. Never a no-op — the constructor already rejected an empty apiKey.
func (a *geminiAdapter) injectAuth(r *http.Request) {
	r.Header.Set("x-goog-api-key", a.apiKey)
}

// requestHeaders returns the headers for one upstream request: a
// Content-Type when the request carries a JSON body, plus whatever
// injectAuth adds.
func (a *geminiAdapter) requestHeaders(hasBody bool) http.Header {
	h := http.Header{}
	if hasBody {
		h.Set("Content-Type", "application/json")
	}
	a.injectAuth(&http.Request{Header: h})
	return h
}

// geminiModelFromRequest reads req["model"] — already rewritten to the
// provider's own model id by the caller, per the providerAdapter interface
// doc — and strips a leading "models/" prefix if present, so a client (or
// an upstream config) that already qualified the id doesn't produce a
// doubled "models/models/..." URL.
func geminiModelFromRequest(req map[string]any) string {
	raw, _ := req["model"].(string)
	return strings.TrimPrefix(raw, "models/")
}

// chatCompletion implements providerAdapter: translates req to a Gemini
// generateContent/streamGenerateContent request via geminiRequestFromOpenAI,
// then forwards the (possibly streamed) response back translated to
// OpenAI's shape. Unlike anthropic and openai-type, the model id is never
// part of the JSON body — Gemini puts it in the URL path — so it is read
// once here and threaded through to both URL construction and the response
// translation's "model" field. A translation failure — a *translateError
// from geminiRequestFromOpenAI — is returned as-is, before any upstream
// request is made.
func (a *geminiAdapter) chatCompletion(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	model := geminiModelFromRequest(req)
	// responseModel (ruling a, ALIAS ECHO): gatewayAliasKey, when present,
	// is the exact id the client requested — echoed into the translated
	// response's "model" field, distinct from model (the bare upstream id),
	// which must keep driving the URL below unchanged. Deleted from req so
	// it cannot leak into the upstream request body.
	responseModel := model
	if alias, ok := req[gatewayAliasKey].(string); ok && alias != "" {
		responseModel = alias
	}
	delete(req, gatewayAliasKey)
	streaming, _ := req["stream"].(bool)

	body, err := geminiRequestFromOpenAI(req)
	if err != nil {
		return usage{}, err
	}

	hdr := a.requestHeaders(true)
	// escapedModel guards against a model id smuggling extra path segments
	// or query syntax ("/", "?", "..") into the upstream URL — url.PathEscape
	// leaves ordinary ids (letters, digits, ":", ".", "-") untouched, per
	// RFC 3986's pchar grammar, so a normal model id is byte-identical after
	// escaping.
	escapedModel := url.PathEscape(model)
	endpoint := a.baseURL + geminiAPIPrefix + escapedModel + ":generateContent"
	if streaming {
		// Ask the upstream for an SSE response. The Content-Type check
		// below still decides which forwarding path runs.
		hdr.Set("Accept", "text/event-stream")
		endpoint = a.baseURL + geminiAPIPrefix + escapedModel + ":streamGenerateContent?alt=sse"
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, endpoint, hdr, body)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	if streaming && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return a.forwardStream(w, resp.Body, responseModel)
	}
	// Either a non-streaming request, or a streaming one whose upstream
	// ignored stream=true and answered with a normal JSON body.
	return a.forwardJSON(w, resp, responseModel)
}

// forwardJSON reads resp's full body (capped at maxResponseBytes),
// translates it to an OpenAI chat.completion response via
// openAIResponseFromGemini, and writes the translated JSON to w.
// gatewayModel becomes the translated response's "model" field, and the
// response's "created" timestamp is generated here — Gemini's response
// carries neither.
func (a *geminiAdapter) forwardJSON(w http.ResponseWriter, resp *http.Response, gatewayModel string) (usage, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return usage{}, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	out, u, err := openAIResponseFromGemini(raw, gatewayModel, time.Now().Unix())
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

// forwardStream reads body as an upstream Gemini text/event-stream,
// translating each event via a geminiStreamState and forwarding every
// resulting OpenAI-shaped chunk to w through a fresh sseWriter, flushing
// per event. gatewayModel and the stream's start time become every chunk's
// "model"/"created" fields. The terminal "[DONE]" is written once readSSE
// returns without error, mirroring ruling (b) — Gemini's stream has no
// distinct terminal event of its own to key off of; a clean EOF is what
// signals the stream ended. On a translate error (a malformed chunk, or a
// genuine stream read error), forwardStream returns without writing
// "[DONE]".
func (a *geminiAdapter) forwardStream(w http.ResponseWriter, body io.Reader, gatewayModel string) (usage, error) {
	st := newGeminiStreamState(gatewayModel, time.Now().Unix())
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

// embeddings implements providerAdapter: translates req's OpenAI "input"
// (a string or an array of strings) to a Gemini :embedContent or
// :batchEmbedContents request, per the embeddings mapping table, and
// translates the response back to an OpenAI embeddings list response.
// Embeddings never stream.
func (a *geminiAdapter) embeddings(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
	model := geminiModelFromRequest(req)
	// responseModel (ruling a, ALIAS ECHO): see chatCompletion's identical
	// pattern above.
	responseModel := model
	if alias, ok := req[gatewayAliasKey].(string); ok && alias != "" {
		responseModel = alias
	}
	delete(req, gatewayAliasKey)

	texts, single, err := geminiEmbeddingInputTexts(req["input"])
	if err != nil {
		return usage{}, err
	}

	var body map[string]any
	var endpoint string
	escapedModel := url.PathEscape(model)
	if single {
		body = geminiEmbedContentRequest(texts[0])
		endpoint = a.baseURL + geminiAPIPrefix + escapedModel + ":embedContent"
	} else {
		// geminiBatchEmbedContentsRequest still takes the unescaped bare
		// model — it writes "models/{model}" into the JSON body, not a URL.
		body = geminiBatchEmbedContentsRequest(model, texts)
		endpoint = a.baseURL + geminiAPIPrefix + escapedModel + ":batchEmbedContents"
	}

	resp, err := upstreamJSON(ctx, a.client, http.MethodPost, endpoint, a.requestHeaders(true), body)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return usage{}, fmt.Errorf("%w: read response body: %w", errUpstream, err)
	}

	out, u, err := openAIEmbeddingResponseFromGemini(raw, single, responseModel, estimatedEmbeddingTokens(texts))
	if err != nil {
		return usage{}, err
	}

	b, err := json.Marshal(out)
	if err != nil {
		return usage{}, fmt.Errorf("%w: marshal translated embeddings response: %w", errUpstream, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return u, nil
}

// geminiModelsListPayload is the "models": [{"name": "models/..."}, ...]
// shape Gemini's models.list endpoint returns (its "name" field is the
// full "models/{model}" resource name, unlike OpenAI's bare "id").
type geminiModelsListPayload struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// listModels implements providerAdapter: GET {base}/v1beta/models,
// stripping the "models/" prefix off each entry's "name" to return bare
// model ids, matching the shape openai-type and anthropic-type adapters
// both return.
func (a *geminiAdapter) listModels(ctx context.Context) ([]string, error) {
	resp, err := upstreamJSON(ctx, a.client, http.MethodGet, a.baseURL+"/v1beta/models", a.requestHeaders(false), nil)
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

	var payload geminiModelsListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode models response: %w", errUpstream, err)
	}

	ids := make([]string, len(payload.Models))
	for i, m := range payload.Models {
		ids[i] = strings.TrimPrefix(m.Name, "models/")
	}
	return ids, nil
}
