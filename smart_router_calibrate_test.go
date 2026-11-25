package traefikllmgateway

import (
	"strings"
	"sync"
	"testing"
)

func feedCalibrator(c *smartTokenCalibrator, provider, model string, n int, bytes, tokens int64) {
	for i := 0; i < n; i++ {
		c.observe(provider, model, bytes, tokens)
	}
}

func TestSmartTokenCalibrator_FallbackBelowMinSamples(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	feedCalibrator(c, "openai", "m", smartCalibMinSamples-1, 4000, 2000)
	if got := c.promptTokens("openai", "m", 4000); got != 1000 {
		t.Errorf("tokens with %d samples = %d, want the bytes/4 fallback 1000", smartCalibMinSamples-1, got)
	}
	if got := c.promptTokens("openai", "unseen", 4001); got != 1001 {
		t.Errorf("unseen model tokens = %d, want ceil(4001/4) = 1001", got)
	}
}

func TestSmartTokenCalibrator_UsesEWMAAfterMinSamples(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	feedCalibrator(c, "openai", "m", smartCalibMinSamples, 3000, 1000)
	if got := c.promptTokens("openai", "m", 3000); got != 1000 {
		t.Errorf("tokens at a steady 3 bytes/token = %d, want 1000", got)
	}
	feedCalibrator(c, "openai", "m", 200, 5000, 1000)
	got := c.promptTokens("openai", "m", 5000)
	if got < 990 || got > 1010 {
		t.Errorf("tokens after the ratio drifts to 5 = %d, want about 1000", got)
	}
	if other := c.promptTokens("anthropic", "m", 3000); other != 750 {
		t.Errorf("a different provider must not share the ratio, got %d, want 750", other)
	}
}

func TestSmartTokenCalibrator_ClampsRatio(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	feedCalibrator(c, "p", "dense", smartCalibMinSamples, 1000, 1000)
	if got := c.promptTokens("p", "dense", 1500); got != 1000 {
		t.Errorf("dense ratio tokens = %d, want clamp at 1.5 bytes/token = 1000", got)
	}
	feedCalibrator(c, "p", "sparse", smartCalibMinSamples, 100000, 1000)
	if got := c.promptTokens("p", "sparse", 6000); got != 1000 {
		t.Errorf("sparse ratio tokens = %d, want clamp at 6 bytes/token = 1000", got)
	}
}

func TestSmartTokenCalibrator_IgnoresUnusableSamples(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	feedCalibrator(c, "p", "m", smartCalibMinSamples*2, 10, 5)
	feedCalibrator(c, "p", "m", smartCalibMinSamples*2, 4000, 0)
	feedCalibrator(c, "p", "m", smartCalibMinSamples*2, 4000, -3)
	if got := c.promptTokens("p", "m", 4000); got != 1000 {
		t.Errorf("tokens = %d, want the fallback 1000 when every sample is unusable", got)
	}
}

func TestSmartTokenCalibrator_BoundedMap(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	for i := 0; i < smartCalibMaxModels+50; i++ {
		c.observe("p", string(rune('a'+i%26))+string(rune('A'+i/26%26))+string(rune('0'+i/676)), 4000, 1000)
	}
	if n := c.size(); n > smartCalibMaxModels {
		t.Errorf("tracked models = %d, want <= %d", n, smartCalibMaxModels)
	}
}

func TestSmartTokenCalibrator_NilIsSafe(t *testing.T) {
	t.Parallel()
	var c *smartTokenCalibrator
	c.observe("p", "m", 4000, 1000)
	if got := c.promptTokens("p", "m", 4000); got != 1000 {
		t.Errorf("nil calibrator tokens = %d, want fallback 1000", got)
	}
}

func TestSmartTokenCalibrator_RaceSafe(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c.observe("p", "m", 3000, 1000)
				_ = c.promptTokens("p", "m", 3000)
			}
		}()
	}
	wg.Wait()
	if got := c.promptTokens("p", "m", 3000); got != 1000 {
		t.Errorf("tokens = %d, want 1000", got)
	}
}

func TestContextFits_UsesCalibratedRatioPerCandidate(t *testing.T) {
	t.Parallel()
	c := newSmartTokenCalibrator()
	feedCalibrator(c, "p", "dense", smartCalibMinSamples, 2000, 1000)
	facts := routerRequestFacts{promptBytes: 40000}
	window := int64(15000)
	if !contextFits(facts, c.promptTokens("p", "plain", facts.promptBytes), window) {
		t.Error("uncalibrated model: 10000 tokens must fit 15000*90%")
	}
	if contextFits(facts, c.promptTokens("p", "dense", facts.promptBytes), window) {
		t.Error("calibrated at 2 bytes/token: 20000 tokens must not fit")
	}
}

func TestPlanSmartRouter_ContextCheckUsesCalibratedRatio(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 1000}, "c1": {ContextTokens: 200000}}
		twoTiers(c, "a1")
	})
	req := userReq(strings.Repeat("a", 4*900))
	if got := firstPlanModel(t, gw, rt, req, cacheEndpointChat); got != "alpha/a1" {
		t.Fatalf("uncalibrated: first = %q, want alpha/a1", got)
	}
	feedCalibrator(gw.tokenCalib, "alpha", "a1", smartCalibMinSamples, 2000, 1000)
	if got := firstPlanModel(t, gw, rt, req, cacheEndpointChat); got != "gamma/c1" {
		t.Errorf("calibrated at 2 bytes/token: first = %q, want gamma/c1", got)
	}
}

func TestRouter_ServedRequestFeedsCalibrator(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","model":"a1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":2}}`
	a := newRouterBackend(200, body)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	rec := routerChat(t, h, routerName, strings.Repeat("plain words ", 400), nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if n := gw.tokenCalib.size(); n != 1 {
		t.Fatalf("calibrator tracked %d models after one served request, want 1", n)
	}
	gw.tokenCalib.mu.Lock()
	st := gw.tokenCalib.stats[smartCalibKey{provider: "alpha", model: "a1"}]
	gw.tokenCalib.mu.Unlock()
	if st == nil || st.samples != 1 || st.ratio < 4 || st.ratio > 5 {
		t.Errorf("stat = %+v, want one sample near 4.8 bytes/token", st)
	}

	routerChat(t, h, routerName, "short", nil)
	if gw.tokenCalib.stats[smartCalibKey{provider: "alpha", model: "a1"}].samples != 1 {
		t.Error("a prompt under the minimum size must not add a sample")
	}
}
