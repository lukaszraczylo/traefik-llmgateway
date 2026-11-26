package traefikllmgateway

import (
	"testing"
	"time"
)

// sharedGatewayPair builds two gateways from one config over one shared store and fake clock, as two pods of a fleet.
func sharedGatewayPair(t *testing.T, mut func(*Config)) (a, b *Gateway, store *sharedRecStore, clk *sharedClock, cfg *Config) {
	t.Helper()
	ba := newRouterBackend(200, successRespBody)
	bb := newRouterBackend(200, successRespBody)
	bc := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(ba, bb, bc) })
	cfg = routerTestConfig(ba, bb, bc, threeTiers())
	if mut != nil {
		mut(cfg)
	}
	store, clk = sharedTestEnv()
	pod := func() *Gateway {
		_, gw := buildRouterGateway(t, cfg)
		gw.sharedState = sharedTestLoop(store, clk)
		gw.wireSharedState(cfg)
		return gw
	}
	return pod(), pod(), store, clk, cfg
}

// sharedOffGateway is a gateway with redis.sharedState off: no loop, so wireSharedState must change nothing.
func sharedOffGateway(t *testing.T) (*Gateway, *Config) {
	t.Helper()
	ba := newRouterBackend(200, successRespBody)
	bb := newRouterBackend(200, successRespBody)
	bc := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(ba, bb, bc) })
	cfg := routerTestConfig(ba, bb, bc, threeTiers())
	_, gw := buildRouterGateway(t, cfg)
	if gw.sharedState != nil {
		t.Fatal("sharedState loop built with the switch off")
	}
	gw.wireSharedState(cfg)
	return gw, cfg
}

func TestSharedRequestBreakerAcrossGateways(t *testing.T) {
	a, b, _, _, _ := sharedGatewayPair(t, nil)
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		a.failoverHealth.record("alpha", false)
	}
	if b.failoverHealth.healthy("alpha") != true {
		t.Fatal("B unhealthy before any refresh, healthy must not read the store")
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	if b.failoverHealth.healthy("alpha") {
		t.Fatal("B healthy after A tripped the breaker and one refresh, want unhealthy")
	}
	if !b.failoverHealth.healthy("beta") {
		t.Fatal("B beta unhealthy, a trip on alpha must not leak to other providers")
	}
	b.failoverHealth.record("alpha", true)
	b.sharedState.refresh()
	if !b.failoverHealth.healthy("alpha") {
		t.Fatal("B still unhealthy after a success, want closed")
	}
}

func TestSharedRequestBreakerClosesAfterWindow(t *testing.T) {
	a, b, _, clk, _ := sharedGatewayPair(t, nil)
	for i := 0; i < requestBreakerFailureThreshold; i++ {
		a.failoverHealth.record("alpha", false)
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	clk.advance(requestBreakerOpenDuration + time.Second)
	if !b.failoverHealth.healthy("alpha") {
		t.Fatal("B unhealthy after the open window elapsed")
	}
}

func TestSharedStateOffIdenticalRequestBreaker(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	rec, _ := sharedTestEnv()
	gw.limiter.store = rec
	off := gw.failoverHealth
	ref := newRequestHealthTracker()
	now := time.Unix(1_800_000_000, 0)
	off.nowFn = func() time.Time { return now }
	ref.nowFn = off.nowFn
	steps := []struct {
		ok      bool
		advance time.Duration
	}{{false, 0}, {false, 0}, {false, 0}, {false, time.Second}, {false, 40 * time.Second}, {false, 0}, {true, 0}, {false, 0}}
	for i, s := range steps {
		now = now.Add(s.advance)
		off.record("alpha", s.ok)
		ref.record("alpha", s.ok)
		if got, want := off.healthy("alpha"), ref.healthy("alpha"); got != want {
			t.Fatalf("step %d: healthy = %v, want %v", i, got, want)
		}
	}
	if len(rec.reads) != 0 || len(rec.writes) != 0 {
		t.Fatalf("store touched with sharedState off: %d reads, %d writes", len(rec.reads), len(rec.writes))
	}
}
