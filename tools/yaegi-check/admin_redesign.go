// This file adds the admin dashboard redesign's own yaegi-check probes
// (COMMON3.md plan §5, WP-G): last-seen's absolute SET reply decode,
// GET /admin/api/usage/series, /usage/totals, /performance, /catalog,
// /config, and /consumers, and a response-cache hit's chit counter —
// every one of them interpreted code this repo's tests (go test) never
// run under Yaegi.
//
// These build their OWN Gateway instance — own config, own upstream, own
// fake Redis — rather than reusing run()'s shared `handler`: they need
// admin.stats.userModel/latency on and a configured Redis (the cache
// probe needs a real response cache, which is Redis-backed only —
// cache.go's buildResponseCache returns nil when no Redis client is
// configured, regardless of cache.enabled), neither of which the shared
// harness config carries, and layering them onto the shared handler
// would add stats traffic every earlier probe's own counter assertions
// never accounted for. A real (fake) Redis also matters here beyond the
// cache probe alone: the last-seen family writes an ABSOLUTE "SET key v
// EX secs" command (limits.go/redis_store.go, admin-redesign WP-A) whose
// "+OK" reply resp.go's decodeReply turns into a Go STRING, not the
// INTEGER reply every other counter write returns — exactly the kind of
// interpreted-vs-compiled type-handling divergence this whole harness
// exists to catch (ground truth: "redis store INCRBY + optional EXPIRE;
// applyIncrReplies expects integers; +OK decodes to Go string",
// resp.go:677-679) — a probe running only against the in-process
// memoryStore fallback would never exercise that wire-protocol path at
// all.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/traefik/yaegi/interp"
)

const (
	adminRedesignUserAPIKey     = "sk-admin-redesign-user"  // #nosec G101 -- test fixture literal, not a real credential
	adminRedesignAdminAPIKey    = "sk-admin-redesign-admin" // #nosec G101 -- test fixture literal, not a real credential
	adminRedesignProviderName   = "arprov"
	adminRedesignProviderAPIKey = "sk-admin-redesign-provider-literal-should-be-redacted" // #nosec G101 -- test fixture literal, not a real credential; the /config probe below asserts this exact string never appears in a redacted response
	adminRedesignModel          = "ar-model"
	adminRedesignFreeModel      = "ar-free-model"
	adminRedesignGroupName      = "arg"
)

// adminRedesignRespValue is one key's stored value plus whether it was
// ever set — probeRedisFake's own map value, mirroring
// redis_store_test.go's behavioralRedisServer.data (a plain
// map[string]string would do too, but a struct keeps the zero-value
// "never set" case explicit rather than relying on Go's own zero string
// being indistinguishable from a genuinely empty stored value).
type adminRedesignRespValue struct {
	v string
}

// probeRedisFake is a minimal RESP2 server supporting exactly the
// commands redis_store.go ever sends (SELECT/AUTH/EXPIRE always succeed;
// GET/SET/INCRBY behave for real) — the identical command set
// redis_store_test.go's own behavioralRedisServer supports, reimplemented
// here because that helper lives in a _test.go file and cannot be
// imported into this separate module/binary.
type probeRedisFake struct {
	data map[string]adminRedesignRespValue
	mu   sync.Mutex
}

func newProbeRedisFake() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the admin-redesign probe's fake Redis: %w", err)
	}
	s := &probeRedisFake{data: make(map[string]adminRedesignRespValue)}
	go s.acceptLoop(ln)
	return ln, nil
}

func (s *probeRedisFake) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed once the probe returns; not an error
		}
		go s.serve(conn)
	}
}

func (s *probeRedisFake) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := adminRedesignReadRESPCommand(r)
		if err != nil {
			return
		}
		if _, err := conn.Write(s.handle(args)); err != nil {
			return
		}
	}
}

func (s *probeRedisFake) handle(args []string) []byte {
	if len(args) == 0 {
		return []byte("-ERR empty command\r\n")
	}
	switch strings.ToUpper(args[0]) {
	case "SELECT", "AUTH", "EXPIRE":
		return []byte("+OK\r\n")
	case "GET":
		s.mu.Lock()
		val, ok := s.data[args[1]]
		s.mu.Unlock()
		if !ok {
			return []byte("$-1\r\n")
		}
		return []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(val.v), val.v))
	case "SET":
		// args[2] is the value; a trailing "EX seconds" (redis_store.go's
		// own absolute-entry command, last-seen's own write) is accepted
		// and ignored — this fake never expires a key, matching
		// behavioralRedisServer's own "no test here runs long enough for
		// a real TTL to matter" reasoning.
		s.mu.Lock()
		s.data[args[1]] = adminRedesignRespValue{v: args[2]}
		s.mu.Unlock()
		return []byte("+OK\r\n")
	case "INCRBY":
		n, _ := strconv.ParseInt(args[2], 10, 64)
		s.mu.Lock()
		cur, _ := strconv.ParseInt(s.data[args[1]].v, 10, 64)
		cur += n
		s.data[args[1]] = adminRedesignRespValue{v: strconv.FormatInt(cur, 10)}
		s.mu.Unlock()
		return []byte(fmt.Appendf(nil, ":%d\r\n", cur))
	default:
		return []byte("-ERR unknown command\r\n")
	}
}

// adminRedesignReadLine and adminRedesignReadRESPCommand port resp.go's
// readLine and resp_test.go's readRESPCommand verbatim (this harness
// cannot import either — see this file's own package doc comment) — the
// exact same RESP2 command-array framing ("*N\r\n$len\r\narg\r\n...")
// every respClient command uses.
func adminRedesignReadLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimSuffix(string(buf), "\r"), nil
		}
		buf = append(buf, b)
	}
}

func adminRedesignReadRESPCommand(r *bufio.Reader) ([]string, error) {
	line, err := adminRedesignReadLine(r)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("want array header, got %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := 0; i < n; i++ {
		lenLine, err := adminRedesignReadLine(r)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(lenLine, "$") {
			return nil, fmt.Errorf("want bulk header, got %q", lenLine)
		}
		l, err := strconv.Atoi(lenLine[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := readFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:l])
	}
	return args, nil
}

// readFull mirrors io.ReadFull without importing "io" solely for this one
// call in a file that otherwise has no other use for it.
func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// adminRedesignSeriesResp/TotalsResp/PerfResp/CatalogResp/ConsumersResp/
// UsageResp/ConfigResp are minimal local decodes of the new endpoints'
// JSON response shapes (admin.go/stats_read.go/admin_catalog.go/
// admin_config.go) — only the fields these probes actually assert on,
// not full mirrors of the plugin's own view types (webui/src/types/api.ts
// is that contract's real, complete mirror).
type adminRedesignUsageResp struct {
	Users []struct {
		ID       string `json:"id"`
		LastSeen int64  `json:"lastSeen"`
	} `json:"users"`
}

type adminRedesignSeriesResp struct {
	Buckets []string `json:"buckets"`
	Series  []struct {
		Scope  string  `json:"scope"`
		Points []int64 `json:"points"`
	} `json:"series"`
}

type adminRedesignTotalsResp struct {
	Rows []struct {
		Values map[string]int64 `json:"values"`
		ID     string           `json:"id"`
	} `json:"rows"`
}

type adminRedesignPerfResp struct {
	Rows []struct {
		ID    string `json:"id"`
		Count int64  `json:"count"`
	} `json:"rows"`
	LatencyEnabled bool `json:"latencyEnabled"`
}

type adminRedesignCatalogResp struct {
	Providers []struct {
		Name   string `json:"name"`
		Models []struct {
			ID          string `json:"id"`
			PriceSource string `json:"priceSource"`
		} `json:"models"`
	} `json:"providers"`
}

type adminRedesignConsumersResp struct {
	Users []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"users"`
}

// adminRedesignGetJSON issues req against handler and decodes a 200
// response's body into out — every probe below's shared "fetch and
// decode" step.
func adminRedesignGetJSON(handler http.Handler, path, apiKey string, out any) ([]byte, error) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		return body, fmt.Errorf("GET %s: status = %d, want 200, body=%s", path, rec.Code, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return body, fmt.Errorf("GET %s: decode response: %w (body=%s)", path, err, body)
		}
	}
	return body, nil
}

// newAdminRedesignHandler builds one interpreted Gateway for
// exerciseAdminRedesignStats' own config (admin.stats.userModel/latency
// on, a real Redis at redisAddr, cache enabled) — split out from
// exerciseAdminRedesignStats itself purely so its own `err` variable's
// scope ends here, at this function's return, rather than staying live
// (and shadow-prone in every later `if _, err := ...` assertion) for the
// rest of that much longer function.
func newAdminRedesignHandler(i *interp.Interpreter, pkgName, upstreamURL, redisAddr string) (http.Handler, error) {
	cfgVal, err := i.Eval(pkgName + ".CreateConfig()")
	if err != nil {
		return nil, fmt.Errorf("admin-redesign probe: call %s.CreateConfig() under yaegi: %w", pkgName, err)
	}
	if !cfgVal.CanInterface() {
		return nil, fmt.Errorf("admin-redesign probe: %s.CreateConfig() result cannot be used via reflect.Value.Interface", pkgName)
	}

	cfgJSON := `{` +
		`"providers":{"` + adminRedesignProviderName + `":{"type":"openai","baseUrl":"` + upstreamURL + `","apiKey":"` + adminRedesignProviderAPIKey + `","models":["` + adminRedesignModel + `","` + adminRedesignFreeModel + `"]}},` +
		`"groups":{"` + adminRedesignGroupName + `":{}},` +
		`"users":{"inline":[` +
		`{"name":"ar-alice","group":"` + adminRedesignGroupName + `","apiKey":"` + adminRedesignUserAPIKey + `"},` +
		`{"name":"ar-admin","group":"` + adminRedesignGroupName + `","apiKey":"` + adminRedesignAdminAPIKey + `","admin":true}` +
		`]},` +
		`"admin":{"enabled":true,"stats":{"userModel":true,"latency":true}},` +
		`"modelMeta":{"` + adminRedesignProviderName + `/` + adminRedesignFreeModel + `":{"free":true}},` +
		`"redis":{"address":"` + redisAddr + `"},` +
		`"cache":{"enabled":true,"ttl":"1m"}` +
		`}`
	if err = json.Unmarshal([]byte(cfgJSON), cfgVal.Interface()); err != nil {
		return nil, fmt.Errorf("admin-redesign probe: decode its own config into the interpreted Config: %w", err)
	}

	newVal, err := i.Eval(pkgName + ".New")
	if err != nil {
		return nil, fmt.Errorf("admin-redesign probe: resolve %s.New under yaegi: %w", pkgName, err)
	}
	if newVal.Kind() != reflect.Func {
		return nil, fmt.Errorf("admin-redesign probe: %s.New is not a function (kind %s)", pkgName, newVal.Kind())
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	results := newVal.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(next),
		cfgVal,
		reflect.ValueOf("llmgw-check-admin-redesign"),
	})
	if len(results) != 2 {
		return nil, fmt.Errorf("admin-redesign probe: %s.New returned %d values, want 2 (http.Handler, error)", pkgName, len(results))
	}
	if errVal := results[1]; !errVal.IsNil() {
		return nil, fmt.Errorf("admin-redesign probe: %s.New returned an error: %v", pkgName, errVal.Interface())
	}
	handler, ok := results[0].Interface().(http.Handler)
	if !ok || handler == nil {
		return nil, fmt.Errorf("admin-redesign probe: %s.New's returned value is not a usable http.Handler", pkgName)
	}
	return handler, nil
}

// exerciseAdminRedesignStats builds its own interpreted Gateway (admin
// dashboard redesign, WP-G's own yaegi-check probes — plan §5) and runs
// every new-endpoint/new-counter-family assertion the plan's yaegi-check
// list names, against it.
func exerciseAdminRedesignStats(i *interp.Interpreter, pkgName string) error {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c-ar-1","object":"chat.completion","model":"` + adminRedesignModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	redisLn, redisErr := newProbeRedisFake()
	if redisErr != nil {
		return redisErr
	}
	defer func() { _ = redisLn.Close() }()

	handler, handlerErr := newAdminRedesignHandler(i, pkgName, upstream.URL, redisLn.Addr().String())
	if handlerErr != nil {
		return handlerErr
	}

	chatBody := `{"model":"` + adminRedesignModel + `","messages":[{"role":"user","content":"hi"}]}`
	chat := func() (*httptest.ResponseRecorder, error) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+adminRedesignUserAPIKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return rec, fmt.Errorf("POST /v1/chat/completions (admin-redesign probe): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		return rec, nil
	}

	// First request: a MISS (checkAndCount writes last-seen + admission
	// counters; accountWith writes umodel/latency; recordProviderAttempt
	// writes the provider's hourly attempt/latency counters). Second
	// request: the SAME body again, a cache HIT (recordCacheHit's own
	// async chit/csave write) — exactly the shape
	// TestRoundTrips_SingleGroupChat_CacheHit_AddsExactlyOneAsyncRoundTrip
	// (stats_write_test.go) drives compiled, now proven under Yaegi too.
	missRec, missErr := chat()
	if missErr != nil {
		return missErr
	}
	if got := missRec.Header().Get("X-Llmgw-Cache"); got != "miss" {
		return fmt.Errorf("admin-redesign probe: first POST /v1/chat/completions X-Llmgw-Cache = %q, want miss (body=%s)", got, missRec.Body.String())
	}
	hitRec, hitErr := chat()
	if hitErr != nil {
		return hitErr
	}
	if got := hitRec.Header().Get("X-Llmgw-Cache"); got != "hit" {
		return fmt.Errorf("admin-redesign probe: second POST /v1/chat/completions X-Llmgw-Cache = %q, want hit (body=%s)", got, hitRec.Body.String())
	}

	// (1) last-seen: GET /admin/api/usage's own lastSeen field, populated
	// by the SAME absolute-SET write the fake Redis above just decoded a
	// "+OK" reply for.
	var usage adminRedesignUsageResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/usage", adminRedesignAdminAPIKey, &usage); err != nil {
		return err
	}
	found := false
	for _, u := range usage.Users {
		if u.ID == "ar-alice" {
			found = true
			if u.LastSeen == 0 {
				return fmt.Errorf("admin-redesign probe: GET /admin/api/usage: ar-alice.lastSeen = 0, want non-zero after an admitted request")
			}
		}
	}
	if !found {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/usage: no user entry named ar-alice")
	}

	// (2) usage/series: two scopes, 200, bucket/point arrays aligned.
	var series adminRedesignSeriesResp
	if _, err := adminRedesignGetJSON(handler,
		"/admin/api/usage/series?scope=user:ar-alice&scope=total&metric=req&window=day&span=3",
		adminRedesignAdminAPIKey, &series); err != nil {
		return err
	}
	if len(series.Series) != 2 {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/usage/series: %d series, want 2 (user:ar-alice, total)", len(series.Series))
	}
	for _, s := range series.Series {
		if len(s.Points) != len(series.Buckets) {
			return fmt.Errorf("admin-redesign probe: GET /admin/api/usage/series: scope %q has %d points, want %d (aligned with buckets)", s.Scope, len(s.Points), len(series.Buckets))
		}
	}

	// (3) usage/totals?kind=user.
	var totals adminRedesignTotalsResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/usage/totals?kind=user&window=day", adminRedesignAdminAPIKey, &totals); err != nil {
		return err
	}
	totalsFound := false
	for _, r := range totals.Rows {
		if r.ID == "ar-alice" {
			totalsFound = true
		}
	}
	if !totalsFound {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/usage/totals?kind=user: no row for ar-alice (rows=%v)", totals.Rows)
	}

	// (4) performance?kind=provider, stats.latency on: count>0 after one
	// request that actually completed (the miss above).
	var perf adminRedesignPerfResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/performance?kind=provider&window=day&id="+adminRedesignProviderName, adminRedesignAdminAPIKey, &perf); err != nil {
		return err
	}
	if !perf.LatencyEnabled {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/performance: latencyEnabled = false, want true (admin.stats.latency is on)")
	}
	perfFound := false
	for _, r := range perf.Rows {
		if r.ID == adminRedesignProviderName {
			perfFound = true
			if r.Count <= 0 {
				return fmt.Errorf("admin-redesign probe: GET /admin/api/performance: provider %q count = %d, want > 0 after a completed request", adminRedesignProviderName, r.Count)
			}
		}
	}
	if !perfFound {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/performance: no row for provider %q", adminRedesignProviderName)
	}

	// (5) catalog: the modelMeta-free model resolves to priceSource
	// "free" (billingPriceSource's modelMetaFree branch, pricing.go).
	var catalog adminRedesignCatalogResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/catalog", adminRedesignAdminAPIKey, &catalog); err != nil {
		return err
	}
	wantFreeID := adminRedesignProviderName + "/" + adminRedesignFreeModel
	catalogFound := false
	for _, p := range catalog.Providers {
		for _, m := range p.Models {
			if m.ID == wantFreeID {
				catalogFound = true
				if m.PriceSource != "free" {
					return fmt.Errorf("admin-redesign probe: GET /admin/api/catalog: %q priceSource = %q, want %q", wantFreeID, m.PriceSource, "free")
				}
			}
		}
	}
	if !catalogFound {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/catalog: no model entry for %q", wantFreeID)
	}

	// (6) config: the literal provider API key never appears in the
	// redacted response, under the interpreter.
	configBody, configErr := adminRedesignGetJSON(handler, "/admin/api/config", adminRedesignAdminAPIKey, nil)
	if configErr != nil {
		return configErr
	}
	if strings.Contains(string(configBody), adminRedesignProviderAPIKey) {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/config: the literal provider apiKey leaked into the redacted response: %s", configBody)
	}

	// (7) consumers: ar-alice's source is "inline" (Users.Inline, never
	// Users.File, in this probe's own config).
	var consumers adminRedesignConsumersResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/consumers", adminRedesignAdminAPIKey, &consumers); err != nil {
		return err
	}
	consumersFound := false
	for _, u := range consumers.Users {
		if u.Name == "ar-alice" {
			consumersFound = true
			if u.Source != "inline" {
				return fmt.Errorf("admin-redesign probe: GET /admin/api/consumers: ar-alice.source = %q, want %q", u.Source, "inline")
			}
		}
	}
	if !consumersFound {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/consumers: no user entry named ar-alice")
	}

	// (8) cache hit increments the total chit series — the second
	// (hit) chat request above.
	var chitSeries adminRedesignSeriesResp
	if _, err := adminRedesignGetJSON(handler, "/admin/api/usage/series?scope=total&metric=chit&window=day&span=1", adminRedesignAdminAPIKey, &chitSeries); err != nil {
		return err
	}
	if len(chitSeries.Series) != 1 || len(chitSeries.Series[0].Points) != 1 || chitSeries.Series[0].Points[0] < 1 {
		return fmt.Errorf("admin-redesign probe: GET /admin/api/usage/series?metric=chit: %+v, want exactly one series/point with a value >= 1 after a cache hit", chitSeries.Series)
	}

	return nil
}
