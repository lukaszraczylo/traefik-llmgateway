package traefikllmgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const deepSeekUsageJSON = `"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100}`

func openAIForwardJSON(t *testing.T, body string) usage {
	t.Helper()
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: nopCloser{strings.NewReader(body)}}
	u, err := (&openaiAdapter{}).forwardJSON(rec, resp)
	require.NoError(t, err)
	return u
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func TestOpenAIUsageDeepSeekHits(t *testing.T) {
	t.Parallel()
	u := openAIForwardJSON(t, `{"choices":[],`+deepSeekUsageJSON+`}`)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 900, u.cached)
}

func TestOpenAIUsageDeepSeekBothFields(t *testing.T) {
	t.Parallel()
	u := openAIForwardJSON(t, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_cache_hit_tokens":900,"prompt_tokens_details":{"cached_tokens":850}}}`)
	require.EqualValues(t, 900, u.cached)
	u = openAIForwardJSON(t, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_cache_hit_tokens":100,"prompt_tokens_details":{"cached_tokens":850}}}`)
	require.EqualValues(t, 850, u.cached)
}

func TestOpenAIStreamUsageDeepSeekHits(t *testing.T) {
	t.Parallel()
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":10,\"prompt_cache_hit_tokens\":900,\"prompt_tokens_details\":{\"cached_tokens\":850}}}\n\n" +
		"data: [DONE]\n\n"
	u, err := (&openaiAdapter{}).forwardStream(httptest.NewRecorder(), strings.NewReader(body), false)
	require.NoError(t, err)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 900, u.cached)
}

const geminiCachedBody = `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":5,"cachedContentTokenCount":700}}`

func TestGeminiUsageCached(t *testing.T) {
	t.Parallel()
	_, u, err := openAIResponseFromGemini([]byte(geminiCachedBody), "m", 1)
	require.NoError(t, err)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 700, u.cached)

	st := newGeminiStreamState("m", 1)
	_, err = st.translate(sseEvent{data: []byte(geminiCachedBody)})
	require.NoError(t, err)
	su := st.usage()
	require.EqualValues(t, 1000, su.prompt)
	require.EqualValues(t, 700, su.cached)
}

const anthropicCacheUsage = `"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":800,"cache_creation_input_tokens":100`

func TestAnthropicUsageCacheWrite(t *testing.T) {
	t.Parallel()
	body := `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{` + anthropicCacheUsage + `}}`
	_, u, err := openAIResponseFromAnthropic([]byte(body), "m", 1)
	require.NoError(t, err)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 800, u.cached)
	require.EqualValues(t, 100, u.cacheWrite)
}

func TestAnthropicStreamCacheWrite(t *testing.T) {
	t.Parallel()
	st := newAnthropicStreamState("m", 1)
	_, err := st.translate(sseEvent{event: "message_start", data: []byte(`{"message":{"id":"msg_1","usage":{` + anthropicCacheUsage + `}}}`)})
	require.NoError(t, err)
	u := st.usage()
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 800, u.cached)
	require.EqualValues(t, 100, u.cacheWrite)
}

func TestPassthroughUsageCacheFields(t *testing.T) {
	t.Parallel()
	u, _, err := extractPassthroughUsage(providerTypeOpenAI, "p", []byte(`{"model":"m",`+deepSeekUsageJSON+`}`))
	require.NoError(t, err)
	require.EqualValues(t, 900, u.cached)

	u, _, err = extractPassthroughUsage(providerTypeGemini, "p", []byte(geminiCachedBody))
	require.NoError(t, err)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 700, u.cached)

	u, _, err = extractPassthroughUsage(providerTypeAnthropic, "p", []byte(`{"model":"m","usage":{`+anthropicCacheUsage+`}}`))
	require.NoError(t, err)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 800, u.cached)
	require.EqualValues(t, 100, u.cacheWrite)
}

func TestPassthroughSSEUsageCacheFields(t *testing.T) {
	t.Parallel()
	raw := "event: message_start\ndata: {\"message\":{\"model\":\"m\",\"usage\":{" + anthropicCacheUsage + "}}}\n\n"
	u, _ := extractPassthroughSSEUsage(providerTypeAnthropic, "p", []byte(raw), nil)
	require.EqualValues(t, 1000, u.prompt)
	require.EqualValues(t, 800, u.cached)
	require.EqualValues(t, 100, u.cacheWrite)

	oa := "data: {\"model\":\"m\"," + deepSeekUsageJSON + "}\n\n"
	u, _ = extractPassthroughSSEUsage(providerTypeOpenAI, "p", nil, []byte(oa))
	require.EqualValues(t, 900, u.cached)
}

func TestUncachedPromptClamp(t *testing.T) {
	t.Parallel()
	require.EqualValues(t, 0, usage{prompt: 100, cached: 90, cacheWrite: 20}.uncachedPrompt())
	require.EqualValues(t, 10, usage{prompt: 100, cached: 80, cacheWrite: 10}.uncachedPrompt())
}
