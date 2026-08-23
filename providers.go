package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
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
//
// MUST be returned bare, never wrapped with fmt.Errorf("%w", ...):
// routes_unified.go's handleAdapterError relies on a direct type assertion
// (err.(*providerHTTPError)), not errors.As, because errors.As panics under
// yaegi when checking whether an interpreted pointer type implements error
// (verified empirically under real Traefik, Task 15's integration suite).
// A wrapped providerHTTPError would make that assertion fail to find it.
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

// errRequestBuildFailed marks an upstreamJSON failure that happened
// before any network I/O — http.NewRequestWithContext rejected the
// method or URL. It is chained alongside errUpstream (Go's multi-%w), so
// an existing errors.Is(err, errUpstream) check still matches, while
// retryPolicy's isTransient (retry.go) checks specifically for this
// sentinel to treat it as non-transient: retrying an identically
// malformed request produces the identical failure every time, so a
// retry only wastes attempts.
var errRequestBuildFailed = errors.New("llmgateway: build upstream request failed")

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
	// provider. Behavior is type-specific: an openai-type adapter treats
	// a resolved-empty API key as a valid keyless upstream, so injectAuth
	// is a no-op in that case. An anthropic-type adapter's constructor
	// rejects an empty key outright (ruling c), so its injectAuth always
	// sets auth headers — there is no keyless case for it to be a no-op
	// over.
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
	// imagesGeneration proxies req to the provider's image-generation
	// endpoint and writes the response to w (spec §3, v0.2). req has
	// already had its "model" field rewritten to the provider's own model
	// id by the caller, matching chatCompletion/embeddings' contract.
	// Images are never cached (spec §2) and never cost-accounted (spec
	// §3) — every implementation returns a zero usage regardless of what
	// its upstream reports. Anthropic has no images API and always
	// returns a *translateError with notSupported set, mapped to HTTP 501
	// by the same handleAdapterError chatCompletion/embeddings errors
	// already go through.
	imagesGeneration(ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error)
	// audioSpeech proxies body — a JSON request the caller has already
	// re-marshaled with "model" rewritten to the provider's own model id
	// — to the provider's text-to-speech endpoint, streaming the (binary)
	// response to w as it arrives rather than buffering it in full (spec
	// §3, v0.2). Gemini and Anthropic have no OpenAI-compatible TTS
	// endpoint and always return a *translateError with notSupported set.
	audioSpeech(ctx context.Context, w http.ResponseWriter, body []byte, contentType string) (usage, error)
	// audioTranscription proxies body — the client's original raw
	// multipart request, replayed unchanged — to the provider's
	// speech-to-text endpoint, forwarding its (JSON or text) response to w
	// verbatim (spec §3, v0.2). Gemini and Anthropic have no
	// OpenAI-compatible STT endpoint and always return a *translateError
	// with notSupported set.
	audioTranscription(ctx context.Context, w http.ResponseWriter, body []byte, contentType string) (usage, error)
	// httpClient returns the adapter's shared *http.Client, so a caller
	// outside this file (the native passthrough route, routes_passthrough.go)
	// can issue upstream requests through the same connection-pooled client
	// every other adapter call uses, instead of building a fresh one per
	// request. Named httpClient rather than client to avoid colliding with
	// every implementation's own "client" struct field of the same name.
	httpClient() *http.Client
	// requestTimeout returns this adapter's resolved per-request timeout
	// (Config.RequestTimeout or ProviderConfig.RequestTimeout,
	// resolveProviderTimeout in timeout.go) — the idle-progress bound every
	// upstream call through this adapter's shared client is held to.
	// Exposed on the interface so a caller outside this file (native
	// passthrough, routes_passthrough.go's handlePassthrough) can apply the
	// identical watchdog proxyUpstream needs to its own resp.Body read,
	// instead of the timeout only ever protecting upstreamJSON/
	// upstreamRawBytes' own callers.
	requestTimeout() time.Duration
}

// newAdapterHTTPClient returns an *http.Client for a provider adapter to
// hold and reuse across every request it makes: no CLIENT-level timeout
// (Client.Timeout is deliberately left unset), since that would bound the
// whole request including body read — killing exactly the long streams
// this gateway must preserve (a 20-minute generation that keeps producing
// tokens must complete) — and a Transport cloned from http.DefaultTransport
// with a larger per-host idle connection pool, since every request from
// one adapter targets the same upstream host.
//
// timeout sets Transport.ResponseHeaderTimeout: the first of this
// feature's two mechanisms — it bounds only the wait for a response's
// headers, never the body, so it is safe for streaming. The second
// mechanism, bounding idle time WITHIN the body (a provider that sends
// headers and then stalls), is watchdogBody (timeout.go), applied by each
// caller of upstreamBytes/proxyUpstream to the *http.Response.Body this
// client's Do returns — Transport has no equivalent field for that half.
func newAdapterHTTPClient(timeout time.Duration) *http.Client {
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
	tr.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: tr}
}

// attemptRecorder is invoked once per upstream HTTP attempt a shared
// chokepoint makes — retryPolicy.do's own retry loop (retry.go), for every
// adapter method that goes through upstreamJSON/upstreamRawBytes, and
// proxyUpstream's single client.Do call (routes_passthrough.go) — with
// that attempt's raw resp/err, for Feature A's (v0.22) per-provider
// success-rate accounting (limiter.recordProviderAttempt, limits.go). A
// recorder must never touch resp.Body (still owned by its caller, read or
// forwarded afterward) and must never retain resp or err past the call.
type attemptRecorder func(resp *http.Response, err error)

// attemptRecorderCtxKey is the unexported context.Value key
// withAttemptRecorder/attemptRecorderFromContext share. An unexported
// struct type, not a string, so no other package's context.WithValue call
// can ever collide with it by accident.
type attemptRecorderCtxKey struct{}

// withAttemptRecorder returns a context carrying rec, so a chokepoint
// shared across call sites that must NOT all record provider accounting —
// retryPolicy.do and proxyUpstream are both also reached by traffic
// Feature A is deliberately out of scope for: registry.go's discovery
// listModels calls (no recorder ever set on their ctx) and the MCP/A2A
// target proxy (handleTargetProxy, mcp_a2a.go, which never wraps its
// request's context this way) — can look the recorder up without either
// function needing a dedicated parameter every call site would otherwise
// have to pass. A nil rec returns ctx unchanged, so a caller may call this
// unconditionally.
func withAttemptRecorder(ctx context.Context, rec attemptRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptRecorderCtxKey{}, rec)
}

// attemptRecorderFromContext returns the attemptRecorder ctx carries via
// withAttemptRecorder, or nil when none was set — the common case for any
// call path Feature A does not account (see withAttemptRecorder's doc
// comment). A bare comma-ok type assertion, not errors.As or any reflect-
// based check: see providerHTTPError's own doc comment for why a
// yaegi-interpreted plugin must avoid errors.As here.
func attemptRecorderFromContext(ctx context.Context) attemptRecorder {
	rec, _ := ctx.Value(attemptRecorderCtxKey{}).(attemptRecorder)
	return rec
}

// latencyRecorder is invoked exactly once per completed upstream body —
// from watchdogBody.Close (timeout.go), never per Read — with the
// latencySample (metrics.go) that body's own watchdog measured: time to
// first byte, total duration, and whether the response was a stream
// (feat: instrument upstream latency, observation-only per the task
// brief — no throttling or control logic reads this).
//
// Threaded via context exactly like attemptRecorder above, for the same
// reason: upstreamBytes/proxyUpstream have no other way to reach
// Gateway-level state (g.latency, metrics.go). A call site that never
// wires one via withLatencyRecorder gets nil back from
// latencyRecorderFromContext, so registry.go's discovery listModels
// calls and the MCP/A2A target proxy (handleTargetProxy, mcp_a2a.go —
// neither ever wraps its context this way, mirroring
// withAttemptRecorder's own identical exclusion) never observe latency
// either — this feature accounts the same traffic Feature A does, not a
// byte more.
type latencyRecorder func(sample latencySample)

// latencyRecorderCtxKey is the unexported context.Value key
// withLatencyRecorder/latencyRecorderFromContext share — its own type,
// never a string, so no other package's context.WithValue call can
// collide with it, mirroring attemptRecorderCtxKey above.
//
// Deliberately NOT a bare `struct{}` like attemptRecorderCtxKey (yaegi-
// check finding, this feature): under the REAL Yaegi interpreter, two
// DISTINCT zero-sized struct types used as `any`-typed context.WithValue
// keys compare EQUAL to each other via `==` — an interpreter-only
// divergence from compiled Go's own interface-equality semantics, where
// two values of different concrete types are never equal regardless of
// value. With attemptRecorderCtxKey ALSO a bare struct{}, a context
// carrying both (routes_unified.go's withAttemptRecorder then
// withLatencyRecorder) let ctx.Value(attemptRecorderCtxKey{}) wrongly
// match the LATER latencyRecorderCtxKey{} entry instead, silently
// returning nil once type-asserted to attemptRecorder — retryPolicy.do
// (retry.go) then never called rec(resp, err) at all, and
// limiter.recordProviderAttempt (Feature A) went permanently dark for
// every metered request, with no error anywhere: exerciseAttemptAccounting
// (tools/yaegi-check/main.go) is what caught it — a compiled `go test`
// run never sees this divergence, since compiled Go's interface equality
// is correct. The `_ byte` field costs nothing at runtime and makes this
// type's size differ from attemptRecorderCtxKey's, which is enough to
// keep the two apart under the interpreter too.
type latencyRecorderCtxKey struct{ _ byte }

// withLatencyRecorder returns a context carrying rec. A nil rec (the
// common case: metrics.go's own callers only ever pass a non-nil one
// when metricsEnabled(g.cfg) was true at the point of the call) returns
// ctx unchanged, so a caller may call this unconditionally, exactly
// mirroring withAttemptRecorder's own nil handling above.
func withLatencyRecorder(ctx context.Context, rec latencyRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, latencyRecorderCtxKey{}, rec)
}

// latencyRecorderFromContext returns the latencyRecorder ctx carries via
// withLatencyRecorder, or nil when none was set — the common case for
// any call path this feature does not account, or for any deployment
// with metrics collection disabled (see latencyRecorder's own doc
// comment).
func latencyRecorderFromContext(ctx context.Context) latencyRecorder {
	rec, _ := ctx.Value(latencyRecorderCtxKey{}).(latencyRecorder)
	return rec
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
//
// timeout and providerName (an adapter's own resolved requestTimeout()/
// name()) arm the idle-progress watchdog upstreamBytes wraps resp.Body
// in, once a response is actually obtained — see watchdogBody's own doc
// comment, timeout.go, for the mechanism and why it, not
// http.Client.Timeout, is what bounds a stalled body.
//
// policy (retry.go) wraps the whole request-and-receive-status exchange:
// a nil policy, or a disabled one, makes exactly one attempt (v0.1
// behavior, byte-identical). Every caller of upstreamJSON — chatCompletion,
// embeddings, listModels, across all three adapter types — sits upstream
// of any response-body forwarding (forwardJSON/forwardStream), so a retry
// here can only ever replace an attempt whose body no caller has read or
// forwarded yet. That is what makes "zero bytes reached the client" hold
// by construction: forwardStream is invoked only after upstreamJSON has
// already returned its final, retried-or-not response, so a stream that
// dies while forwardStream is mid-flight never re-enters this function
// and is therefore never retried.
func upstreamJSON(ctx context.Context, client *http.Client, method, url string, hdr http.Header, body any, policy *retryPolicy, timeout time.Duration, providerName string) (*http.Response, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request body: %w", errUpstream, err)
		}
		bodyBytes = b
	}
	return upstreamBytes(ctx, client, method, url, hdr, bodyBytes, policy, timeout, providerName)
}

// upstreamRawBytes is upstreamJSON's counterpart for a body the caller has
// already encoded: the audio endpoints' openai-type adapter methods
// (provider_openai.go's audioSpeech and audioTranscription) call this
// directly with a body that must never be marshaled again — audioSpeech's
// is a JSON request the caller (routes_media.go) already re-marshaled
// once after rewriting "model", and audioTranscription's is a raw
// multipart payload upstreamJSON's json.Marshal would corrupt outright.
// Shares upstreamBytes with upstreamJSON, so both get the identical
// retry/zero-bytes-reached invariant documented on upstreamJSON.
func upstreamRawBytes(ctx context.Context, client *http.Client, method, url string, hdr http.Header, body []byte, policy *retryPolicy, timeout time.Duration, providerName string) (*http.Response, error) {
	return upstreamBytes(ctx, client, method, url, hdr, body, policy, timeout, providerName)
}

// upstreamBytes is the shared request-build-and-send core behind both
// upstreamJSON (which marshals body to JSON first) and upstreamRawBytes
// (which sends an already-encoded body verbatim): hdr's values are added
// to the request, bodyBytes (nil for a body-less request) becomes the
// request body unchanged, and Content-Type defaults to
// "application/json" only when bodyBytes is non-nil and hdr set none of
// its own.
func upstreamBytes(ctx context.Context, client *http.Client, method, url string, hdr http.Header, bodyBytes []byte, policy *retryPolicy, timeout time.Duration, providerName string) (*http.Response, error) {
	// rec is looked up once, outside the retry loop's call closure: every
	// attempt shares the identical ctx, so the recorder (nil when no
	// caller ever wired one via withAttemptRecorder) is the same across
	// attempts. watchdogBody uses it too (coordinator adversarial review,
	// 2026-08-23, finding F4): retryPolicy.do's own rec(resp, err) call
	// (retry.go) already ran, at header-arrival time, before any byte of
	// the body was read — a mid-body stall was otherwise invisible to
	// provider-health accounting, since nothing else ever called rec
	// again for that attempt.
	rec := attemptRecorderFromContext(ctx)
	// latRec: same one-lookup-outside-the-retry-loop shape as rec above,
	// and the same reasoning applies — every attempt shares ctx, so a
	// latencyRecorder wired via withLatencyRecorder (nil when metrics
	// collection is disabled, latencyRecorder's own doc comment) is
	// identical across attempts. A retried request still gets its own
	// independent latency observation per attempt (below, start is
	// captured fresh inside the closure), matching how watchdogBody
	// itself is rebuilt fresh per attempt.
	latRec := latencyRecorderFromContext(ctx)

	call := func() (*http.Response, error) {
		var r io.Reader
		if bodyBytes != nil {
			r = bytes.NewReader(bodyBytes)
		}

		// reqCtx/cancel bound this attempt's response body to timeout via
		// watchdogBody (timeout.go), derived fresh per attempt so a retried
		// request gets its own full timeout window rather than inheriting
		// an already-ticking one. cancel is released either by
		// watchdogBody (once resp.Body exists — see its own Close/fire) or
		// directly below, on a build/send failure that never produced a
		// body to wrap.
		reqCtx, cancel := context.WithCancel(ctx)

		// gosec G704 (SSRF via taint analysis) flags url below: every call
		// site builds it from an operator-configured value —
		// adapter.baseURL (ProviderConfig.BaseURL, or defaultBaseURLByType
		// when omitted) concatenated with a fixed, hardcoded path string —
		// never anything request-derived that could change the scheme or
		// host. Gemini's endpoints additionally embed a url.PathEscape'd
		// model id mid-path (provider_gemini.go), which cannot introduce a
		// new host or scheme either. See routes_passthrough.go's
		// proxyUpstream for the one adapter call path where a request-
		// derived path segment genuinely reaches the outgoing URL, and its
		// own comment for why that case is safe.
		req, err := http.NewRequestWithContext(reqCtx, method, url, r) //nolint:gosec // operator-configured host; see comment above
		if err != nil {
			cancel()
			return nil, fmt.Errorf("%w: %w: build request: %w", errUpstream, errRequestBuildFailed, err)
		}
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if bodyBytes != nil && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		// start: captured just before the request is actually sent, so a
		// non-nil latRec's eventual sample (armLatency below) measures from
		// here — matching the task brief's own "from just before the
		// upstream request is sent" definition, not from whatever moment
		// newWatchdogBody happens to run at (which is already after
		// headers arrived).
		start := time.Now()
		resp, err := client.Do(req) //nolint:bodyclose,gosec // caller closes resp.Body; upstreamBytes hands the response, not its lifecycle, back — same operator-configured URL as above
		if err != nil {
			cancel()
			return nil, fmt.Errorf("%w: %w", errUpstream, err)
		}
		wb := newWatchdogBody(resp.Body, cancel, timeout, fmt.Sprintf("provider %q", providerName), rec)
		if latRec != nil {
			// streaming: derived from resp's own Content-Type, the SAME
			// convention each adapter's forwardStream/forwardJSON split
			// already applies (isEventStreamResponse, timeout.go) — decided
			// here because it is already knowable, ahead of whichever of
			// those two the caller picks moments from now.
			wb.armLatency(start, isEventStreamResponse(resp), latRec)
		}
		resp.Body = wb
		return resp, nil
	}

	return policy.do(ctx, call)
}

// configNamePattern is the character set allowed in a provider, mcpServers,
// or agents config key: it is embedded verbatim as a path segment in the
// gateway's own routes (passthroughRoute's "/{provider}/*",
// targetRoute's "/mcp/{name}/*" and "/a2a/{name}/*"), so it is restricted to
// characters safe there rather than the full range a Go map key allows.
var configNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// reservedConfigNames are the provider, mcpServers, and agents names
// rejected at construction because they would shadow one of the gateway's
// own fixed top-level routes: "v1" is the unified API namespace
// (/v1/chat/completions, /v1/models, /v1/embeddings, /v1/mcp/servers,
// /v1/agents), "mcp" and "a2a" are the MCP/A2A target-proxy prefixes
// (/mcp/{name}/..., /a2a/{name}/...). ServeHTTP checks those fixed routes
// ahead of passthrough/target routing (llmgateway.go), so a provider, MCP
// server, or agent configured under one of these names would never be
// reachable — reject it here instead of silently shadowing it.
var reservedConfigNames = map[string]bool{"v1": true, "mcp": true, "a2a": true}

// dotOnlyConfigNames are the two names configNamePattern's character class
// permits (it allows ".") but that are rejected anyway: "." and ".." read
// as directory-traversal segments once embedded as a path segment in the
// gateway's own routes (passthroughRoute's "/{provider}/*", targetRoute's
// "/mcp/{name}/*" and "/a2a/{name}/*"), even though nothing in this
// package's own path handling currently mis-resolves them — disallowing
// them at construction is cheap, and removes any dependence on that
// staying true.
var dotOnlyConfigNames = map[string]bool{".": true, "..": true}

// validateConfigName reports an error unless name matches configNamePattern,
// is not one of dotOnlyConfigNames, and is not one of reservedConfigNames.
// kind labels the config section (e.g. "provider", "mcpServers", "agents")
// in the error message.
func validateConfigName(kind, name string) error {
	if !configNamePattern.MatchString(name) {
		return fmt.Errorf("llmgateway: %s name %q is invalid: must match %s", kind, name, configNamePattern.String())
	}
	if dotOnlyConfigNames[name] {
		return fmt.Errorf("llmgateway: %s name %q is invalid: must not be \".\" or \"..\"", kind, name)
	}
	if reservedConfigNames[name] {
		return fmt.Errorf("llmgateway: %s name %q is reserved and would shadow a gateway route", kind, name)
	}
	return nil
}

// validateMetadataPath rejects a non-empty ProviderConfig.MetadataPath
// (feature v0.23) that does not start with "/" (review fix, folded
// minor) — fetchModelMetadata (provider_openai.go) builds its request
// URL by plain string concatenation, a.baseURL+a.metadataPath; a value
// missing the leading slash would silently glue onto baseURL's own path
// with no separator (e.g. "http://host:1234api/v0/models"), a malformed
// URL that only surfaces as a confusing runtime fetch failure instead of
// a clear, named construction error. Empty (the default, metadata
// capture disabled) is always valid.
func validateMetadataPath(providerName, metadataPath string) error {
	if metadataPath == "" {
		return nil
	}
	if !strings.HasPrefix(metadataPath, "/") {
		return fmt.Errorf("llmgateway: provider %q: metadataPath %q must start with \"/\"", providerName, metadataPath)
	}
	return nil
}

// providerPassthroughEnabled reports whether providerName's native
// passthrough route is reachable (security+performance audit,
// 2026-08-22): true when ProviderConfig.Passthrough is nil (the default)
// or explicitly true; false only when an operator set it to false.
// cfg.Providers[providerName] is assumed to already be a known key —
// ServeHTTP's own passthrough route gate (llmgateway.go) checks
// g.adapters[providerName] first, and buildAdapters guarantees adapters
// and cfg.Providers share the same key set — but a missing key still
// defaults to true here rather than panicking, matching every other
// nil-means-default helper in this package.
func providerPassthroughEnabled(cfg *Config, providerName string) bool {
	pc, ok := cfg.Providers[providerName]
	if !ok || pc.Passthrough == nil {
		return true
	}
	return *pc.Passthrough
}

// buildAdapters resolves cfg.Providers into a map of providerAdapter keyed
// by provider name. Every provider name is validated (validateConfigName)
// and every ProviderConfig value must be non-nil before anything else runs,
// so a malformed map key or a nil map value fails construction with a clear
// error instead of a nil-pointer panic. For each provider it resolves
// APIKey via resolveSecret (propagating that error) and defaults BaseURL by
// provider type when the config omits one, trimming any trailing slash
// either way. An unknown provider type is a config error. A resolved empty
// key is a valid keyless upstream for openai-type providers, but a
// constructor error for anthropic-type ones — newAnthropicAdapter enforces
// that, per ruling (c); this function just propagates whatever error it
// returns. A configured gemini provider follows the same constructor-error
// ruling — newGeminiAdapter enforces it too.
//
// buildAdapters also derives cfg.Retry into a single *retryPolicy
// (newRetryPolicy, retry.go — an invalid Retry config fails construction
// the same way any other bad config value does) and attaches it to every
// adapter it constructs. Deriving it here, from cfg, rather than taking it
// as a parameter built upstream in newGateway, keeps buildAdapters'
// signature at its v0.1 shape — buildAdapters(cfg *Config) — so every
// existing direct call to it (production and test) keeps compiling and
// behaving identically when cfg.Retry is left at its zero value.
func buildAdapters(cfg *Config) (map[string]providerAdapter, error) {
	policy, err := newRetryPolicy(cfg.Retry)
	if err != nil {
		return nil, err
	}

	// globalTimeout is resolved once, up front, so a malformed
	// Config.RequestTimeout fails construction even for a deployment where
	// every provider happens to set its own ProviderConfig.RequestTimeout
	// override — resolveProviderTimeout's own doc comment (timeout.go).
	// timeoutErr, not err (govet shadow): a second err reused across this
	// function's top-level scope made every err re-declared inside the
	// provider loop below read as a suspicious shadow.
	globalTimeout, timeoutErr := resolveRequestTimeout(cfg.RequestTimeout, "requestTimeout")
	if timeoutErr != nil {
		return nil, timeoutErr
	}

	adapters := make(map[string]providerAdapter, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		if pc == nil {
			return nil, fmt.Errorf("llmgateway: provider %q: config must not be nil", name)
		}
		if err := validateConfigName("provider", name); err != nil {
			return nil, err
		}
		apiKey, err := resolveSecret(pc.APIKey)
		if err != nil {
			return nil, fmt.Errorf("llmgateway: provider %q: %w", name, err)
		}

		base := strings.TrimSuffix(pc.BaseURL, "/")
		if base == "" {
			base = defaultBaseURLByType[pc.Type]
		}

		if err := validateMetadataPath(name, pc.MetadataPath); err != nil {
			return nil, err
		}

		// timeoutErr, not err (govet shadow — see globalTimeout's identical
		// note above): this loop already declares its own err via
		// apiKey, err := resolveSecret above; reusing it here for a second,
		// later purpose is what made validateMetadataPath's unrelated
		// if err := ...; err != nil read as a suspicious shadow of it.
		timeout, timeoutErr := resolveProviderTimeout(name, pc.RequestTimeout, globalTimeout)
		if timeoutErr != nil {
			return nil, timeoutErr
		}

		switch pc.Type {
		case providerTypeOpenAI:
			a := newOpenAIAdapter(name, base, apiKey)
			a.retry = policy
			a.metadataPath = pc.MetadataPath
			// timeout/client are re-resolved here rather than left at the
			// bare constructor's defaultRequestTimeout default (mirroring
			// how a.retry/a.metadataPath are also only ever set to their
			// real, config-derived values here, not by newOpenAIAdapter
			// itself) — client is rebuilt, not mutated in place, since
			// Transport.ResponseHeaderTimeout is set once at construction
			// (newAdapterHTTPClient, above) and every adapter's Transport
			// is otherwise identical.
			a.timeout = timeout
			a.client = newAdapterHTTPClient(timeout)
			adapters[name] = a
		case providerTypeAnthropic:
			a, err := newAnthropicAdapter(name, base, apiKey)
			if err != nil {
				return nil, err
			}
			a.retry = policy
			a.timeout = timeout
			a.client = newAdapterHTTPClient(timeout)
			adapters[name] = a
		case providerTypeGemini:
			g, err := newGeminiAdapter(name, base, apiKey)
			if err != nil {
				return nil, err
			}
			g.retry = policy
			g.timeout = timeout
			g.client = newAdapterHTTPClient(timeout)
			adapters[name] = g
		default:
			return nil, fmt.Errorf("llmgateway: provider %q: unknown type %q", name, pc.Type)
		}
	}
	return adapters, nil
}
