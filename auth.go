package traefikllmgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
)

// user is an authenticated API-key holder resolved by authStore.identify.
type user struct {
	limits          *LimitsConfig
	name, groupName string
}

// group describes a group's access rules and default limits. Empty
// providers, models, mcpServers, or agents means all are allowed.
type group struct {
	limits                                *LimitsConfig
	name                                  string
	providers, models, mcpServers, agents []string
}

// allowsProvider reports whether name matches one of the group's provider
// glob patterns. An empty pattern list allows every provider.
func (grp *group) allowsProvider(name string) bool {
	return matchesGlob(grp.providers, name)
}

// allowsModel reports whether id matches one of the group's model glob
// patterns. An empty pattern list allows every model. A pattern is checked
// against id as given and, when id has a "provider/model" form, against the
// part after the "/" — so a pattern like "gpt-5*" matches both "gpt-5-mini"
// and "openai/gpt-5-mini".
func (grp *group) allowsModel(id string) bool {
	if matchesGlob(grp.models, id) {
		return true
	}
	if _, bare, found := strings.Cut(id, "/"); found {
		return matchesGlob(grp.models, bare)
	}
	return false
}

// allowsMCP reports whether name matches one of the group's MCP-server glob
// patterns. An empty pattern list allows every server.
func (grp *group) allowsMCP(name string) bool {
	return matchesGlob(grp.mcpServers, name)
}

// allowsAgent reports whether name matches one of the group's agent glob
// patterns. An empty pattern list allows every agent.
func (grp *group) allowsAgent(name string) bool {
	return matchesGlob(grp.agents, name)
}

// matchesGlob reports whether candidate matches any shell-style pattern in
// patterns (path.Match semantics). An empty pattern list matches anything.
// A malformed pattern (path.ErrBadPattern) is treated as a non-match rather
// than a panic or a silent allow.
func matchesGlob(patterns []string, candidate string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, err := path.Match(p, candidate); ok && err == nil {
			return true
		}
	}
	return false
}

// authEntry is the internal record stored per API-key digest.
type authEntry struct {
	user   *user
	group  *group
	digest [32]byte
}

// authStore identifies requests by API key and resolves them to a user and
// their group. Raw API keys are never retained after construction — only
// their SHA-256 digests.
type authStore struct {
	groups   map[string]*group
	inline   map[[32]byte]*authEntry // built once by newAuthStore; never mutated afterward
	byDigest map[[32]byte]*authEntry // inline entries plus the current file-sourced set; guarded by mu
	mu       sync.RWMutex
}

// newAuthStore builds an authStore from cfg's groups and inline users. It
// errors if a user references an unknown group, if a user's resolved API
// key is empty, or if two users resolve to the same API key.
func newAuthStore(cfg *Config) (*authStore, error) {
	a := &authStore{
		groups:   make(map[string]*group, len(cfg.Groups)),
		inline:   make(map[[32]byte]*authEntry),
		byDigest: make(map[[32]byte]*authEntry),
	}
	for name, gc := range cfg.Groups {
		a.groups[name] = &group{
			limits:     gc.Limits,
			name:       name,
			providers:  gc.Providers,
			models:     gc.Models,
			mcpServers: gc.MCPServers,
			agents:     gc.Agents,
		}
	}

	if cfg.Users != nil {
		for _, uc := range cfg.Users.Inline {
			entry, err := a.buildEntry(uc)
			if err != nil {
				return nil, err
			}
			if _, exists := a.inline[entry.digest]; exists {
				return nil, fmt.Errorf("llmgateway: duplicate API key for user %q", uc.Name)
			}
			a.inline[entry.digest] = entry
			a.byDigest[entry.digest] = entry
		}
	}
	return a, nil
}

// buildEntry resolves uc's group and API key into an authEntry. The API key
// goes through resolveSecret before digesting, so env:/file: references
// work the same as for provider keys.
func (a *authStore) buildEntry(uc *UserConfig) (*authEntry, error) {
	if uc == nil {
		return nil, fmt.Errorf("llmgateway: user config entry must not be nil")
	}
	grp, ok := a.groups[uc.Group]
	if !ok {
		return nil, fmt.Errorf("llmgateway: user %q references unknown group %q", uc.Name, uc.Group)
	}
	key, err := resolveSecret(uc.APIKey)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: user %q: %w", uc.Name, err)
	}
	if key == "" {
		return nil, fmt.Errorf("llmgateway: user %q has an empty API key", uc.Name)
	}
	digest := sha256.Sum256([]byte(key))
	return &authEntry{
		digest: digest,
		user:   &user{limits: uc.Limits, name: uc.Name, groupName: uc.Group},
		group:  grp,
	}, nil
}

// replaceFileUsers rebuilds the file-sourced portion of the store from us,
// leaving the inline users untouched. It is used by the config-reload path
// (Task 4) to pick up changes to Users.File without restarting the plugin.
// On error the store is left exactly as it was before the call — the new
// set is validated and built in full before it replaces the old one.
//
// mu is held for the whole rebuild, not just the final swap. That serializes
// concurrent callers (a reload timer must never race itself), so one call's
// result can never be partially overwritten or interleaved with another's.
// identify's reads stay fast (RLock) and reloads are rare, so the extra hold
// time is a good trade.
func (a *authStore) replaceFileUsers(us []*UserConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	next := make(map[[32]byte]*authEntry, len(a.inline)+len(us))
	for digest, entry := range a.inline {
		next[digest] = entry
	}
	for _, uc := range us {
		entry, err := a.buildEntry(uc)
		if err != nil {
			return err
		}
		if _, exists := next[entry.digest]; exists {
			return fmt.Errorf("llmgateway: duplicate API key for user %q", uc.Name)
		}
		next[entry.digest] = entry
	}

	a.byDigest = next
	return nil
}

// identify resolves r's presented API key to a user and their group. It
// reads "Authorization: Bearer <key>" (case-insensitive "bearer" prefix) or
// "x-api-key: <key>"; Bearer wins when both are present. The presented key
// is looked up by SHA-256 digest, then verified with a constant-time
// compare — the key itself is never retained.
func (a *authStore) identify(r *http.Request) (*user, *group, bool) {
	key, ok := presentedKey(r)
	if !ok {
		return nil, nil, false
	}
	digest := sha256.Sum256([]byte(key))

	a.mu.RLock()
	entry, ok := a.byDigest[digest]
	a.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}
	if subtle.ConstantTimeCompare(digest[:], entry.digest[:]) != 1 {
		return nil, nil, false
	}
	return entry.user, entry.group, true
}

// presentedKey extracts the API key from r's Authorization Bearer header or
// its x-api-key header. Bearer takes priority when both are set.
func presentedKey(r *http.Request) (string, bool) {
	if authz := r.Header.Get("Authorization"); authz != "" {
		if scheme, rest, found := strings.Cut(authz, " "); found && strings.EqualFold(scheme, "bearer") {
			if key := strings.TrimSpace(rest); key != "" {
				return key, true
			}
		}
	}
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return key, true
	}
	return "", false
}
