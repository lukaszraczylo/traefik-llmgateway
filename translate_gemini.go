package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// geminiAPIPrefix is the path segment every Gemini generateContent-family
// and embedding-family URL is built under, followed by "{model}:{method}".
const geminiAPIPrefix = "/v1beta/models/"

// geminiUnsupportedFieldError returns a *translateError for an OpenAI
// request field Gemini's API has no equivalent for. Deliberately not
// translate_anthropic.go's unsupportedFieldError: that helper's message
// hardcodes "not supported for anthropic models", which would misreport the
// provider on a Gemini request, and this file may only reuse translateError
// itself, not touch translate_anthropic.go to parameterize it.
func geminiUnsupportedFieldError(field string) *translateError {
	return &translateError{msg: fmt.Sprintf("%s is not supported for gemini models", field)}
}

// geminiSystemTextFromContentParts joins an OpenAI system message's
// array-form content ([{"type":"text","text":...}, ...]) into plain text,
// joined with "\n", per lesson (e). Any part that is not {"type":"text"}
// has no equivalent in Gemini's plain-string systemInstruction text, so it
// is a *translateError rather than being silently dropped. Deliberately
// not translate_anthropic.go's systemTextFromContentParts, for the same
// hardcoded-provider-name reason as geminiUnsupportedFieldError above.
func geminiSystemTextFromContentParts(parts []any) (string, error) {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			return "", &translateError{msg: "system message content[] entries must be objects"}
		}
		ptype, _ := pm["type"].(string)
		if ptype != "text" {
			return "", &translateError{msg: fmt.Sprintf("system message content part type %q is not supported for gemini models", ptype)}
		}
		text, _ := pm["text"].(string)
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n"), nil
}

// dataURIHasBase64Param reports whether rawParams — the ";"-joined
// parameter segment of a data: URI header, after the media type and its
// leading ";" have been cut away — contains a bare "base64" parameter.
// Splitting on ";" rather than checking a suffix or prefix means a
// "base64" token anywhere in the parameter list counts, regardless of
// what other parameters (";charset=utf-8", say) surround it.
func dataURIHasBase64Param(rawParams string) bool {
	if rawParams == "" {
		return false
	}
	for _, p := range strings.Split(rawParams, ";") {
		if p == "base64" {
			return true
		}
	}
	return false
}

// geminiInlineDataFromDataURI parses an OpenAI image_url.url as a
// "data:<media-type-and-params>,<data>" URI and returns Gemini's inlineData
// object. Per lesson (d), the media type is everything before the first
// ";" after "data:" — any parameters after it (";charset=utf-8;base64", for
// instance) are stripped rather than rejected or included. Gemini's
// inlineData.data is always base64, so a data: URI whose parameter list
// does not include a "base64" token is rejected outright — review fix: a
// non-base64 URI like "data:text/plain,Hello" was previously forwarded
// with its raw text bytes labeled as base64 data, corrupting the upstream
// request instead of failing loudly. Any URL that is not a data: URI (an
// http(s) image URL, which Gemini's generateContent API does not fetch)
// also returns a *translateError, per the request mapping table's image
// row.
func geminiInlineDataFromDataURI(uri string) (map[string]any, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return nil, &translateError{msg: "image_url must be a data: URI (http(s) image URLs are not supported for gemini models)"}
	}
	header, data, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, &translateError{msg: "malformed data: URI in image_url"}
	}
	mimeType, rawParams, _ := strings.Cut(header, ";")
	if !dataURIHasBase64Param(rawParams) {
		return nil, &translateError{msg: "data: URI in image_url must be base64-encoded"}
	}
	return map[string]any{"mimeType": mimeType, "data": data}, nil
}

// geminiPartFromOpenAI maps one OpenAI content-array entry ({"type":"text",
// ...} or {"type":"image_url",...}) to a Gemini content part. An entry of
// any other type is dropped (nil, nil) rather than erroring — an
// unrecognized part type is forwarded content this translator does not
// understand, not necessarily an invalid request.
func geminiPartFromOpenAI(pm map[string]any) (map[string]any, error) {
	switch ptype, _ := pm["type"].(string); ptype {
	case "text":
		text, _ := pm["text"].(string)
		return map[string]any{"text": text}, nil
	case "image_url":
		iu, _ := pm["image_url"].(map[string]any)
		url, _ := iu["url"].(string)
		inline, err := geminiInlineDataFromDataURI(url)
		if err != nil {
			return nil, err
		}
		return map[string]any{"inlineData": inline}, nil
	default:
		return nil, nil
	}
}

// geminiFunctionCallPartFromOpenAI maps one assistant tool_calls[] entry
// ({"id","type":"function","function":{"name","arguments"}}) to a Gemini
// {"functionCall":{"name","args"}} content part, parsing the OpenAI
// arguments JSON string into Gemini's structured "args" object. It also
// returns the entry's id and name, so the caller can record them in the
// pending tool_call id map a later role:"tool" message's functionResponse
// needs (Gemini's functionResponse has no id field to correlate by; this
// translator resolves "name" instead, per the request mapping table's
// role:tool row).
func geminiFunctionCallPartFromOpenAI(tc any) (part map[string]any, id, name string, err error) {
	tm, ok := tc.(map[string]any)
	if !ok {
		return nil, "", "", &translateError{msg: "assistant tool_calls[] entry is malformed"}
	}
	id, _ = tm["id"].(string)
	fn, _ := tm["function"].(map[string]any)
	name, _ = fn["name"].(string)
	argsStr, _ := fn["arguments"].(string)

	args := any(map[string]any{})
	if argsStr != "" {
		if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
			return nil, "", "", &translateError{msg: "assistant tool_calls[].function.arguments is not valid JSON"}
		}
	}
	return map[string]any{"functionCall": map[string]any{"name": name, "args": args}}, id, name, nil
}

// geminiMessageFromOpenAI maps one OpenAI user/assistant message to a
// Gemini {"role","parts"} contents entry. geminiRole is "user" or "model"
// (assistant's Gemini-side role name, per the request mapping table).
// toolNameByID accumulates id -> name for every tool_calls[] entry seen on
// an assistant message, so a later role:"tool" message can resolve its
// functionResponse's "name".
func geminiMessageFromOpenAI(msg map[string]any, geminiRole string, toolNameByID map[string]string) (map[string]any, error) {
	var parts []any
	switch c := msg["content"].(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"text": c})
		}
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			part, err := geminiPartFromOpenAI(pm)
			if err != nil {
				return nil, err
			}
			if part != nil {
				parts = append(parts, part)
			}
		}
	}

	if geminiRole == "model" {
		toolCalls, _ := msg["tool_calls"].([]any)
		for _, tc := range toolCalls {
			part, id, name, err := geminiFunctionCallPartFromOpenAI(tc)
			if err != nil {
				return nil, err
			}
			if id != "" {
				toolNameByID[id] = name
			}
			parts = append(parts, part)
		}
	}

	if parts == nil {
		parts = []any{}
	}
	return map[string]any{"role": geminiRole, "parts": parts}, nil
}

// geminiFunctionResponsePart maps one OpenAI role:"tool" message to a
// single Gemini {"functionResponse":{"name","response":{"content"}}}
// content part, per the request mapping table's role:tool row. name is
// looked up from toolNameByID — the id -> name map built while converting
// the assistant message that produced this tool_call_id — and an unknown
// id is a *translateError rather than a silently empty name.
func geminiFunctionResponsePart(msg map[string]any, toolNameByID map[string]string) (map[string]any, error) {
	toolCallID, _ := msg["tool_call_id"].(string)
	name, ok := toolNameByID[toolCallID]
	if !ok {
		return nil, &translateError{msg: fmt.Sprintf("role:tool message references unknown tool_call_id %q", toolCallID)}
	}
	return map[string]any{
		"functionResponse": map[string]any{
			"name":     name,
			"response": map[string]any{"content": msg["content"]},
		},
	}, nil
}

// geminiToolsFromOpenAI maps OpenAI's tools[].function{name, description,
// parameters} to Gemini's tools:[{functionDeclarations:[{name,description,
// parameters}]}], per the request mapping table's tools row.
func geminiToolsFromOpenAI(toolsRaw []any) []any {
	decls := make([]any, 0, len(toolsRaw))
	for _, t := range toolsRaw {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		entry := map[string]any{"name": fn["name"]}
		if desc, ok := fn["description"]; ok {
			entry["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			entry["parameters"] = params
		}
		decls = append(decls, entry)
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// geminiToolConfigFromOpenAI maps OpenAI's "tool_choice" to Gemini's
// toolConfig.functionCallingConfig object, per the request mapping table's
// tool_choice rows. Unlike Anthropic, Gemini has a real NONE mode, so
// "none" needs no special "drop tools" handling. An unrecognized shape
// returns nil — the caller omits "toolConfig" entirely rather than sending
// a malformed one.
func geminiToolConfigFromOpenAI(tc any) map[string]any {
	switch t := tc.(type) {
	case string:
		switch t {
		case "none":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		case "auto":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
		case "required":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]any{"functionCallingConfig": map[string]any{
					"mode":                 "ANY",
					"allowedFunctionNames": []any{name},
				}}
			}
		}
	}
	return nil
}

// geminiRequestFromOpenAI maps an OpenAI chat-completion request body to a
// Gemini generateContent/streamGenerateContent request body, per this
// file's request mapping table. It returns a *translateError — never
// wrapped — for a field Gemini has no equivalent for (n>1, a truthy
// logit_bias or logprobs — controller ruling: unified with anthropic's
// same truthy-only semantics, see isTruthy's doc comment) or a content
// part this translator cannot map (a non-data-URI
// image URL, a malformed tool_calls argument string, a role:"tool" message
// whose tool_call_id was never seen on a preceding assistant message).
// Consecutive role:"tool" messages are merged into one Gemini {"role":
// "user"} contents entry carrying all of their functionResponse parts, per
// lesson (f). The request's "model" field is deliberately never read here —
// Gemini's body carries no model field at all; the adapter reads it
// separately to build the URL.
func geminiRequestFromOpenAI(req map[string]any) (map[string]any, error) {
	if v, ok := req["n"]; ok {
		if n, ok2 := toFloat64(v); ok2 && n > 1 {
			return nil, geminiUnsupportedFieldError("n")
		}
	}
	if v, ok := req["logit_bias"]; ok && isTruthy(v) {
		return nil, geminiUnsupportedFieldError("logit_bias")
	}
	if v, ok := req["logprobs"]; ok && isTruthy(v) {
		return nil, geminiUnsupportedFieldError("logprobs")
	}

	out := map[string]any{}

	msgsRaw, _ := req["messages"].([]any)
	var systemParts []string
	contents := make([]any, 0, len(msgsRaw))
	toolNameByID := map[string]string{}

	// pendingFuncResponses accumulates consecutive role:"tool" messages'
	// functionResponse parts. Gemini, like Anthropic, expects the N
	// tool-result messages following a parallel-tool-call assistant turn
	// to collapse into one contents entry, not N separate ones — see
	// geminiFunctionResponsePart's doc comment for the id resolution this
	// depends on.
	var pendingFuncResponses []any
	flushPending := func() {
		if len(pendingFuncResponses) == 0 {
			return
		}
		contents = append(contents, map[string]any{"role": "user", "parts": pendingFuncResponses})
		pendingFuncResponses = nil
	}

	for _, m := range msgsRaw {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := msg["role"].(string); role {
		case "system":
			switch c := msg["content"].(type) {
			case string:
				systemParts = append(systemParts, c)
			case []any:
				text, err := geminiSystemTextFromContentParts(c)
				if err != nil {
					return nil, err
				}
				systemParts = append(systemParts, text)
			}
		case "user":
			flushPending()
			gm, err := geminiMessageFromOpenAI(msg, "user", toolNameByID)
			if err != nil {
				return nil, err
			}
			contents = append(contents, gm)
		case "assistant":
			flushPending()
			gm, err := geminiMessageFromOpenAI(msg, "model", toolNameByID)
			if err != nil {
				return nil, err
			}
			contents = append(contents, gm)
		case "tool":
			part, err := geminiFunctionResponsePart(msg, toolNameByID)
			if err != nil {
				return nil, err
			}
			pendingFuncResponses = append(pendingFuncResponses, part)
		}
	}
	flushPending()

	if len(systemParts) > 0 {
		out["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": strings.Join(systemParts, "\n\n")}}}
	}
	out["contents"] = contents

	genConfig := map[string]any{}
	var maxTokens int64
	var hasMaxTokens bool
	if v, ok := req["max_completion_tokens"]; ok {
		if n, ok2 := toInt64(v); ok2 {
			maxTokens, hasMaxTokens = n, true
		}
	} else if v, ok := req["max_tokens"]; ok {
		if n, ok2 := toInt64(v); ok2 {
			maxTokens, hasMaxTokens = n, true
		}
	}
	if hasMaxTokens {
		genConfig["maxOutputTokens"] = maxTokens
	}
	if v, ok := req["temperature"]; ok {
		genConfig["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		genConfig["topP"] = v
	}
	if v, ok := req["stop"]; ok {
		seqs, err := stopSequencesFrom(v)
		if err != nil {
			return nil, err
		}
		if len(seqs) > 0 {
			genConfig["stopSequences"] = seqs
		}
	}
	if len(genConfig) > 0 {
		out["generationConfig"] = genConfig
	}

	if toolsRaw, ok := req["tools"].([]any); ok && len(toolsRaw) > 0 {
		out["tools"] = geminiToolsFromOpenAI(toolsRaw)
	}
	if tc, ok := req["tool_choice"]; ok {
		if toolConfig := geminiToolConfigFromOpenAI(tc); toolConfig != nil {
			out["toolConfig"] = toolConfig
		}
	}

	return out, nil
}

// geminiFunctionCall is the subset of a Gemini "functionCall" content
// part's JSON this translator reads. ID is optional on Gemini's side (the
// "Unique identifier of the function call" field, populated only when the
// model chooses to set it); openAIResponseFromGemini and geminiStreamState
// both fall back to a synthesized id when it is empty, since OpenAI's
// tool_calls[].id is required.
type geminiFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// geminiPart is the subset of one Gemini response content part's JSON this
// translator reads: a text part carries Text, a function-call part carries
// FunctionCall.
type geminiPart struct {
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
	Text         string              `json:"text,omitempty"`
}

// geminiContent is a Gemini response candidate's "content" object.
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiCandidate is one entry of a Gemini response's "candidates" array.
type geminiCandidate struct {
	FinishReason string        `json:"finishReason"`
	Content      geminiContent `json:"content"`
}

// geminiUsageMetadata is the "usageMetadata" object on a Gemini
// GenerateContentResponse, non-streaming or streaming alike.
type geminiUsageMetadata struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
}

// geminiGenerateContentResponse is the subset of a Gemini
// GenerateContentResponse this translator reads — the shape of both a
// non-streaming response body and every streaming chunk (each chunk is a
// full GenerateContentResponse increment, per the stream mapping table).
// UsageMetadata is a pointer so the translator can tell "this chunk carries
// no usage" (nil) apart from "this chunk reports zero tokens" (non-nil,
// zero fields) — the stream mapping table's "last usageMetadata wins" rule
// depends on that distinction.
type geminiGenerateContentResponse struct {
	ResponseID     string               `json:"responseId"`
	UsageMetadata  *geminiUsageMetadata `json:"usageMetadata"`
	PromptFeedback geminiPromptFeedback `json:"promptFeedback"`
	Candidates     []geminiCandidate    `json:"candidates"`
}

// geminiPromptFeedback is the "promptFeedback" object Gemini sets when the
// prompt itself — not any generated candidate — was blocked. BlockReason
// non-empty and Candidates empty together mean the model never produced
// any output at all: review fix — this case was previously reported to the
// client as an ordinary empty "stop" completion instead of a content-filter
// rejection.
type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

// geminiFinishReason maps a Gemini finishReason to an OpenAI finish_reason,
// per the response mapping table: MAX_TOKENS -> "length"; SAFETY,
// RECITATION, BLOCKLIST, PROHIBITED_CONTENT, and SPII -> "content_filter";
// STOP and any unrecognized value -> "stop", unless hasFunctionCall is set,
// in which case that default branch reports "tool_calls" instead — Gemini
// has no distinct finishReason value for a function-calling turn the way
// Anthropic's "tool_use" stop_reason does, so the caller's own knowledge of
// whether the candidate carried a functionCall part is what this
// translator uses instead.
func geminiFinishReason(reason string, hasFunctionCall bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		if hasFunctionCall {
			return "tool_calls"
		}
		return "stop"
	}
}

// geminiToolCallID returns a synthesized OpenAI tool_calls[].id for a
// Gemini functionCall part that did not carry its own optional "id" field:
// "call_" followed by ordinal, the part's 0-based position among tool
// calls seen so far in this response or stream (per lesson (a), a
// contiguous per-response/per-stream counter, never a raw content-part
// index).
func geminiToolCallID(ordinal int) string {
	return "call_" + strconv.Itoa(ordinal)
}

// geminiResponseIDOrFallback returns responseID when non-empty, or else a
// stable id derived from created — review fix (folded minor): Gemini does
// not always set "responseId", and chatCompletionIDPrefix alone (yielding
// a bare "chatcmpl-" with nothing after it) is not a usable per-response
// id. created is always available (the adapter fixes it once, before any
// upstream bytes arrive), so it is deterministic and collision-free across
// requests issued in different seconds.
func geminiResponseIDOrFallback(responseID string, created int64) string {
	if responseID != "" {
		return responseID
	}
	return fmt.Sprintf("gemini-%d", created)
}

// openAIResponseFromGemini maps a non-streaming Gemini
// GenerateContentResponse body to an OpenAI chat.completion response, per
// this file's response mapping table. Only candidates[0] is read, matching
// the table. model and created are supplied by the caller — the gateway-
// facing model id and a response timestamp, since Gemini's own response
// carries neither.
//
// A prompt-level safety block — Candidates empty and promptFeedback.
// blockReason set — is reported as finish_reason "content_filter" with a
// null message content, rather than falling through to the "no candidates,
// no finishReason" default of an ordinary "stop" completion (review fix:
// that default previously misreported a blocked prompt as a normal empty
// success).
func openAIResponseFromGemini(body []byte, model string, created int64) (map[string]any, usage, error) {
	var resp geminiGenerateContentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, usage{}, fmt.Errorf("%w: decode gemini response: %w", errUpstream, err)
	}

	var textParts []string
	var toolCalls []any
	var finishReasonRaw string
	if len(resp.Candidates) > 0 {
		c := resp.Candidates[0]
		finishReasonRaw = c.FinishReason
		ordinal := 0
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				args, err := reMarshalToolInput(p.FunctionCall.Args)
				if err != nil {
					return nil, usage{}, err
				}
				id := p.FunctionCall.ID
				if id == "" {
					id = geminiToolCallID(ordinal)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      p.FunctionCall.Name,
						"arguments": args,
					},
				})
				ordinal++
			case p.Text != "":
				textParts = append(textParts, p.Text)
			}
		}
	}

	message := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		message["content"] = strings.Join(textParts, "")
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finishReason := geminiFinishReason(finishReasonRaw, len(toolCalls) > 0)
	promptBlocked := len(resp.Candidates) == 0 && resp.PromptFeedback.BlockReason != ""
	if promptBlocked {
		finishReason = "content_filter"
		message["content"] = nil
	}

	u := usage{prompt: resp.UsageMetadata.safePrompt(), completion: resp.UsageMetadata.safeCompletion()}

	out := map[string]any{
		"id":      chatCompletionIDPrefix + geminiResponseIDOrFallback(resp.ResponseID, created),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     u.prompt,
			"completion_tokens": u.completion,
			"total_tokens":      u.total(),
		},
	}
	return out, u, nil
}

// safePrompt returns m.PromptTokenCount, or 0 when m is nil (no
// usageMetadata was present on the response at all).
func (m *geminiUsageMetadata) safePrompt() int64 {
	if m == nil {
		return 0
	}
	return m.PromptTokenCount
}

// safeCompletion returns m.CandidatesTokenCount, or 0 when m is nil.
func (m *geminiUsageMetadata) safeCompletion() int64 {
	if m == nil {
		return 0
	}
	return m.CandidatesTokenCount
}

// geminiStreamState is one streaming chat completion's translation state:
// the OpenAI-shaped chunk envelope fields (id — set at construction to a
// stable fallback derived from created, per geminiResponseIDOrFallback,
// and locked in place after the first translated chunk; model and created,
// fixed at construction), the running usage total, and the per-stream
// tool-call ordinal counter lesson (a) requires. A caller constructs one
// per stream and calls translate once per upstream sseEvent, in order.
type geminiStreamState struct {
	id      string
	model   string
	u       usage
	created int64
	// nextToolOrdinal is the per-stream contiguous tool-call counter lesson
	// (a) requires.
	nextToolOrdinal int
	startedRole     bool
	// sawFunctionCall latches true the first time any chunk in this stream
	// carries a functionCall part, and stays true for the rest of the
	// stream. Gemini's finishReason is "STOP" whether the turn ended in
	// text or a function call — unlike Anthropic's distinct "tool_use"
	// stop_reason — and the finishReason usually arrives on a later,
	// parts-less chunk of its own. A per-call local flag would forget a
	// functionCall part seen on an earlier chunk by the time the finish
	// chunk arrives, so this must be state on the struct, not a translate
	// local.
	sawFunctionCall bool
}

// newGeminiStreamState returns a geminiStreamState for one streaming chat
// completion. model is the gateway-facing model id and created the
// response timestamp every emitted chunk carries. id starts at the
// created-derived fallback (review fix, folded minor): if the first chunk
// never carries a "responseId" of its own, every chunk in the stream still
// shares one stable, non-empty id instead of drifting between an empty
// string and whatever a later chunk happens to report.
func newGeminiStreamState(model string, created int64) *geminiStreamState {
	return &geminiStreamState{model: model, created: created, id: geminiResponseIDOrFallback("", created)}
}

// usage returns the prompt/completion token counts captured so far, from
// whichever chunk's usageMetadata was translated most recently.
func (st *geminiStreamState) usage() usage {
	return st.u
}

// chunk builds one OpenAI-shaped "chat.completion.chunk" payload carrying
// delta, and finishReason as its choices[0].finish_reason — nil for the
// JSON null every non-terminal chunk carries.
func (st *geminiStreamState) chunk(delta map[string]any, finishReason *string) []byte {
	var fr any
	if finishReason != nil {
		fr = *finishReason
	}
	b, err := json.Marshal(map[string]any{
		"id":      chatCompletionIDPrefix + st.id,
		"object":  "chat.completion.chunk",
		"created": st.created,
		"model":   st.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": fr,
		}},
	})
	if err != nil {
		// delta is always built from this file's own map[string]any/string/
		// int64 literals — never a value that can fail to marshal.
		panic(fmt.Sprintf("llmgateway: gemini stream chunk failed to marshal: %v", err))
	}
	return b
}

// translate maps one upstream Gemini SSE event to zero or more
// OpenAI-shaped "chat.completion.chunk" payloads, per this file's stream
// mapping table. Every event's data is a full GenerateContentResponse
// increment — there is no "event:" type field to switch on the way
// Anthropic's stream has, so translate always attempts to decode ev.data
// as one. A malformed data payload returns an error wrapping errUpstream
// rather than being silently skipped, per lesson (c):
//
//   - The first chunk translate ever emits (across the whole stream) is
//     always a {role:"assistant"} delta.
//   - A text part becomes a {content: text} delta.
//   - A functionCall part becomes a delta.tool_calls[0] chunk carrying the
//     0-based per-stream tool ordinal as its index (lesson (a)), the
//     part's own "id" when Gemini set one or else a synthesized one, and
//     the re-marshaled "args" as function.arguments.
//   - A non-empty finishReason becomes a finish_reason chunk, mapped per
//     geminiFinishReason — using st.sawFunctionCall, latched true the first
//     time any chunk in this stream carried a functionCall part, since the
//     finish chunk is typically its own later, parts-less chunk with no
//     functionCall of its own to inspect.
//   - Any usageMetadata present on the chunk overwrites the running usage
//     total — "last usageMetadata wins", per the stream mapping table.
func (st *geminiStreamState) translate(ev sseEvent) ([][]byte, error) {
	var resp geminiGenerateContentResponse
	if err := json.Unmarshal(ev.data, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode gemini stream chunk: %w", errUpstream, err)
	}

	// isFirstChunk gates both the role-delta chunk below and the id
	// adoption right after it: the id resolves exactly once, on the first
	// translate call, per the "first one wins" ruling. A responseId
	// arriving on any later chunk is ignored — st.id already holds either
	// that first chunk's real id or the construction-time fallback, and
	// stays there for the rest of the stream.
	isFirstChunk := !st.startedRole
	if isFirstChunk && resp.ResponseID != "" {
		st.id = resp.ResponseID
	}

	if resp.UsageMetadata != nil {
		st.u = usage{prompt: resp.UsageMetadata.PromptTokenCount, completion: resp.UsageMetadata.CandidatesTokenCount}
	}

	var chunks [][]byte
	if isFirstChunk {
		st.startedRole = true
		chunks = append(chunks, st.chunk(map[string]any{"role": "assistant"}, nil))
	}

	if len(resp.Candidates) == 0 {
		// A prompt-level safety block reports no candidates at all, only
		// promptFeedback.blockReason — review fix: this chunk previously
		// produced nothing beyond the role delta, so the stream ended with
		// only "[DONE]" and no finish_reason for the client to act on.
		if resp.PromptFeedback.BlockReason != "" {
			reason := "content_filter"
			chunks = append(chunks, st.chunk(map[string]any{}, &reason))
		}
		return chunks, nil
	}
	c := resp.Candidates[0]

	for _, p := range c.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			st.sawFunctionCall = true
			args, err := reMarshalToolInput(p.FunctionCall.Args)
			if err != nil {
				return chunks, err
			}
			id := p.FunctionCall.ID
			if id == "" {
				id = geminiToolCallID(st.nextToolOrdinal)
			}
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": st.nextToolOrdinal,
				"id":    id,
				"type":  "function",
				"function": map[string]any{
					"name":      p.FunctionCall.Name,
					"arguments": args,
				},
			}}}
			st.nextToolOrdinal++
			chunks = append(chunks, st.chunk(delta, nil))
		case p.Text != "":
			chunks = append(chunks, st.chunk(map[string]any{"content": p.Text}, nil))
		}
	}

	if c.FinishReason != "" {
		reason := geminiFinishReason(c.FinishReason, st.sawFunctionCall)
		chunks = append(chunks, st.chunk(map[string]any{}, &reason))
	}

	return chunks, nil
}

// geminiEmbeddingValues is the "embedding" or one "embeddings[]" entry's
// JSON shape on a Gemini embedContent/batchEmbedContents response.
type geminiEmbeddingValues struct {
	Values []float64 `json:"values"`
}

// geminiEmbedContentResponse is the response body shape for Gemini's
// :embedContent endpoint (the OpenAI single-string-input case).
type geminiEmbedContentResponse struct {
	Embedding geminiEmbeddingValues `json:"embedding"`
}

// geminiBatchEmbedContentsResponse is the response body shape for Gemini's
// :batchEmbedContents endpoint (the OpenAI array-input case).
type geminiBatchEmbedContentsResponse struct {
	Embeddings []geminiEmbeddingValues `json:"embeddings"`
}

// geminiEmbedContentRequest builds the :embedContent request body for a
// single OpenAI embeddings input string, per the embeddings mapping table.
func geminiEmbedContentRequest(text string) map[string]any {
	return map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": text}}},
	}
}

// geminiBatchEmbedContentsRequest builds the :batchEmbedContents request
// body for an array of OpenAI embeddings input strings, per the embeddings
// mapping table. model is the bare (no "models/" prefix) Gemini model id;
// each request entry's own "model" field needs the prefix restored, since
// Gemini's per-request model reference is always the full resource name.
func geminiBatchEmbedContentsRequest(model string, texts []string) map[string]any {
	requests := make([]any, 0, len(texts))
	for _, text := range texts {
		requests = append(requests, map[string]any{
			"model":   "models/" + model,
			"content": map[string]any{"parts": []any{map[string]any{"text": text}}},
		})
	}
	return map[string]any{"requests": requests}
}

// geminiEmbeddingInputTexts extracts the OpenAI "input" field (a string or
// an array of strings) as a slice of one-or-more strings, and reports
// whether the original input was a single string — selecting the
// :embedContent endpoint — rather than an array, which selects
// :batchEmbedContents.
func geminiEmbeddingInputTexts(input any) (texts []string, single bool, err error) {
	switch v := input.(type) {
	case string:
		return []string{v}, true, nil
	case []any:
		texts = make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false, &translateError{msg: "input[] entries must be strings"}
			}
			texts = append(texts, s)
		}
		return texts, false, nil
	default:
		return nil, false, &translateError{msg: "input must be a string or an array of strings"}
	}
}

// estimatedEmbeddingTokens estimates a token count for texts the way
// Gemini's embedding endpoints do not: neither :embedContent nor
// :batchEmbedContents reports any usage at all, so this translator
// estimates ceil(total character count / 4) summed across every input
// string, per the embeddings mapping table.
func estimatedEmbeddingTokens(texts []string) int64 {
	var chars int64
	for _, t := range texts {
		chars += int64(len(t))
	}
	return (chars + 3) / 4
}

// openAIEmbeddingResponseFromGemini maps a Gemini embedContent or
// batchEmbedContents response body to an OpenAI embeddings list response,
// per the embeddings mapping table. single selects which of Gemini's two
// response shapes body decodes as. model is the gateway-facing model id and
// estimatedTokens the caller's already-computed estimatedEmbeddingTokens
// result — both usage.prompt_tokens and usage.total_tokens report it, and
// the returned usage is marked estimated, since Gemini reported none.
func openAIEmbeddingResponseFromGemini(body []byte, single bool, model string, estimatedTokens int64) (map[string]any, usage, error) {
	var vectors [][]float64
	if single {
		var resp geminiEmbedContentResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, usage{}, fmt.Errorf("%w: decode gemini embedContent response: %w", errUpstream, err)
		}
		vectors = [][]float64{resp.Embedding.Values}
	} else {
		var resp geminiBatchEmbedContentsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, usage{}, fmt.Errorf("%w: decode gemini batchEmbedContents response: %w", errUpstream, err)
		}
		vectors = make([][]float64, len(resp.Embeddings))
		for i, e := range resp.Embeddings {
			vectors[i] = e.Values
		}
	}

	data := make([]any, len(vectors))
	for i, v := range vectors {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}

	u := usage{prompt: estimatedTokens, estimated: true}
	out := map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage": map[string]any{
			"prompt_tokens": u.prompt,
			"total_tokens":  u.prompt,
		},
	}
	return out, u, nil
}
