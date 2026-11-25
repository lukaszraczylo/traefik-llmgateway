package traefikllmgateway

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitExempt_ManyEmbeddingsLeaveChatLimitsUntouched(t *testing.T) {
	h, gw := exemptGateway(t, []string{"embedding"}, &LimitsConfig{RequestsPerMinute: 5, RequestsPerDay: 10, TokensPerMinute: 100, TokensPerDay: 100}, nil)
	for i := 0; i < 1000; i++ {
		require.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code, "embedding %d", i+1)
	}
	rec := memberLimitsChat(t, h, "sk-a")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "4", rec.Header().Get("x-ratelimit-remaining-requests"), "RPM untouched by embeddings")
	assert.Equal(t, "100", rec.Header().Get("x-ratelimit-limit-tokens"))
	remaining, err := strconv.Atoi(rec.Header().Get("x-ratelimit-remaining-tokens"))
	require.NoError(t, err)
	assert.Greater(t, remaining, 50, "TPM holds only the chat request's own pre-charge, not 1000 embeddings")
	day, _ := gw.limiter.getCounter("user", "a", metricReq, windowDay, gw.limiter.now())
	assert.EqualValues(t, 1, day, "RPD counts the chat request only")
}

func TestLimitExempt_NoLimitWindowCounterMoves(t *testing.T) {
	h, gw := exemptGateway(t, []string{"embedding"}, &LimitsConfig{RequestsPerMinute: 5, TokensPerMinute: 100}, nil)
	now := gw.limiter.now()
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code)
	}
	for _, sc := range []struct{ kind, id string }{{"user", "a"}, {"group", "friends"}, {totalScopeKind, totalScopeID}} {
		for _, m := range []string{metricReq, metricTokIn, metricTokOut, metricCost} {
			for _, w := range []string{windowMin, windowHour, windowDay, windowMonth} {
				v, _ := gw.limiter.getCounter(sc.kind, sc.id, m, w, now)
				assert.EqualValues(t, 0, v, "%s/%s %s %s", sc.kind, sc.id, m, w)
			}
		}
	}
}

func TestLimitExempt_PerModelUsageAndExemptCounterStillRecorded(t *testing.T) {
	h, gw := exemptGateway(t, []string{"embedding"}, &LimitsConfig{RequestsPerMinute: 1}, nil)
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code)
	}
	now := gw.limiter.now()
	reqs, _ := gw.limiter.getCounter(kindModel, "openai/gpt-test", metricReq, windowDay, now)
	assert.EqualValues(t, 4, reqs, "per-model usage still counts exempt requests")
	tok, _ := gw.limiter.getCounter(kindModel, "openai/gpt-test", metricTokIn, windowDay, now)
	assert.EqualValues(t, 4*30, tok)

	x, _ := gw.limiter.getCounter("user", "a", metricExempt, windowMonth, now)
	assert.EqualValues(t, 4, x, "exempt counter")
	got := gw.buildAdminUsage()
	assert.EqualValues(t, 4, findUsageEntry(t, got.Users, "a").ExemptRequestsPerMonth)
	assert.EqualValues(t, 0, findUsageEntry(t, got.Users, "a").RequestsPerDay)
}
