package traefikllmgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// user is an authenticated API-key holder resolved by authStore.identify.
type user struct {
	limits          *LimitsConfig
	name, groupName string
	// admin grants access to the read-only admin dashboard (spec §4,
	// v0.2) — an admin user is otherwise ordinary: their own limits and
	// group authorization still apply, including to the admin routes.
	admin bool
}

// group describes a group's access rules and default limits. Empty
// providers, models, mcpServers, or agents means all are allowed.
type group struct {
	limits *LimitsConfig
	// cache is GroupConfig.Cache carried through unchanged: nil inherits
	// the global cache.enabled setting, non-nil overrides it for this
	// group's requests. Resolved by groupCacheEnabled (cache.go).
	cache      *bool
	name       string
	providers  []string
	models     []string
	mcpServers []string
	agents     []string
	// cacheTTL is GroupConfig.CacheTTL parsed and validated at construction
	// (newAuthStore below): 0 inherits the global responseCache's TTL
	// (effectiveTTL, cache.go), a positive value overrides it for this
	// group's cache entries only. GroupConfig.CacheTTL == "" is the only
	// input that produces 0 here — every other value either becomes a
	// positive duration or fails newAuthStore as a constructor error.
	cacheTTL time.Duration
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
		// A group cannot opt into caching (Cache: true) when the global
		// cache block itself is not configured (cfg.Cache.Enabled false)
		// — there is no ttl/maxBodyBytes to inherit, and nothing for
		// newGateway to wire g.cache from either way (spec §2). Cache:
		// false or nil never hits this check: a group can always opt out
		// of, or inherit, caching regardless of the global setting.
		if gc.Cache != nil && *gc.Cache && !cfg.Cache.Enabled {
			return nil, fmt.Errorf("llmgateway: group %q enables cache but global cache is not configured", name)
		}
		// CacheTTL follows the identical "nothing to inherit from" reasoning
		// as Cache:true above: overriding a TTL only means something when
		// there is a global cache.ttl to override, so CacheTTL set while
		// the global cache block itself is not configured is a constructor
		// error, independent of this group's own Cache setting (a group
		// may set CacheTTL for when caching later becomes enabled globally
		// is NOT supported — cfg.Cache.Enabled is checked here, not
		// per-group, matching Cache:true's own check above).
		cacheTTL, err := groupCacheTTL(name, gc.CacheTTL, cfg.Cache.Enabled)
		if err != nil {
			return nil, err
		}
		a.groups[name] = &group{
			limits:     gc.Limits,
			cache:      gc.Cache,
			cacheTTL:   cacheTTL,
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
		user:   &user{limits: uc.Limits, name: uc.Name, groupName: uc.Group, admin: uc.Admin},
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

// userSummary and groupSummary are the read-only, redaction-safe listing
// authStore.snapshot returns for the admin dashboard (spec §4, v0.2):
// names, group membership, and limits only — never an API key or its
// digest, matching the package-wide "raw keys are never retained past
// construction" rule this file's own doc comment already states for
// byDigest itself.
type userSummary struct {
	limits    *LimitsConfig
	name      string
	groupName string
}

// groupSummary mirrors userSummary for one configured group, plus its
// current member count (how many active users, inline or file-sourced,
// currently reference it).
type groupSummary struct {
	limits      *LimitsConfig
	name        string
	memberCount int
}

// snapshot returns a sorted, redaction-safe listing of every currently
// active user and every configured group. a.mu is read-locked only for
// the byDigest walk, mirroring identify's own RLock usage; a.groups is
// built once by newAuthStore and never mutated afterward (see this
// file's authStore doc comment), so reading it needs no lock.
func (a *authStore) snapshot() ([]userSummary, []groupSummary) {
	a.mu.RLock()
	users := make([]userSummary, 0, len(a.byDigest))
	memberCounts := make(map[string]int, len(a.groups))
	for _, entry := range a.byDigest {
		users = append(users, userSummary{limits: entry.user.limits, name: entry.user.name, groupName: entry.user.groupName})
		memberCounts[entry.user.groupName]++
	}
	a.mu.RUnlock()
	sort.Slice(users, func(i, j int) bool { return users[i].name < users[j].name })

	groups := make([]groupSummary, 0, len(a.groups))
	for name, grp := range a.groups {
		groups = append(groups, groupSummary{limits: grp.limits, name: name, memberCount: memberCounts[name]})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })

	return users, groups
}
