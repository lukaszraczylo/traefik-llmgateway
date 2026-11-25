package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPacingDelayScaling(t *testing.T) {
	p := &PacingConfig{StartPct: 80, MaxDelay: "10s"}
	cases := []struct {
		frac float64
		want time.Duration
	}{
		{0.5, 0},
		{0.8, 0},
		{0.9, 5 * time.Second},
		{1.0, 10 * time.Second},
		{1.5, 10 * time.Second},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, p.delay(c.frac), "frac %v", c.frac)
	}
	var none *PacingConfig
	assert.Equal(t, time.Duration(0), none.delay(1))
}

func TestPacingValidate(t *testing.T) {
	cases := []struct {
		p       *PacingConfig
		name    string
		wantErr bool
	}{
		{nil, "nil", false},
		{&PacingConfig{StartPct: 80, MaxDelay: "10s"}, "ok", false},
		{&PacingConfig{StartPct: 1, MaxDelay: "20s"}, "max 20s", false},
		{&PacingConfig{StartPct: 80, MaxDelay: "21s"}, "over 20s", true},
		{&PacingConfig{StartPct: 80, MaxDelay: "60s"}, "old 60s cap", true},
		{&PacingConfig{StartPct: 80, MaxDelay: "0s"}, "zero delay", true},
		{&PacingConfig{StartPct: 80, MaxDelay: "-1s"}, "negative delay", true},
		{&PacingConfig{StartPct: 80, MaxDelay: "soon"}, "bad duration", true},
		{&PacingConfig{StartPct: 80}, "empty duration", true},
		{&PacingConfig{StartPct: 0, MaxDelay: "1s"}, "start 0", true},
		{&PacingConfig{StartPct: 100, MaxDelay: "1s"}, "start 100", true},
		{&PacingConfig{StartPct: -5, MaxDelay: "1s"}, "start negative", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := (&LimitsConfig{RequestsPerMinute: 10, Pacing: c.p}).validate()
			assert.Equal(t, c.wantErr, err != nil, "err=%v", err)
		})
	}
}

func TestPacingValidate_RequiresRequestsPerMinute(t *testing.T) {
	p := &PacingConfig{StartPct: 80, MaxDelay: "1s"}
	err := (&LimitsConfig{Pacing: p}).validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pacing requires requestsPerMinute")
	err = (&LimitsConfig{TokensPerMinute: 1000, Pacing: p}).validate()
	require.Error(t, err, "tokensPerMinute alone does not bound concurrent paced requests")
	assert.Contains(t, err.Error(), "pacing requires requestsPerMinute")
	assert.NoError(t, (&LimitsConfig{RequestsPerMinute: 5, Pacing: p}).validate())
}

func TestPacing_RejectedOnGroupAggregateLimits(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups: map[string]*GroupConfig{"g": {Limits: &LimitsConfig{
			RequestsPerMinute: 5, Pacing: &PacingConfig{StartPct: 50, MaxDelay: "1s"}}}},
		Users: &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "g", APIKey: "sk-a"}}},
	}
	_, err := newAuthStore(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pacing")
	assert.Contains(t, err.Error(), "memberLimits")

	cfg.Groups["g"] = &GroupConfig{MemberLimits: &LimitsConfig{RequestsPerMinute: 5, Pacing: &PacingConfig{StartPct: 50, MaxDelay: "1s"}}}
	_, err = newAuthStore(cfg)
	assert.NoError(t, err, "memberLimits pacing stays valid")
}

func TestPace_CancellationDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	assert.False(t, pace(ctx, 30*time.Second), "cancelled wait reports gone")
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, pace(context.Background(), 0))
	assert.True(t, pace(context.Background(), time.Millisecond))
}

func TestMemberLimits_PacingMergesAndSources(t *testing.T) {
	a, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups: map[string]*GroupConfig{
			"g1": {MemberLimits: &LimitsConfig{RequestsPerMinute: 5, Pacing: &PacingConfig{StartPct: 90, MaxDelay: "5s"}}},
			"g2": {MemberLimits: &LimitsConfig{RequestsPerMinute: 5, Pacing: &PacingConfig{StartPct: 70, MaxDelay: "2s"}}},
		},
		Users: &UsersConfig{Inline: []*UserConfig{
			{Name: "a", Group: "g1", Groups: []string{"g2"}, APIKey: "sk-a"},
			{Name: "b", Group: "g1", APIKey: "sk-b", Limits: &LimitsConfig{RequestsPerMinute: 5, Pacing: &PacingConfig{StartPct: 50, MaxDelay: "1s"}}},
		}},
	})
	require.NoError(t, err)
	ua, _, _ := identifyWithKey(a, "sk-a")
	assert.EqualValues(t, 70, ua.limits.Pacing.StartPct, "earliest startPct is strictest")
	assert.Equal(t, "5s", ua.limits.Pacing.MaxDelay, "longest maxDelay is strictest")
	assert.Equal(t, 5*time.Second, ua.limits.Pacing.maxDelayDuration())
	assert.Equal(t, "memberLimits:g2", ua.limitSources["pacing"])
	ub, _, _ := identifyWithKey(a, "sk-b")
	assert.EqualValues(t, 50, ub.limits.Pacing.StartPct, "user pacing wins")
	assert.Equal(t, "user", ub.limitSources["pacing"])
}

func TestRateLimitHeaders_AllDialects(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	info := &rateInfo{reset: windowEnd(now, windowMin), reqLimit: 5, reqRemaining: 3, tokLimit: 1000, tokRemaining: 400}
	h := http.Header{}
	writeRateLimitHeaders(h, info, now)

	assert.Equal(t, "5", h.Get("x-ratelimit-limit-requests"))
	assert.Equal(t, "3", h.Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "30s", h.Get("x-ratelimit-reset-requests"))
	assert.Equal(t, "1000", h.Get("x-ratelimit-limit-tokens"))
	assert.Equal(t, "400", h.Get("x-ratelimit-remaining-tokens"))
	assert.Equal(t, "30s", h.Get("x-ratelimit-reset-tokens"))

	assert.Equal(t, "5", h.Get("anthropic-ratelimit-requests-limit"))
	assert.Equal(t, "3", h.Get("anthropic-ratelimit-requests-remaining"))
	assert.Equal(t, "2026-08-20T10:01:00Z", h.Get("anthropic-ratelimit-requests-reset"))
	assert.Equal(t, "1000", h.Get("anthropic-ratelimit-tokens-limit"))
	assert.Equal(t, "400", h.Get("anthropic-ratelimit-tokens-remaining"))
	assert.Equal(t, "2026-08-20T10:01:00Z", h.Get("anthropic-ratelimit-tokens-reset"))
	assert.Empty(t, h.Get("anthropic-ratelimit-input-tokens-limit"), "tokens-* is the combined count; input-tokens-* would mislabel it")

	assert.Equal(t, `"requests";r=3;t=30, "tokens";r=400;t=30`, h.Get("RateLimit"))
	assert.Equal(t, `"requests";q=5;w=60, "tokens";q=1000;w=60`, h.Get("RateLimit-Policy"))
}

func TestRateLimitHeaders_OnlyConfiguredWindows(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	h := http.Header{}
	writeRateLimitHeaders(h, &rateInfo{reset: windowEnd(now, windowMin), reqLimit: 2, reqRemaining: 0}, now)
	assert.Equal(t, "0", h.Get("x-ratelimit-remaining-requests"))
	assert.Empty(t, h.Get("x-ratelimit-limit-tokens"))
	assert.Empty(t, h.Get("anthropic-ratelimit-input-tokens-limit"))
	assert.NotContains(t, h.Get("RateLimit"), "tokens")

	empty := http.Header{}
	writeRateLimitHeaders(empty, nil, now)
	assert.Empty(t, empty)
}

func TestRateLimitPolicy_MergesWithConcurrencyBothOrders(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	info := &rateInfo{reset: windowEnd(now, windowMin), reqLimit: 5, reqRemaining: 3}
	want := `"requests";q=5;w=60, ` + concurrencyPolicyValue(4)

	h := http.Header{}
	writeRateLimitHeaders(h, info, now)
	setConcurrencyHeader(h, 4)
	assert.Equal(t, want, h.Get("RateLimit-Policy"))
	setConcurrencyHeader(h, 4)
	assert.Equal(t, want, h.Get("RateLimit-Policy"), "re-setting replaces its own member only")
	setConcurrencyHeader(h, 0)
	assert.Equal(t, `"requests";q=5;w=60`, h.Get("RateLimit-Policy"), "clearing keeps the quota member")

	h2 := http.Header{}
	setConcurrencyHeader(h2, 4)
	writeRateLimitHeaders(h2, info, now)
	assert.Equal(t, concurrencyPolicyValue(4)+`, "requests";q=5;w=60`, h2.Get("RateLimit-Policy"))
}

type callCountingStore struct {
	counterStore
	calls atomic.Int64
}

func (s *callCountingStore) getMulti(keys []string) ([]int64, error) {
	s.calls.Add(1)
	return s.counterStore.getMulti(keys)
}
func (s *callCountingStore) incrMulti(e []counterIncr) ([]int64, error) {
	s.calls.Add(1)
	return s.counterStore.incrMulti(e)
}
func (s *callCountingStore) incrAndGetMulti(e []counterIncr, r []string) ([]int64, []int64, error) {
	s.calls.Add(1)
	return s.counterStore.incrAndGetMulti(e, r)
}

func TestCheckAndCountInfo_NoExtraStoreCall(t *testing.T) {
	store := &callCountingStore{counterStore: newMemoryStore()}
	l := newLimiter(store, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 5, TokensPerMinute: 100, Pacing: &PacingConfig{StartPct: 50, MaxDelay: "1s"}}}}
	v, info := l.checkAndCountInfo(scopes)
	require.Nil(t, v)
	require.NotNil(t, info)
	assert.EqualValues(t, 1, store.calls.Load(), "one round trip, headers included")
	assert.EqualValues(t, 4, info.reqRemaining)
	assert.EqualValues(t, 100, info.tokRemaining)
}

func TestCheckAndCountInfo_TokenRemainingAndFraction(t *testing.T) {
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC) }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 10, TokensPerMinute: 100}}}
	l.account(scopes, usage{prompt: 60, completion: 20}, 0)
	v, info := l.checkAndCountInfo(scopes)
	require.Nil(t, v)
	assert.EqualValues(t, 20, info.tokRemaining)
	assert.InDelta(t, 0.8, info.frac, 1e-9, "tokens fraction beats requests fraction 0.1")
}

func withRateLimitHeaders(c *Config) { c.RateLimitHeaders = true }

func pacingGateway(t *testing.T, limits *LimitsConfig, memberLimits *LimitsConfig) (http.Handler, *Gateway) {
	t.Helper()
	groups := map[string]*GroupConfig{"friends": {MemberLimits: memberLimits}}
	users := []*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a", Limits: limits}}
	return memberLimitsGateway(t, groups, users, withRateLimitHeaders)
}

func TestPacing_EndToEndHeadersAnd429(t *testing.T) {
	h, _ := pacingGateway(t, nil, &LimitsConfig{RequestsPerMinute: 2})

	rec := memberLimitsChat(t, h, "sk-a")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("x-ratelimit-limit-requests"))
	assert.Equal(t, "1", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "30s", rec.Header().Get("x-ratelimit-reset-requests"))
	assert.Equal(t, "1", rec.Header().Get("anthropic-ratelimit-requests-remaining"))
	assert.Equal(t, `"requests";r=1;t=30`, rec.Header().Get("RateLimit"))
	assert.Empty(t, rec.Header().Get(pacedHeader), "pacing off: no paced header")

	require.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	rec = memberLimitsChat(t, h, "sk-a")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
}

func TestPacing_OffByDefaultLeavesNoLimitHeadersWithoutMinuteLimits(t *testing.T) {
	h, _ := pacingGateway(t, &LimitsConfig{RequestsPerDay: 100}, nil)
	rec := memberLimitsChat(t, h, "sk-a")
	require.Equal(t, http.StatusOK, rec.Code)
	for k := range rec.Header() {
		lk := strings.ToLower(k)
		assert.False(t, strings.Contains(lk, "ratelimit") || lk == "x-llmgw-paced-ms", "unexpected header %s", k)
	}
}

func TestPacing_DelaysAboveStartPct(t *testing.T) {
	h, _ := pacingGateway(t, nil, &LimitsConfig{
		RequestsPerMinute: 10,
		Pacing:            &PacingConfig{StartPct: 50, MaxDelay: "200ms"},
	})
	for i := 0; i < 5; i++ {
		rec := memberLimitsChat(t, h, "sk-a")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Header().Get(pacedHeader), "request %d is at or below startPct", i+1)
	}
	start := time.Now()
	rec := memberLimitsChat(t, h, "sk-a") // 6th of 10 = 60%
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, rec.Code)
	ms, err := strconv.Atoi(rec.Header().Get(pacedHeader))
	require.NoError(t, err)
	assert.InDelta(t, 40, ms, 1, "(0.6-0.5)/0.5 of 200ms")
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
	assert.Less(t, elapsed, 200*time.Millisecond)
}

func TestPacing_ClientCancelDuringWaitWritesNothing(t *testing.T) {
	h, _ := pacingGateway(t, nil, &LimitsConfig{
		RequestsPerMinute: 2,
		Pacing:            &PacingConfig{StartPct: 1, MaxDelay: "20s"},
	})
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-a", body)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	time.AfterFunc(30*time.Millisecond, cancel)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, rec.Body.String(), "no response body after a cancelled pacing wait")
}

func TestPacing_StreamingStillWorks(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	cfg := CreateConfig()
	cfg.RateLimitHeaders = true
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{
		RequestsPerMinute: 4,
		Pacing:            &PacingConfig{StartPct: 1, MaxDelay: "50ms"},
	}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a"}}}
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter.nowFn = func() time.Time { return time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC) }

	body := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-a", body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get(pacedHeader))
	assert.Equal(t, "3", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestPacing_EventRateGated(t *testing.T) {
	h, gw := pacingGateway(t, nil, &LimitsConfig{
		RequestsPerMinute: 10,
		Pacing:            &PacingConfig{StartPct: 10, MaxDelay: "1ms"},
	})
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	}
	n := 0
	for _, ev := range gw.events.ring.snapshot(eventRingCap) {
		if ev.Kind == eventKindPaced {
			n++
		}
	}
	assert.Equal(t, 1, n, "one paced event per user per throttle window")
}

func TestRateLimitHeaders_OffByDefault(t *testing.T) {
	groups := map[string]*GroupConfig{"friends": {}}
	users := []*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a", Limits: &LimitsConfig{RequestsPerMinute: 5, TokensPerMinute: 1000}}}
	h, _ := memberLimitsGateway(t, groups, users)
	rec := memberLimitsChat(t, h, "sk-a")
	require.Equal(t, http.StatusOK, rec.Code)
	for k := range rec.Header() {
		assert.False(t, isRateLimitHeader(k), "default config must not add header %s", k)
	}
}

func rateLimitedUser429(t *testing.T, groupLimits, userLimits *LimitsConfig, calls int) *httptest.ResponseRecorder {
	t.Helper()
	groups := map[string]*GroupConfig{"friends": {Limits: groupLimits}}
	users := []*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a", Limits: userLimits}}
	h, _ := memberLimitsGateway(t, groups, users, withRateLimitHeaders)
	var rec *httptest.ResponseRecorder
	for i := 0; i < calls; i++ {
		rec = memberLimitsChat(t, h, "sk-a")
	}
	return rec
}

func TestRateLimitHeaders_429FromGroupRequestLimitReportsZero(t *testing.T) {
	rec := rateLimitedUser429(t, &LimitsConfig{RequestsPerMinute: 2}, &LimitsConfig{RequestsPerMinute: 10}, 3)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "0", rec.Header().Get("anthropic-ratelimit-requests-remaining"))
	assert.Equal(t, `"requests";r=0;t=30`, rec.Header().Get("RateLimit"))
}

func TestRateLimitHeaders_429FromGroupTokenLimitRollsBackRequestCount(t *testing.T) {
	rec := rateLimitedUser429(t, &LimitsConfig{TokensPerMinute: 100}, &LimitsConfig{RequestsPerMinute: 10, TokensPerMinute: 1_000_000}, 3)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-tokens"), "the refused window reports zero")
	assert.Equal(t, "8", rec.Header().Get("x-ratelimit-remaining-requests"), "two admitted requests; the refused one was rolled back")
}

func TestRateLimitHeaders_429FromDayLimitReportsZeroUntilReset(t *testing.T) {
	rec := rateLimitedUser429(t, nil, &LimitsConfig{RequestsPerMinute: 10, RequestsPerDay: 1}, 2)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-requests"))
	retry := rec.Header().Get("Retry-After")
	require.NotEmpty(t, retry)
	assert.Equal(t, retry+"s", rec.Header().Get("x-ratelimit-reset-requests"), "reset matches Retry-After, not the minute window")
}

func TestPacing_ConcurrentPacedRequestsCappedPerUser(t *testing.T) {
	h, _ := pacingGateway(t, nil, &LimitsConfig{
		RequestsPerMinute: 100,
		Pacing:            &PacingConfig{StartPct: 1, MaxDelay: "400ms"},
	})
	const n = 12
	var paced atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := memberLimitsChat(t, h, "sk-a")
			if rec.Header().Get(pacedHeader) != "" {
				paced.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, paced.Load(), int64(maxConcurrentPaced), "requests above the cap proceed undelayed")
	assert.Greater(t, paced.Load(), int64(0))
}
