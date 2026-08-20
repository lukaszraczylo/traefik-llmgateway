//go:build integration

// Package integration exercises the llmgateway Traefik plugin against a
// real Traefik v3.5 binary loading the plugin via localPlugins, real mock
// upstreams, and a real shared Redis, all orchestrated by
// integration/docker-compose.yml. It assumes the compose stack is already
// up and healthy — the `integration` Makefile target starts it, waits for
// traefik1 to answer, runs this package, then tears the stack down; run it
// directly with `go test -tags integration ./...` from this directory
// after `docker compose up -d --build` if you want to keep the stack alive
// between runs (see the `integration-keep` Makefile target).
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

// TestUnifiedChatOpenAIAndAnthropic covers integration test 2: a unified
// chat completion against the mock OpenAI upstream (passed through
// verbatim) and against the mock Anthropic upstream (translated into the
// same OpenAI-shaped envelope, with the client's requested provider-alias
// echoed back in "model").
func TestUnifiedChatOpenAIAndAnthropic(t *testing.T) {
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
}

// TestStreamingIncrementalityProbe covers integration test 3. Per the
// design spec's Appendix A spike lesson, streaming timing must never be
// judged through the Docker Desktop host port-proxy (it buffers) — so this
// runs the timing assertion as a one-shot container inside the compose
// network (`docker compose run --rm probe`), reading its exit code rather
// than making any timing claim from this (host-side) process itself.
func TestStreamingIncrementalityProbe(t *testing.T) {
	out, err := exec.Command("docker", "compose", "-f", composeFile(), "run", "--rm", "probe").CombinedOutput()
	t.Logf("probe output:\n%s", out)
	if err != nil {
		t.Fatalf("streaming incrementality probe failed: %v", err)
	}
}

// TestCrossReplicaLimits covers integration test 4: bob's limited group
// (requestsPerMinute: 3) is enforced globally across both Traefik
// "replicas" via the Redis they share, regardless of which one a given
// request lands on.
func TestCrossReplicaLimits(t *testing.T) {
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

	deadline := time.Now().Add(10 * time.Second)
	var lastStatus int
	for time.Now().Before(deadline) {
		resp, _ := doJSON(t, http.MethodGet, traefik1URL+"/v1/models", carolKey, nil)
		lastStatus = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("carol's key did not become valid within 10s of users.json changing (last status=%d)", lastStatus)
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
