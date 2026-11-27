package traefikllmgateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const truncatedContentChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n"

func TestOpenAIForwardStream_Truncation(t *testing.T) {
	const finishChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	const done = "data: [DONE]\n\n"
	tests := []struct {
		name      string
		body      string
		truncated bool
	}{
		{"finish and DONE", truncatedContentChunk + finishChunk + done, false},
		{"finish without DONE", truncatedContentChunk + finishChunk, false},
		{"DONE without finish", truncatedContentChunk + done, false},
		{"neither finish nor DONE", truncatedContentChunk, true},
		{"no events at all", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := (&openaiAdapter{}).forwardStream(rec, strings.NewReader(tt.body), false)
			if !tt.truncated {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, errStreamTruncated), "err = %v, want errStreamTruncated", err)
			assert.True(t, errors.Is(err, errUpstream), "a truncated stream is an upstream failure")
			assert.NotContains(t, rec.Body.String(), "[DONE]", "a truncated stream must not look finished")
		})
	}
}

// truncatingServer answers with an event-stream that ends cleanly after
// chunks, with no finish_reason and no [DONE].
func truncatingServer(chunks ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
		}
		w.(http.Flusher).Flush()
	}))
}

func TestHandleChat_TruncatedStreamBeforeOutput_FailsOver(t *testing.T) {
	var betaCalls int64
	alphaSrv := truncatingServer()
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), twoProviderFailoverConfig(alphaSrv, betaSrv), "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "stream": true, "messages": []any{}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(1), atomic.LoadInt64(&betaCalls), "an empty truncated stream must fail over")
	assert.Contains(t, rec.Body.String(), "chatcmpl-ok", "the client gets beta's answer")
}

func TestHandleChat_TruncatedStreamAfterOutput_EndsWithErrorEvent(t *testing.T) {
	var betaCalls int64
	alphaSrv := truncatingServer(truncatedContentChunk)
	defer alphaSrv.Close()
	betaSrv := countingServer(http.StatusOK, successRespBody, &betaCalls)
	defer betaSrv.Close()

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), twoProviderFailoverConfig(alphaSrv, betaSrv), "llmgw")
	require.NoError(t, err)

	body := map[string]any{"model": "shared", "stream": true, "messages": []any{}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))

	const wantErrEvent = "data: {\"error\":{\"message\":\"upstream stream ended before the response finished\",\"type\":\"server_error\"}}\n\n"
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, truncatedContentChunk+wantErrEvent, rec.Body.String())
	assert.Equal(t, int64(0), atomic.LoadInt64(&betaCalls), "no failover once output reached the client")
}

func TestHandleMessages_Stream_TruncatedUpstreamEndsWithErrorEvent(t *testing.T) {
	srv := sseUpstream(t, nil, nil, "data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
	defer srv.Close()
	gw := newMessagesTestGateway(t, "", srv.URL)

	evs := parseAnthropicEvents(t, serveMessages(t, gw, streamMessagesBody("gpt-test")).Body.String())
	names := eventNames(evs)
	if len(names) == 0 || names[len(names)-1] != "error" {
		t.Fatalf("events = %v, want the stream to end with an error event", names)
	}
	for _, n := range names {
		if n == "message_stop" {
			t.Fatal("a truncated stream must not send message_stop")
		}
	}
}

func TestRouter_TruncatedStreamBeforeOutputEscalates(t *testing.T) {
	alphaSrv := truncatingServer()
	defer alphaSrv.Close()
	a := &routerBackend{srv: alphaSrv}
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(b, c)
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "chatcmpl-ok", "the next tier answers")
	assert.Equal(t, int64(1), b.count()+c.count(), "exactly one later tier serves the request")
}
