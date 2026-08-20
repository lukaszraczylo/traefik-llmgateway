package traefikllmgateway

import (
	"encoding/json"
	"fmt"
)

// geminiImageAspectRatioBySize maps OpenAI's images.generations "size"
// values to the Imagen aspectRatio parameter this translator understands,
// per spec §3's mapping table. A size string absent from this map is one
// Imagen has no known aspect ratio for — geminiImageAspectRatio rejects it
// as a *translateError rather than guessing.
var geminiImageAspectRatioBySize = map[string]string{
	"1024x1024": "1:1",
	"1792x1024": "16:9",
	"1024x1792": "9:16",
	"512x512":   "1:1",
}

// geminiImageAspectRatio resolves req's OpenAI "size" field to an Imagen
// aspectRatio parameter: an absent or empty size defaults to "1:1" (spec
// §3), and any other value must be one of geminiImageAspectRatioBySize's
// keys. An unrecognized size is a *translateError, not a silent fallback
// — the client asked for a specific size the gateway cannot honor, so
// letting Imagen apply its own default instead would silently ignore the
// request rather than fail loudly.
func geminiImageAspectRatio(size string) (string, error) {
	if size == "" {
		return "1:1", nil
	}
	ratio, ok := geminiImageAspectRatioBySize[size]
	if !ok {
		return "", &translateError{msg: fmt.Sprintf("size %q is not supported for gemini image generation", size)}
	}
	return ratio, nil
}

// geminiImagesRequestFromOpenAI maps an OpenAI images.generations request
// body ({"model","prompt","n","size","response_format",...}) to Imagen's
// :predict request body ({"instances":[{"prompt"}],"parameters":
// {"sampleCount","aspectRatio"}}), per spec §3. response_format:"url" is
// rejected outright — the gateway stores nothing, so it cannot honor a
// client asking for a hosted URL back; the response this translator
// always produces is b64_json (translate_gemini_images.go's
// openAIImagesResponseFromGemini). A truthy "quality" or "style" field is
// rejected the same way chat's translators reject an unsupported field
// (isTruthy, translate_anthropic.go): Imagen has no equivalent for
// either. n defaults to 1 when absent or non-positive, matching OpenAI's
// own default. The request's "model" field is deliberately never read
// here — the adapter reads it separately to build the :predict URL, the
// same convention chatCompletion/embeddings already follow.
func geminiImagesRequestFromOpenAI(req map[string]any) (map[string]any, error) {
	if rf, ok := req["response_format"].(string); ok && rf == "url" {
		return nil, &translateError{msg: `response_format "url" is not supported for gemini image generation (the gateway stores nothing to host a URL from)`}
	}
	if v, ok := req["quality"]; ok && isTruthy(v) {
		return nil, geminiUnsupportedFieldError("quality")
	}
	if v, ok := req["style"]; ok && isTruthy(v) {
		return nil, geminiUnsupportedFieldError("style")
	}

	prompt, _ := req["prompt"].(string)

	n := int64(1)
	if v, ok := req["n"]; ok {
		if parsed, ok2 := toInt64(v); ok2 && parsed > 0 {
			n = parsed
		}
	}

	size, _ := req["size"].(string)
	aspectRatio, err := geminiImageAspectRatio(size)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"instances": []any{map[string]any{"prompt": prompt}},
		"parameters": map[string]any{
			"sampleCount": n,
			"aspectRatio": aspectRatio,
		},
	}, nil
}

// geminiImagePrediction is one entry of Imagen's :predict response
// "predictions" array this translator reads.
type geminiImagePrediction struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
}

// geminiImagePredictResponse is the subset of Imagen's :predict response
// body this translator reads.
type geminiImagePredictResponse struct {
	Predictions []geminiImagePrediction `json:"predictions"`
}

// openAIImagesResponseFromGemini maps an Imagen :predict response body to
// an OpenAI images.generations response ({"created","data":[{"b64_json"}]}),
// per spec §3's response mapping. created is the caller-supplied response
// timestamp — Imagen's response carries none. An empty predictions array
// decodes to an empty (non-nil) data array, not an error: Imagen
// answering with zero images is a valid, if unusual, response to forward
// as-is.
func openAIImagesResponseFromGemini(body []byte, created int64) (map[string]any, error) {
	var resp geminiImagePredictResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode gemini images response: %w", errUpstream, err)
	}

	data := make([]any, len(resp.Predictions))
	for i, p := range resp.Predictions {
		data[i] = map[string]any{"b64_json": p.BytesBase64Encoded}
	}

	return map[string]any{
		"created": created,
		"data":    data,
	}, nil
}
