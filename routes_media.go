package traefikllmgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// Route paths for the three unified media endpoints (spec §3, v0.2):
// image generation and both audio endpoints are openai-type-only native
// forwards, except images.generations, which gemini translates to Imagen
// (translate_gemini_images.go); anthropic answers 501 on all three
// (provider_anthropic.go). See llmgateway.go's ServeHTTP dispatch.
const (
	imagesGenerationsPath   = "/v1/images/generations"
	audioSpeechPath         = "/v1/audio/speech"
	audioTranscriptionsPath = "/v1/audio/transcriptions"
)

// maxMultipartModelFieldBytes caps how much of the "model" form field
// extractMultipartModel reads: a model id is always a short identifier,
// so this is generous while bounding memory against a form field a
// misbehaving or malicious client names "model" but fills with megabytes
// of data.
const maxMultipartModelFieldBytes = 4096

// decodeMediaJSONRequest reads r.Body capped at maxRequestBytes (routes_
// unified.go's cap, shared here) and decodes it as a JSON object, then
// extracts its "model" field. It mirrors runUnified's own decode path
// exactly (spec §3's "reuse the unified decode path") — used by both the
// images and audio-speech routes, whose request bodies are both single
// JSON objects carrying a top-level "model" field, unlike
// audio-transcriptions' multipart body. A read failure or invalid/missing
// JSON is a 400, already written to sw; ok reports whether the caller may
// proceed.
func (g *Gateway) decodeMediaJSONRequest(sw *statusTrackingWriter, r *http.Request) (req map[string]any, model string, ok bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return nil, "", false
	}
	if err = json.Unmarshal(body, &req); err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return nil, "", false
	}
	model, _ = req["model"].(string)
	if model == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, "", false
	}
	return req, model, true
}

// resolveMediaRequest runs the shared model-routing/authz/limits steps
// every media route needs before calling its adapter (spec §3's "auth →
// model routing (registry) → group authz → limits" — group authz is
// folded into modelRegistry.resolve itself, the same as
// routes_unified.go's runUnified). On success it returns the resolved
// adapter and the upstream model id to route to; ok reports whether the
// caller may proceed — a resolve or limit failure has already written its
// response to sw.
func (g *Gateway) resolveMediaRequest(sw *statusTrackingWriter, u *user, grp *group, requestedModel string) (adapter providerAdapter, upstreamModel string, ok bool) {
	adapter, upstreamModel, _, err := g.registry.resolve(requestedModel, grp)
	if err != nil {
		writeModelResolveError(sw, err)
		return nil, "", false
	}
	if violation := g.limiter.checkAndCount(buildLimitScopes(u, grp)); violation != nil {
		writeLimitViolation(sw, violation)
		return nil, "", false
	}
	return adapter, upstreamModel, true
}

// handleImagesGenerations implements POST /v1/images/generations (spec
// §3): an openai-type provider forwards natively; gemini translates to
// Imagen (translate_gemini_images.go); anthropic always answers 501
// (provider_anthropic.go's imagesGeneration). Images are never cached
// (spec §2) and never cost-accounted (spec §3) — only the request
// counters resolveMediaRequest's checkAndCount call already incremented
// move.
func (g *Gateway) handleImagesGenerations(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	req, requestedModel, ok := g.decodeMediaJSONRequest(sw, r)
	if !ok {
		return
	}
	adapter, upstreamModel, ok := g.resolveMediaRequest(sw, u, grp, requestedModel)
	if !ok {
		return
	}
	req["model"] = upstreamModel

	if _, err := adapter.imagesGeneration(r.Context(), sw, req); err != nil {
		g.handleAdapterError(sw, err, adapter.name())
	}
}

// handleAudioSpeech implements POST /v1/audio/speech (spec §3): an
// openai-type provider forwards natively, streaming the binary audio
// response back via a flushWriter (provider_openai.go's audioSpeech);
// gemini and anthropic both always answer 501. req is re-marshaled to
// JSON after its "model" field is rewritten to the resolved upstream
// model id, mirroring how the unified chat/embeddings routes rewrite
// "model" before handing off to their adapter — audioSpeech's own
// interface method takes a raw []byte body rather than a map
// (providers.go), since its response is binary audio no translator ever
// needs to inspect as JSON again.
func (g *Gateway) handleAudioSpeech(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	req, requestedModel, ok := g.decodeMediaJSONRequest(sw, r)
	if !ok {
		return
	}
	adapter, upstreamModel, ok := g.resolveMediaRequest(sw, u, grp, requestedModel)
	if !ok {
		return
	}
	req["model"] = upstreamModel

	body, err := json.Marshal(req)
	if err != nil {
		// req decoded from the client's own JSON body plus one string-field
		// overwrite ("model") — a value json.Unmarshal has already accepted
		// once cannot fail to re-marshal; this is a defensive 500, not a
		// path any known input reaches.
		g.errorf("audio speech: re-marshal request: %v", err)
		writeOAIError(sw, http.StatusInternalServerError, "server_error", "internal error")
		return
	}

	if _, err := adapter.audioSpeech(r.Context(), sw, body, "application/json"); err != nil {
		g.handleAdapterError(sw, err, adapter.name())
	}
}

// handleAudioTranscriptions implements POST /v1/audio/transcriptions
// (spec §3): an openai-type provider forwards natively; gemini and
// anthropic both always answer 501. The multipart request body is
// replayed upstream unchanged only when the client's own "model" form
// field already equals the resolved upstream model id (the common case —
// a bare id); when it does not (a provider-prefixed or otherwise aliased
// id resolves to a different upstream model string), the body is
// rebuilt via rewriteMultipartModel so the alias never reaches the real
// provider, the same invariant every other unified route enforces
// (controller ruling, 2026-08-20, amends spec §3's original "REPLAY the
// full body upstream unchanged" — see rewriteMultipartModel's doc
// comment).
func (g *Gateway) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request, u *user, grp *group) {
	sw := &statusTrackingWriter{ResponseWriter: w}

	body, oversize, err := readCapped(r.Body, maxRequestBytes)
	if err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	if oversize {
		writeOAIError(sw, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}

	contentType := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid Content-Type")
		return
	}
	if !strings.HasPrefix(mediaType, "multipart/form-data") {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "Content-Type must be multipart/form-data")
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "missing multipart boundary")
		return
	}

	requestedModel, err := extractMultipartModel(body, boundary)
	switch {
	case errors.Is(err, errDuplicateModelField):
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "duplicate model field")
		return
	case err != nil || requestedModel == "":
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	adapter, upstreamModel, ok := g.resolveMediaRequest(sw, u, grp, requestedModel)
	if !ok {
		return
	}

	uploadBody := body
	uploadContentType := contentType
	if upstreamModel != requestedModel {
		rebuilt, rebuiltContentType, rerr := rewriteMultipartModel(body, boundary, upstreamModel)
		if rerr != nil {
			g.errorf("audio transcriptions: rewrite multipart model field: %v", rerr)
			writeOAIError(sw, http.StatusInternalServerError, "server_error", "internal error")
			return
		}
		uploadBody = rebuilt
		uploadContentType = rebuiltContentType
	}

	if _, err := adapter.audioTranscription(r.Context(), sw, uploadBody, uploadContentType); err != nil {
		g.handleAdapterError(sw, err, adapter.name())
	}
}

// readCapped reads up to limit+1 bytes from r, reporting oversize=true
// when more than limit bytes were available. Unlike
// decodeMediaJSONRequest's plain io.LimitReader-and-decode (which
// silently truncates an oversized JSON body into a parse failure),
// audio-transcriptions' multipart body cannot be truncated the same way —
// a cut multipart payload often still parses a valid prefix of it, which
// would forward truncated audio upstream instead of failing loudly — so
// this endpoint alone reports the cap explicitly (spec §3's "bodies above
// maxRequestBytes (10MB) → 413").
func readCapped(r io.Reader, limit int64) (body []byte, oversize bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

// errDuplicateModelField is extractMultipartModel's sentinel for a
// multipart body carrying more than one part named "model" — an
// ambiguous request a first-found parser and a last-found parser would
// resolve differently. handleAudioTranscriptions rejects it outright
// (400) rather than silently picking one.
var errDuplicateModelField = errors.New("multipart body has more than one model field")

// extractMultipartModel scans body — a full multipart/form-data payload
// already read into memory (bounded by handleAudioTranscriptions' own
// readCapped call before this is ever reached) — for its "model" form
// field, returning the field's value. It reads over a fresh
// bytes.NewReader(body), never consuming or mutating body itself, so the
// caller can replay or rewrite the exact same bytes afterward (spec §3).
// It always scans every part, never returning as soon as the first
// "model" part is found, so a second "model" part later in the body is
// caught as errDuplicateModelField instead of being silently ignored.
// Parts are read in whatever order the client sent them — a "model"
// field following a large audio-file part is found the same as one sent
// first. Returns "" with a nil error when no part named "model" is
// present; the caller treats an empty model the same as a missing one.
func extractMultipartModel(body []byte, boundary string) (string, error) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	found := false
	var model string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return model, nil
		}
		if err != nil {
			return "", err
		}
		if part.FormName() != "model" {
			_ = part.Close() //nolint:errcheck // read-side close; nothing actionable on failure
			continue
		}
		if found {
			_ = part.Close() //nolint:errcheck // read-side close; nothing actionable on failure
			return "", errDuplicateModelField
		}
		val, err := io.ReadAll(io.LimitReader(part, maxMultipartModelFieldBytes))
		_ = part.Close() //nolint:errcheck // read-side close; nothing actionable on failure
		if err != nil {
			return "", err
		}
		model = string(val)
		found = true
	}
}

// rewriteMultipartModel rebuilds body — a multipart/form-data payload
// already validated to parse and to carry exactly one "model" field
// (extractMultipartModel) — with a fresh mime/multipart.Writer, copying
// every part verbatim (its original headers and content, in the
// client's original order) except the "model" part's value, which
// becomes upstreamModel.
//
// Controller ruling (2026-08-20, amends spec §3's original "REPLAY the
// full body upstream unchanged"): a client addressing a
// provider-prefixed or otherwise aliased model id must never have that
// alias reach the real upstream provider — the same invariant every
// other unified route already enforces by rewriting "model" before
// forwarding. handleAudioTranscriptions calls this only when the
// client's own model string differs from the resolved upstream model
// id; when they already match (the bare-id case), it replays the
// original bytes untouched instead (the fast path, no rebuild cost).
//
// The rebuilt body uses the Writer's own new boundary, not body's
// original one, and contentType is that Writer's own
// FormDataContentType() — byte-identical boundary preservation is not
// required, only a valid, equivalent multipart body.
func rewriteMultipartModel(body []byte, boundary, upstreamModel string) ([]byte, string, error) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}

		pw, err := mw.CreatePart(part.Header)
		if err != nil {
			_ = part.Close() //nolint:errcheck // read-side close; nothing actionable on failure
			return nil, "", err
		}

		if part.FormName() == "model" {
			_, err = pw.Write([]byte(upstreamModel))
		} else {
			_, err = io.Copy(pw, part)
		}
		_ = part.Close() //nolint:errcheck // read-side close; nothing actionable on failure
		if err != nil {
			return nil, "", err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
