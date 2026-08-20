//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dynamicConfigPath returns the path (relative to this package's directory,
// also `go test`'s working directory) to the rendered dynamic config
// Traefik's directory-based file provider watches — the whole directory
// docker-compose.yml bind-mounts into both traefik1 and traefik2 at
// /etc/traefik/dynamic (traefik.yml's providers.file.directory), rendered
// from dynamic.yml.tmpl by `make integration-up`/`integration-keep`.
func dynamicConfigPath() string {
	return filepath.Join("traefik", "dynamic", "dynamic.yml")
}

// doJSONNonFatal is doJSON's non-fatal counterpart: every error (marshal,
// request construction, transport, or body read) is returned to the
// caller instead of calling t.Fatalf. It exists for the two places in this
// file that poll in a loop — waitForModelAliasBounded and
// settleToBaseline — where a single transient failure (a request landing
// mid-rebuild, a dropped connection) must reset that poll's own retry
// state and try again, never abort the whole test. A non-JSON or empty
// body decodes to a nil map, matching doJSON's own contract.
func doJSONNonFatal(method, url, apiKey string, body any) (*http.Response, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("new request %s %s: %w", method, url, err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("do request %s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read response body from %s %s: %w", method, url, err)
	}
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp, out, nil
}

// modelIDSet extracts the set of model ids from a GET /v1/models response
// body's "data" array. A body that does not decode as expected yields an
// empty set, never a panic — callers compare sets, so a malformed body
// simply fails to match rather than crashing the poll loop it runs inside.
func modelIDSet(body map[string]any) map[string]bool {
	ids := make(map[string]bool)
	data, ok := body["data"].([]any)
	if !ok {
		return ids
	}
	for _, entry := range data {
		if m, ok := entry.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids[id] = true
			}
		}
	}
	return ids
}

// sameModelIDSet reports whether a and b contain exactly the same ids.
func sameModelIDSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

// settleConsecutivePolls is how many consecutive matching polls
// settleToBaseline requires before declaring the live config settled —
// one lucky poll landing between two still-in-flight rebuilds is not
// enough evidence the config has actually stopped changing.
const settleConsecutivePolls = 3

// settleToBaseline polls GET /v1/models (roughly 1s apart) until it
// observes baseline's exact model id set on settleConsecutivePolls
// consecutive polls, or bound elapses without ever reaching that streak;
// it returns whether it settled.
//
// TestConfigHotReload's cleanup calls this unconditionally, on both
// outcomes of the earlier alias poll. On the fired path it is a real
// guard: two rebuilds are known to have been triggered (the original
// add, then this cleanup's own restore), and settling confirms both
// landed before cleanup returns. On the skip path it is best effort
// only — if the original add's rebuild is still pending past the 30s
// alias-poll bound, or never fires at all, no amount of polling here can
// prove that; it can only improve the odds that a still-in-flight
// rebuild resolves before this test hands control back.
//
// Waiting here is still required on the skip path, not a no-op: an
// earlier version of this cleanup polled only for the injected alias to
// DISAPPEAR, which trivially and instantly "succeeded" on that path (the
// alias was never live, so "not present" was already true on the first
// check) — a guaranteed-zero-wait bug, found when it raced
// TestUsersFileHotReload's own non-atomic write to users.json during
// verification: a rebuild this test's own restore triggered landed
// later, mid-write, read a torn file, and failed plugin construction
// outright — a global 404 outage until an unrelated later config change
// triggered a successful rebuild.
func settleToBaseline(baseURL, apiKey string, baseline map[string]bool, bound time.Duration) bool {
	consecutive := 0
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		resp, body, err := doJSONNonFatal(http.MethodGet, baseURL+"/v1/models", apiKey, nil)
		if err == nil && resp.StatusCode == http.StatusOK && sameModelIDSet(modelIDSet(body), baseline) {
			consecutive++
			if consecutive >= settleConsecutivePolls {
				return true
			}
		} else {
			consecutive = 0
		}
		time.Sleep(time.Second)
	}
	return false
}

// runChatLoop issues a POST /v1/chat/completions against baseURL for model
// every interval, from goroutine start until stop is closed, then closes
// done. Every response whose status is not 200, and every request-level
// error, is appended to *failures under mu's protection.
//
// This never calls any *testing.T method: it runs on its own goroutine, and
// testing.T.Fatal/FailNow "must be called from the goroutine running the
// test... not from other goroutines created during the test" (testing
// package doc) — a violation silently corrupts the test result rather than
// failing loudly. Every outcome is instead recorded into failures and
// asserted from the test's own goroutine after the loop stops.
func runChatLoop(baseURL, apiKey, model string, interval time.Duration, stop <-chan struct{}, done chan<- struct{}, mu *sync.Mutex, failures *[]string) {
	defer close(done)

	reqBody, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hot-reload-probe"}},
	})
	if err != nil {
		mu.Lock()
		*failures = append(*failures, fmt.Sprintf("marshal probe request body: %v", err))
		mu.Unlock()
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(reqBody))
			if err != nil {
				mu.Lock()
				*failures = append(*failures, fmt.Sprintf("build probe request: %v", err))
				mu.Unlock()
				continue
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				*failures = append(*failures, fmt.Sprintf("probe request error: %v", err))
				mu.Unlock()
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				mu.Lock()
				*failures = append(*failures, fmt.Sprintf("probe request: status %d", resp.StatusCode))
				mu.Unlock()
			}
		}
	}
}

// waitForModelAliasBounded polls GET /v1/models on baseURL with apiKey
// until an entry with id wantID appears, returning true — or false once
// timeout elapses without ever seeing it. Unlike waitForModelAlias
// (integration_test.go), a timeout here is not itself a test failure:
// TestConfigHotReload uses the bool to choose between asserting success and
// t.Skipf-ing a known, host-dependent limitation (the directory watch not
// firing within this bound). Uses doJSONNonFatal, not doJSON: a transient
// transport error during this poll must be treated as "try again", not as
// an immediate hard failure of the whole test.
func waitForModelAliasBounded(t *testing.T, baseURL, apiKey, wantID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, body, err := doJSONNonFatal(http.MethodGet, baseURL+"/v1/models", apiKey, nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			if data, ok := body["data"].([]any); ok {
				for _, entry := range data {
					if m, ok := entry.(map[string]any); ok && m["id"] == wantID {
						return true
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// TestConfigHotReload is item A's proof-and-parity test: Traefik's
// directory-based file provider (traefik.yml's providers.file.directory,
// switched from a single-file .filename specifically because a single
// file's fsnotify watch never fires under Docker Desktop's VirtioFS bind
// mount) must pick up a config change with no blip in service, and — when
// it does — the newly added config must actually be live.
//
// Every request this test issues — the baseline chat, the sustained probe
// loop, both /v1/models polls, and the final aliased-model chat — uses
// hotReloadProbeKey, a dedicated user in its own "hotreload" group with a
// 100000/min limit (dynamic.yml.tmpl), never aliceKey/"eng". "eng" is
// shared by most of this suite's other tests and carries a much lower
// limit (1000/min); this test's own loop alone can burn several hundred
// requests in one run, and a 429 from that shared budget being exhausted
// would be indistinguishable, to this test's own "zero non-200s"
// assertion, from a genuine reload-caused blip.
//
// Sequence:
//  1. Baseline: an existing model answers 200, and the live /v1/models
//     listing is captured as the "baseline" model id set — the value
//     step 6 below polls for once the config is restored.
//  2. A sustained ~20rps request loop against that same model starts,
//     recording every non-200 (runChatLoop above).
//  3. While the loop runs, the rendered dynamic config is rewritten to add
//     a new model alias ("aliased/hot": "gpt-mock"), written to a temp
//     file in the same directory then os.Rename'd over the target — an
//     atomic swap mirroring a Kubernetes ConfigMap mount's symlink swap,
//     never an in-place truncate+rewrite a watcher could observe
//     half-written.
//  4. GET /v1/models is polled (bounded 30s) for "aliased/hot" to appear —
//     this is the directory watch actually firing.
//  5. The loop stops. The no-blip property — zero non-200s across the
//     entire rewrite/propagation window — is asserted unconditionally,
//     whether or not the watch fired within 30s. On the fired path this is
//     a genuine no-blip-under-reload result; on the skip path it proves
//     only that the stack survived an unreloaded rewrite attempt (see the
//     Skip message below for why that distinction matters).
//  6. Only if the watch fired: a chat request through the newly live
//     "aliased/hot" alias must succeed (200) — proving the config that
//     actually landed is usable, not just present in the listing.
//
// t.Cleanup restores the original rendered config content atomically, the
// same way step 3 writes it, then unconditionally polls (settleToBaseline)
// until the live /v1/models listing matches the captured baseline again —
// see settleToBaseline's own doc comment for the outage this specifically
// prevents.
func TestConfigHotReload(t *testing.T) {
	baseReq := map[string]any{
		"model":    "openai/gpt-mock",
		"messages": []map[string]any{{"role": "user", "content": "baseline"}},
	}
	resp, body := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", hotReloadProbeKey, baseReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("baseline chat: status = %d, body=%#v", resp.StatusCode, body)
	}

	baselineResp, baselineBody, err := doJSONNonFatal(http.MethodGet, traefik1URL+"/v1/models", hotReloadProbeKey, nil)
	if err != nil || baselineResp.StatusCode != http.StatusOK {
		t.Fatalf("capture baseline model set: status=%v err=%v", baselineResp, err)
	}
	baselineIDs := modelIDSet(baselineBody)
	if len(baselineIDs) == 0 {
		t.Fatalf("baseline /v1/models returned no models, body=%#v; cannot verify settle later", baselineBody)
	}

	configPath := dynamicConfigPath()
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read rendered dynamic config %q: %v", configPath, err)
	}
	t.Cleanup(func() {
		tmpPath := configPath + ".restore.tmp"
		if err := os.WriteFile(tmpPath, original, 0o644); err != nil {
			t.Errorf("write restore temp config %q: %v", tmpPath, err)
			return
		}
		if err := os.Rename(tmpPath, configPath); err != nil {
			t.Errorf("rename restore temp config over %q: %v", configPath, err)
			return
		}
		if !settleToBaseline(traefik1URL, hotReloadProbeKey, baselineIDs, 30*time.Second) {
			t.Logf("cleanup: /v1/models did not settle back to the baseline model set within 30s of restoring the config — a later test may race a still-pending rebuild")
		}
	})

	// Sustained request loop: started before the config mutation below and
	// stopped only after the poll resolves, so it spans the entire
	// rewrite/propagation window, not just the moment of the rename.
	stop := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var failures []string
	go runChatLoop(traefik1URL, hotReloadProbeKey, "openai/gpt-mock", 50*time.Millisecond, stop, done, &mu, &failures)

	const anchor = `aliased/claude: "claude-mock"`
	if !strings.Contains(string(original), anchor) {
		close(stop)
		<-done
		t.Fatalf("rendered dynamic config does not contain the expected modelAliases anchor %q — dynamic.yml.tmpl may have changed; update this test's injection point", anchor)
	}
	updated := strings.Replace(string(original), anchor, anchor+"\n            aliased/hot: \"gpt-mock\"", 1)

	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(updated), 0o644); err != nil {
		close(stop)
		<-done
		t.Fatalf("write temp config %q: %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		close(stop)
		<-done
		t.Fatalf("rename %q over %q: %v", tmpPath, configPath, err)
	}

	fired := waitForModelAliasBounded(t, traefik1URL, hotReloadProbeKey, "aliased/hot", 30*time.Second)

	close(stop)
	<-done

	mu.Lock()
	failCount := len(failures)
	sample := append([]string(nil), failures...)
	mu.Unlock()
	if failCount != 0 {
		t.Errorf("sustained request loop saw %d non-200/error result(s) during the config rewrite window, want 0: %v", failCount, sample)
	}

	if !fired {
		t.Skipf("directory-based file provider watch did not surface the new alias within 30s on this host (a VirtioFS/Docker Desktop fsnotify propagation limitation, not a plugin defect). The zero-failures result above proves only that the stack stayed healthy through an unreloaded rewrite attempt — it is NOT evidence that a successful reload is blip-free, since no reload actually happened on this run")
	}

	hotReq := map[string]any{
		"model":    "aliased/hot",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	hotResp, hotBody := doJSON(t, http.MethodPost, traefik1URL+"/v1/chat/completions", hotReloadProbeKey, hotReq)
	if hotResp.StatusCode != http.StatusOK {
		t.Errorf("chat via the newly hot-reloaded alias: status = %d, body=%#v", hotResp.StatusCode, hotBody)
	}
	if content := firstChoiceContent(t, hotBody); content != "mock openai response" {
		t.Errorf("chat via the newly hot-reloaded alias: content = %q, want %q", content, "mock openai response")
	}
}
