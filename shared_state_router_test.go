package traefikllmgateway

import (
	"testing"
	"time"
)

func sharedDecisionCfg(c *Config) {
	pc := c.Providers["alpha"]
	pc.PathBaseURLs = map[string]string{"/v1/decide": pc.BaseURL}
	rc := c.SmartRouters[routerName]
	rc.Classifier = smartClassifierV2
	rc.Decision = &SmartRouterDecisionConfig{Provider: "alpha"}
	other := *rc
	c.SmartRouters["other"] = &other
}

func decisionOf(g *Gateway, router string) *smartDecider {
	return g.registry.routers[router].decision.dec
}

func TestSharedDecisionBreaker(t *testing.T) {
	a, b, _, clk, _ := sharedGatewayPair(t, sharedDecisionCfg)
	for i := 0; i < decisionBreakerFails; i++ {
		decisionOf(a, routerName).record(false)
	}
	if decisionOf(b, routerName).breakerOpen() {
		t.Fatal("B open before any refresh, breakerOpen must not read the store")
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	if !decisionOf(b, routerName).breakerOpen() {
		t.Fatal("B closed after A tripped the decision breaker and one refresh")
	}
	if !decisionOf(b, "other").breakerOpen() {
		t.Fatal("a second router on the same decision provider did not share the breaker")
	}

	decisionOf(b, routerName).record(true)
	b.sharedState.refresh()
	if decisionOf(b, routerName).breakerOpen() || decisionOf(b, "other").breakerOpen() {
		t.Fatal("B still open after a successful decision call")
	}

	for i := 0; i < decisionBreakerFails; i++ {
		decisionOf(a, routerName).record(false)
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	clk.advance(decisionBreakerOpen + time.Second)
	if decisionOf(b, routerName).breakerOpen() {
		t.Fatal("B open after the open window elapsed")
	}
}

func TestSharedStateOffIdenticalDecisionBreaker(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	rec, _ := sharedTestEnv()
	gw.limiter.store = rec
	now := time.Unix(1_800_000_000, 0)
	nowFn := func() time.Time { return now }
	got, want := newSmartDecider(nowFn), newSmartDecider(nowFn)
	steps := []struct {
		ok      bool
		advance time.Duration
	}{{false, 0}, {false, 0}, {false, 0}, {true, 0}, {false, 0}, {false, 31 * time.Second}, {false, 0}, {false, 0}}
	for i, s := range steps {
		now = now.Add(s.advance)
		got.record(s.ok)
		want.record(s.ok)
		if got.breakerOpen() != want.breakerOpen() {
			t.Fatalf("step %d: breakerOpen = %v, want %v", i, got.breakerOpen(), want.breakerOpen())
		}
	}
	if len(rec.reads) != 0 || len(rec.writes) != 0 {
		t.Fatalf("store touched with sharedState off: %d reads, %d writes", len(rec.reads), len(rec.writes))
	}
}
