package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

const (
	reqUserKey     = "bench-user-key"
	reqBodyChat    = `{"model":"bench-model","messages":[{"role":"user","content":"Say hello to the benchmark."}],"max_tokens":64}`
	reqBodyStream  = `{"model":"bench-model","messages":[{"role":"user","content":"Say hello to the benchmark."}],"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`
	reqChatJSON    = `{"id":"chatcmpl-bench","object":"chat.completion","created":1700000000,"model":"bench-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I help you today?"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`
	reqModelsJSON  = `{"object":"list","data":[{"id":"bench-model","object":"model"},{"id":"bench-model-b","object":"model"}]}`
	reqWarmup      = 20
	reqUpstreamEnv = "BENCH_UPSTREAM"
)

var reqStreamChunks = func() string {
	var sb strings.Builder
	words := []string{"Hello", "!", " How", " can", " I", " help", " you", " today", "?"}
	for i, w := range words {
		role := ""
		if i == 0 {
			role = `"role":"assistant",`
		}
		fmt.Fprintf(&sb, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"bench-model\",\"choices\":[{\"index\":0,\"delta\":{%s\"content\":%q},\"finish_reason\":null}]}\n\n", role, w)
	}
	sb.WriteString("data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"bench-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	sb.WriteString("data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"bench-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":9,\"total_tokens\":21}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}()

// requestHook is the interpreted-side half of -mode request: it builds the
// gateway through the plugin's New() and exposes each pipeline stage.
const requestHook = `package %s

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"
)

var (
	benchGW     *Gateway
	benchHandler http.Handler
	benchReq    *http.Request
	benchBodyB  []byte
	benchBodyS  []byte
	benchUser   *user
	benchGrp    *group
	benchScopes []limitScope
	benchModelScopes []limitScope
	benchReqMap map[string]any
	benchCand   resolveCandidate
)

const benchModel = "bench-model"

func BenchReqSetup(upURL, redisAddr string) string {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"oa": &ProviderConfig{Type: "openai", APIKey: "bench-upstream-key", BaseURL: upURL, Models: []string{"bench-model"}},
		"ob": &ProviderConfig{Type: "openai", APIKey: "bench-upstream-key", BaseURL: upURL, Models: []string{"bench-model-b"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": &GroupConfig{}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{&UserConfig{
		Name: "bench", Group: "default", APIKey: "bench-user-key",
		Limits: &LimitsConfig{
			RequestsPerDay: 1000000000, TokensPerDay: 1000000000000, TokensPerMonth: 1000000000000,
			CostPerDayUSD: 1000000, CostPerMonthUSD: 1000000,
		},
	}}}
	cfg.Pricing = map[string]*ModelPricing{
		"bench-model":   &ModelPricing{InputPerM: 1, OutputPerM: 2},
		"bench-model-b": &ModelPricing{InputPerM: 1, OutputPerM: 2},
	}
	cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{UserModel: true, Latency: true}}
	if redisAddr != "" {
		fo := true
		cfg.Redis = &RedisConfig{Address: redisAddr, FailOpen: &fo}
	}
	h, err := New(context.Background(), http.NotFoundHandler(), cfg, "bench")
	if err != nil {
		return err.Error()
	}
	gw, ok := h.(*Gateway)
	if !ok {
		return "New did not return *Gateway"
	}
	benchGW = gw
	benchHandler = h
	benchBodyB = []byte(%q)
	benchBodyS = []byte(%q)
	benchReq = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(benchBodyB))
	benchReq.Header.Set("Authorization", "Bearer bench-user-key")
	u, grp, ok2 := gw.auth.identify(benchReq)
	if !ok2 {
		return "identify failed for the bench user"
	}
	benchUser = u
	benchGrp = grp
	benchScopes = withTotalScope(buildLimitScopes(u, grp))
	benchModelScopes = withModelScope(benchScopes, "oa/bench-model")
	var m map[string]any
	if jerr := json.Unmarshal(benchBodyB, &m); jerr != nil {
		return jerr.Error()
	}
	benchReqMap = m
	c, rerr := gw.registry.resolvePrimaryOnly(benchModel, grp)
	if rerr != nil {
		return rerr.Error()
	}
	benchCand = c
	return ""
}

func BenchReqHandler() http.Handler { return benchHandler }

func BenchReqClose() {
	if benchGW != nil {
		benchGW.Close()
	}
}

func BenchStage(name string, n int) int {
	switch name {
	case "identify":
		return benchIdentify(n)
	case "logAuthEvent":
		return benchLogAuth(n)
	case "scopes":
		return benchScopesRun(n)
	case "checkAndCount":
		return benchCheckAndCount(n)
	case "admitRoute":
		return benchAdmitRoute(n)
	case "recordLastSeen":
		return benchLastSeen(n)
	case "decode":
		return benchDecode(n)
	case "resolve":
		return benchResolve(n)
	case "orderFilter":
		return benchOrderFilter(n)
	case "cost":
		return benchCost(n)
	case "account":
		return benchAccount(n)
	case "accountNoStats":
		return benchAccountNoStats(n)
	case "modelsJSON":
		return benchModelsJSON(n)
	case "upstreamChat":
		return benchUpstreamChat(n)
	case "upstreamStream":
		return benchUpstreamStream(n)
	}
	return -1
}

func benchIdentify(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		_, _, ok := benchGW.auth.identify(benchReq)
		if ok {
			s++
		}
	}
	return s
}

func benchLogAuth(n int) int {
	for i := 0; i < n; i++ {
		benchGW.logAuthEvent(true, authEventUserName(benchUser), benchReq)
	}
	return n
}

func benchScopesRun(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += len(withTotalScope(buildLimitScopes(benchUser, benchGrp)))
	}
	return s
}

func benchCheckAndCount(n int) int {
	s := 0
	reserve := reserveTokensFor(int64(len(benchBodyB)))
	for i := 0; i < n; i++ {
		v, _ := benchGW.limiter.checkAndCountInfo(benchScopes, reserve)
		if v != nil {
			s++
		}
		benchGW.limiter.releaseReservations(benchScopes)
	}
	return s
}

func benchAdmitRoute(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		sw := &statusTrackingWriter{ResponseWriter: rec}
		sc, ok := benchGW.admitRequestForRoute(benchReq.Context(), sw, benchUser, benchGrp, writeOAIError, eventRouteForEndpoint(cacheEndpointChat), int64(len(benchBodyB)), endpointKind(cacheEndpointChat))
		if ok {
			s++
		}
		benchGW.limiter.releaseReservations(sc)
	}
	return s
}

func benchLastSeen(n int) int {
	now := time.Now()
	for i := 0; i < n; i++ {
		benchGW.limiter.recordLastSeen(benchScopes, now)
	}
	return n
}

func benchDecode(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		body, _, err := readCapped(bytes.NewReader(benchBodyB), benchGW.unifiedRequestBytes)
		var req map[string]any
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err == nil {
			s += len(req)
		}
	}
	return s
}

func benchResolve(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		c, err := benchGW.registry.resolvePrimaryOnly(benchModel, benchGrp)
		if err == nil && c.providerName != "" {
			s++
		}
	}
	return s
}

func benchOrderFilter(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		c := benchGW.orderedFailoverCandidates(benchCand, nil)
		c = benchGW.filterCandidatesByCost(c, benchModel)
		c = capFailoverCandidates(c, benchGW.failover.maxAttempts)
		s += len(c)
	}
	return s
}

func benchCost(n int) int {
	var s int64
	u := usage{prompt: 12, completion: 9}
	for i := 0; i < n; i++ {
		s += unifiedCostMicros(benchCand.canonical, benchCand.upstreamModel, u, benchGW.cfg.Pricing, benchGW.cfg.ModelMeta, benchGW.pricingWarn)
	}
	return int(s)
}

func benchAccountRun(n int) int {
	u := usage{prompt: 12, completion: 9}
	x := accountExtras{provider: "oa", hasLat: true, lat: latencySample{duration: 40 * time.Millisecond, ttfb: 10 * time.Millisecond, hasTTFB: true}}
	for i := 0; i < n; i++ {
		cost := unifiedCostMicros(benchCand.canonical, benchCand.upstreamModel, u, benchGW.cfg.Pricing, benchGW.cfg.ModelMeta, benchGW.pricingWarn)
		benchGW.limiter.accountWith(benchModelScopes, u, cost, x)
	}
	return n
}

func benchAccount(n int) int { return benchAccountRun(n) }

func benchAccountNoStats(n int) int {
	a, m, l := benchGW.limiter.statsAdmin, benchGW.limiter.statsUserModel, benchGW.limiter.statsLatency
	benchGW.limiter.statsAdmin, benchGW.limiter.statsUserModel, benchGW.limiter.statsLatency = false, false, false
	r := benchAccountRun(n)
	benchGW.limiter.statsAdmin, benchGW.limiter.statsUserModel, benchGW.limiter.statsLatency = a, m, l
	return r
}

func benchModelsJSON(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += len(benchGW.registry.modelsJSON(benchGrp))
	}
	return s
}

func benchUpstreamChat(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		var req map[string]any
		json.Unmarshal(benchBodyB, &req)
		rec := httptest.NewRecorder()
		u, err := benchCand.adapter.chatCompletion(context.Background(), rec, req)
		if err == nil {
			s += int(u.completion)
		}
	}
	return s
}

func benchUpstreamStream(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		var req map[string]any
		json.Unmarshal(benchBodyS, &req)
		rec := httptest.NewRecorder()
		u, err := benchCand.adapter.chatCompletion(context.Background(), rec, req)
		if err == nil {
			s += int(u.completion)
		}
	}
	return s
}
`

const requestNativeTest = `package %s

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The gateway logs one line per authenticated request to os.Stderr; send it to
// /dev/null so it cannot split go test's result lines. The write syscall stays.
func init() {
	if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = f
	}
}

func benchServe(b *testing.B, method, path, body string) {
	if msg := BenchReqSetup(os.Getenv("BENCH_UPSTREAM"), os.Getenv("BENCH_REDIS")); msg != "" {
		b.Fatal(msg)
	}
	defer BenchReqClose()
	h := BenchReqHandler()
	do := func() *httptest.ResponseRecorder {
		var rd *bytes.Reader
		if body != "" {
			rd = bytes.NewReader([]byte(body))
		} else {
			rd = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, path, rd)
		r.Header.Set("Authorization", "Bearer bench-user-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(); w.Code != http.StatusOK {
		b.Fatalf("status %%d: %%s", w.Code, w.Body.String())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		do()
	}
}

func benchReqKinds(b *testing.B) {
	b.Run("chat", func(b *testing.B) { benchServe(b, "POST", "/v1/chat/completions", %q) })
	b.Run("stream", func(b *testing.B) { benchServe(b, "POST", "/v1/chat/completions", %q) })
	b.Run("models", func(b *testing.B) { benchServe(b, "GET", "/v1/models", "") })
	for _, name := range %s {
		b.Run("stage_"+name, func(b *testing.B) {
			if msg := BenchReqSetup(os.Getenv("BENCH_UPSTREAM"), os.Getenv("BENCH_REDIS")); msg != "" {
				b.Fatal(msg)
			}
			defer BenchReqClose()
			b.ReportAllocs()
			b.ResetTimer()
			BenchStage(name, b.N)
		})
	}
}

func BenchmarkReqMem(b *testing.B) {
	t := os.Getenv("BENCH_REDIS")
	os.Setenv("BENCH_REDIS", "")
	defer os.Setenv("BENCH_REDIS", t)
	benchReqKinds(b)
}

func BenchmarkReqRedis(b *testing.B) {
	if os.Getenv("BENCH_REDIS") == "" {
		b.Skip("no redis")
	}
	benchReqKinds(b)
}
`

var reqStageNames = []string{
	"identify", "logAuthEvent", "scopes", "checkAndCount", "admitRoute", "recordLastSeen",
	"decode", "resolve", "orderFilter", "cost", "account", "accountNoStats", "modelsJSON",
	"upstreamChat", "upstreamStream",
}

func reqStageNamesLiteral() string {
	q := make([]string, len(reqStageNames))
	for i, n := range reqStageNames {
		q[i] = strconv.Quote(n)
	}
	return "[]string{" + strings.Join(q, ", ") + "}"
}

// reqUpstreamHandler serves a fixed OpenAI-shaped chat completion, an SSE
// variant when the body asks for a stream, and a model list.
func reqUpstreamHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reqModelsJSON)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, reqStreamChunks)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reqChatJSON)
	})
	return mux
}

// runUpstream is the -mode upstream child: it prints its URL and serves until killed.
func runUpstream() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	fmt.Println("http://" + ln.Addr().String())
	return http.Serve(ln, reqUpstreamHandler()) //nolint:gosec // local bench server
}

// startUpstream runs the upstream in a child process so its allocations stay out of the measurement.
func startUpstream() (url string, stop func(), err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(exe, "-mode", "upstream") //nolint:gosec // own binary
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err = cmd.Start(); err != nil {
		return "", nil, err
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		return "", nil, fmt.Errorf("read upstream url: %w", err)
	}
	return strings.TrimSpace(line), func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}

func copyPluginWith(root, gopath, file, src string) (pkgDir, pkgName, modulePath string, err error) {
	modulePath, err = readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return
	}
	pkgDir = filepath.Join(gopath, "src", modulePath)
	if err = copyTree(root, pkgDir); err != nil {
		return
	}
	if pkgName, err = readPackageName(pkgDir); err != nil {
		return
	}
	err = os.WriteFile(filepath.Join(pkgDir, file), []byte(fmt.Sprintf(src, pkgName, reqBodyChat, reqBodyStream)), 0o600)
	return
}

type reqKind struct {
	name, method, path, body string
}

func (k reqKind) do(h http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest(k.method, k.path, strings.NewReader(k.body))
	r.Header.Set("Authorization", "Bearer "+reqUserKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func measure(n int, fn func(int)) sample {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	t := time.Now()
	fn(n)
	d := time.Since(t)
	runtime.ReadMemStats(&m1)
	return sample{
		ns:     float64(d.Nanoseconds()) / float64(n),
		bytes:  float64(m1.TotalAlloc-m0.TotalAlloc) / float64(n),
		allocs: float64(m1.Mallocs-m0.Mallocs) / float64(n),
	}
}

func summarizeUS(label string, ss []sample) {
	col := func(f func(sample) float64) []float64 {
		out := make([]float64, len(ss))
		for i, s := range ss {
			out[i] = f(s)
		}
		return out
	}
	ns, lo, hi := median(col(func(s sample) float64 { return s.ns }))
	by, _, _ := median(col(func(s sample) float64 { return s.bytes }))
	al, _, _ := median(col(func(s sample) float64 { return s.allocs }))
	fmt.Printf("%-34s n=%d  median %9.1f us/req  [%.1f..%.1f]  %8.0f B/req  %6.0f allocs/req\n",
		label, len(ss), ns/1e3, lo/1e3, hi/1e3, by, al)
}

func runRequest(root, redisAddr string, samples, n int) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	upURL, stopUp, err := startUpstream()
	if err != nil {
		return err
	}
	defer stopUp()
	gopath, err := os.MkdirTemp("", "llmgw-request-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // temp dir
	_, pkgName, modulePath, err := copyPluginWith(root, gopath, "zz_request_hook.go", requestHook)
	if err != nil {
		return err
	}
	i := interp.New(interp.Options{GoPath: gopath})
	if err = i.Use(stdlib.Symbols); err != nil {
		return err
	}
	if _, err = i.Eval(`import "` + modulePath + `"`); err != nil {
		return fmt.Errorf("import plugin under yaegi: %w", err)
	}
	get := func(name string) (any, error) {
		v, e := i.Eval(pkgName + "." + name)
		if e != nil {
			return nil, e
		}
		return v.Interface(), nil
	}
	setupA, err := get("BenchReqSetup")
	if err != nil {
		return err
	}
	setup := setupA.(func(string, string) string)
	handlerA, err := get("BenchReqHandler")
	if err != nil {
		return err
	}
	handlerFn := handlerA.(func() http.Handler)
	closeA, err := get("BenchReqClose")
	if err != nil {
		return err
	}
	closeFn := closeA.(func())
	stageA, err := get("BenchStage")
	if err != nil {
		return err
	}
	stageFn := stageA.(func(string, int) int)

	kinds := []reqKind{
		{"chat", "POST", "/v1/chat/completions", reqBodyChat},
		{"stream", "POST", "/v1/chat/completions", reqBodyStream},
		{"models", "GET", "/v1/models", ""},
	}
	stores := []string{"mem"}
	if redisAddr != "" {
		stores = append(stores, "redis")
	}
	type slot struct {
		label string
		fn    func(int)
		ss    []sample
	}
	for _, st := range stores {
		addr := ""
		if st == "redis" {
			addr = redisAddr
		}
		if msg := setup(upURL, addr); msg != "" {
			return fmt.Errorf("setup %s: %s", st, msg)
		}
		h := handlerFn()
		var slots []*slot
		for _, k := range kinds {
			if w := k.do(h); w.Code != http.StatusOK {
				return fmt.Errorf("%s %s: status %d: %.200s", st, k.name, w.Code, w.Body.String())
			}
			k := k
			slots = append(slots, &slot{label: "yaegi " + st + " " + k.name, fn: func(n int) {
				for j := 0; j < n; j++ {
					k.do(h)
				}
			}})
		}
		for _, name := range reqStageNames {
			if stageFn(name, 1) < 0 {
				return fmt.Errorf("stage %s missing", name)
			}
			name := name
			slots = append(slots, &slot{label: "yaegi " + st + " stage " + name, fn: func(n int) { stageFn(name, n) }})
		}
		for _, s := range slots {
			s.fn(reqWarmup)
		}
		// Interleave slots per round so machine load drifts hit all alike.
		for r := 0; r < samples; r++ {
			for _, s := range slots {
				s.ss = append(s.ss, measure(n, s.fn))
			}
		}
		for _, s := range slots {
			summarizeUS(s.label, s.ss)
		}
		closeFn()
	}
	return nil
}

var reqBenchLine = regexp.MustCompile(`^Benchmark(Req(?:Mem|Redis))/(\w+)\S*\s+\d+\s+([\d.]+) ns/op\s+([\d.]+) B/op\s+([\d.]+) allocs/op`)

func runRequestNative(root, redisAddr string, samples int) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	upURL, stopUp, err := startUpstream()
	if err != nil {
		return err
	}
	defer stopUp()
	gopath, err := os.MkdirTemp("", "llmgw-request-native-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // temp dir
	pkgDir, pkgName, _, err := copyPluginWith(root, gopath, "zz_request_hook.go", requestHook)
	if err != nil {
		return err
	}
	test := fmt.Sprintf(requestNativeTest, pkgName, reqBodyChat, reqBodyStream, reqStageNamesLiteral())
	if err = os.WriteFile(filepath.Join(pkgDir, "zz_request_bench_test.go"), []byte(test), 0o600); err != nil {
		return err
	}
	pat := "ReqMem"
	if redisAddr != "" {
		pat = "Req(Mem|Redis)"
	}
	cmd := exec.Command("go", "test", "-run", "^$", "-bench", pat, "-benchmem", "-benchtime", "300ms", "-count", strconv.Itoa(samples), ".") //nolint:gosec // G204: fixed binary, pattern from two constants, count is an int
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "BENCH_REDIS="+redisAddr, reqUpstreamEnv+"="+upURL)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // the gateway logs to stderr; keep it out of the result lines
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("native bench: %w\n%s\n%s", err, out, stderr.String())
	}
	groups := map[string][]sample{}
	var order []string
	for _, line := range strings.Split(string(out), "\n") {
		m := reqBenchLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := "native " + strings.ToLower(strings.TrimPrefix(m[1], "Req")) + " " + strings.ReplaceAll(m[2], "stage_", "stage ")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		ns, _ := strconv.ParseFloat(m[3], 64)
		by, _ := strconv.ParseFloat(m[4], 64)
		al, _ := strconv.ParseFloat(m[5], 64)
		groups[key] = append(groups[key], sample{ns, by, al})
	}
	if len(order) == 0 {
		return fmt.Errorf("native bench produced no results:\n%s", out)
	}
	for _, k := range order {
		summarizeUS(k, groups[k])
	}
	return nil
}
