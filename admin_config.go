package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// adminConfigPath is GET /admin/api/config's route (admin dashboard
// redesign, WP-B) — defined here, alongside the rest of this feature's
// own constants, mirroring adminEventsPath's own precedent (events.go).
const adminConfigPath = "/admin/api/config"

// redactedSecretPlaceholder replaces a LITERAL secret value redactConfig
// finds (below) — never an "env:"/"file:" indirection, which is kept
// verbatim (redactSecretString's own doc comment).
const redactedSecretPlaceholder = "[redacted]"

// adminConfigResponse is the full body of GET /admin/api/config: the
// live Config, redacted (redactConfig, below) so no secret — a literal
// API key, Redis password, or embedded URL credential — ever leaves the
// process, plus the SAME construction warnings GET /admin/api/overview
// already exposes (Warnings) and the SAME feature-availability flags
// (Features, admin.go's buildAdminFeatures) every other new admin
// endpoint echoes, so the Config tab's change-helper forms can gate
// their own UI on the identical flags without a second round trip.
type adminConfigResponse struct {
	Config   map[string]any    `json:"config"`
	Version  string            `json:"version"`
	Warnings []string          `json:"warnings"`
	Features adminFeaturesView `json:"features"`
}

// redactedConfigKeys names every JSON object key redactConfig treats as
// carrying a secret VALUE outright (as opposed to baseURLLikeConfigKeys,
// below, whose value is a URL that may EMBED one): "apiKey" (Provider/
// UserConfig) and "password" (RedisConfig). Matched by JSON key name
// alone, not by which Go struct it came from — redactConfig walks the
// generic map[string]any json.Marshal produces, with no notion of the
// original struct type, so a future secret-shaped field is covered
// automatically the moment it reuses one of these same JSON tag names.
var redactedConfigKeys = map[string]bool{
	"apiKey":   true,
	"password": true,
}

// baseURLLikeConfigKeys names every JSON object key redactConfig passes
// through sanitizeBaseURL (admin.go) instead of the literal/env/file
// redaction rule above: ProviderConfig.BaseURL ("baseUrl") and
// TargetConfig/AgentConfig.URL ("url") — a URL is not itself a secret,
// but an operator can embed one in its userinfo or query string
// (sanitizeBaseURL's own doc comment), exactly the same risk GET
// /admin/api/overview's adminProviderView.BaseURL already guards
// against.
var baseURLLikeConfigKeys = map[string]bool{
	"baseUrl": true,
	"url":     true,
}

// redactConfig returns a redacted map[string]any view of cfg, safe to
// serve over GET /admin/api/config (plan §6's security risk: "new
// endpoints expose user/model names... config redaction fuzz-tested").
// cfg is round-tripped through json.Marshal then json.Unmarshal into a
// generic map, then walked recursively (redactValue, below), so the walk
// needs no knowledge of Config's own nested struct types — a field added
// to any struct in llmgateway.go is redacted automatically the moment it
// round-trips through this walk, purely by its JSON key name matching
// redactedConfigKeys or baseURLLikeConfigKeys.
//
// The marshal/unmarshal round trip can fail only for a Config value that
// is not itself valid JSON in the first place — never reachable for a
// *Gateway's own g.cfg, which newGateway already round-tripped through
// Traefik's own YAML/JSON decoder to construct — so an error here is
// reported, never silently swallowed, but is not expected to fire on any
// real, already-running Gateway (buildAdminConfig, below, degrades
// gracefully rather than 500ing if it ever does).
func redactConfig(cfg *Config) (map[string]any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	redactValue(m)
	return m, nil
}

// redactValue walks v in place. json.Unmarshal decodes a JSON object
// into map[string]any and a JSON array into []any — the two cases this
// switch handles — so this walk reaches every nesting depth Config's own
// struct tree can produce (Config.Groups, Config.Users.Inline,
// Config.Providers, Config.MCPServers, Config.Agents, Config.ModelMeta,
// Config.Pricing all nest further object/array values) without needing
// its own copy of that struct tree's shape.
func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			s, isString := val.(string)
			switch {
			case redactedConfigKeys[k] && isString:
				t[k] = redactSecretString(s)
			case baseURLLikeConfigKeys[k] && isString:
				t[k] = sanitizeBaseURL(s)
			default:
				redactValue(val)
			}
		}
	case []any:
		for _, item := range t {
			redactValue(item)
		}
	}
}

// redactSecretString applies plan §1.3(ix)'s redaction rule to one
// apiKey/password value: empty stays empty (nothing configured, nothing
// to hide), an "env:NAME" or "file:/path" reference is kept verbatim
// (resolveSecret's own doc comment, secrets.go — neither form is the
// secret itself, only where to find it, and an operator reading their
// own config back needs to see which indirection a field actually uses),
// and any other value — a literal secret typed directly into config — is
// replaced with redactedSecretPlaceholder. Mirrors resolveSecret's own
// "env:"/"file:" prefix check exactly, so a value this function keeps is
// always one resolveSecret would also resolve through an indirection,
// never a literal — fuzz-tested (admin_config_test.go) so no generated
// literal secret ever survives this function.
func redactSecretString(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasPrefix(s, "env:"), strings.HasPrefix(s, "file:"):
		return s
	default:
		return redactedSecretPlaceholder
	}
}

// buildAdminConfig assembles adminConfigResponse: g.cfg redacted
// (redactConfig, above), the same construction warnings
// configWarningsSnapshot already exposes on GET /admin/api/overview
// (admin.go), and this Gateway's feature-availability flags
// (buildAdminFeatures, admin.go). A redaction failure (redactConfig's
// own doc comment — not expected on any real running Gateway) degrades
// to an empty config map with a warning appended, rather than a 500:
// every other field in the response is still meaningful even if the
// config tree itself could not be built.
func (g *Gateway) buildAdminConfig() adminConfigResponse {
	warnings, _ := g.configWarningsSnapshot()
	redacted, err := redactConfig(g.cfg)
	if err != nil {
		redacted = map[string]any{}
		warnings = append(warnings, "config redaction failed: "+err.Error())
	}
	return adminConfigResponse{
		Config:   redacted,
		Warnings: warnings,
		Features: g.buildAdminFeatures(),
		Version:  pluginVersion,
	}
}

// serveAdminConfig writes buildAdminConfig's result as JSON.
func (g *Gateway) serveAdminConfig(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminConfig())
}
