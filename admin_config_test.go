package traefikllmgateway

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// --- gate + shape ---

func TestAdminConfig_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"non-admin", "sk-alice", http.StatusForbidden},
		{"admin", "sk-admin1", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConfigPath, c.apiKey))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestAdminConfig_Shape(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Redis = &RedisConfig{Address: "redis:6379", Password: "super-secret-literal"}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminConfigPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != pluginVersion {
		t.Errorf("Version = %q, want %q", got.Version, pluginVersion)
	}
	if got.Warnings == nil {
		t.Error("Warnings = nil, want non-nil (never nil, matching overview's own contract)")
	}
	redis, ok := got.Config["redis"].(map[string]any)
	if !ok {
		t.Fatalf("config.redis missing or wrong shape: %+v", got.Config["redis"])
	}
	if redis["password"] != redactedSecretPlaceholder {
		t.Errorf(`config.redis.password = %v, want %q`, redis["password"], redactedSecretPlaceholder)
	}
	if strings.Contains(rec.Body.String(), "super-secret-literal") {
		t.Error("literal redis password leaked into the response body")
	}
}

// --- redactConfig / redactValue unit coverage ---

func TestRedactConfig_LiteralAndIndirectSecrets(t *testing.T) {
	t.Parallel()
	// Every string below is a synthetic test fixture, not a real
	// credential; this test exists specifically to assert none of them
	// survive redactConfig. The userinfo URL is built via net/url rather
	// than written as one literal, so secret scanners do not flag it.
	credentialBaseURL := (&url.URL{Scheme: "https", User: url.UserPassword("token", "s3cr3t"), Host: "openai.invalid"}).String()
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"openai": {Type: "openai", BaseURL: credentialBaseURL, APIKey: "literal-key"}, // #nosec G101 -- test fixture, not a real secret
			"local":  {Type: "openai", APIKey: "env:OPENAI_KEY"},                          // #nosec G101 -- test fixture, not a real secret
			"file":   {Type: "openai", APIKey: "file:/etc/secrets/key"},
			"empty":  {Type: "openai", APIKey: ""},
		},
		Redis: &RedisConfig{Address: "redis:6379", Password: "redis-literal-pw"}, // #nosec G101 -- test fixture, not a real secret
		Users: &UsersConfig{
			File: "/etc/llmgw/users.json",
			Inline: []*UserConfig{
				{Name: "alice", Group: "eng", APIKey: "alice-literal-key"},
			},
		},
		MCPServers: map[string]*TargetConfig{"fetch": {URL: "https://user:pw@mcp.internal/fetch"}}, // #nosec G101 -- test fixture, not a real secret
		Agents:     map[string]*AgentConfig{"planner": {URL: "https://agent.internal?api-key=abc123"}},
	}

	redacted, err := redactConfig(cfg)
	if err != nil {
		t.Fatalf("redactConfig: %v", err)
	}
	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal redacted: %v", err)
	}
	body := string(raw)

	for _, leaked := range []string{"literal-key", "redis-literal-pw", "alice-literal-key", "s3cr3t", "user:pw", "abc123"} {
		if strings.Contains(body, leaked) {
			t.Errorf("redacted config still contains %q: %s", leaked, body)
		}
	}
	for _, kept := range []string{"env:OPENAI_KEY", "file:/etc/secrets/key", "/etc/llmgw/users.json"} {
		if !strings.Contains(body, kept) {
			t.Errorf("redacted config dropped %q, want it kept verbatim: %s", kept, body)
		}
	}

	providers, ok := redacted["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers missing or wrong shape")
	}
	openai, ok := providers["openai"].(map[string]any)
	if !ok {
		t.Fatalf("providers.openai missing or wrong shape")
	}
	if openai["apiKey"] != redactedSecretPlaceholder {
		t.Errorf(`providers.openai.apiKey = %v, want %q`, openai["apiKey"], redactedSecretPlaceholder)
	}
	if empty, emptyOK := providers["empty"].(map[string]any); !emptyOK || empty["apiKey"] != "" {
		t.Errorf(`providers.empty.apiKey = %v, want "" (empty stays empty)`, providers["empty"])
	}
	local, ok := providers["local"].(map[string]any)
	if !ok || local["apiKey"] != "env:OPENAI_KEY" {
		t.Errorf(`providers.local.apiKey = %v, want "env:OPENAI_KEY" kept verbatim`, providers["local"])
	}
	baseURL, _ := openai["baseUrl"].(string)
	if strings.Contains(baseURL, "s3cr3t") || strings.Contains(baseURL, "token") {
		t.Errorf("providers.openai.baseUrl = %q, want userinfo stripped by sanitizeBaseURL", baseURL)
	}
}

// TestRedactSecretString_Table pins redactSecretString's three cases
// directly, independent of the full Config walk above.
func TestRedactSecretString_Table(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"env:FOO", "env:FOO"},
		{"file:/path/to/secret", "file:/path/to/secret"},
		{"plain-literal", redactedSecretPlaceholder},
		{"envelope-not-a-prefix-match", redactedSecretPlaceholder}, // "env" substring, not "env:" prefix
	}
	for _, c := range cases {
		if got := redactSecretString(c.in); got != c.want {
			t.Errorf("redactSecretString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeBaseURL_ParseFailure_NeverLeaksRawValue is P5 (admin
// dashboard redesign verify round): sanitizeBaseURL used to return raw
// verbatim whenever url.Parse failed, on the theory that a value failing
// to parse "names no scheme/host/userinfo net/url can identify" — true
// for genuine garbage, but url.Parse fails on plenty of real-looking URLs
// too. Two real vectors: a password containing an unescaped '/' (net/url
// reads the remainder as an invalid port) and a malformed '%' escape
// anywhere in the string. Both used to leak the embedded credential
// verbatim; both must now return the fixed placeholder instead.
func TestSanitizeBaseURL_ParseFailure_NeverLeaksRawValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"password containing unescaped slash (invalid port)", "https://user:ab/cdSECRET1@host.invalid/v1"},
		{"malformed percent escape", "https://user:50%zzSECRET2@host.invalid/v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := url.Parse(c.raw); err == nil {
				t.Fatalf("fixture %q parses without error — not a real repro of the parse-failure path", c.raw)
			}
			got := sanitizeBaseURL(c.raw)
			if got != unparseableBaseURLPlaceholder {
				t.Errorf("sanitizeBaseURL(%q) = %q, want the placeholder %q", c.raw, got, unparseableBaseURLPlaceholder)
			}
		})
	}
}

// TestAdminConfigAndOverview_UnparseableBaseURL_NeverLeaksCredentials
// drives P5's two vectors through the real HTTP surface: GET
// /admin/api/config (redactConfig -> sanitizeBaseURL) and GET
// /admin/api/overview (buildAdminOverview's own provider view) both used
// to echo the embedded credential verbatim whenever a provider's baseUrl
// failed to parse.
func TestAdminConfigAndOverview_UnparseableBaseURL_NeverLeaksCredentials(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Providers["alpha"].BaseURL = "https://user:ab/cdSECRET1@host.invalid/v1"
	cfg.Providers["zeta"].BaseURL = "https://user:50%zzSECRET2@host.invalid/v1"
	h, _ := newAdminGatewayHandle(t, cfg)

	for _, path := range []string{adminConfigPath, adminOverviewPath} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, path, "sk-admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200, body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, secret := range []string{"SECRET1", "SECRET2"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s response leaks %q (unparseable baseUrl echoed verbatim instead of sanitizeBaseURL's placeholder): %s", path, secret, body)
			}
		}
	}
}

// --- fuzz: no random literal secret ever survives redaction ---

// TestRedactConfig_FuzzNoLiteralSecretSurvives generates many random
// "secret" strings (varying length, charset, and shape — including some
// that happen to start with substrings resembling "env"/"file" without
// the ":" prefix redactSecretString actually checks for) and asserts each
// literal one is fully absent from the redacted, re-marshaled output —
// the property the task explicitly calls for ("Config redaction must be
// fuzz-tested so no literal secret ever appears").
func TestRedactConfig_FuzzNoLiteralSecretSurvives(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260923)) // #nosec G404 -- deterministic test-fixture generator, not security-sensitive
	const iterations = 500

	for i := 0; i < iterations; i++ {
		secret := randomSecretLikeString(rng)
		cfg := &Config{
			// BaseURL/URL userinfo redaction (sanitizeBaseURL) is covered
			// separately by TestRedactConfig_LiteralAndIndirectSecrets — this
			// fuzz loop targets apiKey/password specifically, the fields
			// redactSecretString's literal/env/file rule directly governs, so
			// a fuzzed secret containing URL-structural characters (":", "/")
			// can never produce a false failure from URL re-parsing instead of
			// from redaction itself.
			Providers: map[string]*ProviderConfig{"p": {Type: "openai", APIKey: secret}},
			Redis:     &RedisConfig{Address: "redis:6379", Password: secret},
			Users: &UsersConfig{Inline: []*UserConfig{
				{Name: "u", Group: "g", APIKey: secret},
			}},
		}

		redacted, err := redactConfig(cfg)
		if err != nil {
			t.Fatalf("iteration %d: redactConfig: %v", i, err)
		}

		// Checked field-by-field, not as a whole-body substring search:
		// a short random secret (e.g. "u") can coincidentally collide with
		// an unrelated STRUCTURAL literal already in the config (a user
		// name, a group name, a provider key) that legitimately survives
		// redaction — a substring search over the whole marshaled body
		// would then false-positive on that collision instead of on an
		// actual secret leak. Reading the exact redacted field back is
		// both more precise and a stronger assertion: the field's value
		// must equal secret's indirection form exactly, not merely "not
		// contain the raw secret somewhere in the JSON".
		providers, _ := redacted["providers"].(map[string]any)
		p, _ := providers["p"].(map[string]any)
		redis, _ := redacted["redis"].(map[string]any)
		users, _ := redacted["users"].(map[string]any)
		inline, _ := users["inline"].([]any)
		var userEntry map[string]any
		if len(inline) > 0 {
			userEntry, _ = inline[0].(map[string]any)
		}

		isIndirect := secret == "" || strings.HasPrefix(secret, "env:") || strings.HasPrefix(secret, "file:")
		wantVal := secret
		if !isIndirect {
			wantVal = redactedSecretPlaceholder
		}
		if got, _ := p["apiKey"].(string); got != wantVal {
			t.Fatalf("iteration %d: secret %q -> providers.p.apiKey = %q, want %q", i, secret, got, wantVal)
		}
		if got, _ := redis["password"].(string); got != wantVal {
			t.Fatalf("iteration %d: secret %q -> redis.password = %q, want %q", i, secret, got, wantVal)
		}
		if got, _ := userEntry["apiKey"].(string); got != wantVal {
			t.Fatalf("iteration %d: secret %q -> users.inline[0].apiKey = %q, want %q", i, secret, got, wantVal)
		}

		// Belt-and-braces: the raw secret bytes must never appear ANYWHERE
		// in the re-marshaled JSON either, unless it's short enough to
		// plausibly collide with an unrelated structural literal (name,
		// group, provider key) — only enforced for a long-enough secret,
		// where a coincidental collision is astronomically unlikely.
		if !isIndirect && len(secret) >= 8 {
			raw, err := json.Marshal(redacted)
			if err != nil {
				t.Fatalf("iteration %d: marshal: %v", i, err)
			}
			if strings.Contains(string(raw), secret) {
				t.Fatalf("iteration %d: literal secret %q survived redaction: %s", i, secret, raw)
			}
		}
	}
}

// randomSecretLikeString generates a pseudo-random string a fuzz
// iteration treats as a secret: length 0..63, drawn from a charset wide
// enough to include characters a real API key/password/URL-userinfo
// value might carry (alnum plus a handful of URL/shell-adjacent
// punctuation), occasionally producing an "env:"/"file:"-prefixed value
// on purpose so both code paths (kept verbatim vs. redacted) get
// exercised across the run.
func randomSecretLikeString(rng *rand.Rand) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:+="
	n := rng.Intn(64)
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rng.Intn(len(charset))]
	}
	s := string(b)
	switch rng.Intn(8) {
	case 0:
		return "env:" + s
	case 1:
		return "file:/" + s
	default:
		return s
	}
}
