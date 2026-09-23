package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	traefikllmgateway "github.com/lukaszraczylo/traefik-llmgateway"
)

// fixturePath is a real production config captured from the live cluster
// (`kubectl -n traefik get middleware llmgateway -o json`, converted to
// the ConfigMap-shaped YAML a standalone binary consumes). It carries no
// key material: every real provider key in it is a `file:` reference
// resolved at construction from a mounted volume, never a literal.
//
// It is checked in rather than read from a scratch directory so `make
// test` works from a clean clone. testdata/ is export-ignored
// (.gitattributes), so it never reaches the plugin's release tarball.
const fixturePath = "testdata/llmgw-config.yaml"

// Counts and values below are taken from the fixture itself, not from the
// struct definitions, and are what the direct-yaml path destroys.
const (
	fixtureProviders    = 18
	fixtureMCPServers   = 11
	fixtureModelAliases = 3
	fixtureModelMeta    = 21
	fixtureAgents       = 34
	fixtureGroups       = 3
	fixtureUsersFile    = "/llmgw/users/users.json"
	fixtureMetricsCIDR  = "192.0.2.0/24"
)

// bothFieldProviders are the providers in the fixture that genuinely
// carry BOTH apiKey and baseUrl, so both can be asserted non-empty.
//
// The set is deliberately NOT "every provider": anthropic has an apiKey
// but no baseUrl by design (it defaults upstream via
// defaultBaseURLByType), so a blanket assertion across all 18 would fail
// for a reason that has nothing to do with the decode being correct.
var bothFieldProviders = map[string]struct{ apiKey, baseURL string }{
	// G101 false positive: the "file:" prefix is resolveSecret's
	// file-reference form (secrets.go) — a PATH the gateway reads the key
	// out of at construction, not a key. This fixture carries no real
	// credential.
	//nolint:gosec
	"alibaba": {
		apiKey:  "file:/llmgw/providers/alibaba",
		baseURL: "https://ws-example.eu-central-1.maas.aliyuncs.com/compatible-mode",
	},
	// G101 false positive, same as "alibaba" above: a resolveSecret file
	// reference, not a credential.
	//nolint:gosec
	"cerebras": {
		apiKey:  "file:/llmgw/providers/cerebras",
		baseURL: "https://api.cerebras.ai",
	},
	"codex": {
		apiKey:  "unused",
		baseURL: "http://codex-shim.mcp-servers.svc.example.invalid:8080",
	},
}

// TestLoadConfigRoundTripPopulatesCamelCaseFields is the primary
// regression test for the loader's reason to exist. It asserts the real
// VALUES the round-trip recovers, not merely that loading returned no
// error — a nil error is exactly what the broken path also returns.
func TestLoadConfigRoundTripPopulatesCamelCaseFields(t *testing.T) {
	cfg, err := loadConfig(fixturePath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if got := len(cfg.Providers); got != fixtureProviders {
		t.Fatalf("providers = %d, want %d", got, fixtureProviders)
	}
	for name, want := range bothFieldProviders {
		pc, ok := cfg.Providers[name]
		if !ok {
			t.Fatalf("provider %q missing from decoded config", name)
		}
		if pc.APIKey != want.apiKey {
			t.Errorf("provider %q apiKey = %q, want %q", name, pc.APIKey, want.apiKey)
		}
		if pc.BaseURL != want.baseURL {
			t.Errorf("provider %q baseUrl = %q, want %q", name, pc.BaseURL, want.baseURL)
		}
	}

	// anthropic pins the other half of the contract: apiKey present,
	// baseUrl legitimately absent. If a future change "fixes" this by
	// populating a baseUrl, the defaulting behaviour changed.
	anthropic, ok := cfg.Providers["anthropic"]
	if !ok {
		t.Fatal("provider \"anthropic\" missing from decoded config")
	}
	if anthropic.APIKey != "file:/llmgw/providers/anthropic" {
		t.Errorf("anthropic apiKey = %q, want file:/llmgw/providers/anthropic", anthropic.APIKey)
	}
	if anthropic.BaseURL != "" {
		t.Errorf("anthropic baseUrl = %q, want empty (it defaults upstream)", anthropic.BaseURL)
	}

	// Per-provider camelCase keys beyond apiKey/baseUrl.
	if got := cfg.Providers["gx10"].DiscoveryInterval; got != "5m" {
		t.Errorf("gx10 discoveryInterval = %q, want 5m", got)
	}
	if got := cfg.Providers["gx10"].RequestTimeout; got != "15m" {
		t.Errorf("gx10 requestTimeout = %q, want 15m", got)
	}
	if got := cfg.Providers["macstudio"].MetadataPath; got != "/api/v0/models" {
		t.Errorf("macstudio metadataPath = %q, want /api/v0/models", got)
	}

	// Blocks the direct-yaml path drops wholesale, because their own
	// top-level keys are camelCase.
	if got := len(cfg.MCPServers); got != fixtureMCPServers {
		t.Errorf("mcpServers = %d, want %d", got, fixtureMCPServers)
	}
	if got := len(cfg.ModelAliases); got != fixtureModelAliases {
		t.Errorf("modelAliases = %d, want %d", got, fixtureModelAliases)
	}
	if got := cfg.ModelAliases["deepseek"]; got != "uni/deepseek-v4-flash-0731" {
		t.Errorf("modelAliases[deepseek] = %q, want uni/deepseek-v4-flash-0731", got)
	}
	if got := len(cfg.ModelMeta); got != fixtureModelMeta {
		t.Errorf("modelMeta = %d, want %d", got, fixtureModelMeta)
	}

	// metrics.allowedCIDRs decides whether an unauthenticated scrape is
	// allowed at all, so silently emptying it is a security-relevant
	// decode failure, not a cosmetic one.
	if cfg.Metrics == nil {
		t.Fatal("metrics block missing from decoded config")
	}
	if got := cfg.Metrics.AllowedCIDRs; len(got) != 1 || got[0] != fixtureMetricsCIDR {
		t.Errorf("metrics.allowedCIDRs = %v, want [%s]", got, fixtureMetricsCIDR)
	}
	if cfg.Redis == nil || cfg.Redis.FailOpen == nil || !*cfg.Redis.FailOpen {
		t.Errorf("redis.failOpen did not decode to *true (got %+v)", cfg.Redis)
	}

	// Keys that are already all-lowercase survive BOTH paths. Asserting
	// them here is what makes the direct-path test below meaningful:
	// these are why the broken decode still boots and looks plausible.
	if got := len(cfg.Agents); got != fixtureAgents {
		t.Errorf("agents = %d, want %d", got, fixtureAgents)
	}
	if got := len(cfg.Groups); got != fixtureGroups {
		t.Errorf("groups = %d, want %d", got, fixtureGroups)
	}
	if cfg.Users == nil || cfg.Users.File != fixtureUsersFile {
		t.Errorf("users.file did not decode to %s (got %+v)", fixtureUsersFile, cfg.Users)
	}
}

// TestDirectYAMLUnmarshalSilentlyLosesCamelCaseFields documents WHY
// loadConfig round-trips through JSON, by proving the obvious-looking
// alternative fails the exact assertions above while returning a nil
// error. If yaml tags are ever added to the root package's Config, this
// test fails and the round-trip can be reconsidered deliberately — which
// is the point.
func TestDirectYAMLUnmarshalSilentlyLosesCamelCaseFields(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	direct := traefikllmgateway.CreateConfig()
	if err := yaml.Unmarshal(raw, direct); err != nil {
		t.Fatalf("direct yaml.Unmarshal returned an error (%v); this test assumes it fails SILENTLY", err)
	}

	// The failure is silent, not loud: the structure that makes the
	// gateway boot is all still there.
	if got := len(direct.Providers); got != fixtureProviders {
		t.Fatalf("direct path providers = %d, want %d — the trap is that providers DO survive", got, fixtureProviders)
	}

	// ...but every camelCase leaf is gone.
	for name := range bothFieldProviders {
		pc, ok := direct.Providers[name]
		if !ok {
			t.Fatalf("direct path: provider %q missing entirely", name)
		}
		if pc.APIKey != "" {
			t.Errorf("direct path: provider %q apiKey = %q, want \"\" — expected the direct decode to lose it", name, pc.APIKey)
		}
		if pc.BaseURL != "" {
			t.Errorf("direct path: provider %q baseUrl = %q, want \"\" — expected the direct decode to lose it", name, pc.BaseURL)
		}
	}
	if len(direct.MCPServers) != 0 {
		t.Errorf("direct path: mcpServers = %d, want 0 (key is camelCase)", len(direct.MCPServers))
	}
	if len(direct.ModelAliases) != 0 {
		t.Errorf("direct path: modelAliases = %d, want 0 (key is camelCase)", len(direct.ModelAliases))
	}
	if len(direct.ModelMeta) != 0 {
		t.Errorf("direct path: modelMeta = %d, want 0 (key is camelCase)", len(direct.ModelMeta))
	}
	if direct.Metrics != nil && len(direct.Metrics.AllowedCIDRs) != 0 {
		t.Errorf("direct path: metrics.allowedCIDRs = %v, want empty (key is camelCase)",
			direct.Metrics.AllowedCIDRs)
	}

	// The all-lowercase keys survive, which is precisely why the broken
	// config boots rather than failing fast.
	if direct.Users == nil || direct.Users.File != fixtureUsersFile {
		t.Errorf("direct path: users.file = %+v, want %s to survive (key is all-lowercase)", direct.Users, fixtureUsersFile)
	}
	if len(direct.Agents) != fixtureAgents {
		t.Errorf("direct path: agents = %d, want %d to survive (key is all-lowercase)", len(direct.Agents), fixtureAgents)
	}
}

// TestLoadConfigAcceptsJSON checks the .json shortcut decodes to the same
// Config as the YAML form, since the JSON shape is what kubectl emits.
func TestLoadConfigAcceptsJSON(t *testing.T) {
	fromYAML, err := loadConfig(fixturePath)
	if err != nil {
		t.Fatalf("loadConfig(yaml): %v", err)
	}
	j, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonPath := filepath.Join(t.TempDir(), "config.json")
	if writeErr := os.WriteFile(jsonPath, j, 0o600); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}

	fromJSON, err := loadConfig(jsonPath)
	if err != nil {
		t.Fatalf("loadConfig(json): %v", err)
	}
	again, err := json.Marshal(fromJSON)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(again) != string(j) {
		t.Error("json config path decoded to a different Config than the yaml path")
	}
}

// TestLoadConfigRejectsNonStringMappingKey pins the one round-trip edge
// case that fails loudly: a mapping key YAML reads as a number cannot
// become a JSON object key. The wrapped message must name the fix,
// because the underlying error names only an offset or a map type.
func TestLoadConfigRejectsNonStringMappingKey(t *testing.T) {
	const doc = `
providers:
  openai:
    type: openai
    apiKey: k
modelMeta:
  3.5:
    free: true
`
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("loadConfig accepted a numeric mapping key; want an error")
	}
	if !strings.Contains(err.Error(), "quote it") {
		t.Errorf("error does not explain the fix: %v", err)
	}
}

// TestLoadConfigMissingFile checks a bad -config path fails loudly rather
// than yielding an empty Config that would fail much later with a
// confusing "at least one provider must be configured".
func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("loadConfig accepted a missing file; want an error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error = %v, want it to name the read failure", err)
	}
}

// TestLoadConfigRejectsUnknownField is the F12 regression test
// (review-auth.md finding 12): loadConfig's decoder must run with
// DisallowUnknownFields so a misspelled or nonexistent key fails loudly
// at startup instead of silently doing nothing — the exact failure class
// yamlToJSON's own doc comment already documents one real incident of
// for the direct-yaml path (a whole different bug, same "wrong key,
// nil error" shape). This fails before the fix (plain json.Unmarshal
// drops the key silently, err == nil) and passes after.
func TestLoadConfigRejectsUnknownField(t *testing.T) {
	const doc = `
providers:
  openai:
    type: openai
    apiKey: k
notARealConfigKey: true
`
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("loadConfig accepted an unknown top-level key; want an error")
	}
	if !strings.Contains(err.Error(), "notARealConfigKey") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

// TestLoadConfigStillAcceptsRealFixture is the other half of F12's own
// regression coverage: the strict decoder must not reject a real,
// legitimate production config — the exact fixture
// TestLoadConfigRoundTripPopulatesCamelCaseFields already exercises,
// re-asserted here as its own named test so a future reviewer sees the
// negative (rejects unknown) and positive (accepts known) cases side by
// side.
func TestLoadConfigStillAcceptsRealFixture(t *testing.T) {
	if _, err := loadConfig(fixturePath); err != nil {
		t.Fatalf("loadConfig(fixturePath) = %v, want the real production fixture to still load under strict decoding", err)
	}
}

// TestMetricsCIDRWarning is the F5 regression test (review-auth.md
// finding 5): metricsCIDRWarning must warn exactly when
// Metrics.AllowedCIDRs is non-empty, and stay silent for every other
// shape (nil Metrics, Metrics with no CIDRs at all) — good/bad/edge,
// table-driven.
func TestMetricsCIDRWarning(t *testing.T) {
	cases := []struct {
		cfg  *traefikllmgateway.Config
		name string
		want bool
	}{
		{name: "nil metrics block", cfg: &traefikllmgateway.Config{}, want: false},
		{
			name: "metrics enabled, no allowedCIDRs",
			cfg:  &traefikllmgateway.Config{Metrics: &traefikllmgateway.MetricsConfig{Enabled: true}},
			want: false,
		},
		{
			name: "allowedCIDRs set",
			cfg:  &traefikllmgateway.Config{Metrics: &traefikllmgateway.MetricsConfig{AllowedCIDRs: []string{"10.42.0.0/16"}}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, warn := metricsCIDRWarning(tc.cfg)
			if warn != tc.want {
				t.Fatalf("warn = %v, want %v", warn, tc.want)
			}
			if warn && !strings.Contains(msg, "BEHIND Traefik") {
				t.Errorf("message = %q, want it to explain the source address is Traefik's own", msg)
			}
			if !warn && msg != "" {
				t.Errorf("message = %q, want empty when warn is false", msg)
			}
		})
	}

	t.Run("real fixture triggers the warning", func(t *testing.T) {
		cfg, err := loadConfig(fixturePath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if _, warn := metricsCIDRWarning(cfg); !warn {
			t.Error("want the fixture's own metrics.allowedCIDRs to trigger the warning")
		}
	})
}

// TestTerminalHandlerEnvelope pins the hand-copied 404 envelope against
// the shape the root package's writeOAIError produces. writeOAIError is
// unexported, so this asserts the contract literally: if the root
// package's envelope ever changes, this is what catches the drift.
func TestTerminalHandlerEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	terminalHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the OpenAI error envelope: %v (%s)", err, rec.Body.String())
	}
	if got.Error.Message != "unknown route" || got.Error.Type != "invalid_request_error" || got.Error.Code != "404" {
		t.Errorf("envelope = %+v, want {unknown route invalid_request_error 404}", got.Error)
	}
}

// TestWithHealth covers both probe paths and, more importantly, that
// everything else is delegated with the URL untouched.
func TestWithHealth(t *testing.T) {
	var ready atomic.Bool
	var seenPath, seenEscaped string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenEscaped = r.URL.EscapedPath()
	})
	h := withHealth(next, &ready, "/healthz", "/readyz")

	t.Run("liveness is unconditional", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 even before ready", rec.Code)
		}
	})

	t.Run("readiness reflects the flag", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 while not ready", rec.Code)
		}
		ready.Store(true)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 once ready", rec.Code)
		}
	})

	t.Run("a POST to the probe path is delegated", func(t *testing.T) {
		seenPath = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
		if seenPath != "/healthz" {
			t.Errorf("POST /healthz was answered by the probe; want it delegated (saw %q)", seenPath)
		}
	})

	// The reason this wrapper is hand-written instead of an
	// http.ServeMux: a mux would path-clean and 301-redirect these before
	// dispatch, while the gateway routes on the raw EscapedPath and
	// passthrough's traversal checks depend on it arriving unmodified.
	t.Run("paths reach the gateway unnormalized", func(t *testing.T) {
		for _, raw := range []string{"/v1//models", "/openai/a/../b", "/openai/a%2Fb"} {
			seenPath, seenEscaped = "", ""
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, raw, nil))
			if rec.Code == http.StatusMovedPermanently {
				t.Fatalf("%s was redirected; the wrapper must never path-clean", raw)
			}
			if seenEscaped != raw {
				t.Errorf("EscapedPath for %s = %q, want it delegated verbatim (Path was %q)", raw, seenEscaped, seenPath)
			}
		}
	})
}

// TestParseOptionsPrecedence pins flag > env > default, the house rule
// that an explicit override always wins.
func TestParseOptionsPrecedence(t *testing.T) {
	t.Run("default when neither is set", func(t *testing.T) {
		o, err := parseOptions(nil, os.Stderr)
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if o.listenAddr != defaultListenAddr {
			t.Errorf("listen = %q, want %q", o.listenAddr, defaultListenAddr)
		}
		if o.configPath != defaultConfigPath {
			t.Errorf("config = %q, want %q", o.configPath, defaultConfigPath)
		}
	})

	t.Run("env supplies the default", func(t *testing.T) {
		t.Setenv("LLMGW_LISTEN", ":9999")
		t.Setenv("LLMGW_SHUTDOWN_TIMEOUT", "45s")
		o, err := parseOptions(nil, os.Stderr)
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if o.listenAddr != ":9999" {
			t.Errorf("listen = %q, want :9999 from env", o.listenAddr)
		}
		if o.shutdownWait.String() != "45s" {
			t.Errorf("shutdown-timeout = %s, want 45s from env", o.shutdownWait)
		}
	})

	t.Run("an explicit flag beats env", func(t *testing.T) {
		t.Setenv("LLMGW_LISTEN", ":9999")
		o, err := parseOptions([]string{"-listen", ":7777"}, os.Stderr)
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if o.listenAddr != ":7777" {
			t.Errorf("listen = %q, want :7777 from the flag", o.listenAddr)
		}
	})

	t.Run("an unparseable env duration falls back", func(t *testing.T) {
		t.Setenv("LLMGW_DRAIN_DELAY", "soon")
		o, err := parseOptions(nil, os.Stderr)
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if o.drainDelay != defaultDrainDelay {
			t.Errorf("drain-delay = %s, want the %s default", o.drainDelay, defaultDrainDelay)
		}
	})

	t.Run("identical probe paths are rejected", func(t *testing.T) {
		if _, err := parseOptions([]string{"-health-path", "/x", "-ready-path", "/x"}, os.Stderr); err == nil {
			t.Error("parseOptions accepted identical health and ready paths")
		}
	})
}
