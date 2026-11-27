package traefikllmgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseEventPrefix is the SSE "event:" field prefix.
const sseEventPrefix = "event: "

// anthropicStreamErrorType is the error.type of an in-stream Anthropic error event.
const anthropicStreamErrorType = "api_error"

const (
	streamModeUnset = iota
	streamModeSSE
	streamModeJSON
)

// openAIStreamChunk is the subset of one OpenAI chat.completion.chunk the
// Anthropic translator reads. Null "content" and "finish_reason" decode to "".
type openAIStreamChunk struct {
	ID      string           `json:"id"`
	Error   json.RawMessage  `json:"error"`
	Usage   *chatStreamUsage `json:"usage"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				ID    string `json:"id"`
				Index int    `json:"index"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

type streamToolBlock struct {
	id    string
	block int
}

// anthropicStreamWriter is the http.ResponseWriter handed to a provider
// adapter's chatCompletion on a streaming /v1/messages request. The adapter
// writes OpenAI chunk frames; this writer re-emits them as the Anthropic
// Messages event sequence, flushing per event. Reusing the adapter keeps
// upstream calls, retries, timeouts and usage capture identical across
// every non-anthropic provider type.
type anthropicStreamWriter struct {
	rw       http.ResponseWriter
	tools    map[int]*streamToolBlock
	sse      *sseWriter
	model    string
	finishR  string
	raw      bytes.Buffer
	mode     int
	next     int
	openIdx  int
	started  bool
	done     bool
	hasOpen  bool
	textOpen bool
}

func newAnthropicStreamWriter(rw http.ResponseWriter, model string) *anthropicStreamWriter {
	return &anthropicStreamWriter{rw: rw, model: model, tools: map[int]*streamToolBlock{}}
}

// Header implements http.ResponseWriter.
func (s *anthropicStreamWriter) Header() http.Header { return s.rw.Header() }

// Flush implements http.Flusher.
func (s *anthropicStreamWriter) Flush() {
	if f, ok := s.rw.(http.Flusher); ok {
		f.Flush()
	}
}

// detectMode runs on the first WriteHeader/Write: an upstream that ignored
// stream=true answers JSON, which finish replays as events.
func (s *anthropicStreamWriter) detectMode() {
	if s.mode != streamModeUnset {
		return
	}
	ct := strings.ToLower(s.rw.Header().Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/event-stream") {
		s.mode = streamModeJSON
		return
	}
	s.mode = streamModeSSE
	s.sse = newSSEWriter(s.rw)
}

// WriteHeader implements http.ResponseWriter.
func (s *anthropicStreamWriter) WriteHeader(status int) {
	s.detectMode()
	if s.mode == streamModeSSE {
		s.rw.WriteHeader(status)
	}
}

// Write implements http.ResponseWriter. Each call carries whole frames:
// sseWriter emits one frame per Write.
func (s *anthropicStreamWriter) Write(b []byte) (int, error) {
	s.detectMode()
	if s.mode == streamModeJSON {
		return s.raw.Write(b)
	}
	for _, frame := range bytes.Split(b, []byte("\n\n")) {
		data, ok := sseFrameData(frame)
		if !ok || string(data) == sseDoneMarker {
			continue
		}
		if err := s.handleChunk(data); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// sseFrameData joins a frame's "data:" lines; ok is false for a frame with none.
func sseFrameData(frame []byte) (data []byte, ok bool) {
	var parts [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if v, found := bytes.CutPrefix(line, []byte("data:")); found {
			parts = append(parts, bytes.TrimPrefix(v, []byte(" ")))
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	return bytes.Join(parts, []byte("\n")), true
}

func (s *anthropicStreamWriter) emit(name string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", name, err)
	}
	return s.sse.writeEvent(name, b)
}

func (s *anthropicStreamWriter) start(id string, inputTokens int64) error {
	if s.started {
		return nil
	}
	s.started = true
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
}

func (s *anthropicStreamWriter) openBlock(block map[string]any) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.hasOpen = true
	s.openIdx = s.next
	s.next++
	return s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.openIdx, "content_block": block})
}

func (s *anthropicStreamWriter) closeBlock() error {
	if !s.hasOpen {
		return nil
	}
	s.hasOpen = false
	s.textOpen = false
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.openIdx})
}

func (s *anthropicStreamWriter) delta(index int, delta map[string]any) error {
	return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
}

func (s *anthropicStreamWriter) handleChunk(data []byte) error {
	var c openAIStreamChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%w: decode openai stream chunk: %w", errUpstream, err)
	}
	if e := bytes.TrimSpace(c.Error); len(e) > 0 && string(e) != "null" {
		return fmt.Errorf("%w: upstream stream error: %s", errUpstream, e)
	}
	var input int64
	if c.Usage != nil {
		input = c.Usage.PromptTokens
	}
	if err := s.start(anthropicMessageIDFromOpenAI(c.ID), input); err != nil {
		return err
	}
	if len(c.Choices) == 0 {
		return nil
	}
	ch := c.Choices[0]

	if ch.Delta.Content != "" {
		if !s.textOpen {
			if err := s.openBlock(map[string]any{"type": "text", "text": ""}); err != nil {
				return err
			}
			s.textOpen = true
		}
		if err := s.delta(s.openIdx, map[string]any{"type": "text_delta", "text": ch.Delta.Content}); err != nil {
			return err
		}
	}

	for _, tc := range ch.Delta.ToolCalls {
		tb, known := s.tools[tc.Index]
		if !known || (tc.ID != "" && tb.id != "" && tc.ID != tb.id) {
			err := s.openBlock(map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}})
			if err != nil {
				return err
			}
			tb = &streamToolBlock{block: s.openIdx, id: tc.ID}
			s.tools[tc.Index] = tb
		}
		if tc.Function.Arguments != "" {
			if err := s.delta(tb.block, map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}); err != nil {
				return err
			}
		}
	}

	if ch.FinishReason != "" {
		s.finishR = ch.FinishReason
	}
	return nil
}

// finish closes the open block and writes message_delta and message_stop.
// u is the adapter's final usage. On a JSON-mode upstream it replays the
// buffered response as events. Idempotent.
func (s *anthropicStreamWriter) finish(u usage) error {
	s.detectMode()
	if s.done {
		return nil
	}
	s.done = true
	if s.mode == streamModeJSON {
		return s.replayJSON()
	}
	if err := s.start("", 0); err != nil {
		return err
	}
	return s.stop(anthropicStopReasonFromOpenAI(s.finishR), u.prompt, u.completion)
}

func (s *anthropicStreamWriter) stop(stopReason string, input, output int64) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": input, "output_tokens": output},
	})
	if err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

// replayJSON turns a buffered non-streaming OpenAI response into the event sequence.
func (s *anthropicStreamWriter) replayJSON() error {
	out, err := anthropicResponseFromOpenAI(s.raw.Bytes(), s.model)
	if err != nil {
		return &responseTranslationError{err: err}
	}
	s.sse = newSSEWriter(s.rw)
	usageMap, _ := out["usage"].(map[string]any)
	input, _ := usageMap["input_tokens"].(int64)
	output, _ := usageMap["output_tokens"].(int64)
	id, _ := out["id"].(string)
	if err := s.start(id, input); err != nil {
		return err
	}
	blocks, _ := out["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["type"] == "text" {
			text, _ := block["text"].(string)
			if err := s.openBlock(map[string]any{"type": "text", "text": ""}); err != nil {
				return err
			}
			if err := s.delta(s.openIdx, map[string]any{"type": "text_delta", "text": text}); err != nil {
				return err
			}
			continue
		}
		args, mErr := json.Marshal(block["input"])
		if mErr != nil {
			return &responseTranslationError{err: mErr}
		}
		if err := s.openBlock(map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}); err != nil {
			return err
		}
		if err := s.delta(s.openIdx, map[string]any{"type": "input_json_delta", "partial_json": string(args)}); err != nil {
			return err
		}
	}
	stopReason, _ := out["stop_reason"].(string)
	return s.stop(stopReason, input, output)
}

// writeAnthropicStreamError emits an Anthropic error event on an already-started stream.
func writeAnthropicStreamError(w http.ResponseWriter, msg string) {
	b, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicStreamErrorType, "message": msg},
	})
	if err != nil {
		return
	}
	_ = newSSEWriter(w).writeEvent("error", b) // best-effort: the connection may already be gone
}

// rewriteMessageStartModel replaces message.model in a message_start
// payload, leaving every other byte untouched.
func rewriteMessageStartModel(data []byte, model string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("message_start is not a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read message_start key: %w", err)
		}
		var val json.RawMessage
		if derr := dec.Decode(&val); derr != nil {
			return nil, fmt.Errorf("read message_start value: %w", derr)
		}
		if keyTok != "message" {
			continue
		}
		rewritten, err := rewriteModelField(val, model)
		if err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		start := end - len(val)
		var out bytes.Buffer
		out.Write(data[:start])
		out.Write(rewritten)
		out.Write(data[end:])
		return out.Bytes(), nil
	}
	return nil, fmt.Errorf("message_start has no message object")
}

// errStreamErrorRelayed marks a stream whose upstream error event already
// reached the client, so the route must not write a second one.
var errStreamErrorRelayed = errors.New("upstream stream error relayed to client")

// relayAnthropicMessagesStream forwards an Anthropic Messages stream event by
// event. Only message_start changes: its message.model becomes the client's
// alias. Usage comes from the same parser the chat route uses for this
// provider type.
func relayAnthropicMessagesStream(w http.ResponseWriter, body io.Reader, model string) (usage, error) {
	st := newAnthropicStreamState(model, 0)
	sw := newSSEWriter(w)
	relayed := false
	err := readSSE(body, func(ev sseEvent) error {
		data := ev.data
		if ev.event == "message_start" {
			out, rerr := rewriteMessageStartModel(data, model)
			if rerr != nil {
				return rerr
			}
			data = out
		}
		if werr := sw.writeEvent(ev.event, data); werr != nil {
			return werr
		}
		relayed = ev.event == "error"
		_, terr := st.translate(ev)
		return terr
	})
	if err == nil {
		return st.usage(), nil
	}
	if relayed {
		return st.usage(), fmt.Errorf("%w: %w", errStreamErrorRelayed, err)
	}
	return st.usage(), fmt.Errorf("%w: stream: %w", errUpstream, err)
}

// writeEvent writes one frame with an "event:" line ahead of the data, the
// framing the Anthropic Messages stream uses. event must not contain a line
// terminator; an empty event writes a bare data frame.
func (s *sseWriter) writeEvent(event string, b []byte) error {
	if event == "" {
		return s.writeData(b)
	}
	buf := append([]byte(sseEventPrefix), event...)
	buf = append(buf, '\n')
	for _, line := range splitSSELines(b) {
		buf = append(buf, sseDataPrefix...)
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	buf = append(buf, '\n')

	if _, err := s.w.Write(buf); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}
