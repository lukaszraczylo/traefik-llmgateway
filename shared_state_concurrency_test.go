package traefikllmgateway

import (
	"net/http"
	"testing"
	"time"
)

func sharedConcurrencyCfg(c *Config) {
	c.ConcurrencyAutodetect = true
	c.MaxConcurrency = 1
	rc := c.SmartRouters[routerName]
	rc.KeywordRules = nil
	rc.SaturationDowngrade = true
	rc.Tiers = []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1"}},
		{Name: "t1", MinScore: 100, Models: []string{"b1"}},
		{Name: "t2", MinScore: 400, Models: []string{"c1"}},
	}
}

func TestSharedInflightAcrossGateways(t *testing.T) {
	a, b, _, _, _ := sharedGatewayPair(t, sharedConcurrencyCfg)
	rt := b.registry.routers[routerName]

	if got := planOf(t, b, rt, userReq(downgradeAsk)); got.downgradedFrom != "" {
		t.Fatalf("B downgraded with nothing in flight: %q", got.downgradedFrom)
	}
	end := a.concurrency.begin("beta")
	a.sharedState.refresh()
	b.sharedState.refresh()
	if in, lim := b.concurrency.load("beta"); in != 1 || lim != 1 {
		t.Fatalf("B load = (%d, %d), want (1, 1)", in, lim)
	}
	plan := planOf(t, b, rt, userReq(downgradeAsk))
	if plan.downgradedFrom != "t1" || plan.startTier != "t0" {
		t.Fatalf("B plan = start %q from %q, want t0 downgraded from t1", plan.startTier, plan.downgradedFrom)
	}
	end()
	end()
	a.sharedState.refresh()
	b.sharedState.refresh()
	if in, _ := b.concurrency.load("beta"); in != 0 {
		t.Fatalf("B in-flight = %d after A ended, want 0", in)
	}
}

func TestSharedLearnedLimitAcrossGateways(t *testing.T) {
	a, b, _, _, _ := sharedGatewayPair(t, func(c *Config) { c.ConcurrencyAutodetect = true })
	var ends []func()
	for i := 0; i < 4; i++ {
		ends = append(ends, a.concurrency.begin("alpha"))
	}
	a.sharedState.refresh()
	a.concurrency.observe("alpha", &http.Response{StatusCode: http.StatusTooManyRequests}, nil)
	b.sharedState.refresh()
	if got := b.concurrency.limit("alpha"); got != 2 {
		t.Fatalf("B limit = %d, want 2 (half of the fleet in-flight)", got)
	}
	for _, e := range ends {
		e()
	}
}

func TestSharedStateOffIdenticalConcurrency(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	gw.concurrency = newConcurrencyLearner(0)
	gw.wireSharedState(&Config{})
	rec, _ := sharedTestEnv()
	gw.limiter.store = rec
	ref := newConcurrencyLearner(0)
	var gwEnds, refEnds []func()
	for i := 0; i < 5; i++ {
		gwEnds = append(gwEnds, gw.concurrency.begin("alpha"))
		refEnds = append(refEnds, ref.begin("alpha"))
	}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests}
	ok := &http.Response{StatusCode: http.StatusOK}
	for _, r := range []*http.Response{resp, ok, ok, ok, ok, ok} {
		gw.concurrency.observe("alpha", r, nil)
		ref.observe("alpha", r, nil)
		gi, gl := gw.concurrency.load("alpha")
		ri, rl := ref.load("alpha")
		if gi != ri || gl != rl || gw.concurrency.limit("alpha") != ref.limit("alpha") {
			t.Fatalf("diverged: gw (%d,%d) ref (%d,%d)", gi, gl, ri, rl)
		}
	}
	for i := range gwEnds {
		gwEnds[i]()
		refEnds[i]()
	}
	if len(rec.reads) != 0 || len(rec.writes) != 0 {
		t.Fatalf("store touched with sharedState off: %d reads, %d writes", len(rec.reads), len(rec.writes))
	}
}

func TestMaxProviderTimeout(t *testing.T) {
	cases := []struct {
		cfg  *Config
		name string
		want time.Duration
	}{
		{&Config{}, "default", defaultRequestTimeout},
		{&Config{RequestTimeout: "10s"}, "global", 10 * time.Second},
		{&Config{RequestTimeout: "10s", Providers: map[string]*ProviderConfig{"a": {RequestTimeout: "20m"}, "b": nil}}, "provider longer", 20 * time.Minute},
		{&Config{RequestTimeout: "10m", Providers: map[string]*ProviderConfig{"a": {RequestTimeout: "1m"}}}, "provider shorter", 10 * time.Minute},
		{&Config{RequestTimeout: "x"}, "bad global falls back", defaultRequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxProviderTimeout(tc.cfg); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
