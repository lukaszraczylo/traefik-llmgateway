//go:build integration

// Package integration exercises the llmgateway Traefik plugin against a
// real Traefik v3.5 binary loading the plugin via localPlugins, real mock
// upstreams, and a real shared Redis, all orchestrated by
// integration/docker-compose.yml. It assumes the compose stack is already
// up and healthy — the `integration` Makefile target starts it, waits for
// traefik1 to answer, runs this package, then tears the stack down. To keep
// the stack alive between runs for debugging, use `make integration-keep`
// (or `make integration-up` followed directly by
// `go test -tags integration ./...` from this directory), not a bare
// `docker compose up -d --build`: integration-up first renders
// integration/traefik/dynamic.yml from dynamic.yml.tmpl (substituting
// INTEGRATION_REAL_BASEURL), and that generated dynamic.yml is
// git-ignored — running docker compose directly reuses whatever stale or
// absent dynamic.yml happens to be on disk instead of a freshly rendered
// one.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	traefik1URL = "http://localhost:19081"
	traefik2URL = "http://localhost:19082"

	aliceKey = "sk-int-alice" // eng group, high limit, inline user
	bobKey   = "sk-int-bob"   // limited group (requestsPerMinute: 3), file-sourced user
	carolKey = "sk-int-carol" // added to users.json mid-run by TestUsersFileHotReload
	adminKey = "sk-int-admin" // eng group, inline user, admin: true (spec §4, v0.2)
)

// composeFile returns the path to docker-compose.yml relative to this
// package's directory, which is also `go test`'s working directory.
func composeFile() string { return "docker-compose.yml" }

// doJSON issues one JSON request, returning the raw *http.Response
// (headers and status code still valid — its body has already been read
// and closed) alongside the decoded JSON body. A non-JSON or empty body
// decodes to a nil map rather than failing the test; callers that require
// a body assert on it themselves.
func doJSON(t *testing.T, method, url, apiKey string, body any) (*http.Response, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body from %s %s: %v", method, url, err)
	}
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out) // non-JSON body (e.g. an SSE stream) just yields a nil map
	}
	return resp, out
}

// firstChoiceContent extracts choices[0].message.content from an
// OpenAI-shaped chat completion response body, failing the test if the
// shape doesn't match.
func firstChoiceContent(t *testing.T, body map[string]any) string {
	t.Helper()
	choices, ok := body["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("response has no choices: %#v", body)
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choices[0] is not an object: %#v", choices[0])
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("choices[0].message is not an object: %#v", choice)
	}
	content, _ := message["content"].(string)
	return content
}

// waitForHealthy polls url with apiKey until it answers 200 or timeout
// elapses.
func waitForHealthy(t *testing.T, url, apiKey string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastStatus int
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s did not become healthy within %s (last status=%d, last error=%v)", url, timeout, lastStatus, lastErr)
}

// waitForModelAlias polls GET /v1/models on baseURL with apiKey until an
// entry with id wantID appears, or timeout elapses. Task 6 review fix: a
// plain 200 from /v1/models (waitForHealthy's own check) only proves
// Traefik's file provider has loaded SOME generation of dynamic.yml — not
// necessarily the one carrying modelAliases. A request landing in that
// narrow propagation window would see a config generation from before the
// alias applied and get a spurious "unknown model" 404, which is exactly
// what one cold-start run of this suite hit. Polling the actual listing
// removes that window deterministically instead of guessing at a fixed
// sleep.
func waitForModelAlias(t *testing.T, baseURL, apiKey, wantID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody map[string]any
	for time.Now().Before(deadline) {
		resp, body := doJSON(t, http.MethodGet, baseURL+"/v1/models", apiKey, nil)
		if resp.StatusCode == http.StatusOK {
			lastBody = body
			if data, ok := body["data"].([]any); ok {
				for _, entry := range data {
					if m, ok := entry.(map[string]any); ok && m["id"] == wantID {
						return
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("GET %s/v1/models never listed alias %q within %s, last body=%#v", baseURL, wantID, timeout, lastBody)
}

// checkNoPluginErrors greps docker compose's captured logs for service,
// failing if no plugin-loading message appears at all, or if any
// plugin-related line is logged at error level.
func checkNoPluginErrors(t *testing.T, service string) {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-f", composeFile(), "logs", service).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose logs %s: %v\n%s", service, err, out)
	}
	logs := string(out)
	if !strings.Contains(strings.ToLower(logs), "plugin") {
		t.Fatalf("%s logs contain no plugin-related message at all (plugin may not have loaded):\n%s", service, logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		ll := strings.ToLower(line)
		isPluginLine := strings.Contains(ll, "plugin")
		isErrorLine := strings.Contains(ll, "level=error") || strings.Contains(ll, `"level":"error"`)
		if isPluginLine && isErrorLine {
			t.Fatalf("%s logs contain a plugin error line:\n%s", service, line)
		}
	}
}

// TestPluginLoads covers integration test 1: the plugin loads inside a
// real Traefik binary with no plugin errors logged, and an authenticated
// GET /v1/models returns the models discovery pulled from the mock
// upstreams at plugin construction.
func TestPluginLoads(t *testing.T) {
	waitForHealthy(t, traefik1URL+"/v1/models", aliceKey, 120*time.Second)
	waitForHealthy(t, traefik2URL+"/v1/models", aliceKey, 120*time.Second)

	checkNoPluginErrors(t, "traefik1")
	checkNoPluginErrors(t, "traefik2")

	resp, body := doJSON(t, http.MethodGet, traefik1URL+"/v1/models", aliceKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: status = %d, want 200, body=%#v", resp.StatusCode, body)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("GET /v1/models: expected a non-empty discovered model list, got %#v", body)
	}
}

// TestUnifiedChatAllProviders covers integration test 2: a unified chat
// completion against each of the three mock upstreams under real Traefik.
// openai's response is passed through verbatim (already OpenAI-shaped);
// anthropic's and gemini's are translated into the same OpenAI-shaped
// envelope, each with the client's requested provider-alias echoed back
// in "model" — both translation paths run under real Yaegi here, not just
// go test, which is exactly what caught Task 15's yaegi-only bugs in
// registry.go, routes_unified.go, sse.go, and translate_gemini.go.
func TestUnifiedChatAllProviders(t *testing.T) {
	openaiReq := map[string]any{
		"model":    "openai/gpt-mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, openaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openai chat: status = %d, body=%#v", resp.StatusCode, body)
	}
	if content := firstChoiceContent(t, body); content != "mock openai response" {
		t.Errorf("openai chat: content = %q, want %q", content, "mock openai response")
	}

	anthropicReq := map[string]any{
		"model":    "anthropic/claude-mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp2, body2 := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, anthropicReq)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("anthropic chat: status = %d, body=%#v", resp2.StatusCode, body2)
	}
	if content := firstChoiceContent(t, body2); content != "mock anthropic response" {
		t.Errorf("anthropic chat: content = %q, want %q", content, "mock anthropic response")
	}
	if model, _ := body2["model"].(string); model != "anthropic/claude-mock" {
		t.Errorf("anthropic chat: response model = %q, want the client-requested alias %q echoed back", model, "anthropic/claude-mock")
	}

	geminiReq := map[string]any{
		"model": "gemini/gemini-mock",
		// max_tokens exercises translate_gemini.go's hasMaxTokens=true
		// branch (see its comment on the yaegi multi-value-assignment
		// workaround) under real Traefik + Yaegi, not just go test.
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp3, body3 := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, geminiReq)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("gemini chat: status = %d, body=%#v", resp3.StatusCode, body3)
	}
	if content := firstChoiceContent(t, body3); content != "mock gemini response" {
		t.Errorf("gemini chat: content = %q, want %q", content, "mock gemini response")
	}
	if model, _ := body3["model"].(string); model != "gemini/gemini-mock" {
		t.Errorf("gemini chat: response model = %q, want the client-requested alias %q echoed back", model, "gemini/gemini-mock")
	}
}

// TestModelAliases covers v0.2 integration coverage: task 6 (spec §5).
// dynamic.yml.tmpl configures two operator-defined aliases —
// "aliased/mock" -> "gpt-mock" (openai-type) and "aliased/claude" ->
// "claude-mock" (anthropic-type) — proving alias resolution runs under
// real Traefik+Yaegi, not just go test, including the documented
// echo-behavior asymmetry between provider types (see
// registry_test.go's Gateway-level equivalents for the same assertion
// under go test).
func TestModelAliases(t *testing.T) {
	// Review fix: wait for the alias to actually appear in /v1/models
	// before asserting anything below — see waitForModelAlias's doc
	// comment for why a plain healthy-200 is not enough on its own.
	waitForModelAlias(t, traefik1URL, aliceKey, "aliased/mock", 30*time.Second)

	// openai-type: the upstream response is forwarded verbatim
	// (provider_openai.go), so its "model" field carries whatever the
	// mock echoes back — the resolved upstream id "gpt-mock", not the
	// alias. Correctness here is 200 + real content + the cache header
	// unified routes always set, not the "model" field (documented v0.1
	// passthrough asymmetry, unchanged by aliasing).
	openaiReq := map[string]any{
		"model":    "aliased/mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, openaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aliased openai chat: status = %d, body=%#v", resp.StatusCode, body)
	}
	if content := firstChoiceContent(t, body); content != "mock openai response" {
		t.Errorf("aliased openai chat: content = %q, want %q", content, "mock openai response")
	}
	if got := resp.Header.Get("X-Llmgw-Cache"); got == "" {
		t.Error("aliased openai chat: X-Llmgw-Cache header missing, want \"miss\" or \"hit\"")
	}

	// anthropic-type: translate_anthropic.go builds its own response
	// envelope and echoes the client's exact requested id (the alias)
	// back into "model" — this DOES surface the alias.
	anthropicReq := map[string]any{
		"model":    "aliased/claude",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp2, body2 := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, anthropicReq)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("aliased anthropic chat: status = %d, body=%#v", resp2.StatusCode, body2)
	}
	if content := firstChoiceContent(t, body2); content != "mock anthropic response" {
		t.Errorf("aliased anthropic chat: content = %q, want %q", content, "mock anthropic response")
	}
	if model, _ := body2["model"].(string); model != "aliased/claude" {
		t.Errorf("aliased anthropic chat: response model = %q, want the client-requested alias %q echoed back", model, "aliased/claude")
	}
}

// TestStreamingIncrementalityProbe covers integration test 3.
//
// Content correctness always runs, via assertStreamingContent: a host-side
// client reading the full SSE body is valid for CONTENT regardless of how
// many TCP segments the response arrived as — only a TIMING claim is
// invalidated by output buffering, whether that's the Docker Desktop host
// port-proxy (the design spec's Appendix A spike lesson) or, as Task 15
// discovered, Traefik/Yaegi itself. It verifies all 5 of the mock's delta
// chunks arrive in order, followed by the terminal "[DONE]" event.
//
// The true incremental-delivery TIMING assertion — the raw-TCP in-network
// probe, requiring ≥3 distinct inter-arrival gaps of 100ms or more — is
// gated behind INTEGRATION_STREAMING=1 and skipped otherwise. It is
// currently known to fail under real Traefik+Yaegi: a confirmed, external,
// still-open upstream bug (traefik/traefik#10269, labeled
// "kind/bug/confirmed" by Traefik's own maintainers; see also
// traefik/yaegi#1600) means Yaegi cannot detect http.Flusher support on an
// http.ResponseWriter crossing the compiled-to-interpreted boundary, so no
// code in this plugin can force a real per-chunk flush — see sse.go's
// newSSEWriter and task-15-report.md for the full reproduction. Set
// INTEGRATION_STREAMING=1 to re-check once that upstream issue is fixed.
func TestStreamingIncrementalityProbe(t *testing.T) {
	assertStreamingContent(t)

	if os.Getenv("INTEGRATION_STREAMING") != "1" {
		t.Skipf("INTEGRATION_STREAMING not set to 1; skipping the true incremental-delivery timing assertion — known broken under real Traefik+Yaegi (traefik/traefik#10269), not a plugin defect. Content correctness above still ran and passed. Set INTEGRATION_STREAMING=1 to re-check once upstream fixes it.")
	}

	out, err := exec.Command("docker", "compose", "-f", composeFile(), "run", "--rm", "probe").CombinedOutput()
	t.Logf("probe output:\n%s", out)
	if err != nil {
		t.Fatalf("streaming incrementality probe failed: %v", err)
	}
}

// assertStreamingContent issues one streaming chat completion against the
// mock openai upstream from the host side and verifies its SSE body
// carries the mock's 5 delta chunks — "Hel", "lo", " wor", "ld", "!" — in
// order, followed by the terminal "[DONE]" event.
func assertStreamingContent(t *testing.T) {
	t.Helper()

	reqBody := map[string]any{
		"model":    "openai/gpt-mock",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, traefik1URL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("streaming content request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming content: status = %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streaming body: %v", err)
	}
	body := string(raw)

	searchFrom := 0
	for _, want := range []string{"Hel", "lo", " wor", "ld", "!"} {
		marker := `"content":"` + want + `"`
		idx := strings.Index(body[searchFrom:], marker)
		if idx < 0 {
			t.Fatalf("streaming content: delta %q not found in order at or after byte %d of body:\n%s", want, searchFrom, body)
		}
		searchFrom += idx + len(marker)
	}
	if !strings.Contains(body[searchFrom:], "[DONE]") {
		t.Fatalf("streaming content: [DONE] not found after the final delta in body:\n%s", body)
	}
}

// flushRedis clears every key in the shared Redis so a rate-limit test
// starts from a known-empty state. Required for idempotence when re-run
// against a stack `make integration-keep` left up from a previous run —
// without this, a stale counter left over from that earlier run could
// make the very first request of this run already report 429.
func flushRedis(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-f", composeFile(), "exec", "-T", "redis", "redis-cli", "FLUSHALL").CombinedOutput()
	if err != nil {
		t.Fatalf("flush redis: %v\n%s", err, out)
	}
}

// redisKeys returns every key in the shared Redis matching pattern (a
// redis-cli KEYS glob), via the same docker compose exec pattern
// flushRedis uses above. KEYS is O(n) and blocks Redis briefly — fine
// against this suite's tiny keyspace, and used only for a one-off test
// assertion, never on a hot path.
func redisKeys(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-f", composeFile(), "exec", "-T", "redis", "redis-cli", "KEYS", pattern).CombinedOutput()
	if err != nil {
		t.Fatalf("redis-cli KEYS %s: %v\n%s", pattern, err, out)
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys
}

// TestCrossReplicaLimits covers integration test 4: bob's limited group
// (requestsPerMinute: 3) is enforced globally across both Traefik
// "replicas" via the Redis they share, regardless of which one a given
// request lands on.
func TestCrossReplicaLimits(t *testing.T) {
	flushRedis(t)

	reqBody := map[string]any{
		"model":    "openai/gpt-mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	targets := []string{traefik1URL, traefik1URL, traefik2URL, traefik2URL}

	var statuses []int
	var sawRetryAfter bool
	for _, base := range targets {
		resp, body := doJSON(t, http.MethodPost, base+"/v1/chat/completions", bobKey, reqBody)
		statuses = append(statuses, resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			if resp.Header.Get("Retry-After") != "" {
				sawRetryAfter = true
			}
		default:
			t.Fatalf("unexpected status %d for bob's request against %s, body=%#v", resp.StatusCode, base, body)
		}
	}

	var ok200, too429 int
	for _, s := range statuses {
		if s == http.StatusOK {
			ok200++
		}
		if s == http.StatusTooManyRequests {
			too429++
		}
	}
	if ok200 != 3 || too429 != 1 {
		t.Fatalf("cross-replica limit: statuses = %v, want exactly three 200s and one 429 (order-independent)", statuses)
	}
	if !sawRetryAfter {
		t.Fatalf("cross-replica limit: the 429 response carried no Retry-After header")
	}
}

// TestUsersFileHotReload covers integration test 5: appending a new user
// to the file-sourced users.json (bind-mounted read-only into both
// Traefik containers, edited here on the host) makes their key valid
// within the plugin's reload window, with no restart.
func TestUsersFileHotReload(t *testing.T) {
	usersPath := filepath.Join("users", "users.json")

	original, err := os.ReadFile(usersPath)
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(usersPath, original, 0o644); err != nil {
			t.Errorf("restore users.json: %v", err)
		}
	})

	// carol lands in "eng", not bob's "limited" group: TestCrossReplicaLimits
	// runs earlier and deliberately exhausts "limited"'s shared per-minute
	// bucket, which would make polling below flake if carol shared it.
	updated := `{"users":[` +
		`{"name":"bob","group":"limited","apiKey":"sk-int-bob"},` +
		`{"name":"carol","group":"eng","apiKey":"sk-int-carol"}` +
		`]}`
	if err := os.WriteFile(usersPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write users.json: %v", err)
	}

	// 20s, not the plugin's 5s reload throttle alone: this is a real docker
	// compose stack, not an in-process unit test, and needs margin for
	// bind-mount propagation latency and scheduler jitter on a loaded host.
	deadline := time.Now().Add(20 * time.Second)
	var lastStatus int
	for time.Now().Before(deadline) {
		resp, _ := doJSON(t, http.MethodGet, traefik1URL+"/v1/models", carolKey, nil)
		lastStatus = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("carol's key did not become valid within 20s of users.json changing (last status=%d)", lastStatus)
}

// TestPassthroughAndMCPStream covers integration test 6: native passthrough
// forwards a request with the provider's own configured key injected (the
// client's gateway key never reaches the upstream), and the MCP target
// proxy reverse-proxies an SSE stream through unmodified.
func TestPassthroughAndMCPStream(t *testing.T) {
	reqBody := map[string]any{
		"model":    "gpt-mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/openai/v1/chat/completions", aliceKey, reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("passthrough: status = %d, body=%#v", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("X-Mock-Received-Auth"), "Bearer mock-openai-key"; got != want {
		t.Errorf("passthrough: upstream received Authorization = %q, want the provider's own configured key %q, not the client's", got, want)
	}

	req, err := http.NewRequest(http.MethodGet, traefik1URL+"/mcp/tool/sse", nil)
	if err != nil {
		t.Fatalf("new mcp request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceKey)
	mcpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp target proxy request: %v", err)
	}
	defer func() { _ = mcpResp.Body.Close() }()
	if mcpResp.StatusCode != http.StatusOK {
		t.Fatalf("mcp target proxy: status = %d", mcpResp.StatusCode)
	}
	if ct := mcpResp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("mcp target proxy: Content-Type = %q, want text/event-stream", ct)
	}
	raw, err := io.ReadAll(mcpResp.Body)
	if err != nil {
		t.Fatalf("read mcp target proxy body: %v", err)
	}
	if !strings.Contains(string(raw), "mocktool") {
		t.Errorf("mcp target proxy: streamed body did not carry mocktool's content:\n%s", raw)
	}
}

// TestRealUpstreamSmoke covers integration test 7 (optional): a live round
// trip through a second middleware config carrying a keyless openai-type
// "uni" provider pointed at the operator's real gateway. Gated on
// INTEGRATION_REAL=1 and skipped, rather than failed, on any network or
// upstream problem — this is a smoke test of live infrastructure this
// suite does not own, not a correctness check on the plugin.
func TestRealUpstreamSmoke(t *testing.T) {
	if os.Getenv("INTEGRATION_REAL") != "1" {
		t.Skip("INTEGRATION_REAL not set to 1; skipping real-upstream smoke test")
	}

	reqBody := map[string]any{
		"model":    "uni/deepseek-v4-flash-0731",
		"messages": []map[string]any{{"role": "user", "content": "Say hi in one word."}},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, traefik1URL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "real.localhost" // routes through llmgateway-real-router's Host() rule
	req.Header.Set("Authorization", "Bearer sk-int-real")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("real upstream unreachable, skipping: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read real upstream response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Skipf("real upstream returned status %d, skipping (treated as upstream flake, not a plugin bug): %s", resp.StatusCode, raw)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode real upstream response: %v\nraw=%s", err, raw)
	}
	if content := firstChoiceContent(t, body); strings.TrimSpace(content) == "" {
		t.Errorf("real upstream: response content is empty, body=%#v", body)
	}
}

// --- v0.2: retry, cache, images/audio, admin (task 5) ---

// TestRetryFlakyRecovers covers v0.2 integration coverage: the "flaky"
// mock provider (dynamic.yml.tmpl) fails its first request with 503 and
// succeeds on its second; with retry.enabled (dynamic.yml.tmpl's
// top-level retry block) the client sees a single 200. The mock's own
// "mock_attempt" response field — forwarded verbatim by the openai-type
// adapter's passthrough (forwardJSON, provider_openai.go) — proves the
// retry made exactly two upstream tries, not merely that the mock
// happened to succeed on a fresh process.
//
// Resets the mock's hit counter first via POST /flaky/reset — reachable
// through the "flaky" provider's own native passthrough route
// (routes_passthrough.go), which reaches handleFlakyReset
// (integration/mock/main.go) — so this test reproduces the same
// forced-503-then-success sequence on every run, including a re-run
// against an already-up `make integration-keep` stack with no
// mockopenai restart.
func TestRetryFlakyRecovers(t *testing.T) {
	resetResp, resetBody := doJSON(t, http.MethodPost, traefik1URL+"/flaky/reset", aliceKey, nil)
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("reset flaky counter: status = %d, body=%#v", resetResp.StatusCode, resetBody)
	}

	reqBody := map[string]any{
		"model":    "flaky/flaky-mock",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("flaky chat: status = %d, want 200 (retry should have recovered the forced 503), body=%#v", resp.StatusCode, body)
	}
	if content := firstChoiceContent(t, body); content != "mock flaky response" {
		t.Errorf("flaky chat: content = %q, want %q", content, "mock flaky response")
	}
	attempt, ok := body["mock_attempt"].(float64)
	if !ok {
		t.Fatalf("flaky chat: response has no numeric mock_attempt field, body=%#v", body)
	}
	if attempt != 2 {
		t.Errorf("flaky chat: mock_attempt = %v, want 2 (one forced 503 plus one retry success = exactly two upstream hits)", attempt)
	}
}

// adminUsageEntry issues GET /admin/api/usage as adminKey and returns the
// named user's or group's raw JSON entry. kind is "users" or "groups" —
// the top-level keys adminUsageResponse (admin.go) marshals to.
func adminUsageEntry(t *testing.T, kind, id string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, http.MethodGet, traefik1URL+"/admin/api/usage", adminKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/api/usage: status = %d, body=%#v", resp.StatusCode, body)
	}
	entries, ok := body[kind].([]any)
	if !ok {
		t.Fatalf("GET /admin/api/usage: body[%q] is not an array, body=%#v", kind, body)
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if ok && entry["id"] == id {
			return entry
		}
	}
	t.Fatalf("GET /admin/api/usage: no %q entry for id %q, body=%#v", kind, id, body)
	return nil
}

// TestResponseCache covers v0.2 integration coverage: two identical chat
// completions get X-Llmgw-Cache: miss then hit, and the admin usage API
// (spec §4) proves the hit added zero tokens — only the miss's real
// upstream call did (spec §2's "cached hits increment request counters,
// never token/cost counters"). The request content is a unique string
// found nowhere else in this suite, so this test's result never depends
// on what any other test cached first.
func TestResponseCache(t *testing.T) {
	reqBody := map[string]any{
		"model":    "openai/gpt-mock",
		"messages": []map[string]any{{"role": "user", "content": "cache-probe-8f2c1a9d"}},
	}

	resp1, body1 := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, reqBody)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first (miss) request: status = %d, body=%#v", resp1.StatusCode, body1)
	}
	if got := resp1.Header.Get("X-Llmgw-Cache"); got != "miss" {
		t.Fatalf("first request: X-Llmgw-Cache = %q, want %q", got, "miss")
	}
	if content := firstChoiceContent(t, body1); content != "mock openai response" {
		t.Errorf("first request: content = %q, want %q", content, "mock openai response")
	}
	if keys := redisKeys(t, "llmgw:cache:*"); len(keys) == 0 {
		t.Error("redis holds no llmgw:cache:* key after a cache miss, want the entry the miss's SET (cache.go) should have written")
	}

	tokensAfterMiss, _ := adminUsageEntry(t, "users", "alice")["tokensPerDay"].(float64)
	if tokensAfterMiss <= 0 {
		t.Fatalf("tokensPerDay after the miss = %v, want > 0 (the mock's real usage should have been accounted)", tokensAfterMiss)
	}

	resp2, body2 := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", aliceKey, reqBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second (hit) request: status = %d, body=%#v", resp2.StatusCode, body2)
	}
	if got := resp2.Header.Get("X-Llmgw-Cache"); got != "hit" {
		t.Fatalf("second request: X-Llmgw-Cache = %q, want %q", got, "hit")
	}
	if content := firstChoiceContent(t, body2); content != "mock openai response" {
		t.Errorf("second request: content = %q, want the cached %q", content, "mock openai response")
	}

	tokensAfterHit, _ := adminUsageEntry(t, "users", "alice")["tokensPerDay"].(float64)
	if tokensAfterHit != tokensAfterMiss {
		t.Errorf("tokensPerDay changed across the cache hit: after miss = %v, after hit = %v, want unchanged", tokensAfterMiss, tokensAfterHit)
	}
}

// TestImagesGenerations covers v0.2 integration coverage: POST
// /v1/images/generations native-forwards through the mock openai
// upstream, and translates through Imagen's :predict for a gemini-routed
// model (translate_gemini_images.go).
func TestImagesGenerations(t *testing.T) {
	openaiReq := map[string]any{
		"model":  "openai/gpt-mock",
		"prompt": "a cat",
		"n":      1,
		"size":   "1024x1024",
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/v1/images/generations", aliceKey, openaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openai images: status = %d, body=%#v", resp.StatusCode, body)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("openai images: expected a non-empty data array, got %#v", body)
	}

	geminiReq := map[string]any{
		"model":  "gemini/gemini-mock",
		"prompt": "a cat",
		"n":      1,
		"size":   "1024x1024",
	}
	resp2, body2 := doJSON(t, http.MethodPost, traefik1URL+"/v1/images/generations", aliceKey, geminiReq)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("gemini images: status = %d, body=%#v", resp2.StatusCode, body2)
	}
	data2, ok := body2["data"].([]any)
	if !ok || len(data2) == 0 {
		t.Fatalf("gemini images: expected a non-empty data array translated from Imagen predictions, got %#v", body2)
	}
	first, ok := data2[0].(map[string]any)
	if !ok {
		t.Fatalf("gemini images: data[0] is not an object: %#v", data2[0])
	}
	if b64, _ := first["b64_json"].(string); b64 == "" {
		t.Errorf("gemini images: data[0].b64_json is empty, want the mock's Imagen bytesBase64Encoded translated through")
	}
}

// TestAudioSpeech covers v0.2 integration coverage: POST /v1/audio/speech
// round-trips a binary response byte-for-byte, with its Content-Type
// forwarded (audioSpeech, provider_openai.go).
func TestAudioSpeech(t *testing.T) {
	reqBody := map[string]any{
		"model": "openai/gpt-mock",
		"input": "hello",
		"voice": "alloy",
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, traefik1URL+"/v1/audio/speech", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("audio speech request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audio speech: status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("audio speech: Content-Type = %q, want %q", ct, "audio/mpeg")
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read audio speech body: %v", err)
	}
	const want = "MOCK-AUDIO-BYTES-0123456789-DISTINCTIVE-PAYLOAD"
	if string(raw) != want {
		t.Errorf("audio speech: body = %q, want the mock's exact bytes %q", raw, want)
	}
}

// TestAdminDashboard covers v0.2 integration coverage (spec §4): the
// unauthenticated HTML shell carries no data, the two JSON routes gate
// on admin vs. non-admin vs. unauthenticated, and GET /admin/api/usage
// reflects the traffic this suite has generated by the time this test
// runs — deliberately placed near the end of the file (Go runs tests in
// source order with no -shuffle in this suite's Makefile target) so
// alice's own request counters are already non-zero.
func TestAdminDashboard(t *testing.T) {
	pageReq, err := http.NewRequest(http.MethodGet, traefik1URL+"/admin", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	pageResp, err := http.DefaultClient.Do(pageReq)
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer func() { _ = pageResp.Body.Close() }()
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin (unauthenticated): status = %d, want 200", pageResp.StatusCode)
	}
	pageRaw, err := io.ReadAll(pageResp.Body)
	if err != nil {
		t.Fatalf("read /admin body: %v", err)
	}
	if !strings.Contains(string(pageRaw), "LLM Gateway") {
		t.Errorf("GET /admin: body does not look like the dashboard shell:\n%s", pageRaw)
	}
	for _, key := range []string{aliceKey, bobKey, adminKey} {
		if strings.Contains(string(pageRaw), key) {
			t.Errorf("GET /admin: unauthenticated shell must carry no data of its own, but the body contains a live API key %q", key)
		}
	}

	overviewResp, overviewBody := doJSON(t, http.MethodGet, traefik1URL+"/admin/api/overview", adminKey, nil)
	if overviewResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/api/overview (admin): status = %d, body=%#v", overviewResp.StatusCode, overviewBody)
	}
	providers, ok := overviewBody["providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("GET /admin/api/overview: expected a non-empty providers list, got %#v", overviewBody)
	}

	nonAdminResp, nonAdminBody := doJSON(t, http.MethodGet, traefik1URL+"/admin/api/overview", aliceKey, nil)
	if nonAdminResp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /admin/api/overview (non-admin key): status = %d, want 403, body=%#v", nonAdminResp.StatusCode, nonAdminBody)
	}

	aliceUsage := adminUsageEntry(t, "users", "alice")
	if reqDay, _ := aliceUsage["requestsPerDay"].(float64); reqDay <= 0 {
		t.Errorf("alice requestsPerDay = %v, want > 0 (this suite's own earlier traffic)", reqDay)
	}
	adminUsage := adminUsageEntry(t, "users", "admin")
	if reqDay, _ := adminUsage["requestsPerDay"].(float64); reqDay <= 0 {
		t.Errorf("admin requestsPerDay = %v, want > 0 (this test's own admin API calls count too)", reqDay)
	}
}
