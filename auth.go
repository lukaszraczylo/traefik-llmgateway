package traefikllmgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
	// passthroughPaths is GroupConfig.PassthroughPaths carried through
	// unchanged (security+performance audit, 2026-08-22) — see
	// allowsPassthroughPath below.
	passthroughPaths []string
	// cacheTTL is GroupConfig.CacheTTL parsed and validated at construction
	// (newAuthStore below): 0 inherits the global responseCache's TTL
	// (effectiveTTL, cache.go); a positive value sets the TTL written
	// when THIS group's own request populates a cache entry. It is not a
	// per-group scope — cache entries are shared across every group that
	// can reach the model (groupCacheEnabled, cache.go), so a positive
	// cacheTTL only ever controls a write's TTL, never which group can
	// later read the entry. GroupConfig.CacheTTL == "" is the only input
	// that produces 0 here — every other value either becomes a positive
	// duration or fails newAuthStore as a constructor error.
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

// hasModelRestriction reports whether grp's Models glob is non-empty —
// whether there is anything for allowsModel to actually reject (security
// review fix, 2026-08-22, round 2). handlePassthrough (routes_
// passthrough.go) checks this BEFORE peeking a passthrough request's body
// at all: a group with an empty Models list (matchesGlob's own
// empty-means-all contract — the live cluster's "home" group and every
// other group that has not opted into model restrictions) has nothing
// allowsModel could ever deny, so there is no reason to read, buffer, or
// even look at the request body for model enforcement — zero risk, and
// exactly today's behavior for the overwhelming majority of traffic.
func (grp *group) hasModelRestriction() bool {
	return len(grp.models) > 0
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

// allowsPassthroughPath reports whether rest — the path segment after the
// provider name in a native passthrough request (passthroughRoute's own
// "rest", routes_passthrough.go) — matches one of the group's
// PassthroughPaths glob patterns (security review, 2026-08-22). An empty
// pattern list allows every path, the same fail-open default every other
// allowsX method on group already applies (matchesGlob's own
// empty-means-all contract) — do not invert it. See GroupConfig.
// PassthroughPaths' own doc comment (llmgateway.go) for a path.Match
// footgun a non-empty list inherits: "*" does not cross "/", so ["*"]
// is NOT "allow everything".
//
// %2f BYPASS (security review finding 3, round 3, 2026-08-22): rest
// arrives here still in its ESCAPED form (passthroughRoute's own
// contract, routes_passthrough.go — callers must pass r.URL.
// EscapedPath()), the same form path.Match evaluates literally, with no
// notion of percent-decoding. hasTraversalSegment (routes_passthrough.go)
// decodes rest before reasoning about it; this function used to match
// the RAW escaped form instead — an asymmetry a client can exploit:
// path.Match("v1/*", "v1/chat%2fcompletions") reports true (one escaped
// segment, no literal "/" for the glob to see splitting it), while the
// DECODED equivalent, "v1/chat/completions", is two segments and does
// NOT match "v1/*". A group restricted to ["v1/*"] therefore let
// "/openai/v1/fine_tuning%2Fjobs" through — a path an upstream that
// itself decodes %2F would read as "v1/fine_tuning/jobs", a different,
// unauthorized endpoint. Fixed by decoding rest with the SAME
// url.PathUnescape call hasTraversalSegment already uses before matching,
// and rejecting (denying) a rest that fails to unescape at all — the
// same fail-closed rule hasTraversalSegment already applies. The
// zero-pattern (empty-means-all) fast path runs BEFORE decoding: a group
// that has not opted into PassthroughPaths at all keeps allowing every
// request, malformed encoding included, exactly as before this fix —
// hasTraversalSegment (called unconditionally, later, in
// handlePassthrough) is still the gate that denies a malformed rest for
// that case, unchanged.
func (grp *group) allowsPassthroughPath(rest string) bool {
	if len(grp.passthroughPaths) == 0 {
		return true
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return false
	}
	return matchesGlob(grp.passthroughPaths, decoded)
}

// cloneStringSlice returns an independent copy of s, preserving nil (a nil
// s returns nil, never an empty non-nil slice — groupSummary's own
// providers/models/mcpServers/agents fields rely on that nil-ness to stay
// "omitempty" all the way to the JSON response, admin.go's
// adminUsageEntryView). Plain builtins only (make/copy, no generics, no
// non-stdlib package): the plugin runs interpreted under Yaegi (see
// tools/yaegi-check), whose stdlib symbol table does not resolve the
// generic "slices" package's type parameters — this is why a
// slices.Clone call is not used here.
func cloneStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
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

// authFailureWindow and authFailureLimit bound how many failed identify
// attempts one source IP may make before further attempts from it are
// throttled outright (security audit finding 3, 2026-08-22): identify
// previously had no counter, no backoff, and no lockout at all — an
// unauthenticated caller got unlimited digest-lookup attempts at line
// rate. 20 failures per minute is generous headroom for a legitimate
// client with a stale or mistyped key retried a few times (a human
// pasting the wrong key, a misconfigured script cycling through a couple
// of candidates) while still meaningfully bounding a scripted guesser's
// throughput per source IP. This does not, and cannot, defend the
// underlying key space on its own (a SHA-256 digest lookup already makes
// brute force infeasible) — its real job is capping the synchronous cost
// an unauthenticated caller can impose on the shared Traefik ingress per
// unit time: identify's own digest lookup, and logAuthEvent's stderr
// write (logger.go), both of which this finding's own framing calls out.
const (
	authFailureWindow = time.Minute
	authFailureLimit  = 20
	// authFailureLogEvery rate-limits logAuthEvent's own failed-auth log
	// line (logger.go) the identical way limiter.logStoreError (limits.go)
	// already rate-limits store-error lines — sharing that function's own
	// storeErrorLogEvery interval keeps this codebase's "how often do we
	// log a sustained failure condition" cadence consistent across both
	// call sites, rather than picking a new, unrelated number here.
	authFailureLogEvery = storeErrorLogEvery
)

// authFailureKeyPrefix namespaces authStore's failure-tracking keys in its
// own memoryStore instance (failures, below) — a plain string key, not
// windowKey's (kind, id, metric, window) tuple (limits.go), since auth
// failures are tracked per SOURCE IP, a concept the rest of this
// package's counter keying was never built to express.
const authFailureKeyPrefix = "authfail:"

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
//
// failures is identify's own per-source-IP failed-attempt counter
// (security audit finding 3, 2026-08-22) — the SAME in-process memoryStore
// type limiter's fallback store already uses (limits.go), a deliberately
// reused type rather than a new dependency, given its own TTL-bucketed
// counter semantics already fit this need exactly. It is authStore's own
// dedicated instance, not limiter.fallback: auth failures are a distinct
// concept (throttling an unauthenticated caller) from LLM traffic limits
// (throttling an authenticated one's usage), and authStore must work
// identically whether or not a Gateway even has a limiter wired yet.
// authFailLogMu/authFailLastLogAt back shouldLogAuthFailure, the identical
// "guard a timestamp with a small mutex, rate-limit a log line" shape
// limiter.logMu/lastLogAt already use.
type authStore struct {
	groups            map[string]*group
	inline            map[[32]byte]*authEntry // built once by newAuthStore; never mutated afterward
	byDigest          map[[32]byte]*authEntry // inline entries plus the current file-sourced set; guarded by mu
	usersFile         *usersFile              // nil when Users.File is not configured; maybeReload no-ops
	log               gatewayLogger           // set alongside usersFile; unused when usersFile is nil
	nowFn             func() time.Time        // injected for tests; defaults to time.Now
	failures          *memoryStore            // per-source-IP failed-identify counters; see doc comment above
	lastCheck         time.Time               // guarded by reloadMu
	lastModTime       time.Time               // guarded by reloadMu
	authFailLastLogAt time.Time               // guarded by authFailLogMu; last time a failed-auth line was logged
	fileUserCount     int                     // guarded by mu; size of the current file-sourced user set
	mu                sync.RWMutex
	buildMu           sync.Mutex
	reloadMu          sync.Mutex
	authFailLogMu     sync.Mutex
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
	a.failures = newMemoryStore()
	// Delegate through a.nowFn rather than copying it: a test that
	// overrides a.nowFn AFTER construction (the fakeClock convention
	// newReloadableAuthStore already uses, users_file_test.go) must still
	// control failures' own clock — this closure reads a.nowFn fresh on
	// every call, so it always sees whichever func is currently assigned.
	a.failures.nowFn = func() time.Time { return a.nowFn() }
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
			limits:           gc.Limits,
			cache:            gc.Cache,
			cacheTTL:         cacheTTL,
			name:             name,
			providers:        gc.Providers,
			models:           gc.Models,
			mcpServers:       gc.MCPServers,
			agents:           gc.Agents,
			passthroughPaths: gc.PassthroughPaths,
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

// clientIP returns the source address identify's own throttle keys on:
// r.RemoteAddr with any ":port" suffix stripped — net/http's own "address
// as seen by this server's TCP accept", never a client-supplied header
// (security audit finding 3, 2026-08-22). This plugin runs AS a Traefik
// middleware inside the shared ingress process, not behind some second,
// untrusted proxy of its own, so RemoteAddr is the same trust boundary
// logAuthEvent's own RemoteAddr-only logging (logger.go) already assumes,
// and the same one routes_passthrough.go's dangerousClientHeaderPrefixes
// strip (X-Forwarded-*, X-Auth-*) exists to keep a client from spoofing on
// the OUTBOUND side — deliberately NOT read here either, for the same
// reason. A RemoteAddr with no ":port" (net.SplitHostPort's own error
// case — not expected from net/http in practice, but not assumed) is used
// as-is rather than discarded, so a throttle key still exists for
// whatever value RemoteAddr carries.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authThrottled reports whether ip has already reached authFailureLimit
// failed identify attempts within the current authFailureWindow — a
// read-only check, so a throttled IP's later attempts cost nothing beyond
// one in-process map read, never a digest computation or byDigest lookup.
func (a *authStore) authThrottled(ip string) bool {
	count, _ := a.failures.get(authFailureKeyPrefix + ip) // memoryStore.get never errors
	return count >= authFailureLimit
}

// recordAuthFailure increments ip's failure count for the current
// authFailureWindow, creating a fresh window if the previous one already
// expired (memoryStore.incrBy's own fixed-window contract, limits.go).
func (a *authStore) recordAuthFailure(ip string) {
	_, _ = a.failures.incrBy(authFailureKeyPrefix+ip, 1, authFailureWindow) // memoryStore.incrBy never errors
}

// shouldLogAuthFailure reports whether the next failed-auth log line
// (logAuthEvent, logger.go) should actually be written, rate-limited to
// once per authFailureLogEvery regardless of how many attempts fail in
// between — the identical technique limiter.logStoreError (limits.go)
// already applies to store errors, applied here so an attacker driving
// unlimited failed-auth attempts can never drive unlimited synchronous
// stderr writes on the shared Traefik ingress (security audit finding 3,
// 2026-08-22) even before authThrottled's own per-IP cap engages (a
// distributed attempt spread across many source IPs never trips any
// single IP's own threshold, but must still not flood the log).
// Successful-auth logging is deliberately left unbounded: it requires an
// already-valid API key, so its volume is bounded by legitimate
// authenticated traffic — itself already governed by checkAndCount's
// requests-per-minute/day limits elsewhere — not by an unauthenticated
// caller's attempt rate, which is exactly what this finding is about
// bounding.
func (a *authStore) shouldLogAuthFailure() bool {
	now := a.nowFn()
	a.authFailLogMu.Lock()
	defer a.authFailLogMu.Unlock()
	if now.Sub(a.authFailLastLogAt) < authFailureLogEvery {
		return false
	}
	a.authFailLastLogAt = now
	return true
}

// identify resolves r's presented API key to a user and their group. It
// reads "Authorization: Bearer <key>" (case-insensitive "bearer" prefix) or
// "x-api-key: <key>"; Bearer wins when both are present. The presented key
// is looked up by SHA-256 digest, then verified with a constant-time
// compare — the key itself is never retained.
//
// Per-source-IP throttling (security audit finding 3, 2026-08-22): a
// request from an IP already over authThrottled's own budget is rejected
// immediately, before any digest work at all. Every OTHER failure path
// below — no key presented, key not found, or a (practically unreachable,
// see subtle.ConstantTimeCompare's own call site) digest mismatch —
// records one failure for the caller's IP via recordAuthFailure. A
// successful identify never records a failure and never resets a prior
// count: this is a rate limit on failed attempts, not a lockout a
// legitimate request can clear early.
func (a *authStore) identify(r *http.Request) (*user, *group, bool) {
	ip := clientIP(r)
	if a.authThrottled(ip) {
		return nil, nil, false
	}

	key, ok := presentedKey(r)
	if !ok {
		a.recordAuthFailure(ip)
		return nil, nil, false
	}
	digest := sha256.Sum256([]byte(key))

	a.mu.RLock()
	entry, ok := a.byDigest[digest]
	a.mu.RUnlock()
	if !ok {
		a.recordAuthFailure(ip)
		return nil, nil, false
	}
	if subtle.ConstantTimeCompare(digest[:], entry.digest[:]) != 1 {
		a.recordAuthFailure(ip)
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
// currently reference it) and its configured access lists — providers,
// models, mcpServers, agents — carried through exactly as group.allowsX
// itself reads them (group-access-display task): nil/empty means every
// provider/model/MCP server/agent is allowed (matchesGlob's own contract,
// auth.go), never expanded to the full catalog here — the admin dashboard
// echoes the configured glob list, not a resolved membership set.
//
// snapshot (below) clones all four slices rather than aliasing group's own
// — group.providers/models/mcpServers/agents are the live authorization
// data every allowsX call reads on the request path; groupSummary is a
// read-only, dashboard-facing COPY, so a future caller that sorts or
// otherwise mutates a groupSummary's list in place can never reorder or
// corrupt the slice authorization itself still relies on.
type groupSummary struct {
	limits      *LimitsConfig
	name        string
	providers   []string
	models      []string
	mcpServers  []string
	agents      []string
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
		groups = append(groups, groupSummary{
			limits:      grp.limits,
			name:        name,
			providers:   cloneStringSlice(grp.providers),
			models:      cloneStringSlice(grp.models),
			mcpServers:  cloneStringSlice(grp.mcpServers),
			agents:      cloneStringSlice(grp.agents),
			memberCount: memberCounts[name],
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })

	return users, groups
}
