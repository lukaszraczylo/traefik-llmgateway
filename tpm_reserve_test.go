package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reserveFixture struct {
	h      http.Handler
	gw     *Gateway
	store  *callCountingStore
	upBody string
	hits   atomic.Int64
	status atomic.Int64
	hold   time.Duration
}

// newReserveFixture serves chat completions with usage 30+30 tokens after
// f.hold, for a user alice limited by limits.
func newReserveFixture(t *testing.T, limits *LimitsConfig, hold time.Duration, mut ...func(*Config)) *reserveFixture {
	t.Helper()
	f := &reserveFixture{hold: hold, upBody: memberLimitsUpstreamBody}
	f.status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		time.Sleep(f.hold)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(f.upBody))
	}))
	t.Cleanup(srv.Close)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: providerTypeOpenAI, BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: limits}}}
	for _, m := range mut {
		m(cfg)
	}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	f.gw = h.(*Gateway)
	f.h = h
	f.gw.limiter.spawn = func(fn func()) { fn() }
	f.store = &callCountingStore{counterStore: newMemoryStore()}
	f.gw.limiter.store = f.store
	f.gw.limiter.nowFn = func() time.Time { return time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC) }
	return f
}

// chat posts a chat request whose body is about estTokens*4 bytes.
func (f *reserveFixture) chat(t *testing.T, estTokens int) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", estTokens*4)}}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *reserveFixture) minuteTokens() int64 {
	now := f.gw.limiter.now()
	in, _ := f.gw.limiter.getCounter("user", "alice", metricTokIn, windowMin, now)
	out, _ := f.gw.limiter.getCounter("user", "alice", metricTokOut, windowMin, now)
	return in + out
}

func TestTPMReserve_BurstOvershootBounded(t *testing.T) {
	f := newReserveFixture(t, &LimitsConfig{TokensPerMinute: 1_200_000}, 400*time.Millisecond)
	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch f.chat(t, 180_000).Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, ok.Load(), int64(7), "reservations stop the burst at the limit")
	assert.Equal(t, int64(10), ok.Load()+refused.Load())
}

func TestTPMReserve_ReconcilesToActualUsage(t *testing.T) {
	f := newReserveFixture(t, &LimitsConfig{TokensPerMinute: 1_200_000}, 0)
	require.Equal(t, http.StatusOK, f.chat(t, 180_000).Code)
	assert.EqualValues(t, 60, f.minuteTokens(), "reservation replaced by the 30+30 reported tokens")
}

func TestTPMReserve_ReleasedWhenUpstreamFails(t *testing.T) {
	f := newReserveFixture(t, &LimitsConfig{TokensPerMinute: 1_200_000}, 0)
	f.status.Store(http.StatusBadRequest)
	f.upBody = `{"error":{"message":"bad"}}`
	require.NotEqual(t, http.StatusOK, f.chat(t, 180_000).Code)
	assert.EqualValues(t, 0, f.minuteTokens(), "a failed call releases its reservation")
}

func TestTPMReserve_RolledBackOnRefusal(t *testing.T) {
	f := newReserveFixture(t, &LimitsConfig{RequestsPerMinute: 1, TokensPerMinute: 1_200_000}, 0)
	require.Equal(t, http.StatusOK, f.chat(t, 180_000).Code)
	require.Equal(t, http.StatusTooManyRequests, f.chat(t, 180_000).Code)
	assert.EqualValues(t, 60, f.minuteTokens(), "the refused request leaves no reservation")
}

func TestTPMReserve_NoStoreCallsWithoutTokensPerMinute(t *testing.T) {
	f := newReserveFixture(t, &LimitsConfig{RequestsPerMinute: 100}, 0)
	require.Equal(t, http.StatusOK, f.chat(t, 180_000).Code)
	assert.EqualValues(t, 3, f.store.calls.Load())
	for _, k := range []string{metricTokIn, metricTokOut} {
		v, _ := f.gw.limiter.getCounter("user", "alice", k, windowMin, f.gw.limiter.now())
		assert.EqualValues(t, 0, v, "no minute token counter without tokensPerMinute")
	}
}

func TestTPMReserve_ReservationWithoutLimitMakesNoEntries(t *testing.T) {
	store := &callCountingStore{counterStore: newMemoryStore()}
	l := newLimiter(store, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 5}}, {kind: totalScopeKind, id: totalScopeID}}
	v, _ := l.checkAndCountInfo(scopes, 180_000)
	require.Nil(t, v)
	assert.EqualValues(t, 1, store.calls.Load())
	assert.Nil(t, scopes[0].resv)
	l.releaseReservations(scopes)
	assert.EqualValues(t, 1, store.calls.Load(), "nothing to release")
}

func TestTPMReserve_SettlesIntoCompletionMinute(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerMinute: 1_000_000}}}
	v, _ := l.checkAndCountInfo(scopes, 180_000)
	require.Nil(t, v)
	got, _ := l.getCounter("user", "u", metricTokIn, windowMin, now)
	assert.EqualValues(t, 180_000, got)

	later := now.Add(time.Minute)
	l.nowFn = func() time.Time { return later }
	l.account(scopes, usage{prompt: 500, completion: 20}, 0)
	got, _ = l.getCounter("user", "u", metricTokIn, windowMin, now)
	assert.EqualValues(t, 0, got, "reservation leaves the minute it was made in")
	got, _ = l.getCounter("user", "u", metricTokIn, windowMin, later)
	assert.EqualValues(t, 500, got)

	l.account(scopes, usage{prompt: 500}, 0)
	got, _ = l.getCounter("user", "u", metricTokIn, windowMin, later)
	assert.EqualValues(t, 1000, got, "a settled reservation is not released twice (failover re-accounting)")
}
