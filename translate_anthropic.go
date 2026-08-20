package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicAPIVersion is the "anthropic-version" header value every request
// to Anthropic's Messages API must carry.
const anthropicAPIVersion = "2023-06-01"

// anthropicDefaultMaxTokens is the value anthropicRequestFromOpenAI sends
// for "max_tokens" when the OpenAI request supplies neither "max_tokens"
// nor "max_completion_tokens" — Anthropic requires the field, OpenAI does
// not.
const anthropicDefaultMaxTokens = 4096

// chatCompletionIDPrefix is prepended to Anthropic's message id to form the
// OpenAI-shaped response "id" field, both for a non-streaming response and
// for every streaming chunk's "id".
const chatCompletionIDPrefix = "chatcmpl-"

// translateError is a client-facing translation failure: an OpenAI-shaped
// request or Anthropic-shaped response could not be mapped to the other
// side's wire format. It implements error. The caller (Task 12's unified
// route) maps a *translateError to HTTP 400, or 501 when notSupported is
// set — embeddings has no Anthropic equivalent at all, which is a
// different failure than a single unsupported field on an otherwise valid
// request.
type translateError struct {
	msg          string
	notSupported bool
}

// Error implements the error interface.
func (e *translateError) Error() string { return e.msg }

// unsupportedFieldError returns a *translateError for an OpenAI request
// field Anthropic's API has no equivalent for, per the request mapping
// table's "n>1, logit_bias, logprobs" row.
func unsupportedFieldError(field string) *translateError {
	return &translateError{msg: fmt.Sprintf("%s is not supported for anthropic models", field)}
}

// toFloat64 extracts a numeric value from v, which may be float64 (the
// normal shape after json.Unmarshal into map[string]any) or a Go-native
// int/int64/float32 (a test, or a caller that builds req by hand).
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// toInt64 is toFloat64 truncated to an integer, for fields (max_tokens)
// Anthropic's API requires as a JSON integer.
func toInt64(v any) (int64, bool) {
	f, ok := toFloat64(v)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// stopSequencesFrom maps OpenAI's "stop" field (a string or an array of
// strings) to Anthropic's "stop_sequences" array. A present-but-empty
// string returns a nil slice — anthropicRequestFromOpenAI omits the field
// entirely rather than sending an empty array.
func stopSequencesFrom(v any) ([]string, error) {
	switch s := v.(type) {
	case string:
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	case []any:
		seqs := make([]string, 0, len(s))
		for _, e := range s {
			str, ok := e.(string)
			if !ok {
				return nil, &translateError{msg: "stop[] entries must be strings"}
			}
			seqs = append(seqs, str)
		}
		return seqs, nil
	default:
		return nil, nil
	}
}

// anthropicImageSourceFromDataURI parses an OpenAI image_url.url as a
// "data:<media-type>;base64,<data>" URI and returns Anthropic's image
// source object. Any URL that is not a base64 data URI (an http(s) image
// URL, which Anthropic's Messages API does not fetch) returns a
// *translateError, per the request mapping table's image row.
func anthropicImageSourceFromDataURI(uri string) (map[string]any, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return nil, &translateError{msg: "image_url must be a data: URI (http(s) image URLs are not supported for anthropic models)"}
	}
	header, data, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, &translateError{msg: "malformed data: URI in image_url"}
	}
	mediaType, ok := strings.CutSuffix(header, ";base64")
	if !ok {
		return nil, &translateError{msg: "data: URI in image_url must be base64-encoded"}
	}
	return map[string]any{"type": "base64", "media_type": mediaType, "data": data}, nil
}

// anthropicContentBlockFromOpenAI maps one OpenAI content-array entry
// ({"type":"text",...} or {"type":"image_url",...}) to an Anthropic
// content block. An entry of any other type is dropped (nil, nil) rather
// than erroring — an unrecognized part type is forwarded content this
// translator does not understand, not necessarily an invalid request.
func anthropicContentBlockFromOpenAI(pm map[string]any) (map[string]any, error) {
	switch ptype, _ := pm["type"].(string); ptype {
	case "text":
		text, _ := pm["text"].(string)
		return map[string]any{"type": "text", "text": text}, nil
	case "image_url":
		iu, _ := pm["image_url"].(map[string]any)
		url, _ := iu["url"].(string)
		src, err := anthropicImageSourceFromDataURI(url)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "image", "source": src}, nil
	default:
		return nil, nil
	}
}

// anthropicToolUseBlockFromOpenAI maps one assistant tool_calls[] entry
// ({"id","type":"function","function":{"name","arguments"}}) to an
// Anthropic {"type":"tool_use","id","name","input"} content block,
// parsing the OpenAI arguments JSON string into Anthropic's structured
// "input" object.
func anthropicToolUseBlockFromOpenAI(tc any) (map[string]any, error) {
	tm, ok := tc.(map[string]any)
	if !ok {
		return nil, &translateError{msg: "assistant tool_calls[] entry is malformed"}
	}
	id, _ := tm["id"].(string)
	fn, _ := tm["function"].(map[string]any)
	name, _ := fn["name"].(string)
	argsStr, _ := fn["arguments"].(string)

	input := any(map[string]any{})
	if argsStr != "" {
		if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
			return nil, &translateError{msg: "assistant tool_calls[].function.arguments is not valid JSON"}
		}
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, nil
}

// anthropicMessageFromOpenAI maps one OpenAI user/assistant message to an
// Anthropic message with the same role. A plain string content with no
// tool_calls passes through as a string (the mapping table's "content
// string" row); any other combination — a content array, an assistant
// message carrying tool_calls, or both — produces a content block array,
// per the mapping table's remaining rows.
func anthropicMessageFromOpenAI(msg map[string]any, role string) (map[string]any, error) {
	contentVal := msg["content"]

	var toolCalls []any
	if role == "assistant" {
		toolCalls, _ = msg["tool_calls"].([]any)
	}

	if s, ok := contentVal.(string); ok && len(toolCalls) == 0 {
		return map[string]any{"role": role, "content": s}, nil
	}

	var blocks []any
	switch c := contentVal.(type) {
	case string:
		if c != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": c})
		}
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			block, err := anthropicContentBlockFromOpenAI(pm)
			if err != nil {
				return nil, err
			}
			if block != nil {
				blocks = append(blocks, block)
			}
		}
	}

	for _, tc := range toolCalls {
		block, err := anthropicToolUseBlockFromOpenAI(tc)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}

	if blocks == nil {
		blocks = []any{}
	}
	return map[string]any{"role": role, "content": blocks}, nil
}

// anthropicToolResultBlock maps one OpenAI role:"tool" message to a single
// Anthropic {"type":"tool_result","tool_use_id","content"} content block,
// per the mapping table's "messages[role=tool]" row. content passes
// through unchanged — Anthropic's tool_result.content accepts a string,
// matching the common case, and this translator does not need to further
// interpret it. The caller (anthropicRequestFromOpenAI) accumulates
// consecutive tool messages' blocks into one Anthropic user turn — see
// its doc comment.
func anthropicToolResultBlock(msg map[string]any) map[string]any {
	toolCallID, _ := msg["tool_call_id"].(string)
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolCallID,
		"content":     msg["content"],
	}
}

// systemTextFromContentParts joins an OpenAI system message's array-form
// content ([{"type":"text","text":...}, ...]) into plain text, joined
// with "\n". Any part that is not {"type":"text"} has no equivalent in
// Anthropic's plain-string top-level "system" field, so it is a
// *translateError rather than being silently dropped.
func systemTextFromContentParts(parts []any) (string, error) {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			return "", &translateError{msg: "system message content[] entries must be objects"}
		}
		ptype, _ := pm["type"].(string)
		if ptype != "text" {
			return "", &translateError{msg: fmt.Sprintf("system message content part type %q is not supported for anthropic models", ptype)}
		}
		text, _ := pm["text"].(string)
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n"), nil
}

// anthropicToolChoiceFromOpenAI maps OpenAI's "tool_choice" to Anthropic's
// tool_choice object, and reports whether "tools" must be dropped from the
// request. Per the mapping table, tool_choice:"none" has no valid
// Anthropic {"type":"none"} equivalent for this translator to use, so
// "none" is expressed by omitting "tools" (and "tool_choice") entirely
// instead.
func anthropicToolChoiceFromOpenAI(tc any) (choice map[string]any, dropTools bool) {
	switch t := tc.(type) {
	case string:
		switch t {
		case "none":
			return nil, true
		case "auto":
			return map[string]any{"type": "auto"}, false
		case "required":
			return map[string]any{"type": "any"}, false
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]any{"type": "tool", "name": name}, false
			}
		}
	}
	return nil, false
}

// anthropicToolsFromOpenAI maps OpenAI's tools[].function{name,
// description, parameters} to Anthropic's tools[]{name, description,
// input_schema}, per the mapping table's tools row.
func anthropicToolsFromOpenAI(toolsRaw []any) []any {
	out := make([]any, 0, len(toolsRaw))
	for _, t := range toolsRaw {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		entry := map[string]any{
			"name":         fn["name"],
			"input_schema": fn["parameters"],
		}
		if desc, ok := fn["description"]; ok {
			entry["description"] = desc
		}
		out = append(out, entry)
	}
	return out
}

// anthropicRequestFromOpenAI maps an OpenAI chat-completion request body
// to an Anthropic Messages API request body, per this file's request
// mapping table. It returns a *translateError — never wrapped — for a
// field Anthropic has no equivalent for (n>1, logit_bias, logprobs) or a
// content part this translator cannot map (a non-data-URI image URL, a
// malformed tool_calls argument string). Consecutive role:"tool" messages
// are merged into one Anthropic user turn carrying all of their
// tool_result blocks — see anthropicToolResultBlock's doc comment.
func anthropicRequestFromOpenAI(req map[string]any) (map[string]any, error) {
	if v, ok := req["n"]; ok {
		if n, ok2 := toFloat64(v); ok2 && n > 1 {
			return nil, unsupportedFieldError("n")
		}
	}
	if _, ok := req["logit_bias"]; ok {
		return nil, unsupportedFieldError("logit_bias")
	}
	if _, ok := req["logprobs"]; ok {
		return nil, unsupportedFieldError("logprobs")
	}

	out := map[string]any{}
	if model, ok := req["model"].(string); ok {
		out["model"] = model
	}

	msgsRaw, _ := req["messages"].([]any)
	var systemParts []string
	anthMsgs := make([]any, 0, len(msgsRaw))
	// pendingToolResults accumulates consecutive role:"tool" messages'
	// blocks. Anthropic requires alternating user/assistant turns and
	// 400s on two consecutive same-role messages, and parallel tool calls
	// (one assistant message with N tool_calls, followed by N role:"tool"
	// messages) are the standard agentic transcript shape — so those N
	// messages must collapse into one Anthropic user turn carrying N
	// tool_result blocks, not N separate user turns.
	var pendingToolResults []any
	flushPendingToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		anthMsgs = append(anthMsgs, map[string]any{"role": "user", "content": pendingToolResults})
		pendingToolResults = nil
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
				text, err := systemTextFromContentParts(c)
				if err != nil {
					return nil, err
				}
				systemParts = append(systemParts, text)
			}
		case "user", "assistant":
			flushPendingToolResults()
			am, err := anthropicMessageFromOpenAI(msg, role)
			if err != nil {
				return nil, err
			}
			anthMsgs = append(anthMsgs, am)
		case "tool":
			pendingToolResults = append(pendingToolResults, anthropicToolResultBlock(msg))
		}
	}
	flushPendingToolResults()
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = anthMsgs

	maxTokens := int64(anthropicDefaultMaxTokens)
	if v, ok := req["max_completion_tokens"]; ok {
		if n, ok2 := toInt64(v); ok2 {
			maxTokens = n
		}
	} else if v, ok := req["max_tokens"]; ok {
		if n, ok2 := toInt64(v); ok2 {
			maxTokens = n
		}
	}
	out["max_tokens"] = maxTokens

	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := req["stop"]; ok {
		seqs, err := stopSequencesFrom(v)
		if err != nil {
			return nil, err
		}
		if len(seqs) > 0 {
			out["stop_sequences"] = seqs
		}
	}
	if v, ok := req["stream"]; ok {
		out["stream"] = v
	}

	toolsRaw, hasTools := req["tools"].([]any)
	dropTools := false
	if tc, ok := req["tool_choice"]; ok {
		var choice map[string]any
		choice, dropTools = anthropicToolChoiceFromOpenAI(tc)
		if choice != nil {
			out["tool_choice"] = choice
		}
	}
	if hasTools && !dropTools {
		out["tools"] = anthropicToolsFromOpenAI(toolsRaw)
	}

	if v, ok := req["user"]; ok {
		out["metadata"] = map[string]any{"user_id": v}
	}

	return out, nil
}

// anthropicContentBlock is the subset of one Anthropic response content
// block's JSON this translator reads: "text" blocks carry Text, "tool_use"
// blocks carry ID/Name/Input.
type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// anthropicUsagePayload is the "usage" object on both a non-streaming
// Anthropic response and a streaming message_start/message_delta event.
type anthropicUsagePayload struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// anthropicResponseBody is the subset of a non-streaming Anthropic
// Messages API response this translator reads.
type anthropicResponseBody struct {
	ID         string                  `json:"id"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsagePayload   `json:"usage"`
}

// anthropicFinishReason maps an Anthropic stop_reason to an OpenAI
// finish_reason: max_tokens -> "length", tool_use -> "tool_calls",
// end_turn and stop_sequence both -> "stop". An unrecognized value
// defaults to "stop", the OpenAI-safe default for a completed turn.
func anthropicFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// openAIResponseFromAnthropic maps a non-streaming Anthropic Messages API
// response body to an OpenAI chat.completion response, per this file's
// response mapping table. model and created are supplied by the caller —
// Anthropic's response carries neither the gateway-facing model id nor a
// response timestamp, so this function has no other source for them.
func openAIResponseFromAnthropic(body []byte, model string, created int64) (map[string]any, usage, error) {
	var resp anthropicResponseBody
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, usage{}, fmt.Errorf("%w: decode anthropic response: %w", errUpstream, err)
	}

	var textParts []string
	var toolCalls []any
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			args, err := reMarshalToolInput(b.Input)
			if err != nil {
				return nil, usage{}, err
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
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

	u := usage{prompt: resp.Usage.InputTokens, completion: resp.Usage.OutputTokens}

	out := map[string]any{
		"id":      chatCompletionIDPrefix + resp.ID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": anthropicFinishReason(resp.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     u.prompt,
			"completion_tokens": u.completion,
			"total_tokens":      u.total(),
		},
	}
	return out, u, nil
}

// reMarshalToolInput decodes a tool_use block's raw "input" JSON and
// re-marshals it to a compact JSON string, the shape OpenAI's
// tool_calls[].function.arguments field expects. An empty input decodes
// to "{}", matching a tool call with no arguments.
func reMarshalToolInput(raw json.RawMessage) (string, error) {
	parsed := any(map[string]any{})
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", fmt.Errorf("%w: decode tool_use input: %w", errUpstream, err)
		}
	}
	b, err := json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("%w: re-marshal tool_use input: %w", errUpstream, err)
	}
	return string(b), nil
}

// anthropicStreamState is one streaming chat completion's translation
// state: the OpenAI-shaped chunk envelope fields (id, captured from the
// message_start event; model and created, fixed at construction) and the
// running usage total. A caller constructs one per stream and calls
// translate once per upstream sseEvent, in order.
type anthropicStreamState struct {
	// toolOrdinals maps an Anthropic content-block index to the 0-based
	// ordinal of that tool call among all tool_use blocks seen so far in
	// this message. OpenAI's tool_calls[].index must be contiguous
	// starting at 0 over the tool-call array; Anthropic's content-block
	// index also counts any preceding non-tool_use blocks (a tool_use
	// block right after a text block is content-block index 1, but it is
	// still the first — index 0 — tool call).
	toolOrdinals    map[int]int
	id              string
	model           string
	u               usage
	created         int64
	nextToolOrdinal int
}

// newAnthropicStreamState returns an anthropicStreamState for one
// streaming chat completion. model is the gateway-facing model id and
// created the response timestamp every emitted chunk carries — the
// adapter fixes both once, before the first upstream event arrives.
func newAnthropicStreamState(model string, created int64) *anthropicStreamState {
	return &anthropicStreamState{model: model, created: created, toolOrdinals: map[int]int{}}
}

// usage returns the prompt/completion token counts captured so far, from
// the message_start and message_delta events already translated.
func (st *anthropicStreamState) usage() usage {
	return st.u
}

// chunk builds one OpenAI-shaped "chat.completion.chunk" payload carrying
// delta, and finishReason as its choices[0].finish_reason — nil for the
// JSON null every non-terminal chunk carries.
func (st *anthropicStreamState) chunk(delta map[string]any, finishReason *string) []byte {
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
		panic(fmt.Sprintf("llmgateway: anthropic stream chunk failed to marshal: %v", err))
	}
	return b
}

// anthropicMessageStartPayload is the "message_start" event's data shape.
type anthropicMessageStartPayload struct {
	Message struct {
		ID    string                `json:"id"`
		Usage anthropicUsagePayload `json:"usage"`
	} `json:"message"`
}

// anthropicContentBlockStartPayload is the "content_block_start" event's
// data shape.
type anthropicContentBlockStartPayload struct {
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Index int `json:"index"`
}

// anthropicContentBlockDeltaPayload is the "content_block_delta" event's
// data shape: Delta.Type distinguishes "text_delta" (Delta.Text) from
// "input_json_delta" (Delta.PartialJSON).
type anthropicContentBlockDeltaPayload struct {
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Index int `json:"index"`
}

// anthropicMessageDeltaPayload is the "message_delta" event's data shape.
type anthropicMessageDeltaPayload struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsagePayload `json:"usage"`
}

// anthropicErrorEventPayload is the "error" event's data shape.
type anthropicErrorEventPayload struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// translate maps one upstream Anthropic SSE event to zero or more
// OpenAI-shaped "chat.completion.chunk" payloads, per this file's stream
// mapping table:
//
//   - message_start: a {role:"assistant"} chunk, and captures input_tokens.
//   - content_block_start (tool_use): a delta.tool_calls[0] chunk carrying
//     id, type, and function.name with an empty arguments string. A
//     content_block_start for any other block type produces nothing.
//   - content_block_delta (text_delta): a {content: text} chunk.
//   - content_block_delta (input_json_delta): a delta.tool_calls[0] chunk
//     carrying only index and function.arguments, the incremental JSON
//     fragment.
//   - message_delta: captures output_tokens, and a finish_reason chunk
//     mapped per anthropicFinishReason.
//   - message_stop, ping, content_block_stop: nothing — the caller (the
//     adapter) writes the terminal "[DONE]" itself once the upstream
//     stream ends, on message_stop or on a clean EOF alike, per ruling
//     (b): a data-less "event:" frame is never relied on to signal the
//     end of the stream.
//   - error: one OpenAI-shaped {"error":{...}} payload, and a non-nil
//     error — translate's caller must stop reading the stream after this
//     return, but forwards the returned chunk to the client first.
//
// An unrecognized event type is ignored (nil, nil) — this translator does
// not treat an event it does not know about as a broken stream. Malformed
// data on a recognized event type is different: it returns an error
// wrapping errUpstream instead of silently producing nothing, since a
// dropped input_json_delta (say) would otherwise corrupt the client's
// reassembled tool-call arguments without either side noticing.
func (st *anthropicStreamState) translate(ev sseEvent) ([][]byte, error) {
	switch ev.event {
	case "message_start":
		var p anthropicMessageStartPayload
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return nil, fmt.Errorf("%w: decode anthropic message_start event: %w", errUpstream, err)
		}
		st.id = p.Message.ID
		st.u.prompt = p.Message.Usage.InputTokens
		return [][]byte{st.chunk(map[string]any{"role": "assistant"}, nil)}, nil

	case "content_block_start":
		var p anthropicContentBlockStartPayload
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return nil, fmt.Errorf("%w: decode anthropic content_block_start event: %w", errUpstream, err)
		}
		if p.ContentBlock.Type != "tool_use" {
			return nil, nil
		}
		ordinal := st.nextToolOrdinal
		st.toolOrdinals[p.Index] = ordinal
		st.nextToolOrdinal++
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": ordinal,
			"id":    p.ContentBlock.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      p.ContentBlock.Name,
				"arguments": "",
			},
		}}}
		return [][]byte{st.chunk(delta, nil)}, nil

	case "content_block_delta":
		var p anthropicContentBlockDeltaPayload
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return nil, fmt.Errorf("%w: decode anthropic content_block_delta event: %w", errUpstream, err)
		}
		switch p.Delta.Type {
		case "text_delta":
			return [][]byte{st.chunk(map[string]any{"content": p.Delta.Text}, nil)}, nil
		case "input_json_delta":
			ordinal, ok := st.toolOrdinals[p.Index]
			if !ok {
				// Defensive: an input_json_delta for a content-block index
				// this translator never saw a tool_use content_block_start
				// for. Fall back to the raw content-block index rather than
				// dropping the fragment — a wrong index is recoverable by
				// the client's own reassembly; a silently dropped argument
				// fragment is not.
				ordinal = p.Index
			}
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index":    ordinal,
				"function": map[string]any{"arguments": p.Delta.PartialJSON},
			}}}
			return [][]byte{st.chunk(delta, nil)}, nil
		default:
			return nil, nil
		}

	case "message_delta":
		var p anthropicMessageDeltaPayload
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return nil, fmt.Errorf("%w: decode anthropic message_delta event: %w", errUpstream, err)
		}
		st.u.completion = p.Usage.OutputTokens
		reason := anthropicFinishReason(p.Delta.StopReason)
		return [][]byte{st.chunk(map[string]any{}, &reason)}, nil

	case "error":
		var p anthropicErrorEventPayload
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return nil, fmt.Errorf("%w: decode anthropic error event: %w", errUpstream, err)
		}
		chunk, err := json.Marshal(map[string]any{
			"error": map[string]any{"message": p.Error.Message, "type": p.Error.Type},
		})
		if err != nil {
			return nil, fmt.Errorf("%w: marshal anthropic error chunk: %w", errUpstream, err)
		}
		return [][]byte{chunk}, fmt.Errorf("llmgateway: anthropic stream error: %s", p.Error.Message)

	default:
		// message_stop, ping, content_block_stop, and anything unrecognized.
		return nil, nil
	}
}
