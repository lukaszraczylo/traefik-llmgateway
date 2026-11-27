package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	openAIStreamLength = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":700}}\n\ndata: [DONE]\n\n"
	openAIStreamEmpty = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"
	openAIStreamTool = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"
	anthropicStreamHead = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n"
	geminiTextLength    = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"MAX_TOKENS\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":9}}\n\n"
	geminiNoText        = "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":0}}\n\n"
)

func anthropicStream(text, stop string) string {
	body := anthropicStreamHead
	if text != "" {
		body += "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"
	}
	return body + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop + "\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func streamFlags(t *testing.T, kind, body string) (bool, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	var u usage
	var err error
	switch kind {
	case "openai":
		u, err = (&openaiAdapter{}).forwardStream(rec, strings.NewReader(body), false)
	case "anthropic":
		u, err = (&anthropicAdapter{}).forwardStream(rec, strings.NewReader(body), "m", false)
	default:
		u, err = (&geminiAdapter{}).forwardStream(rec, strings.NewReader(body), "m", false)
	}
	require.NoError(t, err)
	return u.finishLength, u.emptyOutput
}

func TestUsageFinishLengthStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, kind, body     string
		wantLength, wantNone bool
	}{
		{"openai length", "openai", openAIStreamLength, true, false},
		{"openai empty", "openai", openAIStreamEmpty, false, true},
		{"openai tool call", "openai", openAIStreamTool, false, false},
		{"anthropic length", "anthropic", anthropicStream("hello", "max_tokens"), true, false},
		{"anthropic end_turn", "anthropic", anthropicStream("hello", "end_turn"), false, false},
		{"anthropic empty", "anthropic", anthropicStream("", "end_turn"), false, true},
		{"gemini length", "gemini", geminiTextLength, true, false},
		{"gemini empty", "gemini", geminiNoText, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			length, empty := streamFlags(t, tt.kind, tt.body)
			assert.Equal(t, tt.wantLength, length, "finishLength")
			assert.Equal(t, tt.wantNone, empty, "emptyOutput")
		})
	}
}

func TestUsageFinishLengthStreamCutShortSetsNothing(t *testing.T) {
	t.Parallel()
	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n"
	u, err := (&openaiAdapter{}).forwardStream(httptest.NewRecorder(), strings.NewReader(cut), false)
	require.ErrorIs(t, err, errStreamTruncated, "a stream without a finish reason is a cut connection")
	assert.False(t, u.finishLength)
	assert.False(t, u.emptyOutput, "a cut connection is not an empty answer")
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func TestUsageEmptyOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, kind, body     string
		wantLength, wantNone bool
	}{
		{"openai text", "openai", successRespBody, false, false},
		{"openai null content", "openai", `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, false, true},
		{"openai blank content", "openai", `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`, false, true},
		{"openai tool call", "openai", `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c"}]},"finish_reason":"tool_calls"}]}`, false, false},
		{"openai filtered", "openai", `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}]}`, false, false},
		{"openai length", "openai", `{"choices":[{"message":{"role":"assistant","content":"cut"},"finish_reason":"length"}]}`, true, false},
		{"openai embeddings body", "openai", `{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`, false, false},
		{"anthropic text", "anthropic", `{"id":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`, false, false},
		{"anthropic empty", "anthropic", `{"id":"m","stop_reason":"end_turn","content":[],"usage":{"input_tokens":3,"output_tokens":0}}`, false, true},
		{"anthropic max_tokens", "anthropic", `{"id":"m","stop_reason":"max_tokens","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`, true, false},
		{"gemini text", "gemini", `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`, false, false},
		{"gemini empty", "gemini", `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`, false, true},
		{"gemini blocked prompt", "gemini", `{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`, false, false},
		{"gemini max tokens", "gemini", `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"MAX_TOKENS"}]}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			var u usage
			var err error
			switch tt.kind {
			case "openai":
				u, err = (&openaiAdapter{}).forwardJSON(rec, jsonResponse(tt.body))
			case "anthropic":
				u, err = (&anthropicAdapter{}).forwardJSON(rec, jsonResponse(tt.body), "m")
			default:
				u, err = (&geminiAdapter{}).forwardJSON(rec, jsonResponse(tt.body), "m")
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantLength, u.finishLength, "finishLength")
			assert.Equal(t, tt.wantNone, u.emptyOutput, "emptyOutput")
		})
	}
}

func TestUsageFlagsLeaveBillingUntouched(t *testing.T) {
	t.Parallel()
	u := withAnswerFlags(usage{prompt: 3, completion: 5}, "length", false)
	assert.Equal(t, int64(8), u.total())
	assert.True(t, u.finishLength)
	assert.True(t, u.emptyOutput)
	assert.Equal(t, usage{prompt: 3}, withAnswerFlags(usage{prompt: 3}, "", false), "no finish reason, no flags")
}

func TestBadResultRules(t *testing.T) {
	t.Parallel()
	free := &routerPlan{}
	capped := &routerPlan{maxTokensSet: true}
	cut := func(completion int64) usage { return usage{completion: completion, finishLength: true} }
	assert.True(t, free.badResult(cut(10), nil, false), "no client max_tokens: any length cut is bad")
	assert.False(t, capped.badResult(cut(100), nil, false), "a small client cap cut is the client's choice")
	assert.True(t, capped.badResult(cut(512), nil, false), "512 tokens is the threshold")
	assert.False(t, capped.badResult(cut(511), nil, false))
	assert.True(t, capped.badResult(usage{emptyOutput: true}, nil, false))
	assert.True(t, capped.badResult(usage{}, errors.New("reset"), true), "failure after the first byte is bad")
	assert.False(t, capped.badResult(usage{}, errors.New("refused"), false), "failure before any byte is an error, not bad")
	assert.False(t, capped.badResult(usage{completion: 9}, nil, false))
}

func TestBadResultIgnoresCallerEndedAttempts(t *testing.T) {
	t.Parallel()
	p := &routerPlan{}
	assert.False(t, p.badResult(usage{}, context.Canceled, true), "client cancel after headers is not the model's fault")
	assert.False(t, p.badResult(usage{}, fmt.Errorf("copy: %w", context.Canceled), true))
	assert.False(t, p.badResult(usage{}, errRequestBuildFailed, true))
	assert.True(t, p.badResult(usage{}, &providerHTTPError{status: http.StatusBadGateway}, true), "upstream 5xx after headers is bad")
	assert.False(t, p.badResult(usage{}, &providerHTTPError{status: http.StatusBadGateway}, false))
}

func TestChunkHelpers(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "length", chunkFinishReason([]byte(`{"choices":[{"delta":{},"finish_reason":"length"}]}`)))
	assert.Equal(t, "stop", chunkFinishReason([]byte(`{"choices":[{"finish_reason": "stop"}]}`)))
	assert.Equal(t, "", chunkFinishReason([]byte(`{"choices":[{"finish_reason":null}]}`)))
	assert.Equal(t, "", chunkFinishReason([]byte(`{"choices":[]}`)))
	assert.True(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)))
	assert.True(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content": "x"}}]}`)))
	assert.False(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`)))
	assert.False(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content":null,"tool_calls":null}}]}`)))
	assert.False(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"tool_calls":[]}}]}`)))
	assert.True(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content":"","tool_calls":[{"index":0}]}}]}`)))
	assert.True(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content":"a \"x\":1"}}]}`)), "escaped quotes inside text must not break the scan")
	assert.False(t, chunkHasOutput([]byte(`{"choices":[{"delta":{"content":""}}],"note":"\"content\":\"x\""}`)), "a key inside an escaped string is not a key")
	assert.True(t, smartJSONEmpty([]byte(` null `)))
	assert.True(t, smartJSONEmpty(nil))
	assert.False(t, smartJSONEmpty([]byte(`"x"`)))
}

func outcomeGateway(t *testing.T, body string) (http.Handler, *Gateway) {
	t.Helper()
	a := newRouterBackend(200, body)
	b := newRouterBackend(200, body)
	c := newRouterBackend(200, body)
	t.Cleanup(func() { closeBackends(a, b, c) })
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Metrics = &MetricsConfig{Enabled: true}
	return buildRouterGateway(t, cfg)
}

func outcomeCount(gw *Gateway, outcome string) int64 {
	_, outs := gw.routerStats.extraSnapshot()
	var n int64
	for k, v := range outs {
		if k.outcome == outcome {
			n += v
		}
	}
	return n
}

func TestOutcomeCountersBad(t *testing.T) {
	cut := func(completion string) string {
		return `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":` + completion + `}}`
	}
	tests := []struct {
		extra      map[string]any
		name, body string
		wantBad    int64
	}{
		{name: "cut with no client cap", body: cut("20"), wantBad: 1},
		{name: "cut at a long answer", body: cut("600"), extra: map[string]any{"max_tokens": 700}, wantBad: 1},
		{name: "cut by a small client cap", body: cut("20"), extra: map[string]any{"max_tokens": 20}},
		{name: "empty answer", body: `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":0}}`, wantBad: 1},
		{name: "normal answer", body: successRespBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, gw := outcomeGateway(t, tt.body)
			rec := routerChat(t, h, routerName, "hi", tt.extra)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantBad, outcomeCount(gw, routerOutcomeBad))
			if tt.wantBad == 0 {
				assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeOK))
			} else {
				assert.Zero(t, outcomeCount(gw, routerOutcomeOK), "bad replaces ok")
			}
		})
	}
}

func TestOutcomeBadDoesNotChangeRouting(t *testing.T) {
	h, gw := outcomeGateway(t, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":0}}`)
	for i := 0; i < 3; i++ {
		rec := routerChat(t, h, routerName, "hi", nil)
		assert.Equal(t, "t0", rec.Header().Get(headerRouterTier), "bad outcomes alone never move the tier")
	}
	assert.Equal(t, int64(3), outcomeCount(gw, routerOutcomeBad))
}
