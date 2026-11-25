package traefikllmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

type decisionFixture struct {
	g    *Gateway
	rt   *smartRouter
	clk  *decisionClock
	hits *int64
}

type decisionClock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *decisionClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *decisionClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const decisionOKBody = `{"model":"THX-02","answers":{"tier":{"type":"score","probabilities":{"0":0.1,"1":0.15,"2":0.75}}},"latency_ms":5}`

// newDecisionFixture builds a gateway whose "gx10" provider serves /v1/decide from handler.
func newDecisionFixture(t *testing.T, tierCount int, timeout string, handler http.HandlerFunc) *decisionFixture {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	tiers := make([]SmartRouterTier, tierCount)
	models := make([]string, tierCount)
	for i := range tiers {
		models[i] = "m" + string(rune('0'+i))
		tiers[i] = SmartRouterTier{Name: "t" + string(rune('a'+i)), MinScore: i * 250, Models: []string{models[i]}}
	}
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"gx10": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: models, PathBaseURLs: map[string]string{"/v1/decide": srv.URL}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	cfg.SmartRouters = map[string]*SmartRouterConfig{"auto": {
		Tiers:      tiers,
		Classifier: smartClassifierV2,
		Decision:   &SmartRouterDecisionConfig{Provider: "gx10", Timeout: timeout},
	}}
	_, g := buildRouterGateway(t, cfg)
	rt := g.registry.routers["auto"]
	if rt == nil || rt.decision == nil {
		t.Fatal("router or decision missing")
	}
	clk := &decisionClock{t: time.Unix(1_700_000_000, 0)}
	rt.decision.dec = newSmartDecider(clk.now)
	return &decisionFixture{g: g, rt: rt, clk: clk, hits: &hits}
}

func (f *decisionFixture) hitCount() int64 { return atomic.LoadInt64(f.hits) }

func okHandler(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, decisionOKBody)
}

func TestRouterDecisionRequestBody(t *testing.T) {
	for _, n := range []int{2, 4} {
		f := newDecisionFixture(t, n, "", okHandler)
		var got decisionRequest
		if err := json.Unmarshal(decisionRequestBody(f.rt, "hello"), &got); err != nil {
			t.Fatal(err)
		}
		names := make([]string, n)
		for i, tr := range f.rt.tiers {
			names[i] = tr.name
		}
		q := got.Questions["tier"]
		if q.Type != "score" || got.State != "hello" || !reflect.DeepEqual(q.Criteria, names) {
			t.Fatalf("n=%d body wrong: %+v", n, got)
		}
		want := "Pick the cheapest tier whose model can answer this request well. Tiers run from cheapest and weakest to strongest: " + strings.Join(names, ", ") + "."
		if q.Instructions != want {
			t.Fatalf("instructions %q", q.Instructions)
		}
	}
}

func TestRouterDecisionDecode(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		tiers  int
		rank   int
		prob   int
		wantOK bool
	}{
		{"argmax", `{"answers":{"tier":{"probabilities":{"0":0.1,"1":0.15,"2":0.75}}}}`, 3, 2, 750, true},
		{"rounds per mille", `{"answers":{"tier":{"probabilities":{"0":0.9235,"1":0.0765}}}}`, 2, 0, 924, true},
		{"missing answers", `{"model":"THX-02"}`, 3, 0, 0, false},
		{"key out of range", `{"answers":{"tier":{"probabilities":{"0":0.1,"3":0.9}}}}`, 3, 0, 0, false},
		{"non json", `not json`, 3, 0, 0, false},
		{"empty map", `{"answers":{"tier":{"probabilities":{}}}}`, 3, 0, 0, false},
		{"error envelope", `{"error":"bad"}`, 3, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rank, prob, ok := decodeDecisionAnswer([]byte(tt.body), tt.tiers)
			if ok != tt.wantOK || (ok && (rank != tt.rank || prob != tt.prob)) {
				t.Fatalf("got (%d,%d,%v)", rank, prob, ok)
			}
		})
	}
}

func TestRouterDecisionState(t *testing.T) {
	sys := strings.Repeat("s", 2000)
	ask := strings.Repeat("a", 10000)
	if got := decisionState(sys, ask); len(got) != 1024+2+8192 {
		t.Fatalf("len %d", len(got))
	}
	if got := decisionState("", ask); got != ask[:8192] {
		t.Fatalf("empty system len %d", len(got))
	}
	if got := decisionState(sys, ask); !strings.HasPrefix(got, sys[:1024]+"\n\n"+ask[:8]) {
		t.Fatal("head of system and ask not kept")
	}
	multi := strings.Repeat("é", 600)
	if got := decisionState(multi, multi); !utf8.ValidString(got) {
		t.Fatal("cut inside a rune")
	}
}

func TestRouterDecisionStatusErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"nope"}`)
		})
		if a := f.g.askDecisionModel(context.Background(), f.rt, "s"); a.outcome != decisionError {
			t.Fatalf("status %d: outcome %q", status, a.outcome)
		}
	}
}

func TestRouterDecisionBadBodyIsError(t *testing.T) {
	f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	if a := f.g.askDecisionModel(context.Background(), f.rt, "s"); a.outcome != decisionError {
		t.Fatalf("outcome %q", a.outcome)
	}
}

func TestRouterDecisionSendsAuthAndPath(t *testing.T) {
	var auth, path string
	f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		okHandler(w, r)
	})
	a := f.g.askDecisionModel(context.Background(), f.rt, "s")
	if a.outcome != "" || a.rank != 2 || a.prob != 750 {
		t.Fatalf("answer %+v", a)
	}
	if auth != "Bearer k" || path != "/v1/decide" {
		t.Fatalf("auth %q path %q", auth, path)
	}
}

func TestRouterDecisionTimeout(t *testing.T) {
	f := newDecisionFixture(t, 3, "50ms", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		okHandler(w, r)
	})
	start := time.Now()
	a := f.g.askDecisionModel(context.Background(), f.rt, "s")
	if a.outcome != decisionTimeout {
		t.Fatalf("outcome %q", a.outcome)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %s", el)
	}
}

func TestRouterDecisionClientCancelSkipsBreaker(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var slow atomic.Bool
	slow.Store(true)
	f := newDecisionFixture(t, 3, "2s", func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			started <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		okHandler(w, r)
	})
	t.Cleanup(func() { close(release) })
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			cancel()
		}()
		if a := f.g.askDecisionModel(ctx, f.rt, "c"+strconv.Itoa(i)); a.outcome != decisionError {
			t.Fatalf("cancel %d outcome %q", i, a.outcome)
		}
	}
	slow.Store(false)
	before := f.hitCount()
	if a := f.g.askDecisionModel(context.Background(), f.rt, "after"); a.outcome != "" || f.hitCount() != before+1 {
		t.Fatalf("breaker opened by client cancels: outcome %q", a.outcome)
	}
}

func TestRouterDecisionBreaker(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})
	ctx := context.Background()
	ask := func(i int) decisionAnswer { return f.g.askDecisionModel(ctx, f.rt, "s"+strconv.Itoa(i)) }
	for i := 0; i < 3; i++ {
		if a := ask(i); a.outcome != decisionError {
			t.Fatalf("call %d outcome %q", i, a.outcome)
		}
	}
	if a := ask(3); a.outcome != decisionBreaker || f.hitCount() != 3 {
		t.Fatalf("outcome %q hits %d", a.outcome, f.hitCount())
	}
	f.clk.advance(30 * time.Second)
	fail.Store(false)
	if a := ask(4); a.outcome != "" || f.hitCount() != 4 {
		t.Fatalf("after reopen: outcome %q hits %d", a.outcome, f.hitCount())
	}
	// A success resets the count: two failures then no trip.
	fail.Store(true)
	ask(5)
	ask(6)
	fail.Store(false)
	ask(7)
	fail.Store(true)
	ask(8)
	ask(9)
	if a := ask(10); a.outcome == decisionBreaker {
		t.Fatal("breaker tripped although a success reset the count")
	}
}

func TestRouterDecisionCache(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	ctx := context.Background()
	first := f.g.askDecisionModel(ctx, f.rt, "same")
	second := f.g.askDecisionModel(ctx, f.rt, "same")
	if first.outcome != "" || second.outcome != decisionCache || second.rank != first.rank || second.prob != first.prob || f.hitCount() != 1 {
		t.Fatalf("first %+v second %+v hits %d", first, second, f.hitCount())
	}
	f.clk.advance(10*time.Minute + time.Second)
	if a := f.g.askDecisionModel(ctx, f.rt, "same"); a.outcome != "" || f.hitCount() != 2 {
		t.Fatalf("after ttl: %+v hits %d", a, f.hitCount())
	}
	for i := 0; i < 1100; i++ {
		f.g.askDecisionModel(ctx, f.rt, "state-"+strconv.Itoa(i))
	}
	if n := f.rt.decision.dec.cacheLen(); n > 1024 {
		t.Fatalf("cache size %d", n)
	}
}

func TestRouterDecisionCacheServedWhileBreakerOpen(t *testing.T) {
	var fail atomic.Bool
	f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		okHandler(w, r)
	})
	ctx := context.Background()
	f.g.askDecisionModel(ctx, f.rt, "warm")
	fail.Store(true)
	for i := 0; i < 3; i++ {
		f.g.askDecisionModel(ctx, f.rt, "cold"+strconv.Itoa(i))
	}
	if a := f.g.askDecisionModel(ctx, f.rt, "warm"); a.outcome != decisionCache {
		t.Fatalf("outcome %q", a.outcome)
	}
}

func TestRouterDecisionContextCancel(t *testing.T) {
	f := newDecisionFixture(t, 3, "", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan decisionAnswer, 1)
	go func() { done <- f.g.askDecisionModel(ctx, f.rt, "s") }()
	select {
	case a := <-done:
		if a.outcome != decisionTimeout && a.outcome != decisionError {
			t.Fatalf("outcome %q", a.outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("hung")
	}
}

func TestRouterDecisionConcurrent(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.g.askDecisionModel(context.Background(), f.rt, "s"+strconv.Itoa(i%4))
		}(i)
	}
	wg.Wait()
}

func TestRouterDecisionOutcomeNames(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range routerDecisionOutcomes {
		if o == "" || seen[o] {
			t.Fatalf("bad outcome %q", o)
		}
		seen[o] = true
	}
	if len(seen) != 7 {
		t.Fatalf("got %d outcomes", len(seen))
	}
}

func TestRouterDecisionIgnoresRequestRecorders(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	var attempts, latencies int64
	ctx := withAttemptRecorder(context.Background(), func(*http.Response, error) { atomic.AddInt64(&attempts, 1) })
	ctx = withLatencyRecorder(ctx, func(latencySample) { atomic.AddInt64(&latencies, 1) })
	if a := f.g.askDecisionModel(ctx, f.rt, "s"); a.outcome != "" {
		t.Fatalf("outcome %q", a.outcome)
	}
	if n := atomic.LoadInt64(&attempts); n != 0 {
		t.Fatalf("attempt recorder called %d times", n)
	}
	if n := atomic.LoadInt64(&latencies); n != 0 {
		t.Fatalf("latency recorder called %d times", n)
	}
}

func TestRouterDecisionNonOpenAIProviderIsError(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	f.g.adapters["gx10"] = &fakeAdapter{}
	if a := f.g.askDecisionModel(context.Background(), f.rt, "s"); a.outcome != decisionError {
		t.Fatalf("outcome %q", a.outcome)
	}
}

func TestRouterDecisionParentCancelIsError(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a := f.g.askDecisionModel(ctx, f.rt, "s"); a.outcome != decisionError {
		t.Fatalf("outcome %q", a.outcome)
	}
}
