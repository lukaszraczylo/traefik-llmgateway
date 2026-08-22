package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// messagesPath is the inbound Anthropic Messages API route this gateway
// serves: POST /v1/messages. Distinct from anthropicMessagesPath
// (provider_anthropic.go), which is the SAME literal path string but
// names where an anthropic-type adapter's OWN outbound call lands on its
// configured upstream — two different constants for two different
// directions, kept apart so a future change to one is never mistaken for
// the other.
const messagesPath = "/v1/messages"

// cacheEndpointMessages is cacheKey's endpoint argument (cache.go) for
// this route, mirroring cacheEndpointChat/cacheEndpointEmbeddings
// (routes_unified.go): keeps a /v1/messages request from ever colliding
// into a /v1/chat/completions cache entry even when a request happens to
// canonicalize to an identical provider/upstreamModel/body — the two
// routes answer in different wire shapes (Anthropic vs OpenAI), so a
// cache hit across them would hand a client the wrong envelope entirely.
const cacheEndpointMessages = "messages"

// anthropicPassthroughForwardedHeaders is the ALLOWLIST of client request
// headers callAnthropicMessagesPassthrough forwards to an anthropic-type
// upstream (item 5 fix, 2026-08-22 review): Claude Code and the Anthropic
// SDKs send anthropic-beta to opt into features this gateway must not
// silently disable on a route named "passthrough" — 1M context,
// token-efficient tools, computer use, among others. Deliberately an
// allowlist, never a blanket forward: Authorization, x-api-key, Cookie,
// and every hop-by-hop header must never reach an upstream this gateway
// itself authenticates against on the caller's behalf. injectAuth is
// always applied AFTER this copy (callAnthropicMessagesPassthrough,
// below), so it always wins if a future entry here ever collided with
// what injectAuth itself sets.
var anthropicPassthroughForwardedHeaders = []string{"anthropic-beta"}

// handleMessages implements POST /v1/messages: the Anthropic Messages API
// shape, inbound. From model resolution through response caching, usage
// accounting, and adapter-error handling, this route shares ONE
// implementation with routes_unified.go's runUnified — runMeteredCall,
// routes_unified.go (item 7 fix, 2026-08-22 review) — rather than
// duplicating that pipeline. Only two things are genuinely specific to
// this route: which wire shape a request/response is in (Anthropic, not
// OpenAI — see callAnthropicMessagesPassthrough/callTranslatedMessages
// below) and which error envelope shape a failure gets written in
// (Anthropic's {"type":"error","error":{...}}, not OpenAI's
// {"error":{...}} — see writeAnthropicError and its callers, below).
//
// ORDERING (item 2 fix, 2026-08-22 review): body admission + decode run
// BEFORE admitRequest (the rate-limit check) on THIS route — the
// opposite order from runUnified's own finding 1a. anthropic-sdk-python
// retries any status >= 500, and Claude Code streams on every call: the
// previous ordering (admitRequest first, streaming rejected only after
// decode) meant every real client hitting this not-yet-streaming-capable
// route burned three requests of rate-limit budget per logical call —
// verified repro: requestsPerMinute 2, three attempts -> 501, 501, 429,
// day counter reads 3. Deciding "is this streaming" requires the decoded
// body, so decode has to happen first; the trade-off finding 1a accepts
// for runUnified (an adversarial caller paying nothing to retry a
// malformed body) does not apply here, since the common case hitting
// this ordering is not adversarial retries but every legitimate
// streaming-capable client that exists today.
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	body, req, ok := g.readAndDecodeUnifiedBody(sw, r, writeAnthropicError)
	if !ok {
		return
	}

	requestedModel, _ := req["model"].(string)
	if requestedModel == "" {
		writeAnthropicError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	// Streaming is rejected cleanly and explicitly here, before the
	// rate-limit check, model resolution, or any upstream call: this cut
	// of the route ships non-streaming only (brief scope) — a follow-up
	// branch adds the Anthropic SSE event translator (message_start/
	// content_block_delta/message_stop), a genuinely different event
	// model from OpenAI's stream shape this route already knows how to
	// forward. A caller that asked for streaming must see a clear, typed
	// error, never a silently-ignored flag answered non-streamed.
	//
	// 400, not 501 (item 2 fix, 2026-08-22 review): anthropic-sdk-python's
	// retry classifies purely on status code and retries anything >= 500
	// — a 501 here turned every streaming call into an SDK-driven retry
	// storm against this route's own rate limit (this function's own doc
	// comment above has the verified repro). 400 maps to the SDK's
	// non-retryable BadRequestError, and invalid_request_error is already
	// the correct error type for a 400.
	if isStreamingRequested(req) {
		writeAnthropicError(sw, http.StatusBadRequest, "invalid_request_error", "streaming is not yet supported on /v1/messages")
		return
	}

	scopes, ok := g.admitRequest(sw, u, grp, writeAnthropicError)
	if !ok {
		return
	}

	g.runMeteredCall(sw, r, scopes, body, req, requestedModel, grp, cacheEndpointMessages, "messages route", writeAnthropicError, writeAnthropicProviderUpstreamError,
		func(a providerAdapter, ctx context.Context, w http.ResponseWriter, req map[string]any) (usage, error) {
			if a.typeName() == providerTypeAnthropic {
				return g.callAnthropicMessagesPassthrough(ctx, w, r.Header, a, req, requestedModel)
			}
			return g.callTranslatedMessages(ctx, w, a, req, requestedModel)
		})
}

// isStreamingRequested reports whether req's "stream" field is anything
// other than the JSON literal false, JSON null, or an absent key (item
// 9/F7 fix, 2026-08-22/23 review). A bare req["stream"].(bool) assertion
// silently treats a non-bool value ("stream":"true", "stream":1) as
// false and lets it through to an upstream that might itself honor a
// loosely-typed truthy value — exactly the "silently answered
// non-streamed" outcome this route must reject explicitly instead of
// allowing by accident. JSON null is treated the same as an absent key
// (F7 fix): an SDK that serializes an unset optional field as explicit
// null must not get a hard 400 on an ordinary non-streaming call — null
// decodes to a Go nil interface value, which the item-9 fix's "anything
// not the literal false" rule would otherwise also reject.
func isStreamingRequested(req map[string]any) bool {
	v, ok := req["stream"]
	if !ok || v == nil {
		return false
	}
	return v != false
}

// responseTranslationError wraps a failure translating an already-
// successful, already-billed upstream response into this route's own
// client-facing Anthropic shape (item 1/12 fix, 2026-08-22 review): the
// upstream call itself succeeded, so its usage travels back to the
// caller as call's own first return value regardless of this error (see
// callTranslatedMessages/callAnthropicMessagesPassthrough, below) — a
// client who can induce a translation failure (verified repro: a
// provider response with usage prompt=5000/completion=900,
// finish_reason "length", and a truncated tool-call arguments string)
// must not get unlimited free tokens against their budget just because
// this gateway's own re-encoding step broke after the provider already
// did, and was paid for, the work. Kept distinct from an errUpstream-
// wrapped error (a genuine connectivity/protocol failure) so
// handleAdapterErrorEnvelope's (routes_unified.go) generic "upstream
// connection error" branch never misclassifies a translation bug as a
// network problem in the log — see that function's own
// *responseTranslationError branch.
type responseTranslationError struct {
	err error
}

// Error implements the error interface.
func (e *responseTranslationError) Error() string { return "translate response: " + e.err.Error() }

// callAnthropicMessagesPassthrough handles a /v1/messages request already
// resolved to an anthropic-type provider: forward req to that provider's
// own Messages endpoint UNTRANSLATED (brief: "pass through, no
// translation of the message body") and copy its response back to w. req's
// only mutation, applied by the shared pipeline (runMeteredCall,
// routes_unified.go) before this function ever runs, is the "model"
// field rewrite to the resolved upstream model id — the same rewrite
// every route in this package applies before any adapter call; it is
// request ROUTING, not message translation.
//
// This deliberately bypasses providerAdapter.chatCompletion:
// chatCompletion's contract (providers.go) is OpenAI-shape in, OpenAI-
// shape out for every provider type — anthropicAdapter.chatCompletion
// always calls anthropicRequestFromOpenAI on whatever it is handed, which
// would corrupt an already-Anthropic-shaped body instead of passing it
// through.
//
// clientHeaders is the client's own incoming request headers, forwarded
// through anthropicPassthroughForwardedHeaders' allowlist only (item 5
// fix, above).
//
// The response body reaches the client BYTE-FOR-BYTE except "model",
// rewritten to requestedModel — the client's own requested alias (item 4
// fix, 2026-08-22 review: the PREVIOUS version left the upstream's own
// real model id in the response, inconsistent with callTranslatedMessages'
// own alias echo on the same route, and leaking the operator's real
// upstream model id, which gatewayAliasKey exists specifically to
// prevent). Byte-for-byte is load-bearing, not a nicety: see
// rewriteModelField's own doc comment (item 4/F1 fix, 2026-08-23 review)
// for why a decode-then-re-marshal round trip through a Go value
// silently corrupts data on this route.
func (g *Gateway) callAnthropicMessagesPassthrough(ctx context.Context, w http.ResponseWriter, clientHeaders http.Header, adapter providerAdapter, req map[string]any, requestedModel string) (usage, error) {
	delete(req, gatewayAliasKey) // this route never sets it via req itself, but a client-sent "__alias" field must never reach the real Anthropic API

	// Plain type assertion, not an added providerAdapter interface method:
	// this branch only ever runs when the caller already confirmed
	// adapter.typeName() == providerTypeAnthropic, and buildAdapters
	// (providers.go) only ever constructs a *anthropicAdapter for that
	// type, so the assertion is guaranteed to succeed. aa.retry is
	// nil-safe either way (retryPolicy.do's own contract, retry.go), so a
	// failed assertion still degrades safely to one-attempt behavior
	// rather than panicking.
	aa, _ := adapter.(*anthropicAdapter)
	var retry *retryPolicy
	if aa != nil {
		retry = aa.retry
	}

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	for _, name := range anthropicPassthroughForwardedHeaders {
		for _, v := range clientHeaders.Values(name) {
			hdr.Add(name, v)
		}
	}
	// injectAuth runs AFTER the allowlist copy above, so it always wins
	// (item 5 ruling) — in practice the two never collide (the allowlist
	// carries no auth-shaped header), but the ordering is deliberate and
	// documented, not incidental.
	adapter.injectAuth(&http.Request{Header: hdr})

	resp, err := upstreamJSON(ctx, adapter.httpClient(), http.MethodPost, adapter.base()+anthropicMessagesPath, hdr, req, retry)
	if err != nil {
		return usage{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usage{}, newProviderHTTPError(resp)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// F5 fix, 2026-08-23 review: the upstream already answered 200
		// (already billed server-side) — a body-read failure past that
		// point is classified as a translation failure, not a
		// connectivity problem, consistent with the usage-decode failure
		// below. Nothing is recoverable here: with no bytes read at all,
		// there is neither a usage number to extract nor a body to
		// deliver, so this is still an error return.
		return usage{}, &responseTranslationError{err: fmt.Errorf("read response body: %w", err)}
	}

	// F5 fix, 2026-08-23 review: a failure parsing USAGE out of raw must
	// never block delivering raw itself to the client — the upstream
	// already fully answered, and this route's whole purpose is passing
	// that answer through. billed stays usage{} on a parse failure;
	// runMeteredCall's own zero-usage estimate (from request body size,
	// routes_unified.go) takes over, exactly like every other
	// unparseable-usage-shape case already relies on. This is NOT a
	// repeat of the "silently drop tokens a client can exploit" bug item
	// 1 fixed elsewhere: there, callErr was returned non-nil and skipped
	// that estimate fallback entirely; here callErr stays nil, so the
	// fallback still runs and the request still gets billed something.
	var parsed anthropicResponseBody
	var billed usage
	if unmarshalErr := json.Unmarshal(raw, &parsed); unmarshalErr != nil {
		g.warnf("messages route: could not decode usage from an anthropic passthrough response (forwarding the response to the client regardless): %v", unmarshalErr)
	} else {
		// totalInputTokens folds Anthropic's prompt-cache counters
		// (cache_creation_input_tokens/cache_read_input_tokens) into the
		// billed prompt count (item 6 fix, 2026-08-22 review) — see its
		// own doc comment, translate_anthropic.go, for why dropping them
		// under-bills a cache-heavy caller (the common case for Claude
		// Code) by orders of magnitude.
		billed = usage{prompt: parsed.Usage.totalInputTokens(), completion: parsed.Usage.OutputTokens}
	}

	out, err := rewriteModelField(raw, requestedModel)
	if err != nil {
		return billed, &responseTranslationError{err: err}
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)

	return billed, nil
}

// rewriteModelField replaces raw's top-level "model" field with a JSON
// string of newModel, preserving every other field's VALUE bytes and
// their original relative order exactly (item 4/F1 fix, 2026-08-23
// review). A prior version decoded the whole response into
// map[string]any and re-marshaled it — not enough: Go's float64 cannot
// represent every JSON integer exactly (verified repro:
// 12345678901234567890 becomes 12345678901234567000, and
// 9007199254740993 becomes 9007199254740992 — both silent corruption of
// a value that could be, for instance, a long numeric id inside a
// tool_use block's arbitrary "input"), json.Marshal also rewrites 1.0 to
// 1 and HTML-escapes < and & by default, and re-marshaling a Go map
// always re-sorts its keys regardless of the source's own order — wrong
// on a route documented as passthrough.
//
// This function never turns any value but "model" into a Go value at
// all: it walks raw's top-level object with json.Decoder.Token()/Decode()
// and captures every other field's value as a json.RawMessage, which
// preserves the exact source bytes of that value — including a nested
// object or array's own internal structure — without parsing it further.
// The rebuilt object is written compactly (no inter-token whitespace):
// for the compact JSON every provider in this codebase actually returns
// over the wire, that reproduces the input byte-for-byte apart from the
// "model" field; a pretty-printed input's insignificant whitespace would
// not survive, which no known caller of this function ever produces.
func rewriteModelField(raw []byte, newModel string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("response is not a JSON object")
	}

	newModelJSON, err := json.Marshal(newModel)
	if err != nil {
		return nil, fmt.Errorf("marshal model field: %w", err)
	}

	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read field key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected non-string object key")
		}

		var val json.RawMessage
		if decodeErr := dec.Decode(&val); decodeErr != nil {
			return nil, fmt.Errorf("read field %q value: %w", key, decodeErr)
		}

		if !first {
			out.WriteByte(',')
		}
		first = false

		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, fmt.Errorf("marshal field key %q: %w", key, err)
		}
		out.Write(keyJSON)
		out.WriteByte(':')
		if key == "model" {
			out.Write(newModelJSON)
		} else {
			out.Write(val)
		}
	}
	out.WriteByte('}')

	return out.Bytes(), nil
}

// callTranslatedMessages handles a /v1/messages request resolved to a
// non-anthropic-type provider (openai, or gemini — both implement
// providerAdapter.chatCompletion against the SAME OpenAI-shaped
// contract, providers.go, regardless of what their own upstream wire
// format is): translate req from Anthropic's shape to OpenAI's
// (openAIRequestFromAnthropic, translate_anthropic.go), run it through
// the resolved adapter's own chatCompletion exactly as routes_unified.go's
// handleChat does, capturing what it writes into an in-memory recorder
// instead of letting it reach the client directly, then translate that
// captured OpenAI-shaped response back to Anthropic's shape
// (anthropicResponseFromOpenAI) before writing the result to w.
//
// USAGE ON A TRANSLATION FAILURE (item 1 fix, 2026-08-22 review):
// chatCompletion's own returned usage (result, below) is what the
// upstream actually reported and this gateway already billed the
// provider for — it is preserved and returned even when translating
// THAT successful response into Anthropic's shape fails afterward
// (verified repro: a response with usage prompt=5000/completion=900,
// finish_reason "length", and a tool-call whose arguments string was
// truncated mid-JSON by the provider's own max_tokens cutoff). The
// PREVIOUS version of this function returned usage{} on that path,
// discarding tokens the provider had already charged for — a client who
// could reliably induce a truncated tool-call response got unlimited
// free tokens against any budget, and this route was strictly less
// robust than /v1/chat/completions, which forwards that same malformed
// response to the client successfully instead of erroring on it.
func (g *Gateway) callTranslatedMessages(ctx context.Context, w http.ResponseWriter, adapter providerAdapter, req map[string]any, requestedModel string) (usage, error) {
	openaiReq, dropped, err := openAIRequestFromAnthropic(req)
	if err != nil {
		return usage{}, err
	}
	// thinking (Anthropic's extended-thinking config) and any other
	// field this translator recognizes but cannot map has no OpenAI
	// equivalent — logged rather than silently discarded (item 11 fix,
	// 2026-08-22 review), so an operator can see a client's request was
	// only partially honored instead of the drop being invisible.
	for _, field := range dropped {
		g.warnf("messages route: request field %q has no OpenAI equivalent for provider %q and was dropped in translation", field, adapter.name())
	}

	rec := newMessagesResponseRecorder()
	result, err := adapter.chatCompletion(ctx, rec, openaiReq)
	if err != nil {
		// result, not usage{} (F5 fix, 2026-08-23 review): harmless today
		// — streaming is rejected before this route ever reaches here, so
		// chatCompletion's non-streaming contract already guarantees
		// usage{} on any error. It matters the day streaming lands: a
		// streaming chatCompletion call (forwardStream) CAN return
		// nonzero usage alongside an error — a usage chunk that arrived
		// just before a dropped connection must still be billed — and
		// discarding it here would reintroduce item 1's original
		// CRITICAL (usage silently dropped on an error path) the moment
		// this route gains streaming support.
		return result, err
	}

	out, err := anthropicResponseFromOpenAI(rec.body.Bytes(), requestedModel)
	if err != nil {
		return result, &responseTranslationError{err: err}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return result, &responseTranslationError{err: fmt.Errorf("marshal translated response: %w", err)}
	}

	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)

	return result, nil
}

// messagesResponseRecorder is a minimal in-memory http.ResponseWriter
// callTranslatedMessages hands to a resolved adapter's own chatCompletion
// in place of the real client connection. chatCompletion's contract
// (providerAdapter, providers.go) is OpenAI-shape in, OpenAI-shape out
// regardless of provider type, and this route needs to translate that
// output to Anthropic's shape before anything reaches the real client —
// so the adapter's write is captured here and never forwarded; the real
// ResponseWriter only ever sees the translated bytes callTranslatedMessages
// writes afterward. Streaming can never reach this recorder: handleMessages
// rejects a streaming request before either call path runs, so
// chatCompletion's non-streaming forwardJSON is the only path that ever
// writes to it.
type messagesResponseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newMessagesResponseRecorder() *messagesResponseRecorder {
	return &messagesResponseRecorder{header: http.Header{}}
}

func (r *messagesResponseRecorder) Header() http.Header { return r.header }

func (r *messagesResponseRecorder) WriteHeader(status int) { r.status = status }

func (r *messagesResponseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK // mirrors net/http's implicit-200 default
	}
	return r.body.Write(b)
}

// writeAnthropicError writes an Anthropic Messages API-shaped error
// envelope: {"type":"error","error":{"type":...,"message":...}} — what an
// Anthropic SDK client parses. errType uses the same vocabulary this
// gateway's OpenAI-shaped envelope already uses everywhere else
// (writeOAIError, errors.go: invalid_request_error, authentication_error,
// rate_limit_error, server_error), so an operator reading logs sees one
// consistent taxonomy regardless of which route answered; only the JSON
// shape around it differs, to match what this route's own clients expect.
// Matches envelopeWriter's signature (routes_unified.go), so this route's
// error paths thread straight into the shared pipeline there.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
}

// writeAnthropicProviderUpstreamError is writeProviderUpstreamError's
// (routes_unified.go) Anthropic-shaped counterpart: perr's upstream body
// is embedded under error.upstream (decodeUpstreamErrorBody,
// routes_unified.go — shared with writeProviderUpstreamError, item 7/8
// fix) inside the Anthropic {"type":"error","error":{...}} envelope
// instead of the OpenAI one. Matches providerUpstreamErrorWriter's
// signature (routes_unified.go).
func writeAnthropicProviderUpstreamError(w http.ResponseWriter, providerName string, perr *providerHTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(perr.status)
	_ = json.NewEncoder(w).Encode(map[string]any{ // headers already committed; nothing useful to do on encode failure
		"type": "error",
		"error": map[string]any{
			"type":     "upstream_error",
			"message":  providerName + " upstream error",
			"code":     strconv.Itoa(perr.status),
			"upstream": decodeUpstreamErrorBody(perr),
		},
	})
}
