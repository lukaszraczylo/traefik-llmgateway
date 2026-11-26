package traefikllmgateway

import (
	"testing"
)

// shareLimiterStore points a pod's limiter at the shared store and makes its async writes synchronous.
func shareLimiterStore(g *Gateway, store counterStore) {
	g.limiter.store = store
	g.limiter.spawn = func(f func()) { f() }
}

func TestSharedCalibratorAcrossGateways(t *testing.T) {
	a, b, store, _, _ := sharedGatewayPair(t, nil)
	shareLimiterStore(a, store)
	shareLimiterStore(b, store)
	const bytes, tokens = 1000, 400 // 2.5 bytes per token

	if got := b.tokenCalib.promptTokens("alpha", "a1", bytes); got != 250 {
		t.Fatalf("uncalibrated estimate = %d, want 250", got)
	}
	for i := 0; i < smartCalibMinSamples-1; i++ {
		a.tokenCalib.observe("alpha", "a1", bytes, tokens)
	}
	b.refreshRouterLearning(b.limiter.now())
	if got := b.tokenCalib.promptTokens("alpha", "a1", bytes); got != 250 {
		t.Fatalf("estimate below the minimum sample count = %d, want 250", got)
	}
	a.tokenCalib.observe("alpha", "a1", bytes, tokens)
	if got := b.tokenCalib.promptTokens("alpha", "a1", bytes); got != 250 {
		t.Fatalf("estimate changed before the learn refresh = %d, want 250", got)
	}
	b.refreshRouterLearning(b.limiter.now())
	if got := b.tokenCalib.promptTokens("alpha", "a1", bytes); got != 400 {
		t.Fatalf("shared estimate = %d, want 400", got)
	}
	if got := b.tokenCalib.promptTokens("beta", "b1", bytes); got != 250 {
		t.Fatalf("an unobserved model changed to %d, want 250", got)
	}
}

func TestSharedCalibratorSkipsUnusableSamples(t *testing.T) {
	a, _, store, _, _ := sharedGatewayPair(t, nil)
	shareLimiterStore(a, store)
	a.tokenCalib.observe("alpha", "a1", smartCalibMinBytes-1, 10)
	a.tokenCalib.observe("alpha", "a1", 1000, 0)
	if len(store.writes) != 0 {
		t.Fatalf("unusable samples wrote %d counters, want 0", len(store.writes))
	}
}

func TestSharedCalibratorStartsRouterLearning(t *testing.T) {
	a, _, _, _, _ := sharedGatewayPair(t, nil)
	off, _ := sharedOffGateway(t)
	for name, gw := range map[string]*Gateway{"shared": a, "off": off} {
		rt := gw.registry.routers[routerName]
		if rt.learns() {
			t.Fatalf("%s: router learns without calibration or bandit, fixture is wrong", name)
		}
		gw.touchRouterLearning(rt)
		t.Cleanup(gw.closeRouterLearning)
	}
	if !a.routerLearn.running {
		t.Error("shared: router learning loop did not start for a calibrator-only router")
	}
	if off.routerLearn.running {
		t.Error("off: router learning loop started with sharedState off")
	}
}

func TestSharedStateOffIdenticalCalibrator(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	rec, _ := sharedTestEnv()
	gw.limiter.store = rec
	gw.limiter.spawn = func(f func()) { f() }
	ref := newSmartTokenCalibrator()
	for i := 0; i < 80; i++ {
		gw.tokenCalib.observe("alpha", "a1", 1000+int64(i), 400)
		ref.observe("alpha", "a1", 1000+int64(i), 400)
		if got, want := gw.tokenCalib.promptTokens("alpha", "a1", 5000), ref.promptTokens("alpha", "a1", 5000); got != want {
			t.Fatalf("observation %d: estimate %d, want %d", i, got, want)
		}
	}
	gw.refreshRouterLearning(gw.limiter.now())
	if len(rec.reads) != 0 || len(rec.writes) != 0 {
		t.Fatalf("store touched with sharedState off: %d reads, %d writes", len(rec.reads), len(rec.writes))
	}
}
