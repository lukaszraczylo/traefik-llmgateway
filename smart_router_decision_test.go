package traefikllmgateway

import (
	"strings"
	"testing"
	"time"
)

func decisionInt(v int) *int { return &v }

func TestCompileSmartDecisionDefaults(t *testing.T) {
	d, err := compileSmartDecision("r", &SmartRouterDecisionConfig{Provider: "gx10"}, smartClassifierV2)
	if err != nil {
		t.Fatal(err)
	}
	if d.provider != "gx10" || d.path != "/v1/decide" || d.live || d.timeout != 500*time.Millisecond || d.minProb != 700 || d.band != 100 {
		t.Fatalf("defaults wrong: %+v", d)
	}
}

func TestCompileSmartDecisionLiveAndOverrides(t *testing.T) {
	d, err := compileSmartDecision("r", &SmartRouterDecisionConfig{
		Provider: "gx10", Path: "/x", Mode: "live", Timeout: "2s", MinProb: decisionInt(1000), Band: decisionInt(1),
	}, smartClassifierV2)
	if err != nil {
		t.Fatal(err)
	}
	if !d.live || d.path != "/x" || d.timeout != 2*time.Second || d.minProb != 1000 || d.band != 1 {
		t.Fatalf("overrides wrong: %+v", d)
	}
	if d, err = compileSmartDecision("r", &SmartRouterDecisionConfig{Provider: "g", Timeout: "50ms"}, smartClassifierV2); err != nil || d.timeout != 50*time.Millisecond {
		t.Fatalf("min timeout rejected: %v %+v", err, d)
	}
}

func TestCompileSmartDecisionErrors(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *SmartRouterDecisionConfig
		classifier string
	}{
		{"mode on", &SmartRouterDecisionConfig{Provider: "p", Mode: "on"}, "v2"},
		{"timeout too low", &SmartRouterDecisionConfig{Provider: "p", Timeout: "10ms"}, "v2"},
		{"timeout too high", &SmartRouterDecisionConfig{Provider: "p", Timeout: "3s"}, "v2"},
		{"timeout garbage", &SmartRouterDecisionConfig{Provider: "p", Timeout: "abc"}, "v2"},
		{"minProb 0", &SmartRouterDecisionConfig{Provider: "p", MinProb: decisionInt(0)}, "v2"},
		{"minProb 1001", &SmartRouterDecisionConfig{Provider: "p", MinProb: decisionInt(1001)}, "v2"},
		{"band 0", &SmartRouterDecisionConfig{Provider: "p", Band: decisionInt(0)}, "v2"},
		{"band 1001", &SmartRouterDecisionConfig{Provider: "p", Band: decisionInt(1001)}, "v2"},
		{"empty provider", &SmartRouterDecisionConfig{}, "v2"},
		{"classifier v1", &SmartRouterDecisionConfig{Provider: "p"}, "v1"},
		{"classifier empty", &SmartRouterDecisionConfig{Provider: "p"}, ""},
		{"path without slash", &SmartRouterDecisionConfig{Provider: "p", Path: "v1/decide"}, "v2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := compileSmartDecision("myrouter", tc.cfg, tc.classifier)
			if err == nil || d != nil {
				t.Fatalf("want error, got %+v %v", d, err)
			}
			if !strings.Contains(err.Error(), `router "myrouter"`) {
				t.Fatalf("error must name router: %v", err)
			}
		})
	}
}

func TestCompileSmartDecisionOff(t *testing.T) {
	if d, err := compileSmartDecision("r", nil, smartClassifierV1); d != nil || err != nil {
		t.Fatalf("nil cfg: %+v %v", d, err)
	}
	if d, err := compileSmartDecision("r", &SmartRouterDecisionConfig{Mode: "off"}, smartClassifierV1); d != nil || err != nil {
		t.Fatalf("off: %+v %v", d, err)
	}
}

func decisionRouters(provider string) map[string]*smartRouter {
	return map[string]*smartRouter{"r": {name: "r", decision: &smartDecisionCfg{provider: provider, path: "/v1/decide"}}}
}

func TestValidateRouterDecisions(t *testing.T) {
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"gx10":  {PathBaseURLs: map[string]string{"/v1/decide": "http://x"}},
		"bare":  {},
		"decid": {PathBaseURLs: map[string]string{"/v1/decider": "http://x"}},
		"wide":  {PathBaseURLs: map[string]string{"/v1": "http://x"}},
	}}
	if err := validateRouterDecisions(decisionRouters("gx10"), cfg); err != nil {
		t.Fatalf("matching key: %v", err)
	}
	if err := validateRouterDecisions(decisionRouters("wide"), cfg); err != nil {
		t.Fatalf("whole-segment prefix: %v", err)
	}
	if err := validateRouterDecisions(map[string]*smartRouter{"r": {name: "r"}}, cfg); err != nil {
		t.Fatalf("no decision: %v", err)
	}
	err := validateRouterDecisions(decisionRouters("nope"), cfg)
	if err == nil || !strings.Contains(err.Error(), `"r"`) || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("unknown provider: %v", err)
	}
	for _, p := range []string{"bare", "decid"} {
		if err := validateRouterDecisions(decisionRouters(p), cfg); err == nil || !strings.Contains(err.Error(), p) {
			t.Fatalf("provider %s without matching prefix: %v", p, err)
		}
	}
}

func TestLongestPathPrefix(t *testing.T) {
	m := map[string]string{"/v1": "a", "/v1/decide": "b"}
	if k, ok := longestPathPrefix("/v1/decide/x", m); !ok || k != "/v1/decide" {
		t.Fatalf("got %q %v", k, ok)
	}
	if _, ok := longestPathPrefix("/v2", m); ok {
		t.Fatal("unexpected match")
	}
}

func TestCompileSmartRouterDecisionWiring(t *testing.T) {
	cfg := &SmartRouterConfig{
		Classifier: "v2",
		Tiers:      []SmartRouterTier{{Name: "a", Models: []string{"p/a"}}},
		Decision:   &SmartRouterDecisionConfig{Provider: "gx10", Mode: "live"},
	}
	rt, err := compileSmartRouter("r", cfg, nil)
	if err != nil || rt.decision == nil || !rt.decision.live {
		t.Fatalf("decision not compiled: %v %+v", err, rt)
	}
	cfg.Classifier = ""
	if _, err = compileSmartRouter("r", cfg, nil); err == nil {
		t.Fatal("v1 router with decision must fail")
	}
	cfg.Decision = nil
	if rt, err = compileSmartRouter("r", cfg, nil); err != nil || rt.decision != nil {
		t.Fatalf("absent decision must stay off: %v", err)
	}
}
