// This file adds the smart router v2 probes: calibration (live mode), adaptive
// selection and the saturation downgrade, interpreted under Yaegi.
//
// They build their OWN Gateway (own config, own upstreams) so the learning
// counters start at zero and the shared harness's traffic cannot skew them.
// Calibration offsets and bandit arms refresh on the router learner's 60 s
// ticker (routerLearnInterval, not configurable), so the probe seeds both from
// real traffic, waits for one tick, and then asserts the routing change.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/traefik/yaegi/interp"
)

const (
	rv2UserKey  = "sk-rv2-user"  // #nosec G101 -- test fixture literal, not a real credential
	rv2AdminKey = "sk-rv2-admin" // #nosec G101 -- test fixture literal, not a real credential

	rv2Calib = "rv2-calib"
	rv2Adapt = "rv2-adapt"
	rv2Sat   = "rv2-sat"

	// rv2Ask scores 151 under the default v1 weights: below the rv2-calib t1 minScore (200)
	// until calibration lowers that threshold by 100, and 51 points above the rv2-sat t1 minScore.
	rv2Ask = "write a function in python that reverses a linked list and explain the complexity"

	rv2Seeds        = 60
	rv2CalibDelta   = 100
	rv2AdaptChecks  = 25
	rv2LearnWait    = 100 * time.Second
	rv2PollEvery    = time.Second
	rv2HoldArrival  = 5 * time.Second
	rv2GoodBody     = `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	rv2EmptyBody    = `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":0}}`
	rv2HoldProvider = "rv2-hold"
)

// rv2Upstream answers every request with body.
func rv2Upstream(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// rv2Hold is an upstream that parks requests until released, so a provider shows in-flight load.
type rv2Hold struct {
	srv     *httptest.Server
	arrived chan struct{}
	release chan struct{}
	block   atomic.Bool
}

func newRV2Hold() *rv2Hold {
	h := &rv2Hold{arrived: make(chan struct{}, 4), release: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.block.Load() {
			h.arrived <- struct{}{}
			select {
			case <-h.release:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rv2GoodBody))
	}))
	return h
}

func rv2Post(handler http.Handler, model, text string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+model+`","messages":[{"role":"user","content":"`+text+`"}]}`,
	))
	req.Header.Set("Authorization", "Bearer "+rv2UserKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func rv2Metrics(handler http.Handler) string {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+rv2AdminKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Body.String()
}

// rv2Sample returns the value of the first metrics line that starts with prefix, and whether one exists.
func rv2Sample(body, prefix string) (int, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if err == nil {
			return v, true
		}
	}
	return 0, false
}

func newRouterV2Handler(i *interp.Interpreter, pkgName, cfgJSON string) (http.Handler, error) {
	cfgVal, err := i.Eval(pkgName + ".CreateConfig()")
	if err != nil {
		return nil, fmt.Errorf("router v2 probe: call %s.CreateConfig() under yaegi: %w", pkgName, err)
	}
	if !cfgVal.CanInterface() {
		return nil, fmt.Errorf("router v2 probe: %s.CreateConfig() result cannot be used via reflect.Value.Interface", pkgName)
	}
	if err = json.Unmarshal([]byte(cfgJSON), cfgVal.Interface()); err != nil {
		return nil, fmt.Errorf("router v2 probe: decode its own config into the interpreted Config: %w", err)
	}
	newVal, err := i.Eval(pkgName + ".New")
	if err != nil {
		return nil, fmt.Errorf("router v2 probe: resolve %s.New under yaegi: %w", pkgName, err)
	}
	if newVal.Kind() != reflect.Func {
		return nil, fmt.Errorf("router v2 probe: %s.New is not a function (kind %s)", pkgName, newVal.Kind())
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	results := newVal.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(next),
		cfgVal,
		reflect.ValueOf("llmgw-check-router-v2"),
	})
	if len(results) != 2 {
		return nil, fmt.Errorf("router v2 probe: %s.New returned %d values, want 2 (http.Handler, error)", pkgName, len(results))
	}
	if errVal := results[1]; !errVal.IsNil() {
		return nil, fmt.Errorf("router v2 probe: %s.New returned an error: %v", pkgName, errVal.Interface())
	}
	handler, ok := results[0].Interface().(http.Handler)
	if !ok || handler == nil {
		return nil, fmt.Errorf("router v2 probe: %s.New's returned value is not a usable http.Handler", pkgName)
	}
	return handler, nil
}

func rv2ProviderJSON(name, url string) string {
	return `"` + name + `":{"type":"openai","baseUrl":"` + url + `","apiKey":"sk-up","models":["m"]}`
}

func rv2Config(urls map[string]string) string {
	providers := make([]string, 0, len(urls))
	for name, url := range urls {
		providers = append(providers, rv2ProviderJSON(name, url))
	}
	return `{"providers":{` + strings.Join(providers, ",") + `},` +
		`"groups":{"rv2g":{}},` +
		`"users":{"inline":[{"name":"rv2-user","group":"rv2g","apiKey":"` + rv2UserKey + `"},{"name":"rv2-admin","group":"rv2g","apiKey":"` + rv2AdminKey + `","admin":true}]},` +
		`"admin":{"enabled":true},"metrics":{"enabled":true},` +
		`"concurrencyAutodetect":true,"maxConcurrency":1,` +
		`"smartRouters":{` +
		`"` + rv2Calib + `":{"calibration":{"mode":"live","step":` + strconv.Itoa(rv2CalibDelta) + `,"minSamples":50},"tiers":[{"name":"t0","minScore":0,"models":["rv2-low/m"]},{"name":"t1","minScore":200,"models":["rv2-hi/m"]}]},` +
		`"` + rv2Adapt + `":{"selection":"adaptive","adaptive":{"explorePct":0},"tiers":[{"name":"t0","minScore":0,"models":["rv2-good/m","rv2-flaky/m"]}]},` +
		`"` + rv2Sat + `":{"saturationDowngrade":true,"tiers":[{"name":"t0","minScore":0,"models":["rv2-fast/m"]},{"name":"t1","minScore":100,"models":["` + rv2HoldProvider + `/m"]}]}}}`
}

// exerciseRouterV2 runs the calibration, adaptive-selection and saturation-downgrade probes against one
// dedicated interpreted Gateway.
func exerciseRouterV2(i *interp.Interpreter, pkgName string) error {
	good, empty := rv2Upstream(rv2GoodBody), rv2Upstream(rv2EmptyBody)
	hold := newRV2Hold()
	defer good.Close()
	defer empty.Close()
	defer hold.srv.Close()

	handler, err := newRouterV2Handler(i, pkgName, rv2Config(map[string]string{
		"rv2-low": empty.URL, "rv2-hi": good.URL, "rv2-good": good.URL, "rv2-flaky": empty.URL,
		"rv2-fast": good.URL, rv2HoldProvider: hold.srv.URL,
	}))
	if err != nil {
		return err
	}
	if err := exerciseRouterSaturation(handler, hold); err != nil {
		return err
	}
	return exerciseRouterLearning(handler)
}

// exerciseRouterSaturation parks one request on rv2-hold's only slot, so tier t1 of rv2-sat is saturated:
// a score-routed request starts one tier down, and returns to t1 once the slot frees.
func exerciseRouterSaturation(handler http.Handler, hold *rv2Hold) error {
	rec := rv2Post(handler, rv2Sat, rv2Ask)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Llmgw-Router-Tier") != "t1" || rec.Header().Get("X-Llmgw-Router-Downgraded") != "" {
		return fmt.Errorf("router %q unsaturated: status=%d tier=%q downgraded=%q, want 200 t1 and no downgrade header", rv2Sat, rec.Code, rec.Header().Get("X-Llmgw-Router-Tier"), rec.Header().Get("X-Llmgw-Router-Downgraded"))
	}

	hold.block.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rv2Post(handler, rv2HoldProvider+"/m", "hold")
	}()
	select {
	case <-hold.arrived:
	case <-time.After(rv2HoldArrival):
		close(hold.release)
		return fmt.Errorf("router %q: the parked request never reached the %s upstream", rv2Sat, rv2HoldProvider)
	}

	rec = rv2Post(handler, rv2Sat, rv2Ask)
	downgraded := rec.Code == http.StatusOK && rec.Header().Get("X-Llmgw-Router-Tier") == "t0" && rec.Header().Get("X-Llmgw-Router-Downgraded") == "t1"
	body := rv2Metrics(handler)
	close(hold.release)
	<-done
	hold.block.Store(false)
	if !downgraded {
		return fmt.Errorf("router %q saturated: status=%d tier=%q downgraded=%q, want 200 t0 downgraded from t1", rv2Sat, rec.Code, rec.Header().Get("X-Llmgw-Router-Tier"), rec.Header().Get("X-Llmgw-Router-Downgraded"))
	}
	want := `llmgateway_router_requests_total{router="` + rv2Sat + `",tier="t0",reason="saturated"} `
	if n, ok := rv2Sample(body, want); !ok || n != 1 {
		return fmt.Errorf("GET /metrics: %q = %d (found %v), want 1", want, n, ok)
	}

	rec = rv2Post(handler, rv2Sat, rv2Ask)
	if rec.Header().Get("X-Llmgw-Router-Tier") != "t1" || rec.Header().Get("X-Llmgw-Router-Downgraded") != "" {
		return fmt.Errorf("router %q after the slot freed: tier=%q downgraded=%q, want t1 and no downgrade header", rv2Sat, rec.Header().Get("X-Llmgw-Router-Tier"), rec.Header().Get("X-Llmgw-Router-Downgraded"))
	}
	fmt.Println("yaegi-check: saturated tier t1 downgraded a score-routed request to t0 with the downgrade header and counter, and routed back to t1 once the slot freed under the interpreter")
	return nil
}

// exerciseRouterLearning seeds calibration counters (tier t0 answers empty, which counts as bad) and bandit
// arms (one good and one empty-answer candidate) with real traffic, waits for the learner's tick, and checks
// that the calibrated threshold now sends the ask to t1 and that adaptive selection always leads with the
// good candidate.
func exerciseRouterLearning(handler http.Handler) error {
	for n := 0; n < rv2Seeds; n++ {
		if rec := rv2Post(handler, rv2Calib, rv2Ask); rec.Code != http.StatusOK || rec.Header().Get("X-Llmgw-Router-Tier") != "t0" {
			return fmt.Errorf("router %q seed %d: status=%d tier=%q, want 200 t0", rv2Calib, n, rec.Code, rec.Header().Get("X-Llmgw-Router-Tier"))
		}
		if rec := rv2Post(handler, rv2Adapt, "hi"); rec.Code != http.StatusOK {
			return fmt.Errorf("router %q seed %d: status=%d body=%s", rv2Adapt, n, rec.Code, rec.Body.String())
		}
	}

	gauge := `llmgateway_router_calibration_delta{router="` + rv2Calib + `",tier="t1",mode="live"} `
	deadline := time.Now().Add(rv2LearnWait)
	var delta int
	var ok bool
	for time.Now().Before(deadline) {
		if delta, ok = rv2Sample(rv2Metrics(handler), gauge); ok && delta != 0 {
			break
		}
		time.Sleep(rv2PollEvery)
	}
	if delta != rv2CalibDelta {
		return fmt.Errorf("GET /metrics: %q = %d (found %v) after %s, want %d from %d bad answers", gauge, delta, ok, rv2LearnWait, rv2CalibDelta, rv2Seeds)
	}
	if rec := rv2Post(handler, rv2Calib, rv2Ask); rec.Header().Get("X-Llmgw-Router-Tier") != "t1" {
		return fmt.Errorf("router %q after calibration: tier=%q, want t1 (threshold 200 lowered by %d to 100, ask scores 151)", rv2Calib, rec.Header().Get("X-Llmgw-Router-Tier"), rv2CalibDelta)
	}

	for n := 0; n < rv2AdaptChecks; n++ {
		rec := rv2Post(handler, rv2Adapt, "hi")
		if got := rec.Header().Get("X-Llmgw-Routed-Model"); rec.Code != http.StatusOK || got != "rv2-good/m" {
			return fmt.Errorf("router %q adaptive check %d: status=%d routed=%q, want 200 rv2-good/m first every time", rv2Adapt, n, rec.Code, got)
		}
	}
	fmt.Printf("yaegi-check: live calibration lowered the t1 threshold by %d after %d bad answers and moved the ask to t1; adaptive selection led with the good candidate %d of %d times under the interpreter\n", rv2CalibDelta, rv2Seeds, rv2AdaptChecks, rv2AdaptChecks)
	return nil
}
