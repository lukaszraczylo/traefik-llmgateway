package traefikllmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fbTimeout = 200 * time.Millisecond
	fbSlow    = 3 * time.Second

	fbChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	fbDone  = "data: [DONE]\n\n"
)

type fbMode int

const (
	fbSlowJSON fbMode = iota
	fbHeadersThenPingThenStall
	fbEventThenStall
)

// fbBackend answers per mode, always unblocking when the request is canceled so the server can close.
func fbBackend(mode fbMode, delay time.Duration) *routerBackend {
	b := &routerBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&b.calls, 1)
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice a closed connection
		wait := func() bool {
			select {
			case <-time.After(delay):
				return true
			case <-r.Context().Done():
				return false
			}
		}
		switch mode {
		case fbSlowJSON:
			if !wait() {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(successRespBody))
		case fbHeadersThenPingThenStall:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(": keep-alive\n\n"))
			w.(http.Flusher).Flush()
			if !wait() {
				return
			}
			_, _ = w.Write([]byte(fbChunk + fbDone))
		case fbEventThenStall:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fbChunk))
			w.(http.Flusher).Flush()
			if !wait() {
				return
			}
			_, _ = w.Write([]byte(fbDone))
		}
	}))
	return b
}

// fbGateway wires a router over alpha, beta, gamma with the first-byte deadline set on the compiled router,
// bypassing the 1s config floor so tests stay fast.
func fbGateway(t *testing.T, alpha, beta, gamma *routerBackend, d time.Duration) (http.Handler, *Gateway) {
	t.Helper()
	cfg := routerTestConfig(alpha, beta, gamma, threeTiers())
	cfg.Metrics = &MetricsConfig{Enabled: true}
	h, gw := buildRouterGateway(t, cfg)
	gw.registry.routers[routerName].firstByte = d
	return h, gw
}

func TestFirstByte_SlowNonStreamEscalatesToNextCandidate(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := fbGateway(t, a, b, c, fbTimeout)

	start := time.Now()
	rec := routerChat(t, h, routerName, "hi", nil)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "t1", rec.Header().Get("X-Llmgw-Router-Tier"))
	assert.Equal(t, "beta/b1", rec.Header().Get("X-Llmgw-Routed-Model"))
	assert.Less(t, elapsed, fbSlow/2, "served by the second candidate well before the slow upstream would answer")
	assert.GreaterOrEqual(t, elapsed, fbTimeout)
	assert.Equal(t, int64(1), a.count())
	assert.Equal(t, int64(1), b.count())
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeSlow), "the request counts under the slow outcome")
	reqs, escs := gw.routerStats.snapshot()
	assert.NotEmpty(t, reqs)
	var crossed int64
	for k, v := range escs {
		if k.from == "t0" && k.to == "t1" {
			crossed += v
		}
	}
	assert.Equal(t, int64(1), crossed, "crossing tiers on a slow attempt counts as an escalation")
}

func TestFirstByte_SlowStreamWithHeadersAndComments(t *testing.T) {
	a := fbBackend(fbHeadersThenPingThenStall, fbSlow)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := fbGateway(t, a, b, c, fbTimeout)

	start := time.Now()
	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get("X-Llmgw-Routed-Model"), "headers and a comment line are not a first data event")
	assert.Less(t, elapsed, fbSlow/2)
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeSlow))
}

func TestFirstByte_BytesWrittenNoEscalation(t *testing.T) {
	a := fbBackend(fbEventThenStall, 4*fbTimeout)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := fbGateway(t, a, b, c, fbTimeout)

	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.True(t, strings.HasPrefix(body, fbChunk), body)
	assert.True(t, strings.Contains(body, "[DONE]"), "the stream finished on its own tier after the first event")
	assert.Equal(t, int64(0), b.count()+c.count())
	assert.Equal(t, "alpha/a1", rec.Header().Get("X-Llmgw-Routed-Model"))
	assert.Zero(t, outcomeCount(gw, routerOutcomeSlow), "the deadline is over once the first event arrived")
}

func TestFirstByte_OffIsUnchanged(t *testing.T) {
	a := fbBackend(fbSlowJSON, 4*fbTimeout)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := fbGateway(t, a, b, c, 0)

	rec := routerChat(t, h, routerName, "hi", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "alpha/a1", rec.Header().Get("X-Llmgw-Routed-Model"))
	assert.Equal(t, int64(0), b.count())
	assert.Zero(t, outcomeCount(gw, routerOutcomeSlow))
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeOK))
}

func TestFirstByte_NotAppliedToDirectRequests(t *testing.T) {
	a := fbBackend(fbSlowJSON, 4*fbTimeout)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := fbGateway(t, a, b, c, fbTimeout)

	rec := routerChat(t, h, "alpha/a1", "hi", nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(1), a.count())
	assert.Equal(t, int64(0), b.count())
}

func TestFirstByte_AllCandidatesSlowReturns504(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := fbGateway(t, a, b, c, fbTimeout)

	start := time.Now()
	rec := routerChat(t, h, routerName, "hi", nil)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Less(t, elapsed, 3*fbTimeout+fbTimeout, "one deadline per candidate, not the upstream's own delay")
	assert.GreaterOrEqual(t, elapsed, 3*fbTimeout)
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "timeout_error", env.Error.Type)
	assert.Contains(t, env.Error.Message, "did not start responding in time")
	assert.Equal(t, int64(3), a.count()+b.count()+c.count())
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeSlow))
}

func TestFirstByte_AllCandidatesSlowAnthropicShape(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := fbGateway(t, a, b, c, fbTimeout)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/messages", "sk-alice", map[string]any{
		"model": routerName, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "error", env.Type)
	assert.Equal(t, "timeout_error", env.Error.Type)
	assert.Contains(t, env.Error.Message, "did not start responding in time")
}

func TestFirstByte_AttemptDeadlineDoesNotCancelClient(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := fbGateway(t, a, b, c, fbTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", map[string]any{
		"model": routerName, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NoError(t, ctx.Err(), "the attempt deadline cancels the attempt only")
}

func TestFirstByte_NoGoroutineLeak(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := fbGateway(t, a, b, c, 50*time.Millisecond)

	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, routerChat(t, h, routerName, "hi", nil).Code)
	}
	settle := func() int {
		deadline := time.Now().Add(3 * time.Second)
		n := runtime.NumGoroutine()
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			if m := runtime.NumGoroutine(); m < n {
				n = m
			} else {
				break
			}
		}
		return n
	}
	before := settle()
	const rounds = 20
	for i := 0; i < rounds; i++ {
		require.Equal(t, http.StatusOK, routerChat(t, h, routerName, "hi", nil).Code)
	}
	after := settle()
	assert.Less(t, after-before, rounds/2, "goroutines grew from %d to %d over %d slow attempts", before, after, rounds)
}

func TestFirstByte_TimeoutErrorIsFailoverEligible(t *testing.T) {
	var err error = &firstByteTimeoutError{provider: "alpha", timeout: time.Second}
	assert.True(t, failoverEligible(err))
	assert.True(t, routerEscalatable(err, true))
	success, record := requestHealthOutcome(err)
	assert.False(t, success, "a slow attempt counts as a failure, like a stalled body")
	assert.True(t, record)
	assert.Contains(t, err.Error(), "alpha")
	assert.True(t, isFirstByteTimeout(err))
	assert.False(t, isFirstByteTimeout(context.Canceled))
	assert.False(t, isFirstByteTimeout(nil))
}

func TestSSEDataDetector(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "comment only", chunks: []string{": ping\n\n"}},
		{name: "event line only", chunks: []string{"event: message_start\n"}},
		{name: "data line", chunks: []string{"data: {}\n\n"}, want: true},
		{name: "data after comment", chunks: []string{": ping\n\ndata: x\n\n"}, want: true},
		{name: "data after event line", chunks: []string{"event: m\ndata: x\n"}, want: true},
		{name: "split across reads", chunks: []string{"da", "ta", ": x"}, want: true},
		{name: "data inside a comment", chunks: []string{": data: x\n"}},
		{name: "data inside another field", chunks: []string{"id: data:1\n"}},
		{name: "crlf lines", chunks: []string{": a\r\ndata: x\r\n"}, want: true},
		{name: "byte order mark", chunks: []string{"\xef\xbb\xbfdata: x\n"}, want: true},
		{name: "empty", chunks: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d sseDataDetector
			got := false
			for _, c := range tt.chunks {
				if d.feed([]byte(c)) {
					got = true
				}
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFirstByteGuard(t *testing.T) {
	t.Run("fires and cancels when never satisfied", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newFirstByteGuard(cancel, 20*time.Millisecond)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("guard did not cancel the attempt")
		}
		assert.True(t, g.timedOut())
		assert.False(t, g.satisfy(), "a byte after the deadline does not count")
	})
	t.Run("satisfied before the deadline never fires", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newFirstByteGuard(cancel, 20*time.Millisecond)
		assert.True(t, g.satisfy())
		time.Sleep(60 * time.Millisecond)
		assert.NoError(t, ctx.Err())
		assert.False(t, g.timedOut())
		assert.True(t, g.satisfy(), "satisfy is idempotent")
	})
	t.Run("stop disarms", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newFirstByteGuard(cancel, 20*time.Millisecond)
		g.stop()
		time.Sleep(60 * time.Millisecond)
		assert.NoError(t, ctx.Err())
		assert.False(t, g.timedOut())
	})
	t.Run("nil guard is inert", func(t *testing.T) {
		var g *firstByteGuard
		assert.True(t, g.satisfy())
		assert.False(t, g.timedOut())
		g.stop()
	})
}

func TestFirstByteTimeoutConfig(t *testing.T) {
	build := func(raw string, mutate func(*Config)) error {
		a, b, c := newRouterBackend(200, "{}"), newRouterBackend(200, "{}"), newRouterBackend(200, "{}")
		defer closeBackends(a, b, c)
		cfg := routerTestConfig(a, b, c, threeTiers())
		cfg.SmartRouters[routerName].FirstByteTimeout = raw
		if mutate != nil {
			mutate(cfg)
		}
		_, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
		return err
	}
	assert.NoError(t, build("", nil))
	assert.NoError(t, build("1s", nil))
	assert.NoError(t, build("40s", nil))
	for _, bad := range []string{"500ms", "0s", "-5s", "soon", "40"} {
		err := build(bad, nil)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "firstByteTimeout", bad)
	}
	err := build("40s", func(c *Config) { c.Providers["alpha"].RequestTimeout = "30s" })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requestTimeout")
	assert.NoError(t, build("20s", func(c *Config) { c.Providers["alpha"].RequestTimeout = "30s" }))
	assert.NoError(t, build("40s", func(c *Config) {
		c.Providers["zeta"] = &ProviderConfig{Type: "openai", BaseURL: "http://127.0.0.1:1", APIKey: "k", Models: []string{"z"}, RequestTimeout: "5s"}
	}),
		"a provider the router does not use does not bound the deadline")
}

func TestFirstByteTimeoutShownInAdminRouterView(t *testing.T) {
	a, b, c := newRouterBackend(200, "{}"), newRouterBackend(200, "{}"), newRouterBackend(200, "{}")
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.SmartRouters[routerName].FirstByteTimeout = "40s"
	_, gw := buildRouterGateway(t, cfg)

	views := gw.buildAdminRouterViews()
	require.Len(t, views, 1)
	assert.Equal(t, "40s", views[0].FirstByteTimeout)
	raw, err := json.Marshal(views[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"firstByteTimeout":"40s"`)
}
