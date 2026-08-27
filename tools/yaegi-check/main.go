// Command yaegi-check replicates the Traefik Plugin Catalog analyzer
// (piceus)'s yaegiMiddlewareCheck for this plugin. It loads the plugin
// package under Yaegi exactly as the catalog and Traefik's own plugin
// loader do: interpret the source tree, evaluate CreateConfig(), decode
// the plugin's real .traefik.yml testData into the resulting Config
// value, then call the plugin's New constructor by reflection from
// outside the interpreter. It then exercises the returned handler with
// two real HTTP requests, proving ServeHTTP, auth, the model registry,
// statusTrackingWriter, and the gateway's logger all run under Yaegi —
// not merely that the package's imports resolve.
//
// Any failure — a Yaegi-incompatible construct in the plugin's stdlib
// usage, a constructor error, a nil handler, or a wrong response from the
// live handler — exits non-zero: this is the gate the plugin must pass
// before it can list in the Traefik Plugin Catalog.
//
// This module is intentionally separate from the plugin module
// (llmgw-yaegi-check, not github.com/lukaszraczylo/traefik-llmgateway):
// this harness's own dependencies (yaegi itself) must never become the
// plugin's, and nothing here may be reachable from interpreted code.
//
// The plugin module carries exactly ONE non-stdlib runtime dependency,
// github.com/lukaszraczylo/oss-telemetry, which is vendored and therefore
// interpretable — Yaegi resolves it out of vendor/ in the GOPATH copy,
// the same way Traefik resolves it from the cloned plugin tree. That is
// why vendor/ is no longer excluded below. Every OTHER runtime import
// must stay stdlib-only: an unvendored third-party import breaks under
// interpretation even though `go build` never notices, and this harness
// is what catches it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// testDataUserAPIKey and testDataWantModel are drawn directly from the
// plugin's own .traefik.yml testData block (users.inline[0].apiKey and
// providers.openai.models[0]) — see exerciseHandler.
//
// attemptAccountingAdminAPIKey names the second, admin-flagged user run()
// layers on top of testData (see its own override comment) — SHOULD-4
// (v0.22 review round): this harness proves recordProviderAttempt's
// SHOULD-1 deadline/cancel classification (limits.go's matchesSentinel/
// isDeadlineExceeded) actually runs correctly INTERPRETED, not merely
// compiled: a bug there would not show up under `go test`, only here —
// which is exactly what happened in round 2 (see slowProviderName's own
// doc comment immediately below, and matchesSentinel's in limits.go, for
// the full account of the interpreter-only bug this harness caught).
//
// slowProviderName/slowProviderModel/slowUpstreamSleep/
// slowRequestDeadline back exerciseAttemptAccounting's SECOND request
// (SHOULD-A, v0.22 review round, round 2): the FIRST version of this
// harness only ever drove an upstream that answers instantly, so
// isDeadlineExceeded never actually saw a non-nil error, interpreted or
// otherwise — a comment claiming to prove the interpreted deadline path
// while never exercising it. Once fixed to actually drive a timeout, it
// caught a real bug: round 2's original matchesSentinel (a hand-rolled
// Unwrap walk against two locally declared interfaces, comma-ok matched)
// passed every compiled `go test` row for a double-%w error shape but
// silently misclassified it under the real interpreter — an interpreted
// interface type failing to match a compiled concrete value's method
// set. matchesSentinel now calls real errors.Is instead (limits.go's own
// doc comment has the full account of why that is safe here
// specifically). The second request below: a second, DELIBERATELY slow
// provider, addressed by its own provider-prefixed model id
// (`slowProviderName+"/"+slowProviderModel`, routableModelId's own
// "provider/model" convention) so it can never collide with
// testDataWantModel's bare "gpt-test" on the "openai" provider, driven
// with a request context deadline shorter than the upstream's own
// sleep — the exact shape the route-level Go test regression (routes_
// unified_test.go's TestHandleChat_ContextDeadlineExceeded_
// RecordsProviderFailure) already proves compiled; this proves it
// interpreted.
const (
	testDataUserAPIKey           = "test-user-key"
	testDataWantModel            = "gpt-test"
	attemptAccountingAdminAPIKey = "sk-admin1"
	slowProviderName             = "slow"
	slowProviderModel            = "gpt-test"
	slowUpstreamSleep            = 600 * time.Millisecond
	slowRequestDeadline          = 60 * time.Millisecond
	// yaegiMetaContextTokens is the modelMeta config-override
	// contextTokens value the attempt-accounting harness sets for
	// testDataWantModel (feature v0.23) — a string, not an int, since it
	// is spliced directly into attemptAccountingOverride's hand-built
	// JSON text below.
	yaegiMetaContextTokens = "128000"
)

// timeoutProviderName/timeoutProviderModel/timeoutProviderRequestTimeout/
// timeoutUpstreamSleep/timeoutProbeMaxWait back exerciseRequestTimeout
// (feature: request timeout): a provider whose own ProviderConfig.
// RequestTimeout ("80ms") is far shorter than timeoutUpstream's
// deliberate 600ms silence before it ever writes a byte. Distinct from
// slowProviderName/slowRequestDeadline above (SHOULD-A): that probe
// proves a CLIENT-set context deadline is classified correctly;
// timeoutProviderName proves the GATEWAY'S OWN adapter-level timeout
// (newAdapterHTTPClient's Transport.ResponseHeaderTimeout, providers.go;
// watchdogBody, timeout.go) aborts a hang under a request that sets NO
// deadline of its own — the shape of a real, unbounded production
// client — interpreted, not merely compiled.
const (
	timeoutProviderName = "hungtimeout"
	// timeoutProviderModel is deliberately NOT "gpt-test"
	// (testDataWantModel): a bare model id shared across providers
	// resolves to exactly one winner (modelRegistry's own bare-id rule),
	// and reusing testDataWantModel here made "hungtimeout" win it
	// instead of "openai" — silently hijacking exerciseAttemptAccounting's
	// own bare-"gpt-test" request onto this hung provider and failing
	// that unrelated probe. A unique id keeps this provider's own model
	// space from ever colliding with another probe's.
	timeoutProviderModel          = "gpt-timeout-test"
	timeoutProviderRequestTimeout = "80ms"
	timeoutUpstreamSleep          = 600 * time.Millisecond
	// timeoutProbeMaxWait bounds exerciseRequestTimeout's own assertion:
	// generous relative to timeoutProviderRequestTimeout (80ms), but far
	// under timeoutUpstreamSleep (600ms) — a pass proves the request was
	// aborted BY the timeout feature, not by timeoutUpstream eventually
	// answering on its own.
	timeoutProbeMaxWait = 3 * time.Second
)

// hungBodyProviderName/hungBodyProviderModel/hungBodyProviderRequestTimeout/
// hungBodyUpstreamSleep back exerciseRequestTimeoutMidBodyStall (finding
// F7, coordinator adversarial review, 2026-08-23): timeoutProviderName
// above never sends headers at all, so it only exercises Transport.
// ResponseHeaderTimeout — watchdogBody.fire (timeout.go) — the
// interpreted method value handed to time.AfterFunc, plus sync/atomic's
// function API and a context.CancelFunc — never runs under that probe.
// hungBodyUpstream sends headers immediately, then goes silent, so the
// body-read path (upstreamBytes' watchdogBody wrap, providers.go) is
// what has to abort it, proving the OTHER half of this feature
// interpreted.
const (
	hungBodyProviderName = "hungbody"
	// hungBodyProviderModel: same reasoning as timeoutProviderModel's own
	// doc comment above — must not collide with testDataWantModel or any
	// other probe's model id.
	hungBodyProviderModel          = "gpt-bodystall-test"
	hungBodyProviderRequestTimeout = "80ms"
	hungBodyUpstreamSleep          = 600 * time.Millisecond
)

// provenanceEstProviderName/provenanceEstProviderModel back
// exerciseUsageProvenance (feat: expose token-accounting provenance): a
// provider whose upstream answers 200 with NO "usage" field at all —
// decodes to chatUsagePayload's zero value (provider_openai.go's
// forwardJSON), the exact shape runUnified's own estimation fallback
// (routes_unified.go) classifies as "estimated" — proving the
// substitution math (ceil(len(body)/4)) and the new
// llmgateway_usage_provenance_requests_total/-_tokens_total families
// (metrics.go) all run correctly under the REAL interpreter, not merely
// compiled. "reported" is already exercised, under the interpreter, by
// exerciseAttemptAccounting's own real, usage-bearing "openai" traffic
// above — no separate fixture needed for that provenance here.
const (
	provenanceEstProviderName = "provenance-est"
	// provenanceEstProviderModel: same reasoning as timeoutProviderModel's
	// own doc comment above — must not collide with testDataWantModel or
	// any other probe's model id.
	provenanceEstProviderModel = "gpt-provenance-est-test"
)

// excludedTopLevelDirs lists repo-root directories the GOPATH copy must
// never include: build tooling, integration fixtures, planning docs and
// VCS metadata have nothing to do with the plugin package Yaegi imports.
//
// vendor/ USED TO be excluded here, on the reasoning that it held only
// the test-only testify dependency and _test.go files are never
// interpreted. That stopped being true when the plugin took on
// oss-telemetry as a runtime dependency: leaving vendor/ out would make
// the interpreted copy unable to resolve an import the real plugin tree
// resolves fine, so this harness would fail on code that works in
// production — or, worse, quietly stop representing it. Copying the whole
// vendor tree, nested "go.yaml.in"-style module dirs included, resolves
// correctly under yaegi v0.16.1 (verified: the gate passes with the
// telemetry import live and reached).
//
// webui/ (Vue admin-panel task) is still excluded: its own go.mod already
// walls it off from the repo root's `go build`/`vet`/`test ./...`, and
// its node_modules tree — hundreds of MB,
// including a stray bundled .go file
// (node_modules/flatted/golang/pkg/flatted.go) — would otherwise both
// balloon copyRepoSource's wall time and risk Yaegi tripping over code
// this repo neither wrote nor can build. The plugin only ever imports the
// generated admin_assets_gen.go at the repo root, never anything under
// webui/ itself.
var excludedTopLevelDirs = map[string]bool{
	"tools":        true,
	"integration":  true,
	".superpowers": true,
	"docs":         true,
	".git":         true,
	"webui":        true,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "yaegi-check: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("yaegi-check: OK")
}

// run resolves the plugin's repo root (os.Args[1], defaulting to the
// current directory), builds a temporary GOPATH containing a copy of its
// source, interprets the plugin package under Yaegi, and exercises the
// constructed handler. It returns the first error encountered, wrapped
// with enough context to diagnose which stage failed.
func run() error {
	repoRoot := "."
	if len(os.Args) > 1 {
		repoRoot = os.Args[1]
	}
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}

	modulePath, err := readModulePath(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	gopath, err := os.MkdirTemp("", "llmgw-yaegi-check-*")
	if err != nil {
		return fmt.Errorf("create temp GOPATH: %w", err)
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // best-effort cleanup of a temp dir

	pkgDir := filepath.Join(gopath, "src", modulePath)
	if err = copyRepoSource(repoRoot, pkgDir); err != nil {
		return fmt.Errorf("copy repo source into GOPATH: %w", err)
	}

	// Suppress telemetry BEFORE anything is interpreted, and keep it
	// suppressed for the whole run. stampReleaseVersion below makes the
	// interpreted copy look like a stamped release build so that New's
	// telemetry call is actually REACHED under Yaegi; without these env
	// vars that would fire a real ping at the public ingest endpoint from
	// every CI run and every developer's `make yaegi-check`. Both names
	// are set because either one alone suppresses, so a rename upstream
	// cannot silently re-enable it.
	if err = os.Setenv("DO_NOT_TRACK", "1"); err != nil {
		return fmt.Errorf("set DO_NOT_TRACK: %w", err)
	}
	if err = os.Setenv("OSS_TELEMETRY_DISABLED", "1"); err != nil {
		return fmt.Errorf("set OSS_TELEMETRY_DISABLED: %w", err)
	}
	if err = stampReleaseVersion(pkgDir); err != nil {
		return fmt.Errorf("stamp a release version into the interpreted copy: %w", err)
	}

	pkgName, err := readPackageName(pkgDir)
	if err != nil {
		return fmt.Errorf("read plugin package name: %w", err)
	}

	i := interp.New(interp.Options{GoPath: gopath})
	if err = i.Use(stdlib.Symbols); err != nil {
		return fmt.Errorf("interp.Use(stdlib.Symbols): %w", err)
	}
	if _, err = i.Eval(`import "` + modulePath + `"`); err != nil {
		return fmt.Errorf("import plugin package under yaegi: %w", err)
	}

	cfgVal, err := i.Eval(pkgName + ".CreateConfig()")
	if err != nil {
		return fmt.Errorf("call %s.CreateConfig() under yaegi: %w", pkgName, err)
	}
	if !cfgVal.CanInterface() {
		return fmt.Errorf("%s.CreateConfig() result cannot be used via reflect.Value.Interface", pkgName)
	}

	testData, err := extractTraefikYAMLTestData(filepath.Join(repoRoot, ".traefik.yml"))
	if err != nil {
		return fmt.Errorf("extract .traefik.yml testData: %w", err)
	}
	testDataJSON, err := json.Marshal(testData)
	if err != nil {
		return fmt.Errorf("marshal testData: %w", err)
	}
	if err = json.Unmarshal(testDataJSON, cfgVal.Interface()); err != nil {
		return fmt.Errorf("decode .traefik.yml testData into the interpreted Config: %w", err)
	}

	// Feature A (v0.22) attempt-accounting harness (SHOULD-4, review
	// round): a second json.Unmarshal into the same cfgVal layers this
	// override on top of testData's own decode, rather than replacing it
	// outright — encoding/json's own map/struct-merge semantics keep
	// every testData field this override does not mention (Groups.default
	// in particular, which .traefik.yml's own testData.groups block
	// already provides and this override never touches). It replaces
	// providers.openai wholesale (a real map key, not merged field-by-
	// field) with one pointed at a local httptest upstream so the harness
	// can drive a real chat completion without a live provider, and adds
	// a second, admin-flagged user (Users.Inline is a slice — fully
	// replaced, not merged, which is why the "tester" user from testData
	// is respecified here too, identical apiKey/group, rather than
	// silently dropped).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// slowUpstream never answers within slowRequestDeadline — see
	// slowProviderName's own doc comment above (SHOULD-A) for why this
	// second provider exists at all.
	slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowUpstreamSleep)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer slowUpstream.Close()

	// timeoutUpstream writes nothing at all for timeoutUpstreamSleep,
	// well past timeoutProviderName's own 80ms ProviderConfig.
	// RequestTimeout (attemptAccountingOverride below) — see the
	// timeoutProviderName const block's own doc comment for why this
	// probe exists alongside slowUpstream.
	timeoutUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(timeoutUpstreamSleep)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c4","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer timeoutUpstream.Close()

	// hungBodyUpstream sends headers immediately (so ResponseHeaderTimeout
	// is satisfied and never fires), then goes silent for
	// hungBodyUpstreamSleep before ever writing a body byte — well past
	// hungBodyProviderName's own 80ms ProviderConfig.RequestTimeout. See
	// the hungBodyProviderName const block's own doc comment (finding F7)
	// for why this probe exists alongside timeoutUpstream, not instead of
	// it.
	hungBodyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(hungBodyUpstreamSleep)
		_, _ = w.Write([]byte(`{"id":"c5","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer hungBodyUpstream.Close()

	// provenanceEstUpstream backs exerciseUsageProvenance (feat: expose
	// token-accounting provenance): answers 200 with NO "usage" field at
	// all — see provenanceEstProviderName's own const block doc comment
	// for why this is the exact shape runUnified's estimation fallback
	// classifies as "estimated".
	provenanceEstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c-prov","object":"chat.completion","model":"` + provenanceEstProviderModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer provenanceEstUpstream.Close()

	// mcpProbeUpstream answers federation's outbound tools/call with a
	// body deliberately larger than mcpBackendCallResponseMaxBytes, so
	// exerciseHandler's POST /mcp probe below drives doBackendJSONRPC's
	// oversize branch — and therefore its errors.Is(err,
	// errMCPResponseTooLarge) match — under the REAL interpreter.
	//
	// This exists because a security review round objected that no gate
	// exercised federation's tools/call path interpreted, which left
	// errors.Is there justified by reasoning rather than by evidence.
	// Reasoning is exactly what the Yaegi traps in this codebase have
	// repeatedly defeated, so the path is now covered instead of argued.
	mcpProbeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"`))
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for written := 0; written <= mcpProbeOversizeBytes; written += len(chunk) {
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}]}}`))
	}))
	defer mcpProbeUpstream.Close()
	// anthropicProbeUpstream answers an Anthropic Messages API call with a
	// real Anthropic-shaped body, so the /v1/messages passthrough branch
	// (callAnthropicMessagesPassthrough) runs interpreted. A body
	// containing "make-it-fail" gets a 429, driving
	// handleAdapterErrorEnvelope's *providerHTTPError branch interpreted
	// too — the exact construct class that has broken under Yaegi before
	// (see handleAdapterError's own doc comment, routes_unified.go).
	anthropicProbeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("make-it-fail")) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		if r.Header.Get("anthropic-version") == "" || r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"missing anthropic headers"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_probe","type":"message","role":"assistant","model":"claude-upstream-real","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`))
	}))
	defer anthropicProbeUpstream.Close()

	// builtinLookupContextTokens (review fix, SHOULD-6): read directly
	// out of the generated pricing_data_gen.go rather than hardcoding a
	// number, so this assertion self-updates across a `make
	// pricing-sync` regeneration instead of pinning a value that could
	// legitimately drift when LiteLLM's own upstream data changes.
	builtinLookupContextTokens, err := readBuiltinContextTokens(repoRoot, builtinLookupModelID)
	if err != nil {
		return fmt.Errorf("read builtin context tokens for %q from pricing_data_gen.go: %w", builtinLookupModelID, err)
	}

	// --- breaker probe (feat/provider-health): a provider whose
	// /v1/models 401s, exactly the xiaomi production case, driven under
	// the REAL interpreter. brkHealthy flips the upstream from always-401
	// to serving real data once the probe wants to observe recovery;
	// brkModelsHits counts every /v1/models hit so exerciseBreaker can
	// prove the open window actually suppresses discovery traffic (a
	// count staying flat), not merely that healthState reads "open".
	brkModelsHits := new(int64)
	brkHealthy := new(int64)
	brkUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/models") {
			atomic.AddInt64(brkModelsHits, 1)
			if atomic.LoadInt64(brkHealthy) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"brk-model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c3","object":"chat.completion","model":"brk-model","choices":[],"usage":{}}`))
	}))
	defer brkUpstream.Close()

	// failoverAUpstream/failoverBUpstream back exerciseFailover (feat/
	// failover, below): two providers configured with the IDENTICAL bare
	// model id (failoverModelID) — the exact shape registry.go's bareWinner
	// collision handles, and the shape production logs show for whisper-1/
	// tts-1/tts-1-hd/the embeddings models across openai/openai-audio/
	// copilot. A always answers 500; B always answers 200. This proves the
	// failover candidate loop (runMeteredCall, routes_unified.go) falls
	// through and serves correctly under the REAL interpreter, not merely
	// compiled — this package's own doc comment is exactly why that
	// distinction matters on this codebase.
	failoverAUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"failover-a is down","type":"server_error"}}`))
	}))
	defer failoverAUpstream.Close()
	failoverBHits := new(int64)
	failoverBUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(failoverBHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c-failover","object":"chat.completion","model":"` + failoverModelID + `","choices":[{"index":0,"message":{"role":"assistant","content":"served by failover-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer failoverBUpstream.Close()

	// bareWinnerAUpstream/bareWinnerBUpstream back exerciseBareWinnerGroupAware
	// (fix/group-aware-bare-winner, 2026-08-27): two providers configured
	// with the IDENTICAL bare model id (bareWinnerModelID) — the same
	// collision shape failoverAUpstream/failoverBUpstream above drive, but
	// exercising the bareWinner AUTHORIZATION fix, not failover. A must
	// NEVER be hit: bareWinnerGroupName can only use "bare-winner-b", so a
	// correct group-aware bareWinner never even considers "bare-winner-a"
	// as a candidate for this group's request, unlike a failover fall-
	// through (which WOULD hit A first). bareWinnerAHits catches a
	// regression back to the pre-fix, group-blind selection, which would
	// route here and get an error response instead of the expected 200.
	bareWinnerAHits := new(int64)
	bareWinnerAUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(bareWinnerAHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"bare-winner-a must never be reached by bareWinnerGroupName","type":"server_error"}}`))
	}))
	defer bareWinnerAUpstream.Close()
	bareWinnerBUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c-bare-winner","object":"chat.completion","model":"` + bareWinnerModelID + `","choices":[{"index":0,"message":{"role":"assistant","content":"served by bare-winner-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer bareWinnerBUpstream.Close()

	// failover.enabled must be set explicitly here: FailoverConfig.Enabled
	// defaults to false (coordinator ruling — a version upgrade with no
	// config change must preserve prior behavior), so without this the
	// exerciseFailover probe below would find failover-a's 500 returned
	// to the client unchanged, never reaching failover-b at all.

	// metricsProbeTrickyUserNameJSON: a user name containing a double
	// quote, a backslash, and a newline — auth.go's buildEntry validates
	// a name as non-empty only, never restricted to a URL-path-segment
	// character set (see its own doc comment), so this is a real,
	// legal config, not a contrived one. json.Marshal, not hand-built
	// string concatenation, produces the correctly JSON-escaped literal
	// to splice into attemptAccountingOverride below — this is JSON
	// escaping for the CONFIG document, a separate concern from the
	// Prometheus label escaping exerciseMetricsRoute actually verifies
	// in the RENDERED /metrics body.
	trickyNameJSON, err := json.Marshal(metricsProbeTrickyUserName)
	if err != nil {
		return fmt.Errorf("marshal metrics escaping probe user name: %w", err)
	}

	attemptAccountingOverride := `{"failover":{"enabled":true},"breaker":{"failureThreshold":2,"openDuration":"3s","maxOpenDuration":"6s"},` +
		`"providers":{"openai":{"type":"openai","baseUrl":"` + upstream.URL + `","apiKey":"sk-up","models":["` + testDataWantModel + `","` + builtinLookupModelID + `"]},` +
		`"brk":{"type":"openai","baseUrl":"` + brkUpstream.URL + `","apiKey":"sk-up","discovery":true,"discoveryInterval":"1ms"},"` +
		slowProviderName + `":{"type":"openai","baseUrl":"` + slowUpstream.URL + `","apiKey":"sk-up","models":["` + slowProviderModel + `"]},` +
		// timeoutProviderName (feature: request timeout) — see its own
		// const block doc comment above for why this provider exists
		// alongside slowProviderName.
		`"` + timeoutProviderName + `":{"type":"openai","baseUrl":"` + timeoutUpstream.URL + `","apiKey":"sk-up","requestTimeout":"` + timeoutProviderRequestTimeout + `","models":["` + timeoutProviderModel + `"]},` +
		// hungBodyProviderName (finding F7) — see its own const block doc
		// comment above for why this provider exists alongside
		// timeoutProviderName.
		`"` + hungBodyProviderName + `":{"type":"openai","baseUrl":"` + hungBodyUpstream.URL + `","apiKey":"sk-up","requestTimeout":"` + hungBodyProviderRequestTimeout + `","models":["` + hungBodyProviderModel + `"]},` +
		// provenanceEstProviderName (feat: expose token-accounting
		// provenance) — see its own const block doc comment above.
		`"` + provenanceEstProviderName + `":{"type":"openai","baseUrl":"` + provenanceEstUpstream.URL + `","apiKey":"sk-up","models":["` + provenanceEstProviderModel + `"]},` +
		// "anthropic" backs the /v1/messages passthrough probes
		// (exerciseMessagesRoute, below): a real anthropic-type provider,
		// interpreted end to end through
		// callAnthropicMessagesPassthrough.
		`"anthropic":{"type":"anthropic","baseUrl":"` + anthropicProbeUpstream.URL + `","apiKey":"sk-anth","models":["claude-test"]},` + // #nosec G101 -- test fixture literal, not a real credential
		`"failover-a":{"type":"openai","baseUrl":"` + failoverAUpstream.URL + `","apiKey":"sk-up","models":["` + failoverModelID + `"]},` +
		`"failover-b":{"type":"openai","baseUrl":"` + failoverBUpstream.URL + `","apiKey":"sk-up","models":["` + failoverModelID + `"]},` +
		// bareWinnerModelID's own const doc comment (above) has the full
		// account of why "bare-winner-a"/"bare-winner-b" exist alongside
		// failover-a/failover-b: same collision shape, different fix under
		// test.
		`"bare-winner-a":{"type":"openai","baseUrl":"` + bareWinnerAUpstream.URL + `","apiKey":"sk-up","models":["` + bareWinnerModelID + `"]},` +
		`"bare-winner-b":{"type":"openai","baseUrl":"` + bareWinnerBUpstream.URL + `","apiKey":"sk-up","models":["` + bareWinnerModelID + `"]}},` +
		// groups: ADDS bareWinnerGroupName to the map Unmarshal already
		// populated from .traefik.yml's own testData.groups.default — map
		// keys merge (run()'s own doc comment, above, on why "brk" and the
		// other new provider keys above are additive rather than
		// replacing), so "default" (providers: [], allow-all) is untouched.
		`"groups":{"` + bareWinnerGroupName + `":{"providers":["bare-winner-b"]}},` +
		// modelMeta (feature v0.23): a config-override entry for
		// testDataWantModel, so exerciseHandler's GET /v1/models
		// assertion below proves resolveModelMeta's config-override
		// layer, modelObject's context_window/pricing extension fields,
		// and the whole registry.go/modelmeta.go resolution path all
		// run correctly INTERPRETED, not merely compiled — cheap to add
		// to this existing harness request, per this feature's own gate
		// requirement. builtinLookupModelID carries NO config override
		// at all (SHOULD-6, review round) — its context_window can only
		// come from an interpreted LOOKUP into the real, 200+-entry
		// builtinModelMetaTable map literal, proving that specific path
		// runs correctly under Yaegi too, not just the config-override
		// one testDataWantModel already exercises.
		`"modelMeta":{"` + testDataWantModel + `":{"contextTokens":` + yaegiMetaContextTokens + `,"inputCostPerMTokMicroUsd":1250000,"outputCostPerMTokMicroUsd":10000000}},` +
		`"mcpServers":{"` + mcpProbeServerName + `":{"url":"` + mcpProbeUpstream.URL + `"}},` +
		// metrics (Prometheus text-exposition endpoint): enabled with
		// modelLabel on, so exerciseMetricsRoute below exercises the
		// opt-in per-(provider,model) breakdown under the interpreter
		// too, not just the always-on families. allowedCIDRs names the
		// exact block httptest.NewRequest's own default RemoteAddr
		// (192.0.2.1:1234, verified empirically) falls inside, so the
		// CIDR-bypass path is reachable with a plain httptest.NewRequest
		// carrying no explicit RemoteAddr override.
		`"metrics":{"enabled":true,"allowedCIDRs":["` + metricsProbeAllowedCIDR + `"],"modelLabel":true},` +
		// The third inline user (metricsProbeTrickyUserName) exists
		// purely so exerciseMetricsRoute can prove the escaping path
		// under the interpreter — it makes no requests of its own, but
		// still renders a real (zero-traffic) llmgateway_requests_total
		// series, since writeUsageMetrics (metrics.go) lists every
		// active user regardless of traffic.
		//
		// The fourth inline user (bareWinnerFriendAPIKey) is
		// exerciseBareWinnerGroupAware's own — group bareWinnerGroupName,
		// authorized for "bare-winner-b" only (the "groups" override
		// above).
		`"admin":{"enabled":true},"users":{"inline":[{"name":"tester","group":"default","apiKey":"` + testDataUserAPIKey + `"},{"name":"admin1","group":"default","apiKey":"` + attemptAccountingAdminAPIKey + `","admin":true},{"name":` + string(trickyNameJSON) + `,"group":"default","apiKey":"sk-tricky"},{"name":"bare-winner-friend","group":"` + bareWinnerGroupName + `","apiKey":"` + bareWinnerFriendAPIKey + `"}]}}`
	if err = json.Unmarshal([]byte(attemptAccountingOverride), cfgVal.Interface()); err != nil {
		return fmt.Errorf("decode attempt-accounting harness override into the interpreted Config: %w", err)
	}

	newVal, err := i.Eval(pkgName + ".New")
	if err != nil {
		return fmt.Errorf("resolve %s.New under yaegi: %w", pkgName, err)
	}
	if newVal.Kind() != reflect.Func {
		return fmt.Errorf("%s.New is not a function (kind %s)", pkgName, newVal.Kind())
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	results := newVal.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(next),
		cfgVal,
		reflect.ValueOf("llmgw-check"),
	})
	if len(results) != 2 {
		return fmt.Errorf("%s.New returned %d values, want 2 (http.Handler, error)", pkgName, len(results))
	}
	if errVal := results[1]; !errVal.IsNil() {
		return fmt.Errorf("%s.New returned an error: %v", pkgName, errVal.Interface())
	}
	handlerVal := results[0]
	if handlerVal.IsNil() {
		return fmt.Errorf("%s.New returned a nil handler", pkgName)
	}
	handler, ok := handlerVal.Interface().(http.Handler)
	if !ok {
		return fmt.Errorf("%s.New's returned value does not implement http.Handler", pkgName)
	}

	if err := exerciseHandler(handler, builtinLookupContextTokens, failoverBHits); err != nil {
		return err
	}
	// exerciseBareWinnerGroupAware (fix/group-aware-bare-winner,
	// 2026-08-27) runs right after exerciseHandler and before
	// exerciseBreaker: it drives its own, freshly named providers and
	// model id (bareWinnerModelID's own doc comment above), so it cannot
	// perturb exerciseBreaker's "brk"-specific hit counting or any later
	// probe's /metrics assertions, which are all keyed to OTHER provider/
	// model names.
	if err := exerciseBareWinnerGroupAware(handler, bareWinnerAHits); err != nil {
		return err
	}
	// exerciseBreaker (feat/provider-health, adversarial-review round 2):
	// runs after exerciseHandler, against the SAME interpreted handler —
	// proves the discovery circuit breaker's open/half-open/closed state
	// machine, its new interpreted breakerState (a named int32 with a
	// String() method), and recordHealthLocked's classification call all
	// run correctly under Yaegi, not merely compiled. limits.go's own
	// matchesSentinel doc comment (Trap-4/reverse-direction history) is
	// exactly why this class of divergence needs its own interpreted
	// probe: a compiled `go test` pass here would prove nothing about the
	// interpreter.
	if err := exerciseBreaker(handler, brkModelsHits, brkHealthy); err != nil {
		return err
	}
	// exerciseMetricsRoute runs after exerciseBreaker has already settled:
	// GET /metrics runs registry.maybeRefresh like every other route
	// (ServeHTTP's own unconditional call at entry), and exerciseBreaker's
	// own hit-counting is timing-sensitive against the "brk" provider
	// specifically — probing metrics first could perturb the very
	// discovery-attempt counts that test samples. Ordering the metrics
	// probe after avoids any such interaction.
	if err := exerciseMetricsRoute(handler); err != nil {
		return err
	}
	// exerciseLatencyMetrics (feat: instrument upstream latency) runs
	// after exerciseMetricsRoute, for the same reason exerciseMetricsRoute
	// itself used to be last: it reads the exact same /metrics endpoint
	// and must not race or perturb anything exerciseBreaker/
	// exerciseMetricsRoute still care about. It relies on exerciseHandler's
	// own exerciseAttemptAccounting sub-probe (already run, above) having
	// driven a real, non-streaming POST /v1/chat/completions against the
	// "openai" provider — see its own doc comment.
	if err := exerciseLatencyMetrics(handler); err != nil {
		return err
	}
	// exerciseUsageProvenance (feat: expose token-accounting provenance)
	// runs LAST, for the identical reason: it reads /metrics and GET
	// /admin/api/overview one more time and must not perturb any counter
	// an earlier probe already asserted on.
	if err := exerciseUsageProvenance(handler); err != nil {
		return err
	}
	// exerciseModelUsageRanking (feat: per-model usage statistics) runs
	// after everything above: it only READS admin endpoints plus one more
	// chat completion of its own, and asserts on a kindModel counter no
	// earlier probe touches, so it can neither perturb nor be perturbed by
	// the provider-level counters they assert on.
	return exerciseModelUsageRanking(handler)
}

// builtinLookupModelID is a real, stable entry in the generated
// builtinModelMetaTable (pricing_data_gen.go) — review fix, SHOULD-6 —
// added to the harness's openai provider with NO modelMeta config
// override of its own, so its GET /v1/models context_window can only
// ever come from an interpreted lookup into the real map literal, not
// the config-override path testDataWantModel already exercises.
const builtinLookupModelID = "gpt-4o"

// failoverModelID is the bare model id both "failover-a" and "failover-b"
// (run(), above) are configured with — the run()-owned upstream servers
// exerciseFailover (below) drives.
const failoverModelID = "failover-shared"

// bareWinnerModelID is the bare model id both "bare-winner-a" and
// "bare-winner-b" (run(), above) are configured with — deliberately the
// identical collision SHAPE as failoverModelID above, but exercising
// registry.go's bareWinner AUTHORIZATION fix (2026-08-27) rather than
// failover: bareWinnerGroupName (below) is authorized for "bare-winner-b"
// only, yet "bare-winner-a" sorts first alphabetically. Before the fix,
// bareWinner always returned the GLOBAL sorted-first owner regardless of
// the caller, so this exact group got errModelDenied for a model it was
// actually entitled to use — the production repro this whole fix exists
// for (uni/macstudio serving text-embedding-multilingual-e5-base, the
// "friends" group entitled to uni but denied because macstudio sorts
// first). exerciseBareWinnerGroupAware (below) proves the fix resolves
// and serves under the REAL interpreter, not merely compiled.
const bareWinnerModelID = "bare-winner-shared"

// bareWinnerGroupName is the group exerciseBareWinnerGroupAware
// authenticates as: authorized for "bare-winner-b" only (run()'s "groups"
// override), never "bare-winner-a" — see bareWinnerModelID's own doc
// comment for why that specific asymmetry is the point.
const bareWinnerGroupName = "bare-winner-friends"

// bareWinnerFriendAPIKey authenticates bareWinnerGroupName's one inline
// user (run()'s "users" override).
const bareWinnerFriendAPIKey = "sk-bare-winner-friend" // #nosec G101 -- test fixture literal, not a real credential

// mcpProbeServerName is the federated MCP server run() configures against
// mcpProbeUpstream, and mcpProbeToolName is a tool id carrying its
// "<server>_" prefix so resolveFederatedTool routes POST /mcp's
// tools/call there. The tool needs no tools/list entry — resolution is
// pure prefix matching on the configured, group-allowed server names.
const (
	mcpProbeServerName = "probe"
	mcpProbeToolName   = mcpProbeServerName + "_big"
)

// metricsProbeAllowedCIDR is the CIDR block attemptAccountingOverride's
// own metrics.allowedCIDRs names — see that literal's own comment for why
// this must contain httptest.NewRequest's default RemoteAddr.
const metricsProbeAllowedCIDR = "192.0.2.0/24"

// metricsProbeOutsideAddr is a source address deliberately OUTSIDE
// metricsProbeAllowedCIDR (TEST-NET-3, RFC 5737) — exerciseMetricsRoute's
// own proof that the allowlist is not simply matching everything.
const metricsProbeOutsideAddr = "203.0.113.9:5555"

// metricsProbeTrickyUserName is a configured user name containing a
// double quote, a backslash, and a newline — the exact character set
// escapeLabelValue (metrics.go) exists to handle, driven under the REAL
// interpreter (review fix, adversarial verification 2026-08-23): the
// compiled test suite already proves escaping correct
// (TestMetrics_LabelValuesWithSpecialChars_Escaped,
// TestEscapeLabelValue_InvalidUTF8_Injective), but this codebase's own
// history is that a compiled pass has repeatedly said nothing about the
// interpreted shape.
const metricsProbeTrickyUserName = "weird\"user\\name\nwith-newline"

// mcpProbeOversizeBytes is how much filler mcpProbeUpstream writes: over
// mcpBackendCallResponseMaxBytes (maxRequestBytes, 10MiB) so the response
// trips doBackendJSONRPC's cap. Kept as its own named value rather than
// importing the plugin's const, because this harness deliberately builds
// nothing from the plugin package at compile time — everything it asserts
// about the plugin must come from the interpreter.
const mcpProbeOversizeBytes = 11 << 20

// mcpTooLargeWantMessage is the exact JSON-RPC error message
// mcpFederatedToolsCall writes when errors.Is matches
// errMCPResponseTooLarge. "upstream error" here instead means errors.Is
// returned false under the interpreter for a chain that is true when
// compiled — which is precisely the class of Yaegi divergence this
// harness exists to catch, and which no compiled test can see.
const mcpTooLargeWantMessage = "response too large"

// stampedCheckVersion is the fake release version stampReleaseVersion
// writes into the interpreted copy. It only has to differ from
// devPluginVersion; nothing asserts its value.
const stampedCheckVersion = "9.9.9-yaegicheck"

// stampReleaseVersion rewrites pluginVersion in the GOPATH COPY of
// version.go (never the repo's own file) so the copy no longer carries
// the dev sentinel.
//
// Without this, New's telemetry gate returns before ever calling into the
// vendored oss-telemetry package, and this harness would report OK while
// leaving the plugin's only non-stdlib runtime import unexercised under
// the interpreter — precisely the blind spot yaegi-check exists to close.
// Import resolution alone is not enough: a cross-package call into
// interpreted third-party code is its own risk under Yaegi.
//
// Telemetry stays suppressed by the DO_NOT_TRACK/OSS_TELEMETRY_DISABLED
// env vars the caller sets first, so Send is entered and returns without
// touching the network. The HTTP path beyond that env check is plain
// net/http, already exercised elsewhere in this harness.
func stampReleaseVersion(pkgDir string) error {
	path := filepath.Join(pkgDir, "version.go")
	data, err := os.ReadFile(path) //nolint:gosec // a temp dir this harness just created
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	pattern := regexp.MustCompile(`(pluginVersion\s*=\s*")[^"]*(")`)
	if !pattern.Match(data) {
		return fmt.Errorf("no pluginVersion constant found in %s — has version.go been renamed?", path)
	}
	stamped := pattern.ReplaceAll(data, []byte(`${1}`+stampedCheckVersion+`${2}`))
	// pkgDir is a temp dir run() created itself, and the filename is a
	// constant — no external input reaches this path.
	if err := os.WriteFile(path, stamped, 0o600); err != nil { //nolint:gosec // G703: path is filepath.Join(<temp dir we created>, "version.go")
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readBuiltinContextTokens reads repoRoot/pricing_data_gen.go and
// extracts modelID's own ContextTokens value directly out of the
// generated source (review fix, SHOULD-6) — rather than hardcoding a
// number in this harness, which would need hand-updating (and could
// silently drift out of sync) every time `make pricing-sync`
// regenerates the table against updated upstream data.
func readBuiltinContextTokens(repoRoot, modelID string) (int, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "pricing_data_gen.go"))
	if err != nil {
		return 0, fmt.Errorf("read pricing_data_gen.go: %w", err)
	}
	pattern := regexp.MustCompile(regexp.QuoteMeta(`"`+modelID+`":`) + `\s*\{ContextTokens:\s*(\d+)`)
	m := pattern.FindSubmatch(data)
	if m == nil {
		return 0, fmt.Errorf("no builtinModelMetaTable entry found for %q — pick a different, currently-present stable id", modelID)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, fmt.Errorf("parse ContextTokens for %q: %w", modelID, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("builtinModelMetaTable entry for %q has ContextTokens=%d, want > 0", modelID, n)
	}
	return n, nil
}

// readHealth returns provider name's healthState string from the admin
// overview, decoded generically like readProviderAttemptCounters does
// (feat/provider-health).
func readHealth(handler http.Handler, name string) (string, error) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("GET /admin/api/overview: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return "", err
	}
	providers, _ := body["providers"].([]any)
	for _, raw := range providers {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if p["name"] == name {
			s, _ := p["healthState"].(string)
			return s, nil
		}
	}
	return "", fmt.Errorf("provider %q not in overview: %s", name, rec.Body.String())
}

// drive fires n authenticated GET /v1/models requests, each of which runs
// maybeRefresh at ServeHTTP entry, with a small gap so the 1ms discovery
// interval always permits a fresh attempt when the breaker is closed
// (feat/provider-health).
func drive(handler http.Handler, n int) {
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
		handler.ServeHTTP(httptest.NewRecorder(), req)
		time.Sleep(20 * time.Millisecond)
	}
}

// exerciseBreaker drives the "brk" provider (attemptAccountingOverride,
// run()) through the discovery circuit breaker's full state cycle —
// closed -> open -> half-open -> closed again — against the REAL
// interpreter, proving under Yaegi what registry_health_test.go already
// proves compiled: tryBeginRefresh/finishRefresh/recordHealthLocked, the
// interpreted breakerState named-int32 type and its String() method, and
// the classification call recordHealthLocked makes all behave correctly
// interpreted, not merely compiled (feat/provider-health, adversarial-
// review round 2 — limits.go's matchesSentinel doc comment is exactly why
// this class of divergence needs its own interpreted probe).
func exerciseBreaker(handler http.Handler, hits, healthy *int64) error {
	// Phase 1: hammer with the upstream 401ing. The breaker must open.
	deadline := time.Now().Add(10 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		drive(handler, 5)
		s, err := readHealth(handler, "brk")
		if err != nil {
			return err
		}
		state = s
		if s == "open" {
			break
		}
	}
	if state != "open" {
		return fmt.Errorf("BREAKER-1: healthState = %q after sustained 401s, want %q (upstream /v1/models hits=%d)", state, "open", atomic.LoadInt64(hits))
	}
	openedAtHits := atomic.LoadInt64(hits)
	fmt.Printf("yaegi-check: breaker opened after %d upstream /v1/models hits\n", openedAtHits)

	// Phase 2: while open, further traffic must NOT reach the upstream.
	// openDuration is 3s; hammer for ~1s and require zero new hits.
	drive(handler, 30)
	duringOpen := atomic.LoadInt64(hits) - openedAtHits
	if duringOpen != 0 {
		return fmt.Errorf("BREAKER-2: %d upstream /v1/models hits during the open window, want 0 — backoff did not suppress discovery", duringOpen)
	}
	fmt.Println("yaegi-check: breaker open window suppressed all discovery hits")

	// Phase 3: provider recovers. After the backoff window elapses the
	// half-open probe must run and CLOSE the breaker again.
	atomic.StoreInt64(healthy, 1)
	recovered := false
	recoverDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(recoverDeadline) {
		drive(handler, 5)
		s, err := readHealth(handler, "brk")
		if err != nil {
			return err
		}
		if s == "closed" {
			recovered = true
			break
		}
	}
	if !recovered {
		return fmt.Errorf("BREAKER-3: breaker never returned to closed after the provider recovered (hits=%d)", atomic.LoadInt64(hits))
	}
	fmt.Println("yaegi-check: recovered provider closed the breaker again")
	return nil
}

// exerciseMetricsRoute drives GET /metrics against the interpreted
// handler and proves both its auth gate and its rendering run correctly
// under Yaegi — the metrics feature's own gate requirement: compiled
// tests have repeatedly passed on this codebase while the interpreted
// shape failed (matchesSentinel's own doc comment, limits.go, is the
// canonical example), so a metrics-specific probe is not optional here.
//
// Four requests, mirroring TestMetrics_GateMatrix (metrics_test.go)
// exactly so this harness and the compiled test suite assert the
// identical contract from two different angles:
//  1. unauthenticated, from an address outside metrics.allowedCIDRs -> 401
//  2. authenticated as "tester" (not admin) -> 403
//  3. authenticated as the admin user -> 200, body inspected
//  4. unauthenticated, from an address INSIDE metrics.allowedCIDRs -> 200
//
// Request 3's body is inspected for real, live-data content — not just a
// 200 status — because a Yaegi-specific bug in this codebase's history
// (matchesSentinel, limits.go) was a case where the interpreted code ran
// without panicking yet produced a WRONG answer; a bare status-code check
// would not have caught that class of failure here either. Specifically:
// the "openai" provider's name, and — since attemptAccountingOverride
// enables metrics.modelLabel — testDataWantModel's own id inside the
// opt-in provider-model breakdown, both of which can only appear via a
// live registry.snapshot() walk and a live providerModelScopeID join
// under the real interpreter, not a static string in this harness.
func exerciseMetricsRoute(handler http.Handler) error {
	unauthReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	unauthReq.RemoteAddr = metricsProbeOutsideAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, unauthReq)
	if rec.Code != http.StatusUnauthorized {
		return fmt.Errorf("GET /metrics (no key, outside allowedCIDRs): status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}

	nonAdminReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	nonAdminReq.RemoteAddr = metricsProbeOutsideAddr
	nonAdminReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, nonAdminReq)
	if rec.Code != http.StatusForbidden {
		return fmt.Errorf("GET /metrics (non-admin key): status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	adminReq.RemoteAddr = metricsProbeOutsideAddr
	adminReq.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, adminReq)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /metrics (admin key): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		return fmt.Errorf("GET /metrics (admin key): Content-Type = %q, want a text/plain prefix", ct)
	}
	body := rec.Body.String()
	// metricsProbeEscapedTrickyName is escapeLabelValue's OWN expected
	// output for metricsProbeTrickyUserName — a raw Go string literal
	// (backticks: every backslash below is a literal backslash
	// character, not a Go escape), computed by hand from the format
	// rule (\\ -> \\\\, " -> \\", newline -> \\n) rather than by calling
	// the function itself, so this assertion cannot pass merely because
	// the interpreted and this harness's own understanding of the rule
	// happen to agree by construction.
	const metricsProbeEscapedTrickyName = `weird\"user\\name\nwith-newline`
	for _, want := range []string{
		"# TYPE llmgateway_requests_total counter",
		"# TYPE llmgateway_provider_healthy gauge",
		`llmgateway_provider_attempts_total{provider="openai"}`,
		"# TYPE llmgateway_provider_model_attempts_total counter",
		`llmgateway_provider_model_attempts_total{provider="openai",model="` + testDataWantModel + `"}`,
		`llmgateway_requests_total{scope_kind="user",scope_id="` + metricsProbeEscapedTrickyName + `"}`,
	} {
		if !strings.Contains(body, want) {
			return fmt.Errorf("GET /metrics (admin key): body missing %q — interpreted rendering diverged from the compiled shape; body=%s", want, body)
		}
	}
	if strings.Contains(body, metricsProbeTrickyUserName) {
		return fmt.Errorf("GET /metrics (admin key): body contains the RAW, unescaped tricky user name — escapeLabelValue did not run interpreted; body=%s", body)
	}

	allowlistedReq := httptest.NewRequest(http.MethodGet, "/metrics", nil) // default RemoteAddr, inside metricsProbeAllowedCIDR
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, allowlistedReq)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /metrics (no key, inside allowedCIDRs): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	fmt.Println("yaegi-check: GET /metrics auth gate and rendering both verified interpreted")
	return nil
}

// exerciseLatencyMetrics proves the upstream-latency histograms (feat:
// instrument upstream latency, task brief) render correctly under the
// REAL interpreter — not merely that a compiled `go test` accepts the
// same bytes. This package's own doc comment explains why that
// distinction matters here specifically: compiled tests have repeatedly
// passed on this codebase while the interpreted shape failed.
//
// No new traffic is driven here: exerciseHandler's own
// exerciseAttemptAccounting sub-probe (already run, above, as part of
// the exerciseHandler call in run()) already drove a real, non-streaming
// POST /v1/chat/completions against the "openai" provider, so
// llmgateway_upstream_duration_seconds{provider="openai",stream="false"}
// already carries at least one real, interpreted observation by the
// time this runs.
//
// Unlike exerciseMetricsRoute's own strings.Contains checks, this parses
// the rendered body with prometheus/common/expfmt's REAL TextParser —
// the same library a real Prometheus server's scrape loop is built on —
// instead of this project's own hand-rolled parser (parsePrometheusText,
// metrics_test.go, used by the compiled suite). A malformed histogram
// (wrong cumulative order, a missing +Inf bucket, a mismatched
// sample_count) does not merely fail one assertion below, it fails to
// PARSE at all — exactly the failure mode that takes a real scrape
// target DOWN rather than dropping one metric (metricWriter.histogram's
// own doc comment, metrics.go, has the full account of why that
// distinction matters).
func exerciseLatencyMetrics(handler http.Handler) error {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = metricsProbeOutsideAddr
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /metrics (latency histogram probe): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		return fmt.Errorf("GET /metrics: a real Prometheus TextParser rejected the interpreted rendering: %w (body=%s)", err, rec.Body.String())
	}

	durationFamily, ok := families["llmgateway_upstream_duration_seconds"]
	if !ok {
		return fmt.Errorf("GET /metrics: no llmgateway_upstream_duration_seconds family in the parsed output (body=%s)", rec.Body.String())
	}
	if durationFamily.GetType() != dto.MetricType_HISTOGRAM {
		return fmt.Errorf("llmgateway_upstream_duration_seconds type = %s, want HISTOGRAM", durationFamily.GetType())
	}

	var openaiHist *dto.Histogram
	for _, m := range durationFamily.GetMetric() {
		var provider, stream string
		for _, lp := range m.GetLabel() {
			switch lp.GetName() {
			case "provider":
				provider = lp.GetValue()
			case "stream":
				stream = lp.GetValue()
			}
		}
		if provider == "openai" && stream == "false" {
			openaiHist = m.GetHistogram()
			break
		}
	}
	if openaiHist == nil {
		return fmt.Errorf("GET /metrics: no llmgateway_upstream_duration_seconds{provider=\"openai\",stream=\"false\"} series found (body=%s)", rec.Body.String())
	}

	// Real-parser structural checks, matching the compiled suite's own
	// TestMetrics_LatencyHistogram_BucketsCumulativeOrdered_
	// PlusInfPresent_SumCountConsistent (metrics_test.go) — but here,
	// against dto.Bucket values the REAL parser produced, not this
	// project's own hand-rolled promSample.
	buckets := openaiHist.GetBucket()
	if len(buckets) == 0 {
		return fmt.Errorf("llmgateway_upstream_duration_seconds{provider=\"openai\",stream=\"false\"}: no buckets parsed")
	}
	prevBound := math.Inf(-1)
	var prevCount uint64
	for i, b := range buckets {
		if b.GetUpperBound() <= prevBound {
			return fmt.Errorf("bucket %d upper_bound=%v is not strictly greater than the previous bucket's %v — buckets must be strictly ascending", i, b.GetUpperBound(), prevBound)
		}
		if b.GetCumulativeCount() < prevCount {
			return fmt.Errorf("bucket %d cumulative_count=%d is less than the previous bucket's %d — buckets must be cumulative (non-decreasing)", i, b.GetCumulativeCount(), prevCount)
		}
		prevBound = b.GetUpperBound()
		prevCount = b.GetCumulativeCount()
	}
	if !math.IsInf(buckets[len(buckets)-1].GetUpperBound(), 1) {
		return fmt.Errorf("last bucket upper_bound = %v, want +Inf", buckets[len(buckets)-1].GetUpperBound())
	}
	if openaiHist.GetSampleCount() != prevCount {
		return fmt.Errorf("sample_count = %d, want %d — must exactly equal the final +Inf cumulative bucket", openaiHist.GetSampleCount(), prevCount)
	}
	if openaiHist.GetSampleCount() == 0 {
		return fmt.Errorf("sample_count = 0, want at least 1 (exerciseAttemptAccounting already drove one real, non-streaming chat completion against \"openai\")")
	}

	ttfbFamily, ok := families["llmgateway_upstream_ttfb_seconds"]
	if !ok {
		return fmt.Errorf("GET /metrics: no llmgateway_upstream_ttfb_seconds family in the parsed output (body=%s)", rec.Body.String())
	}
	if ttfbFamily.GetType() != dto.MetricType_HISTOGRAM {
		return fmt.Errorf("llmgateway_upstream_ttfb_seconds type = %s, want HISTOGRAM", ttfbFamily.GetType())
	}

	fmt.Println("yaegi-check: upstream-latency histograms parsed by a real Prometheus TextParser; buckets cumulative, strictly ascending, +Inf-terminated, sample_count consistent")
	return nil
}

// exerciseUsageProvenance proves the usage-provenance metric surface
// (feat: expose token-accounting provenance, metrics.go's
// provenanceStore/writeProvenanceMetrics, admin.go's
// buildAdminProvenanceViews) renders correctly under the REAL
// interpreter — not merely that a compiled `go test` accepts the same
// bytes, this package's own doc comment's exact concern.
//
// Drives one real, non-streaming POST /v1/chat/completions against
// provenanceEstProviderName, whose upstream (provenanceEstUpstream, run())
// answers 200 with no "usage" field at all — the exact shape
// runUnified's estimation fallback (routes_unified.go) classifies as
// "estimated". "reported" is not driven fresh here: exerciseHandler's own
// exerciseAttemptAccounting sub-probe (already run, above) already drove
// a real, usage-bearing POST /v1/chat/completions against "openai",
// which is reported provenance by construction — reusing that traffic
// instead of a third fixture, mirroring exerciseLatencyMetrics' own
// "no new traffic" reasoning for its own duration-histogram assertion.
func exerciseUsageProvenance(handler http.Handler) error {
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+provenanceEstProviderName+`/`+provenanceEstProviderModel+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	chatReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (usage-provenance harness): status = %d, want 200, body=%s", chatRec.Code, chatRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = metricsProbeOutsideAddr
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /metrics (usage-provenance probe): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		return fmt.Errorf("GET /metrics: a real Prometheus TextParser rejected the interpreted rendering: %w (body=%s)", err, rec.Body.String())
	}

	requestsFamily, ok := families["llmgateway_usage_provenance_requests_total"]
	if !ok {
		return fmt.Errorf("GET /metrics: no llmgateway_usage_provenance_requests_total family in the parsed output (body=%s)", rec.Body.String())
	}
	if requestsFamily.GetType() != dto.MetricType_COUNTER {
		return fmt.Errorf("llmgateway_usage_provenance_requests_total type = %s, want COUNTER", requestsFamily.GetType())
	}
	tokensFamily, ok := families["llmgateway_usage_provenance_tokens_total"]
	if !ok {
		return fmt.Errorf("GET /metrics: no llmgateway_usage_provenance_tokens_total family in the parsed output (body=%s)", rec.Body.String())
	}
	if tokensFamily.GetType() != dto.MetricType_COUNTER {
		return fmt.Errorf("llmgateway_usage_provenance_tokens_total type = %s, want COUNTER", tokensFamily.GetType())
	}

	findSample := func(fam *dto.MetricFamily, provider, provenance string) *dto.Metric {
		for _, m := range fam.GetMetric() {
			var p, k string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "provider":
					p = lp.GetValue()
				case "provenance":
					k = lp.GetValue()
				}
			}
			if p == provider && k == provenance {
				return m
			}
		}
		return nil
	}

	estReq := findSample(requestsFamily, provenanceEstProviderName, "estimated")
	if estReq == nil {
		return fmt.Errorf(`GET /metrics: no llmgateway_usage_provenance_requests_total{provider=%q,provenance="estimated"} sample found (body=%s)`, provenanceEstProviderName, rec.Body.String())
	}
	if estReq.GetCounter().GetValue() != 1 {
		return fmt.Errorf(`llmgateway_usage_provenance_requests_total{provider=%q,provenance="estimated"} = %v, want 1`, provenanceEstProviderName, estReq.GetCounter().GetValue())
	}
	estTok := findSample(tokensFamily, provenanceEstProviderName, "estimated")
	if estTok == nil {
		return fmt.Errorf(`GET /metrics: no llmgateway_usage_provenance_tokens_total{provider=%q,provenance="estimated"} sample found (body=%s)`, provenanceEstProviderName, rec.Body.String())
	}
	if estTok.GetCounter().GetValue() <= 0 {
		return fmt.Errorf(`llmgateway_usage_provenance_tokens_total{provider=%q,provenance="estimated"} = %v, want > 0 (estimated substitutes a nonzero prompt count)`, provenanceEstProviderName, estTok.GetCounter().GetValue())
	}

	reportedReq := findSample(requestsFamily, "openai", "reported")
	if reportedReq == nil {
		return fmt.Errorf(`GET /metrics: no llmgateway_usage_provenance_requests_total{provider="openai",provenance="reported"} sample found (body=%s) — exerciseAttemptAccounting's own earlier real, usage-bearing traffic must have been classified "reported"`, rec.Body.String())
	}
	if reportedReq.GetCounter().GetValue() < 1 {
		return fmt.Errorf(`llmgateway_usage_provenance_requests_total{provider="openai",provenance="reported"} = %v, want >= 1`, reportedReq.GetCounter().GetValue())
	}

	// GET /admin/api/overview: proves admin.go's buildAdminProvenanceViews
	// wiring also runs correctly interpreted, decoded generically (this
	// harness module is compiled, not interpreted; adminOverviewResponse/
	// adminProviderView are unexported — see readProviderAttemptCounters'
	// own doc comment for why a generic decode is simpler here than
	// exporting test-only types across that boundary).
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	adminReq.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	adminRec := httptest.NewRecorder()
	handler.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		return fmt.Errorf("GET /admin/api/overview (usage-provenance probe): status = %d, want 200, body=%s", adminRec.Code, adminRec.Body.String())
	}
	var overview map[string]any
	if err := json.Unmarshal(adminRec.Body.Bytes(), &overview); err != nil {
		return fmt.Errorf("decode GET /admin/api/overview body: %w", err)
	}
	providers, _ := overview["providers"].([]any)
	var provenanceEstProvider map[string]any
	for _, p := range providers {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if pm["name"] == provenanceEstProviderName {
			provenanceEstProvider = pm
			break
		}
	}
	if provenanceEstProvider == nil {
		return fmt.Errorf("GET /admin/api/overview: no provider entry named %q (body=%s)", provenanceEstProviderName, adminRec.Body.String())
	}
	provenanceField, _ := provenanceEstProvider["provenance"].(map[string]any)
	estimatedEntry, _ := provenanceField["estimated"].(map[string]any)
	if estimatedEntry == nil {
		return fmt.Errorf(`GET /admin/api/overview: provider %q has no provenance.estimated entry: %+v`, provenanceEstProviderName, provenanceField)
	}
	if reqs, _ := estimatedEntry["requests"].(float64); reqs != 1 {
		return fmt.Errorf("GET /admin/api/overview: provider %q provenance.estimated.requests = %v, want 1", provenanceEstProviderName, estimatedEntry["requests"])
	}
	if _, hasReported := provenanceField["reported"]; hasReported {
		return fmt.Errorf(`GET /admin/api/overview: provider %q provenance has a "reported" key, want it excluded from this compact admin summary: %+v`, provenanceEstProviderName, provenanceField)
	}

	fmt.Println("yaegi-check: usage-provenance metrics (llmgateway_usage_provenance_requests_total/-_tokens_total) and admin overview parsed by a real Prometheus TextParser/JSON decoder; estimated and reported provenances both present and distinct")
	return nil
}

// exerciseHandler runs two real requests against the interpreted
// handler, drawn from the plugin's own .traefik.yml testData: an
// authenticated GET /v1/models, which must return the configured
// testDataWantModel; and the same route without credentials, which must
// be refused with 401 — proving ServeHTTP, auth.identify, the model
// registry, statusTrackingWriter, and the gateway's logger all execute
// for real under Yaegi, not merely that the package's imports resolve.
// builtinContextTokens is builtinLookupModelID's own real, extracted
// builtinModelMetaTable value (SHOULD-6) — see that constant's own doc
// comment. failoverBHits backs exerciseFailover's own call, below.
func exerciseHandler(handler http.Handler, builtinContextTokens int, failoverBHits *int64) error {
	authedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authedReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	authedRec := httptest.NewRecorder()
	handler.ServeHTTP(authedRec, authedReq)
	if authedRec.Code != http.StatusOK {
		return fmt.Errorf("GET /v1/models with the testData user key: status = %d, want 200, body=%s", authedRec.Code, authedRec.Body.String())
	}
	if !strings.Contains(authedRec.Body.String(), testDataWantModel) {
		return fmt.Errorf("GET /v1/models body does not contain %q: %s", testDataWantModel, authedRec.Body.String())
	}

	// modelMeta (feature v0.23), interpreted, decoded structurally (not
	// a raw substring match — SHOULD-6, review round: a bare Contains
	// check cannot tell WHICH model entry a given context_window value
	// belongs to, which matters once two entries can legitimately share
	// the same number) so each assertion below is pinned to its own
	// named model id.
	var modelsBody struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(authedRec.Body.Bytes(), &modelsBody); err != nil {
		return fmt.Errorf("decode GET /v1/models body: %w (body=%s)", err, authedRec.Body.String())
	}
	byID := make(map[string]map[string]any, len(modelsBody.Data))
	for _, entry := range modelsBody.Data {
		if id, ok := entry["id"].(string); ok {
			byID[id] = entry
		}
	}

	// run() layers a modelMeta config override for testDataWantModel
	// onto cfgVal before New() is ever called (attemptAccountingOverride
	// above) — proving resolveModelMeta's config-override layer and
	// modelObject's context_window/pricing extension fields both run
	// correctly, interpreted, through registry.go's listFor/
	// resolveMetaFor.
	overridden, ok := byID[testDataWantModel]
	if !ok {
		return fmt.Errorf("GET /v1/models body has no entry for %q: %s", testDataWantModel, authedRec.Body.String())
	}
	if cw, _ := overridden["context_window"].(float64); cw != 128000 {
		return fmt.Errorf("GET /v1/models %q context_window = %v, want 128000 (feature v0.23 config-override metadata harness): %s", testDataWantModel, overridden["context_window"], authedRec.Body.String())
	}
	pricing, _ := overridden["pricing"].(map[string]any)
	if pricing == nil || pricing["input_per_mtok_usd"] != 1.25 || pricing["output_per_mtok_usd"] != 10.0 {
		return fmt.Errorf("GET /v1/models %q pricing = %v, want {input_per_mtok_usd:1.25 output_per_mtok_usd:10} (feature v0.23 config-override metadata harness): %s", testDataWantModel, overridden["pricing"], authedRec.Body.String())
	}

	// builtinLookupModelID (SHOULD-6, review round) carries NO modelMeta
	// config override — its context_window can only come from an
	// interpreted LOOKUP into the real, 200+-entry builtinModelMetaTable
	// map literal (modelmeta.go's resolveModelMeta, builtin branch),
	// proving that specific path — not just the config-override one
	// above — runs correctly under Yaegi.
	builtinLookedUp, ok := byID[builtinLookupModelID]
	if !ok {
		return fmt.Errorf("GET /v1/models body has no entry for %q: %s", builtinLookupModelID, authedRec.Body.String())
	}
	if cw, _ := builtinLookedUp["context_window"].(float64); int(cw) != builtinContextTokens {
		return fmt.Errorf("GET /v1/models %q context_window = %v, want %d (feature v0.23 builtin-table-lookup metadata harness, SHOULD-6): %s", builtinLookupModelID, builtinLookedUp["context_window"], builtinContextTokens, authedRec.Body.String())
	}

	if err := exerciseAttemptAccounting(handler); err != nil {
		return err
	}

	if err := exerciseFailover(handler, failoverBHits); err != nil {
		return err
	}

	if err := exerciseRequestTimeout(handler); err != nil {
		return err
	}

	if err := exerciseRequestTimeoutMidBodyStall(handler); err != nil {
		return err
	}

	if err := exerciseFederatedTooLarge(handler); err != nil {
		return err
	}

	if err := exerciseFederatedSSENegotiation(handler); err != nil {
		return err
	}

	if err := exerciseStampedVersion(handler); err != nil {
		return err
	}

	if err := exerciseMessagesRoute(handler); err != nil {
		return err
	}

	unauthedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	unauthedRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthedRec, unauthedReq)
	if unauthedRec.Code != http.StatusUnauthorized {
		return fmt.Errorf("GET /v1/models without a key: status = %d, want 401, body=%s", unauthedRec.Code, unauthedRec.Body.String())
	}
	return nil
}

// exerciseAttemptAccounting drives two real requests through handler —
// Feature A's (v0.22) per-provider success-rate accounting, exercised
// end to end under Yaegi: runUnified's attemptRecorder (routes_unified.
// go) -> retryPolicy.do's context lookup (retry.go) ->
// limiter.recordProviderAttempt's isTransient/isDeadlineExceeded
// classification (limits.go, including matchesSentinel's errors.Is call
// through providers.go's own multi-%w wrapping) -> its fire-and-forget
// spawn (SHOULD-5) -> buildAdminOverview's own batched providerUsage read
// (admin.go). Any interpreter-only failure in that chain — a construct
// `go build`/`go test` cannot catch, exactly the class of bug this
// harness exists for (SHOULD-4, v0.22 review round) — surfaces here as a
// non-1/non-0 count or an outright panic under Yaegi.
//
// The first request (against "openai", instant upstream) proves the
// success path: one attempt, zero failures. The second (against
// slowProviderName, an upstream that never answers within
// slowRequestDeadline — SHOULD-A, round 2) proves the FAILURE path:
// exactly what round 1 of this harness claimed to prove but never
// actually drove — the interpreted plugin never saw a non-nil error at
// all, so isDeadlineExceeded was never exercised under this harness even
// while it ran (and passed) under `go test`. THIS is what SHOULD-A was
// for: the first version of this second request, run against round 2's
// hand-rolled comma-ok Unwrap-walk fix (limits.go's matchesSentinel,
// since replaced), FAILED right here — under the interpreter only,
// despite passing every compiled `go test` row for the identical error
// shape — because an interpreted interface type does not correctly match
// a compiled concrete value's method set in Yaegi. matchesSentinel now
// uses real errors.Is instead (its own doc comment in limits.go has the
// full account); this harness is what caught the difference.
// exerciseStampedVersion asserts the interpreted plugin reports
// stampedCheckVersion in GET /admin/api/overview.
//
// This is the proof that stampReleaseVersion actually landed, and so the
// proof that New's telemetry gate was PASSED rather than short-circuited:
// the gate compares this same pluginVersion constant against
// devPluginVersion, so a version of stampedCheckVersion here means the
// interpreted code went on to call into the vendored oss-telemetry
// package. Without this assertion a silently failed stamp would leave the
// dev sentinel in place and the harness would still print OK, claiming
// coverage it does not have.
func exerciseStampedVersion(handler http.Handler) error {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /admin/api/overview: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var overview struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		return fmt.Errorf("decode GET /admin/api/overview body: %w", err)
	}
	if overview.Version != stampedCheckVersion {
		return fmt.Errorf("interpreted plugin reports version %q, want %q — stampReleaseVersion did not take effect, so New's telemetry gate short-circuited on the dev sentinel and the vendored oss-telemetry call was never exercised under Yaegi", overview.Version, stampedCheckVersion)
	}
	return nil
}

// postMessages drives one POST /v1/messages through handler with the given
// auth header name/value and JSON body, returning the recorder.
func postMessages(handler http.Handler, hdrName, hdrValue, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrName, hdrValue)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// exerciseMessagesRoute drives the inbound Anthropic Messages API route
// under the REAL interpreter: both auth header forms, the streaming
// rejection, a bad key's error envelope, an unknown model's error
// envelope, the openai-type TRANSLATED path (openAIRequestFromAnthropic
// -> chatCompletion -> anthropicResponseFromOpenAI), the anthropic-type
// PASSTHROUGH path including its own upstream-error branch, and a
// tool_use round trip through the translated path. This exists because
// go test never runs under Yaegi — a construct that compiles and passes
// `go test` can still panic or silently misbehave interpreted (this
// package's own doc comment has the general case; handleAdapterError's
// doc comment, routes_unified.go, has this route's own specific history
// with plain type assertions over errors.As).
func exerciseMessagesRoute(handler http.Handler) error {
	// 1. x-api-key auth + translated path (openai-type provider).
	rec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"`+testDataWantModel+`","max_tokens":64,"system":"be terse","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/messages (x-api-key, translated): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		return fmt.Errorf("POST /v1/messages: decode body: %w (body=%s)", err, rec.Body.String())
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		return fmt.Errorf("POST /v1/messages translated: type/role = %v/%v, want message/assistant: %s", msg["type"], msg["role"], rec.Body.String())
	}
	if msg["model"] != testDataWantModel {
		return fmt.Errorf("POST /v1/messages translated: model = %v, want %q (the client's requested alias echoed back, item 4 fix): %s", msg["model"], testDataWantModel, rec.Body.String())
	}
	if msg["stop_reason"] != "end_turn" {
		return fmt.Errorf("POST /v1/messages translated: stop_reason = %v, want end_turn: %s", msg["stop_reason"], rec.Body.String())
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		return fmt.Errorf("POST /v1/messages translated: content len = %d, want 1: %s", len(content), rec.Body.String())
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		return fmt.Errorf("POST /v1/messages translated: content[0] = %v, want text/hi: %s", content[0], rec.Body.String())
	}
	usg, _ := msg["usage"].(map[string]any)
	if usg == nil || usg["input_tokens"] != float64(1) || usg["output_tokens"] != float64(1) {
		return fmt.Errorf("POST /v1/messages translated: usage = %v, want input=1 output=1: %s", msg["usage"], rec.Body.String())
	}

	// 2. Authorization: Bearer must authenticate the same route.
	bearerRec := postMessages(handler, "Authorization", "Bearer "+testDataUserAPIKey,
		`{"model":"`+testDataWantModel+`","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)
	if bearerRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/messages (Authorization: Bearer): status = %d, want 200, body=%s", bearerRec.Code, bearerRec.Body.String())
	}

	// 3. A bad key gets a 401 in the ANTHROPIC error shape.
	badRec := postMessages(handler, "x-api-key", "not-a-real-key",
		`{"model":"`+testDataWantModel+`","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)
	if badRec.Code != http.StatusUnauthorized {
		return fmt.Errorf("POST /v1/messages with a bad key: status = %d, want 401, body=%s", badRec.Code, badRec.Body.String())
	}
	if err := assertAnthropicErrorShape("bad key 401", badRec.Body.Bytes()); err != nil {
		return err
	}

	// 4. "stream": true is rejected explicitly, in the Anthropic shape,
	// as a 400 (item 2 fix, 2026-08-22 review) — NOT 501: a 501 makes
	// anthropic-sdk-python (which retries any status >= 500) turn every
	// streaming call into a three-request retry storm against this
	// route's own rate limit. 400 maps to the SDK's non-retryable
	// BadRequestError.
	streamRec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"`+testDataWantModel+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if streamRec.Code != http.StatusBadRequest {
		return fmt.Errorf("POST /v1/messages with stream=true: status = %d, want 400, body=%s", streamRec.Code, streamRec.Body.String())
	}
	if err := assertAnthropicErrorShape("stream rejection", streamRec.Body.Bytes()); err != nil {
		return err
	}

	// 5. An unknown model's 404 must also be Anthropic-shaped.
	unknownRec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"no-such-model-anywhere","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)
	if unknownRec.Code != http.StatusNotFound {
		return fmt.Errorf("POST /v1/messages with an unknown model: status = %d, want 404, body=%s", unknownRec.Code, unknownRec.Body.String())
	}
	if err := assertAnthropicErrorShape("unknown model 404", unknownRec.Body.Bytes()); err != nil {
		return err
	}

	// 6. The anthropic-type PASSTHROUGH branch, interpreted: every field
	// survives except "model", rewritten to the client's own requested
	// alias (item 4 fix) — a routing rewrite, not message-body
	// translation.
	passRec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"claude-test","max_tokens":64,"system":"be terse","messages":[{"role":"user","content":"ping"}]}`)
	if passRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/messages (anthropic passthrough): status = %d, want 200, body=%s", passRec.Code, passRec.Body.String())
	}
	var pass map[string]any
	if err := json.Unmarshal(passRec.Body.Bytes(), &pass); err != nil {
		return fmt.Errorf("POST /v1/messages passthrough: decode body: %w (body=%s)", err, passRec.Body.String())
	}
	if pass["id"] != "msg_probe" {
		return fmt.Errorf("POST /v1/messages passthrough: id = %v, want msg_probe (every field but \"model\" must survive the passthrough unchanged): %s", pass["id"], passRec.Body.String())
	}
	if pass["model"] != "claude-test" {
		return fmt.Errorf("POST /v1/messages passthrough: model = %v, want \"claude-test\" (the client's requested alias echoed back, item 4 fix — not anthropicProbeUpstream's own \"claude-upstream-real\"): %s", pass["model"], passRec.Body.String())
	}

	// 6b. handleAdapterErrorEnvelope's *providerHTTPError type assertion,
	// interpreted — the exact construct class that has broken under Yaegi
	// before (see handleAdapterError's own doc comment, routes_unified.go).
	upErrRec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"make-it-fail"}]}`)
	if upErrRec.Code != http.StatusTooManyRequests {
		return fmt.Errorf("POST /v1/messages (upstream 429): status = %d, want 429, body=%s", upErrRec.Code, upErrRec.Body.String())
	}
	if err := assertAnthropicErrorShape("upstream 429", upErrRec.Body.Bytes()); err != nil {
		return err
	}
	// A stale X-Llmgw-Cache header on an error response was deliberately
	// NOT asserted here (F6 fix, 2026-08-23 review): this harness's own
	// fixture configures no cache/Redis, so g.cache is nil and the
	// header is never set on any response regardless of what this route
	// does — the assertion could not fail no matter what code ran,
	// confirmed by a mutation pass that deleted the header-clear line it
	// was meant to guard (handleAdapterErrorEnvelope, routes_unified.go)
	// with make yaegi-check still reporting OK. That behavior is already
	// covered where it can actually fail — a real cache fixture, go
	// test: TestHandleChat_CacheableRequest_UpstreamDown_502HasNoCacheHeader
	// (routes_unified_test.go).

	// 7. A tool_use round trip through the TRANSLATED path, to drive
	// openAIToolCallFromAnthropic/openAIToolMessageFromAnthropic and
	// anthropicToolInputFromOpenAI interpreted.
	toolRec := postMessages(handler, "x-api-key", testDataUserAPIKey,
		`{"model":"`+testDataWantModel+`","max_tokens":64,`+
			`"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],`+
			`"tool_choice":{"type":"auto"},`+
			`"messages":[{"role":"user","content":"weather?"},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"London"}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}]}`)
	if toolRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/messages (tool_use round trip): status = %d, want 200, body=%s", toolRec.Code, toolRec.Body.String())
	}
	return nil
}

// assertAnthropicErrorShape checks b is {"type":"error","error":{"type":...,
// "message":...}} — what an Anthropic SDK client parses.
func assertAnthropicErrorShape(label string, b []byte) error {
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("/v1/messages %s: body is not JSON: %w (body=%s)", label, err, string(b))
	}
	if env["type"] != "error" {
		return fmt.Errorf("/v1/messages %s: top-level \"type\" = %v, want \"error\" (Anthropic envelope): %s", label, env["type"], string(b))
	}
	inner, _ := env["error"].(map[string]any)
	if inner == nil {
		return fmt.Errorf("/v1/messages %s: no \"error\" object: %s", label, string(b))
	}
	if _, ok := inner["type"].(string); !ok {
		return fmt.Errorf("/v1/messages %s: error.type missing: %s", label, string(b))
	}
	if _, ok := inner["message"].(string); !ok {
		return fmt.Errorf("/v1/messages %s: error.message missing: %s", label, string(b))
	}
	return nil
}

// exerciseFederatedTooLarge drives POST /mcp (federation's tools/call
// path) against mcpProbeUpstream, whose response deliberately exceeds
// mcpBackendCallResponseMaxBytes, and asserts the interpreted plugin
// answers with mcpTooLargeWantMessage rather than the generic "upstream
// error".
//
// What this actually pins is errors.Is under the interpreter. A security
// review round replaced that errors.Is with a strings.Contains marker on
// the grounds that no gate exercised this path interpreted — true at the
// time, and the honest objection. The reasoning for errors.Is is sound
// (errors.New and fmt.Errorf both return COMPILED values, so declaring
// the sentinel variable in interpreted code leaves the whole chain
// errors.Is walks compiled, which is why retry.go's isTransient has
// worked in production since v0.2.0), and a standalone yaegi v0.16.1
// harness confirmed it. But this codebase's own history — matchesSentinel
// (limits.go) — is a case where sound reasoning about the reflect
// boundary was wrong and only an interpreted probe caught it. So the
// path is covered here rather than argued in a comment.
//
// A failure surfaces as "upstream error": errors.Is returning false
// interpreted for a chain that is true compiled. No `go test` row can
// see that difference.
func exerciseFederatedTooLarge(handler http.Handler) error {
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + mcpProbeToolName + `","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("POST /mcp tools/call: status = %d, want 200 (JSON-RPC reports the failure in the body, not the status), body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return fmt.Errorf("decode POST /mcp tools/call body: %w (body=%s)", err, rec.Body.String())
	}
	if resp.Error == nil {
		return fmt.Errorf("POST /mcp tools/call returned no JSON-RPC error, want %q — the probe upstream's oversized response should have tripped the cap: %s", mcpTooLargeWantMessage, rec.Body.String())
	}
	if resp.Error.Message != mcpTooLargeWantMessage {
		return fmt.Errorf("POST /mcp tools/call error message = %q, want %q — errors.Is(err, errMCPResponseTooLarge) did not match under the interpreter (mcp_federation.go)", resp.Error.Message, mcpTooLargeWantMessage)
	}
	return nil
}

// exerciseFederatedSSENegotiation drives two real POST /mcp "ping" calls
// against the interpreted handler — one carrying no Accept header
// (today's plain JSON framing) and one carrying the reported client's
// exact "Accept: application/json, text/event-stream" header — proving
// content negotiation (mcpNegotiateFormat, mcpAcceptsSSE) and the SSE
// branch's reuse of sse.go's newSSEWriter/writeData both run correctly
// under the REAL interpreter. mcp_federation_sse_test.go already covers
// this compiled; this harness exists because a compiled pass on this
// codebase has repeatedly said nothing about the interpreted shape.
//
// "ping" is deliberately chosen over tools/list or tools/call: it is
// answered entirely locally (handleMCPFederated's own doc comment), so
// this probe needs no additional upstream server and cannot be confused
// with mcpProbeUpstream's own, deliberately oversized, tools/call-only
// response above.
//
// This is also the first time under this harness that sse.go's
// newSSEWriter/writeData actually executes interpreted at all. The
// streaming (chat completions) path is documented as losing its
// incremental Flush under Yaegi (newSSEWriter's own doc comment,
// confirmed by Task 15's integration suite) — harmless here, since this
// is a single-shot envelope that writes once and returns rather than a
// response that depends on Flush for time-to-first-byte, but it means no
// existing gate has ever run this writer interpreted before now.
func exerciseFederatedSSENegotiation(handler http.Handler) error {
	pingBody := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	jsonReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(pingBody))
	jsonReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	jsonReq.Header.Set("Content-Type", "application/json")
	jsonRec := httptest.NewRecorder()
	handler.ServeHTTP(jsonRec, jsonReq)
	if jsonRec.Code != http.StatusOK {
		return fmt.Errorf("POST /mcp ping (no Accept header): status = %d, want 200, body=%s", jsonRec.Code, jsonRec.Body.String())
	}
	if ct := jsonRec.Header().Get("Content-Type"); ct != "application/json" {
		return fmt.Errorf("POST /mcp ping (no Accept header): Content-Type = %q, want %q — default framing must not change interpreted", ct, "application/json")
	}

	sseReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(pingBody))
	sseReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	sseReq.Header.Set("Content-Type", "application/json")
	sseReq.Header.Set("Accept", "application/json, text/event-stream")
	sseRec := httptest.NewRecorder()
	handler.ServeHTTP(sseRec, sseReq)
	if sseRec.Code != http.StatusOK {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): status = %d, want 200, body=%s", sseRec.Code, sseRec.Body.String())
	}
	if ct := sseRec.Header().Get("Content-Type"); ct != "text/event-stream" {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): Content-Type = %q, want %q — SSE negotiation did not run interpreted", ct, "text/event-stream")
	}

	body := sseRec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): body does not start with \"data: \": %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): body does not end with the SSE blank-line terminator — the exact missing-terminator defect class this negotiation was added to fix: %q", body)
	}

	dataLine := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	var resp struct {
		Result map[string]any  `json:"result"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(dataLine), &resp); err != nil {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): data line is not valid JSON-RPC: %w (line=%q)", err, dataLine)
	}
	if string(resp.ID) != "1" {
		return fmt.Errorf("POST /mcp ping (Accept: text/event-stream): id = %s, want the client's own id 1 echoed back", resp.ID)
	}

	fmt.Println("yaegi-check: federated MCP content negotiation served both JSON and SSE framing correctly, interpreted")
	return nil
}

// exerciseFailover drives one real POST /v1/chat/completions against the
// interpreted handler for failoverModelID — a bare model id both
// "failover-a" (always 500s) and "failover-b" (always 200s) are
// configured with — proving the failover candidate loop (runMeteredCall,
// routes_unified.go/failover.go) falls through a genuine provider
// failure and serves the second provider's response under the REAL
// interpreter. Compiled tests already prove this (failover_test.go); this
// harness exists because compiled tests have repeatedly passed on this
// codebase while the interpreted shape failed (this package's own doc
// comment).
func exerciseFailover(handler http.Handler, bHits *int64) error {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+failoverModelID+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (feat/failover harness): status = %d, want 200 (failover-a's 500 must fall through to failover-b), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served by failover-b") {
		return fmt.Errorf("POST /v1/chat/completions (feat/failover harness): body does not contain failover-b's own content, want the SECOND provider's response: %s", rec.Body.String())
	}
	if atomic.LoadInt64(bHits) != 1 {
		return fmt.Errorf("feat/failover harness: failover-b upstream hit count = %d, want exactly 1", atomic.LoadInt64(bHits))
	}
	fmt.Println("yaegi-check: failover fell through from failover-a to failover-b and served its response")
	return nil
}

// exerciseBareWinnerGroupAware proves registry.go's group-aware bareWinner
// fix (2026-08-27) under the REAL interpreter: "bare-winner-a" and
// "bare-winner-b" both serve bareWinnerModelID; bareWinnerGroupName may
// use "bare-winner-b" only, yet "bare-winner-a" sorts first
// alphabetically. Before the fix, resolve's bareWinner call always
// returned the GLOBAL sorted-first owner regardless of the caller's
// group, so this exact request got errModelDenied for a model the group
// was actually entitled to use — this is the production repro the fix
// exists for (bug report: "friends" group entitled to "uni", denied a
// bare id because "macstudio" sorts first and also serves it).
//
// Unlike exerciseFailover (immediately above, same colliding-bare-id
// shape): a correct group-aware bareWinner never even CONSIDERS
// "bare-winner-a" as a candidate for this group's request — it is not a
// failover fall-through after a failed attempt. aHits staying at 0 proves
// that: a pre-fix, group-blind bareWinner would route here first, and
// "bare-winner-a"'s upstream (run(), above) answers 500 with a body
// naming the exact wrong outcome, so a regression is unambiguous either
// way (aHits != 0, or a non-200 status, or the wrong response body).
//
// MUTATION VERIFIED: temporarily reverting bareWinner (registry.go) to
// its pre-fix, group-blind body (ignore grp entirely, always return the
// first provider in m.providerNames whose known model set contains id)
// made this fail with "status = 403" (errModelDenied) — bareWinnerGroupName
// cannot use "bare-winner-a", the pre-fix global winner. Reverted before
// committing.
func exerciseBareWinnerGroupAware(handler http.Handler, aHits *int64) error {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+bareWinnerModelID+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Authorization", "Bearer "+bareWinnerFriendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (fix/group-aware-bare-winner harness): status = %d, want 200 (%s is entitled to \"bare-winner-b\", which serves the bare id), body=%s", rec.Code, bareWinnerGroupName, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served by bare-winner-b") {
		return fmt.Errorf("POST /v1/chat/completions (fix/group-aware-bare-winner harness): body does not contain bare-winner-b's own content, want the group-visible provider's response: %s", rec.Body.String())
	}
	if got := atomic.LoadInt64(aHits); got != 0 {
		return fmt.Errorf("fix/group-aware-bare-winner harness: bare-winner-a upstream hit count = %d, want exactly 0 (%s can never reach bare-winner-a)", got, bareWinnerGroupName)
	}

	// GET /v1/models, same group: listFor's own half of the fix
	// (registry.go) — the bare id must be attributed to "bare-winner-b"
	// (the group's own visible winner), and "bare-winner-a" must never
	// appear anywhere in this group's catalog, bare or prefixed.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listReq.Header.Set("Authorization", "Bearer "+bareWinnerFriendAPIKey)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		return fmt.Errorf("GET /v1/models (fix/group-aware-bare-winner harness): status = %d, want 200, body=%s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), "bare-winner-a") {
		return fmt.Errorf("GET /v1/models (fix/group-aware-bare-winner harness): body names bare-winner-a, want it invisible to %s entirely: %s", bareWinnerGroupName, listRec.Body.String())
	}
	var listBody struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		return fmt.Errorf("decode GET /v1/models body (fix/group-aware-bare-winner harness): %w (body=%s)", err, listRec.Body.String())
	}
	found := false
	for _, entry := range listBody.Data {
		if entry["id"] != bareWinnerModelID {
			continue
		}
		found = true
		if entry["owned_by"] != "bare-winner-b" {
			return fmt.Errorf("GET /v1/models entry %v: owned_by = %v, want %q (this group's own visible winner)", entry, entry["owned_by"], "bare-winner-b")
		}
	}
	if !found {
		return fmt.Errorf("GET /v1/models body has no bare entry for %q: %s", bareWinnerModelID, listRec.Body.String())
	}

	fmt.Println("yaegi-check: group-aware bareWinner resolved and listed bare-winner-shared via bare-winner-b, never touching bare-winner-a")
	return nil
}

// exerciseRequestTimeout proves the request-timeout feature aborts a
// hung provider under the REAL interpreter, not merely in compiled
// tests: timeoutProviderName's own ProviderConfig.RequestTimeout ("80ms")
// is far shorter than timeoutUpstream's deliberate 600ms silence, and the
// request below carries NO client-side deadline of its own — unlike
// exerciseAttemptAccounting's slowProviderName probe (SHOULD-A), which
// proves a CLIENT-set context deadline classifies correctly, this proves
// the GATEWAY'S OWN adapter-level timeout aborts a hang no caller ever
// bounded, exactly the shape of a real, unbounded production client.
// A pass here means newAdapterHTTPClient's Transport.
// ResponseHeaderTimeout (providers.go) and watchdogBody's construction
// (timeout.go — context.WithCancel, time.AfterFunc, sync/atomic's
// function API) all compile and run correctly interpreted; a failure
// (a status other than 502, or an elapsed time anywhere near
// timeoutUpstreamSleep) would mean this feature works compiled but not
// under Yaegi, the exact class of divergence this harness exists to
// catch.
//
// MUTATION VERIFIED: temporarily removing
// `tr.ResponseHeaderTimeout = timeout` from newAdapterHTTPClient
// (providers.go) made `make yaegi-check` fail here with "status = 200,
// want 502" — timeoutUpstream's 600ms sleep let the request succeed
// instead of aborting at timeoutProviderRequestTimeout (80ms), proving
// this probe genuinely exercises the interpreted mechanism rather than
// passing regardless. Reverted before committing.
func exerciseRequestTimeout(handler http.Handler) error {
	body := `{"model":"` + timeoutProviderName + `/` + timeoutProviderModel + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > timeoutProbeMaxWait {
		return fmt.Errorf("POST /v1/chat/completions (hung provider %q) took %s, want well under %s — a hung provider must be aborted under the interpreter, not merely in compiled tests: %s", timeoutProviderName, elapsed, timeoutProbeMaxWait, rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		return fmt.Errorf("POST /v1/chat/completions (hung provider %q): status = %d, want 502 (upstream connection error, provider-attributed), body=%s", timeoutProviderName, rec.Code, rec.Body.String())
	}
	return nil
}

// exerciseRequestTimeoutMidBodyStall proves the OTHER half of the
// request-timeout feature aborts under the REAL interpreter (finding F7,
// coordinator adversarial review, 2026-08-23): exerciseRequestTimeout
// above drives timeoutProviderName, whose upstream never sends headers
// at all, so it only exercises Transport.ResponseHeaderTimeout —
// watchdogBody.fire (timeout.go) never runs under that probe, since the
// request never gets far enough to construct a watchdogBody in the first
// place. hungBodyProviderName's upstream sends headers immediately, then
// stalls, so this drives the idle-progress body watchdog itself:
// time.AfterFunc handed the interpreted method value wb.fire,
// context.WithCancel/CancelFunc, and sync/atomic's function API
// (atomic.StoreInt32/LoadInt32) all have to work correctly interpreted
// for this to abort rather than hang.
//
// MUTATION VERIFIED: temporarily removing the `resp.Body =
// newWatchdogBody(...)` line from upstreamBytes (providers.go) made
// exerciseRequestTimeout (the header-timeout probe) still PASS
// unchanged, while this probe failed with "status = 200, want 502" —
// hungBodyUpstream's 600ms sleep let the request succeed once nothing
// guarded the body read. This confirms the two probes exercise genuinely
// different code paths, and that this one specifically needs
// watchdogBody to pass. Reverted before committing.
func exerciseRequestTimeoutMidBodyStall(handler http.Handler) error {
	body := `{"model":"` + hungBodyProviderName + `/` + hungBodyProviderModel + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > timeoutProbeMaxWait {
		return fmt.Errorf("POST /v1/chat/completions (mid-body-stall provider %q) took %s, want well under %s — a provider that sends headers then stalls must still be aborted under the interpreter, not merely in compiled tests: %s", hungBodyProviderName, elapsed, timeoutProbeMaxWait, rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		return fmt.Errorf("POST /v1/chat/completions (mid-body-stall provider %q): status = %d, want 502 (upstream connection error, provider-attributed), body=%s", hungBodyProviderName, rec.Code, rec.Body.String())
	}
	return nil
}

func exerciseAttemptAccounting(handler http.Handler) error {
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+testDataWantModel+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	chatReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (attempt-accounting harness): status = %d, want 200, body=%s", chatRec.Code, chatRec.Body.String())
	}

	attemptsDay, failuresDay, err := pollProviderAttemptCounters(handler, "openai")
	if err != nil {
		return err
	}
	if attemptsDay != 1 {
		return fmt.Errorf(`admin overview provider "openai" attemptsDay = %v, want 1 (Feature A attempt-accounting harness)`, attemptsDay)
	}
	if failuresDay != 0 {
		return fmt.Errorf(`admin overview provider "openai" failuresDay = %v, want 0`, failuresDay)
	}

	// SHOULD-A: a real, interpreted context-deadline timeout against a
	// deliberately slow upstream must record a FAILURE — the whole
	// reason this harness exists after round 2's blocker.
	slowCtx, cancel := context.WithTimeout(context.Background(), slowRequestDeadline)
	defer cancel()
	slowReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+slowProviderName+`/`+slowProviderModel+`","messages":[{"role":"user","content":"hi"}]}`,
	)).WithContext(slowCtx)
	slowReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	slowReq.Header.Set("Content-Type", "application/json")
	slowRec := httptest.NewRecorder()
	handler.ServeHTTP(slowRec, slowReq)
	// No status assertion here on purpose: the point of interest is the
	// PROVIDER-HEALTH counter a timed-out upstream attempt leaves behind,
	// not what status code a canceled/timed-out client request itself
	// gets back (which the existing route-level Go tests already pin).

	slowAttemptsDay, slowFailuresDay, err := pollProviderAttemptCounters(handler, slowProviderName)
	if err != nil {
		return err
	}
	if slowAttemptsDay != 1 {
		return fmt.Errorf("admin overview provider %q attemptsDay = %v, want 1 (SHOULD-A timeout harness)", slowProviderName, slowAttemptsDay)
	}
	if slowFailuresDay != 1 {
		return fmt.Errorf("admin overview provider %q failuresDay = %v, want 1 — a real interpreted context-deadline timeout must count as a provider-health failure (SHOULD-A, closes the round-2 blocker)", slowProviderName, slowFailuresDay)
	}
	return nil
}

// pollProviderAttemptCounters drives GET /admin/api/overview against
// handler repeatedly until providerName's attemptsDay is non-zero or a 2s
// budget elapses, then returns its final reading either way — SHOULD-5
// (v0.22 review round): recordProviderAttempt's store write runs on its
// own goroutine (limiter.spawn), off the request that triggered it, so a
// single immediate read right after that request returns could race
// ahead of the write landing. A real goroutine still schedules promptly
// under Yaegi (only the INTERPRETED code driving it runs slower, not the
// underlying Go runtime's scheduler), so this is a short poll, not a
// long one.
func pollProviderAttemptCounters(handler http.Handler, providerName string) (attemptsDay, failuresDay float64, err error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		attemptsDay, failuresDay, err = readProviderAttemptCounters(handler, providerName)
		if err != nil {
			return 0, 0, err
		}
		if attemptsDay != 0 || time.Now().After(deadline) {
			return attemptsDay, failuresDay, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readProviderAttemptCounters reads GET /admin/api/overview, finds the
// entry named providerName, and returns its attemptsDay/failuresDay
// fields via a generic map[string]any decode — this harness module is
// compiled, not interpreted, so it could import the plugin's own admin.go
// types directly, but they are unexported (adminOverviewResponse,
// adminProviderView); decoding generically here is simpler than exporting
// test-only types across that boundary just for this one harness. Looked
// up by name, not index 0 (round 1 of this harness only ever had one
// configured provider): buildAdminOverview sorts providers by name
// (registry.go), so adding slowProviderName ("slow") after "openai"
// changed which index held which provider.
func readProviderAttemptCounters(handler http.Handler, providerName string) (attemptsDay, failuresDay float64, err error) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return 0, 0, fmt.Errorf("GET /admin/api/overview: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return 0, 0, fmt.Errorf("decode GET /admin/api/overview body: %w", err)
	}
	providers, ok := body["providers"].([]any)
	if !ok {
		return 0, 0, fmt.Errorf("admin overview body has no providers array: %s", rec.Body.String())
	}
	for _, raw := range providers {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := p["name"].(string); name != providerName {
			continue
		}
		attemptsDay, _ = p["attemptsDay"].(float64)
		failuresDay, _ = p["failuresDay"].(float64)
		return attemptsDay, failuresDay, nil
	}
	return 0, 0, fmt.Errorf("admin overview body has no provider named %q: %s", providerName, rec.Body.String())
}

// readModulePath returns the module path declared by goModPath's
// "module" directive.
func readModulePath(goModPath string) (string, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no module directive found in %s", goModPath)
}

// readPackageName returns the package name declared by the first
// non-test .go file it finds directly inside dir. The plugin is a single
// root package, so any one of its files' declarations suffices.
func readPackageName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		pkgName, err := scanPackageDecl(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if pkgName != "" {
			return pkgName, nil
		}
	}
	return "", fmt.Errorf("no package declaration found in %s", dir)
}

// scanPackageDecl reads path's leading "package X" line, if any.
func scanPackageDecl(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "package "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", scanner.Err()
}

// copyRepoSource copies repoRoot into destDir, skipping every directory
// named in excludedTopLevelDirs. destDir plays the role of
// GOPATH/src/{modulePath} for the interpreter: Yaegi's source loader
// resolves the plugin's own import path by walking GOPATH exactly as the
// standard toolchain would for a pre-modules GOPATH-mode build.
func copyRepoSource(repoRoot, destDir string) error {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return err
	}
	for _, e := range entries {
		if excludedTopLevelDirs[e.Name()] {
			continue
		}
		src := filepath.Join(repoRoot, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if e.IsDir() {
			if err := copyDir(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target)
	})
}

// copyFile copies src to dst, creating dst's parent directory if needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-side close; nothing actionable on failure

	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // Close's error is checked explicitly below
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// yamlLine is one non-blank, non-comment line of a YAML file, with
// leading whitespace stripped and recorded as indent.
type yamlLine struct {
	text   string
	indent int
}

// extractTraefikYAMLTestData reads path (the plugin's .traefik.yml) and
// decodes its top-level "testData:" block into a generic
// map[string]any/[]any/scalar tree, using a small hand-rolled,
// indentation-based YAML subset parser: just enough for this one file's
// shape (nested maps, lists of scalars, lists of flat maps, "{}"/"[]"
// empty collections), not a general YAML implementation. It exists only
// in this harness module — the plugin module never parses YAML itself,
// Traefik does that before handing the plugin its already-decoded Config.
func extractTraefikYAMLTestData(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := tokenizeYAMLLines(string(raw))
	block := extractTestDataBlock(lines)
	if block == nil {
		return nil, fmt.Errorf("%s has no top-level testData: block", path)
	}
	pos := 0
	v := parseBlock(block, &pos, block[0].indent)
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s testData: block is not a map", path)
	}
	return m, nil
}

// tokenizeYAMLLines splits raw into non-blank, non-comment yamlLines,
// each carrying its own leading-space count as indent.
func tokenizeYAMLLines(raw string) []yamlLine {
	var out []yamlLine
	for _, ln := range strings.Split(raw, "\n") {
		trimmedRight := strings.TrimRight(ln, " \t\r")
		content := strings.TrimLeft(trimmedRight, " ")
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		out = append(out, yamlLine{indent: len(trimmedRight) - len(content), text: content})
	}
	return out
}

// extractTestDataBlock returns the lines nested under a top-level (indent
// 0) "testData:" line, or nil if lines has none.
func extractTestDataBlock(lines []yamlLine) []yamlLine {
	for i, l := range lines {
		if l.indent != 0 || l.text != "testData:" {
			continue
		}
		var block []yamlLine
		for j := i + 1; j < len(lines) && lines[j].indent > 0; j++ {
			block = append(block, lines[j])
		}
		return block
	}
	return nil
}

// parseBlock parses the run of lines starting at *pos that share indent,
// dispatching to parseList when the first such line is a "-" item and to
// parseMap otherwise.
func parseBlock(lines []yamlLine, pos *int, indent int) any {
	if *pos >= len(lines) || lines[*pos].indent != indent {
		return map[string]any{}
	}
	if strings.HasPrefix(lines[*pos].text, "-") {
		return parseList(lines, pos, indent)
	}
	return parseMap(lines, pos, indent)
}

// parseMap consumes every consecutive "key: value" line at indent,
// recursing into parseBlock for a key whose value is empty (a nested
// block follows on deeper-indented lines).
func parseMap(lines []yamlLine, pos *int, indent int) map[string]any {
	m := map[string]any{}
	for *pos < len(lines) && lines[*pos].indent == indent {
		key, val, _ := strings.Cut(lines[*pos].text, ":")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		*pos++
		if val != "" {
			m[key] = collectionOrScalar(val)
			continue
		}
		if *pos < len(lines) && lines[*pos].indent > indent {
			m[key] = parseBlock(lines, pos, lines[*pos].indent)
		} else {
			m[key] = map[string]any{}
		}
	}
	return m
}

// parseList consumes every consecutive "- ..." line at indent. A "-"
// item containing a colon starts a flat map (its own "key: value" plus
// every following line indented at least indent+2, until a line back at
// indent or shallower); any other item is a bare scalar.
func parseList(lines []yamlLine, pos *int, indent int) []any {
	var out []any
	for *pos < len(lines) && lines[*pos].indent == indent && strings.HasPrefix(lines[*pos].text, "-") {
		item := strings.TrimSpace(strings.TrimPrefix(lines[*pos].text, "-"))
		if item == "" {
			*pos++
			continue
		}
		if !strings.Contains(item, ":") {
			out = append(out, parseScalar(item))
			*pos++
			continue
		}

		m := map[string]any{}
		key, val, _ := strings.Cut(item, ":")
		m[strings.TrimSpace(key)] = collectionOrScalar(strings.TrimSpace(val))
		*pos++

		childIndent := indent + 2
		for *pos < len(lines) && lines[*pos].indent >= childIndent {
			k, v, _ := strings.Cut(lines[*pos].text, ":")
			m[strings.TrimSpace(k)] = collectionOrScalar(strings.TrimSpace(v))
			*pos++
		}
		out = append(out, m)
	}
	return out
}

// collectionOrScalar maps a "key: value" line's already-trimmed val to
// an empty map/slice for "{}"/"[]", or to parseScalar(val) otherwise. It
// does not recurse into a further nested block — no key in this file's
// testData needs a map/list value inside a list item's own fields, and
// this harness parses only what .traefik.yml actually contains.
func collectionOrScalar(val string) any {
	switch val {
	case "{}":
		return map[string]any{}
	case "[]":
		return []any{}
	default:
		return parseScalar(val)
	}
}

// parseScalar converts a YAML scalar token to a bool, int64, float64, or
// string, in that preference order, stripping a matching pair of quotes
// first.
func parseScalar(s string) any {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// modelUsageCanonicalID is the kindModel scope id exerciseModelUsageRanking
// expects its own chat completion to be attributed to: the canonical
// "provider/model" of the bare testDataWantModel's winning provider
// ("openai" — the same attribution exerciseAttemptAccounting already
// relies on).
const modelUsageCanonicalID = "openai/" + testDataWantModel

// exerciseModelUsageRanking drives the per-model usage feature end to end
// under the INTERPRETER (feat: per-model usage statistics): one real chat
// completion, then GET /admin/api/usage/models for the ranking and GET
// /admin/api/usage/history for that same model's own series.
//
// Why this needs an interpreted probe at all, when compiled tests already
// cover the same code: the feature adds a scope kind resolved POST-response
// (limits.go's withModelScope/kindModel), reached through a sort.Slice
// closure and a generic JSON encode of a newly declared struct type. Every
// one of those is a construct this repo has previously seen diverge under
// Yaegi — a compiled pass proves nothing about the interpreter, which is
// the whole premise of this harness.
func exerciseModelUsageRanking(handler http.Handler) error {
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"`+testDataWantModel+`","messages":[{"role":"user","content":"hi"}]}`,
	))
	chatReq.Header.Set("Authorization", "Bearer "+testDataUserAPIKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		return fmt.Errorf("POST /v1/chat/completions (model-usage harness): status = %d, want 200, body=%s", chatRec.Code, chatRec.Body.String())
	}

	models, err := pollModelRanking(handler)
	if err != nil {
		return err
	}

	// The ranking must contain the model that just served, and EVERY entry
	// must be non-zero — the operator requirement the server-side filter
	// exists for. A zero here would mean the filter did not run under the
	// interpreter even though it does when compiled.
	var found bool
	for _, raw := range models {
		entry, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("GET /admin/api/usage/models: entry is not an object: %v", raw)
		}
		id, _ := entry["id"].(string)
		value, _ := entry["value"].(float64)
		if value == 0 {
			return fmt.Errorf("GET /admin/api/usage/models listed %q at value 0 — only non-zero models may be returned", id)
		}
		if id == modelUsageCanonicalID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("GET /admin/api/usage/models did not list %q after a served request: %v", modelUsageCanonicalID, models)
	}

	// The same model must also be reachable as an ordinary history scope —
	// the point of keying model usage as a scope kind rather than a
	// bespoke counter.
	histReq := httptest.NewRequest(http.MethodGet,
		"/admin/api/usage/history?scope=model:"+url.QueryEscape(modelUsageCanonicalID)+"&metric=req&window=day&span=1", nil)
	histReq.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	histRec := httptest.NewRecorder()
	handler.ServeHTTP(histRec, histReq)
	if histRec.Code != http.StatusOK {
		return fmt.Errorf("GET /admin/api/usage/history for scope model:%s: status = %d, want 200, body=%s",
			modelUsageCanonicalID, histRec.Code, histRec.Body.String())
	}
	var hist map[string]any
	if err := json.Unmarshal(histRec.Body.Bytes(), &hist); err != nil {
		return fmt.Errorf("decode model history body: %w", err)
	}
	points, ok := hist["points"].([]any)
	if !ok || len(points) != 1 {
		return fmt.Errorf("model history points = %v, want exactly 1 bucket: %s", hist["points"], histRec.Body.String())
	}
	point, ok := points[0].(map[string]any)
	if !ok {
		return fmt.Errorf("model history point is not an object: %v", points[0])
	}
	if value, _ := point["value"].(float64); value < 1 {
		return fmt.Errorf("model history value = %v, want at least 1 request attributed to %q", point["value"], modelUsageCanonicalID)
	}

	// An id outside the catalog must 404 rather than answer an empty
	// series — the guard that keeps a mistyped or retired model from
	// looking like a model with no traffic.
	unknownReq := httptest.NewRequest(http.MethodGet,
		"/admin/api/usage/history?scope=model:openai/no-such-model&metric=req&window=day&span=1", nil)
	unknownReq.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
	unknownRec := httptest.NewRecorder()
	handler.ServeHTTP(unknownRec, unknownReq)
	if unknownRec.Code != http.StatusNotFound {
		return fmt.Errorf("GET /admin/api/usage/history for an uncatalogued model: status = %d, want 404, body=%s",
			unknownRec.Code, unknownRec.Body.String())
	}

	fmt.Println("yaegi-check: per-model usage ranking and model-scoped history served under the interpreter; only non-zero models listed, uncatalogued model 404s")
	return nil
}

// pollModelRanking reads GET /admin/api/usage/models until it reports at
// least one model or a 2s budget elapses. The poll mirrors
// pollProviderAttemptCounters' own rationale: interpreted code driving the
// request runs slower than compiled code, so a single immediate read can
// outrun the accounting write the preceding request triggered.
func pollModelRanking(handler http.Handler) ([]any, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/models?metric=req&window=day", nil)
		req.Header.Set("Authorization", "Bearer "+attemptAccountingAdminAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return nil, fmt.Errorf("GET /admin/api/usage/models: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return nil, fmt.Errorf("decode GET /admin/api/usage/models body: %w", err)
		}
		models, ok := body["models"].([]any)
		if !ok {
			return nil, fmt.Errorf("admin usage/models body has no models array: %s", rec.Body.String())
		}
		if len(models) > 0 || time.Now().After(deadline) {
			return models, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
