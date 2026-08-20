package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Provider type identifiers, matched against ProviderConfig.Type.
const (
	providerTypeOpenAI    = "openai"
	providerTypeAnthropic = "anthropic"
	providerTypeGemini    = "gemini"
)

// Default base URLs applied by buildAdapters when a ProviderConfig omits
// BaseURL, keyed by provider type.
const (
	defaultBaseOpenAI    = "https://api.openai.com"
	defaultBaseAnthropic = "https://api.anthropic.com"
	defaultBaseGemini    = "https://generativelanguage.googleapis.com"
)

// defaultBaseURLByType maps a provider type to its default base URL. It is
// read-only after package initialization, the same pattern pricing.go uses
// for builtinPricing.
var defaultBaseURLByType = map[string]string{
	providerTypeOpenAI:    defaultBaseOpenAI,
	providerTypeAnthropic: defaultBaseAnthropic,
	providerTypeGemini:    defaultBaseGemini,
}

// maxResponseBytes caps how much of a non-streaming upstream response body
// an adapter reads into memory: 32MiB, generous for a chat completion or
// embeddings JSON body while bounding memory against a misbehaving or
// oversized upstream response.
const maxResponseBytes = 32 << 20

// adapterIdleConnsPerHost bounds each adapter's shared http.Client's idle
// connection pool for its single upstream host, so repeated requests reuse
// connections instead of paying a fresh TLS handshake per call.
const adapterIdleConnsPerHost = 16

// providerHTTPError reports a non-2xx response from an upstream provider.
// Adapters return it instead of writing the error to the client themselves,
// so the caller — the unified route in a later task — decides whether to
// pass the body through verbatim or wrap it in the gateway's own error
// envelope.
type providerHTTPError struct {
	body   []byte
	status int
}

// Error implements the error interface. It deliberately omits body: the
// caller that inspects providerHTTPError.body decides what, if anything, of
// the upstream response to surface.
func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("provider returned status %d", e.status)
}

// newProviderHTTPError builds a providerHTTPError from a non-2xx upstream
// response, reading its body (capped at maxResponseBytes) before the caller
// closes it.
func newProviderHTTPError(resp *http.Response) *providerHTTPError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return &providerHTTPError{status: resp.StatusCode, body: body}
}

// errUpstream is a sentinel wrapped into errors an adapter returns when it
// cannot reach, or cannot make sense of a response from, its upstream —
// distinct from providerHTTPError, which means the upstream answered, just
// with a non-2xx status.
var errUpstream = errors.New("llmgateway: upstream request failed")

// providerAdapter is the gateway's uniform interface over one configured
// upstream LLM provider. Task 9 (anthropic) and Task 10 (gemini) implement
// it alongside this task's openai-type adapter; Tasks 11-12 wire a built
// map of these into the request routing path.
type providerAdapter interface {
	// name returns the provider's configured name (the key under
	// Config.Providers).
	name() string
	// typeName returns the provider's configured type, e.g. "openai".
	typeName() string
	// base returns the provider's resolved base URL, trailing slash
	// trimmed.
	base() string
	// injectAuth sets whatever headers r needs to authenticate against the
	// provider. It is a no-op when the provider's resolved API key is
	// empty — a keyless upstream is a valid configuration, not an error.
	injectAuth(r *http.Request)
	// chatCompletion proxies req to the provider's chat completions
	// endpoint and writes the (possibly streamed) response to w. req has
	// already had its "model" field rewritten to the provider's own model
	// id by the caller. On a non-2xx upstream response, chatCompletion
	// writes nothing to w and returns a *providerHTTPError instead.
	chatCompletion(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)
	// embeddings mirrors chatCompletion for the provider's embeddings
	// endpoint. It never streams.
	embeddings(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)
	// listModels returns the provider's available model ids, for discovery.
	listModels(ctx context.Context) ([]string, error)
}

// newAdapterHTTPClient returns an *http.Client for a provider adapter to
// hold and reuse across every request it makes: no client-level timeout,
// since a streaming chat completion can legitimately run for minutes and
// callers bound requests with a context deadline instead, and a Transport
// cloned from http.DefaultTransport with a larger per-host idle connection
// pool, since every request from one adapter targets the same upstream
// host.
func newAdapterHTTPClient() *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		tr = tr.Clone()
	} else {
		// Defensive fallback: something in the shared Traefik process
		// replaced http.DefaultTransport with a type that is not
		// *http.Transport. A fresh zero-value Transport (Go's own defaults)
		// is safer than panicking at adapter construction over a type
		// assertion this package does not control.
		tr = &http.Transport{}
	}
	tr.MaxIdleConnsPerHost = adapterIdleConnsPerHost
	return &http.Client{Transport: tr}
}

// upstreamJSON issues an HTTP request to url: body, when non-nil, is
// marshaled as the JSON request body; hdr's values are added to the
// request (a caller builds this from its adapter's auth and content-type
// headers). It returns the raw *http.Response for the caller to inspect
// the status code and read or stream the body from — closing resp.Body is
// the caller's responsibility. A failure to encode the body, build the
// request, or reach the upstream returns an error wrapping errUpstream;
// a non-2xx status is not itself an error here, since some callers (e.g.
// listModels) have nothing to write through and every current caller
// checks resp.StatusCode itself.
func upstreamJSON(ctx context.Context, client *http.Client, method, url string, hdr http.Header, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request body: %w", errUpstream, err)
		}
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", errUpstream, err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req) //nolint:bodyclose // caller closes resp.Body; upstreamJSON hands the response, not its lifecycle, back
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUpstream, err)
	}
	return resp, nil
}

// buildAdapters resolves cfg.Providers into a map of providerAdapter keyed
// by provider name. For each provider it resolves APIKey via resolveSecret
// (propagating that error, but treating a resolved empty key as a valid
// keyless upstream rather than an error) and defaults BaseURL by provider
// type when the config omits one, trimming any trailing slash either way.
// An unknown provider type is a config error. A configured anthropic or
// gemini provider currently errors too — Tasks 9 and 10 fill in those
// switch cases; every other case here is unaffected by that.
func buildAdapters(cfg *Config) (map[string]providerAdapter, error) {
	adapters := make(map[string]providerAdapter, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		apiKey, err := resolveSecret(pc.APIKey)
		if err != nil {
			return nil, fmt.Errorf("llmgateway: provider %q: %w", name, err)
		}

		base := strings.TrimSuffix(pc.BaseURL, "/")
		if base == "" {
			base = defaultBaseURLByType[pc.Type]
		}

		switch pc.Type {
		case providerTypeOpenAI:
			adapters[name] = newOpenAIAdapter(name, base, apiKey)
		case providerTypeAnthropic:
			return nil, fmt.Errorf("llmgateway: provider %q: type %q not yet implemented", name, pc.Type)
		case providerTypeGemini:
			return nil, fmt.Errorf("llmgateway: provider %q: type %q not yet implemented", name, pc.Type)
		default:
			return nil, fmt.Errorf("llmgateway: provider %q: unknown type %q", name, pc.Type)
		}
	}
	return adapters, nil
}
