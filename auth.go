package traefikllmgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
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
// patterns, exactly as given. An empty pattern list allows every model.
//
// allowsModel does no "provider/model" prefix-splitting of its own — a
// pattern like "deepseek-*" must not spuriously match a bare model id that
// merely happens to contain a slash (e.g. an upstream's own
// "uni/deepseek-v4-flash-0731" naming, where "uni" is not a configured
// provider: there is no legitimate prefix to strip there at all). A caller
// that genuinely knows an id's leading segment names a configured provider
// (modelRegistry.resolveAgainst, modelRegistry.listFor) generates both the
// full and bare-suffix candidate strings itself and calls allowsModel once
// per candidate.
func (grp *group) allowsModel(id string) bool {
	return matchesGlob(grp.models, id)
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
//
// mu guards byDigest and fileUserCount for readers (identify) and the final
// swap in replaceFileUsers. buildMu serializes the whole body of
// replaceFileUsers — including buildEntry's potentially slow file-backed
// secret resolution — so concurrent reload attempts never interleave or
// overwrite each other. buildMu is never held at the same time as mu, so a
// rebuild in progress never blocks identify's readers. reloadMu (Task 4)
// serializes maybeReload's throttle check and the reload it may trigger.
type authStore struct {
	groups        map[string]*group
	inline        map[[32]byte]*authEntry // built once by newAuthStore; never mutated afterward
	byDigest      map[[32]byte]*authEntry // inline entries plus the current file-sourced set; guarded by mu
	usersFile     *usersFile              // nil when Users.File is not configured; maybeReload no-ops
	log           gatewayLogger           // set alongside usersFile; unused when usersFile is nil
	nowFn         func() time.Time        // injected for tests; defaults to time.Now
	lastCheck     time.Time               // guarded by reloadMu
	lastModTime   time.Time               // guarded by reloadMu
	fileUserCount int                     // guarded by mu; size of the current file-sourced user set
	mu            sync.RWMutex
	buildMu       sync.Mutex
	reloadMu      sync.Mutex
}

// newAuthStore builds an authStore from cfg's groups and inline users. It
// errors if a user references an unknown group, if a user's resolved API
// key is empty, or if two users resolve to the same API key.
func newAuthStore(cfg *Config) (*authStore, error) {
	a := &authStore{
		groups:   make(map[string]*group, len(cfg.Groups)),
		inline:   make(map[[32]byte]*authEntry),
		byDigest: make(map[[32]byte]*authEntry),
		nowFn:    time.Now,
	}
	for name, gc := range cfg.Groups {
		if gc == nil {
			return nil, fmt.Errorf("llmgateway: group %q: config must not be nil", name)
		}
		if err := gc.Limits.validate(); err != nil {
			return nil, fmt.Errorf("llmgateway: group %q: %w", name, err)
		}
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
	if err := uc.Limits.validate(); err != nil {
		return nil, fmt.Errorf("llmgateway: user %q: %w", uc.Name, err)
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

// replaceFileUsers rebuilds the file-sourced portion of the store from us. A
// file user overrides an inline user of the same name — the inline entry is
// dropped from the rebuilt set, even if its API key differs from the file
// user's — so an operator can promote or rotate a statically-configured user
// through the hot-reloadable file. Inline users with no same-named file
// counterpart are carried forward unchanged. This is used by the
// config-reload path (Task 4) to pick up changes to Users.File without
// restarting the plugin. On error the store is left exactly as it was before
// the call — the new set is validated and built in full before it replaces
// the old one.
//
// buildMu is held for the whole rebuild, not just the final swap — that
// serializes concurrent callers (a reload timer must never race itself), so
// one call's result can never be partially overwritten or interleaved with
// another's. mu, which identify's reads use (RLock), is locked only for the
// final map swap below: a slow rebuild (buildEntry's resolveSecret can do
// blocking file I/O for "file:"-prefixed API keys) no longer stalls every
// identify call for its whole duration.
func (a *authStore) replaceFileUsers(us []*UserConfig) error {
	a.buildMu.Lock()
	defer a.buildMu.Unlock()

	fileNames := make(map[string]bool, len(us))
	for _, uc := range us {
		if uc != nil {
			fileNames[uc.Name] = true
		}
	}

	next := make(map[[32]byte]*authEntry, len(a.inline)+len(us))
	for digest, entry := range a.inline {
		if fileNames[entry.user.name] {
			continue // a file user of the same name overrides this inline user
		}
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

	a.mu.Lock()
	a.byDigest = next
	a.fileUserCount = len(us)
	a.mu.Unlock()
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
