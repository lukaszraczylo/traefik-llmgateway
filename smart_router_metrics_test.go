package traefikllmgateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routerStatusBody(status int) string {
	if status == 200 {
		return successRespBody
	}
	return routerFail
}

// newMetricsRouterGateway builds the "auto" router with metrics on over three backends.
func newMetricsRouterGateway(t *testing.T, statuses [3]int, tiers []SmartRouterTier) (http.Handler, *Gateway) {
	t.Helper()
	a := newRouterBackend(statuses[0], routerStatusBody(statuses[0]))
	b := newRouterBackend(statuses[1], routerStatusBody(statuses[1]))
	c := newRouterBackend(statuses[2], routerStatusBody(statuses[2]))
	t.Cleanup(func() { closeBackends(a, b, c) })
	cfg := routerTestConfig(a, b, c, tiers)
	cfg.Metrics = &MetricsConfig{Enabled: true}
	return buildRouterGateway(t, cfg)
}

func routerEvents(gw *Gateway) []gatewayEvent {
	var out []gatewayEvent
	for _, ev := range gw.events.ring.snapshot(eventRingCap) {
		if ev.Kind == eventKindRouterEscalation {
			out = append(out, ev)
		}
	}
	return out
}

func scrape(gw *Gateway) string { return string(gw.renderMetrics()) }

const (
	reqSeries = `llmgateway_router_requests_total{router="auto",tier="%s",reason="%s"} %d`
	escSeries = `llmgateway_router_escalations_total{router="auto",from_tier="%s",to_tier="%s"} %d`
)

func TestRouterMetrics_SingleTierSuccess(t *testing.T) {
	h, gw := newMetricsRouterGateway(t, [3]int{200, 200, 200}, threeTiers())
	require.Equal(t, 200, routerChat(t, h, routerName, "hi", nil).Code)

	out := scrape(gw)
	assert.Contains(t, out, fmt.Sprintf(reqSeries, "t0", "score", 1))
	assert.NotContains(t, out, "llmgateway_router_escalations_total")
	assert.Empty(t, routerEvents(gw))
}

func TestRouterMetrics_KeywordReason(t *testing.T) {
	h, gw := newMetricsRouterGateway(t, [3]int{200, 200, 200}, threeTiers())
	require.Equal(t, 200, routerChat(t, h, routerName, routerKeyword, nil).Code)
	assert.Contains(t, scrape(gw), fmt.Sprintf(reqSeries, "t2", "keyword", 1))
}

func TestRouterMetrics_SameTierFailoverIsNotEscalation(t *testing.T) {
	tiers := []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1", "b1"}},
		{Name: "t1", MinScore: 300, Models: []string{"c1"}},
	}
	h, gw := newMetricsRouterGateway(t, [3]int{503, 200, 200}, tiers)
	require.Equal(t, 200, routerChat(t, h, routerName, "hi", nil).Code)

	out := scrape(gw)
	assert.Contains(t, out, fmt.Sprintf(reqSeries, "t0", "score", 1))
	assert.NotContains(t, out, "llmgateway_router_escalations_total")
	assert.Empty(t, routerEvents(gw))
}

func TestRouterMetrics_CrossTierEscalation(t *testing.T) {
	h, gw := newMetricsRouterGateway(t, [3]int{503, 503, 200}, threeTiers())
	require.Equal(t, 200, routerChat(t, h, routerName, "hi", nil).Code)

	out := scrape(gw)
	assert.Contains(t, out, fmt.Sprintf(reqSeries, "t0", "score", 1), "request counted once, not per attempt")
	assert.Contains(t, out, fmt.Sprintf(escSeries, "t0", "t1", 1))
	assert.Contains(t, out, fmt.Sprintf(escSeries, "t1", "t2", 1))

	evs := routerEvents(gw)
	require.Len(t, evs, 1, "second escalation falls inside the rate gate window")
	assert.Contains(t, evs[0].Message, `escalated from tier "t0" to tier "t1"`)
	assert.Equal(t, "alice", evs[0].User)
}

func TestRouterMetrics_ExhaustedPlanEvent(t *testing.T) {
	h, gw := newMetricsRouterGateway(t, [3]int{503, 503, 503}, threeTiers())
	require.Equal(t, 503, routerChat(t, h, routerName, routerKeyword, nil).Code)

	evs := routerEvents(gw)
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Message, "exhausted")
	assert.NotContains(t, scrape(gw), "llmgateway_router_escalations_total")
}

func TestRouterMetrics_ClientErrorIsNotExhaustion(t *testing.T) {
	h, gw := newMetricsRouterGateway(t, [3]int{400, 200, 200}, threeTiers())
	require.Equal(t, 400, routerChat(t, h, routerName, "hi", nil).Code)
	assert.Empty(t, routerEvents(gw))
}

func TestRouterMetrics_SkippedReasonWhenLowerTiersSkipped(t *testing.T) {
	plan := &routerPlan{router: "auto", reason: "score", startTier: "t0", entries: []routerPlanEntry{{tier: "t1"}}}
	assert.Equal(t, "skipped", routerRequestReason(plan))
	plan.entries[0].tier = "t0"
	assert.Equal(t, "score", routerRequestReason(plan))
}

func TestRouterMetrics_DisabledRecordsNoCounters(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))
	require.Equal(t, 200, routerChat(t, h, routerName, "hi", nil).Code)
	assert.NotContains(t, scrape(gw), "llmgateway_router_")
}

func TestRouterEventGate_SuppressesFlood(t *testing.T) {
	_, gw := newMetricsRouterGateway(t, [3]int{200, 200, 200}, threeTiers())
	now := time.Now()
	gw.events.nowFn = func() time.Time { return now }
	plan := &routerPlan{router: "auto", reason: "score", startTier: "t0", entries: []routerPlanEntry{{tier: "t0"}, {tier: "t1"}}}

	for i := 0; i < 50; i++ {
		gw.recordRouterAttempt(plan, 1, cacheEndpointChat, nil)
	}
	require.Len(t, routerEvents(gw), 1)

	now = now.Add(authFailureLogEvery + time.Second)
	gw.recordRouterAttempt(plan, 1, cacheEndpointChat, nil)
	evs := routerEvents(gw)
	require.Len(t, evs, 2)
	assert.True(t, strings.Contains(evs[0].Message, "49 more suppressed"), evs[0].Message)
}
