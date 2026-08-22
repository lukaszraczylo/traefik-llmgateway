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
// attempts one source IP may make within a window before this package
// treats it as "throttled" for OBSERVABILITY purposes only (security
// audit finding 3, 2026-08-22; revised by security review round 2,
// 2026-08-22, critical finding 1 — see identify's own doc comment for
// why this is observability, not enforcement, post-revision). 20
// failures per minute is generous headroom for a legitimate client with
// a stale or mistyped key retried a few times, while still flagging a
// source failing fast enough to be worth an operator's attention.
const (
	authFailureWindow = time.Minute
	authFailureLimit  = 20
	// authFailureLogEvery rate-limits BOTH logAuthEvent's routine
	// failed-auth line and its distinct throttle-engaged line (logger.go)
	// — the identical technique limiter.logStoreError (limits.go) already
	// applies to store-error lines, sharing that function's own
	// storeErrorLogEvery interval for cadence consistency.
	authFailureLogEvery = storeErrorLogEvery
)

// authFailureMapCap bounds authFailureTracker's total tracked-source-IP
// count across both its generations (security review round 2,
// 2026-08-22, important finding 1): a distributed attacker sending
// failed attempts from many distinct IPs could otherwise grow the
// tracker's memory without any limit, and a naive full-map sweep to
// expire old entries would hold its lock for a duration that scales with
// tracked-IP count (measured, on the general-purpose memoryStore this
// field used before this revision: ~83ms mutex-held at 1M tracked IPs,
// blocking every identify call for that duration). authFailureTracker's
// own two-generation rotation (its doc comment below) bounds total memory
// to at most 2×authFailureMapCap entries with an O(1) rotation, never a
// scan.
const authFailureMapCap = 50_000

// authFailureEntry is one source IP's failure count within
// authFailureTracker, expiring authFailureWindow after its first failure
// in the current generation.
type authFailureEntry struct {
	expiry time.Time
	count  int64
}

// authFailureTracker is a bounded, two-generation per-source-IP failed-
// auth-attempt counter (security review round 2, 2026-08-22, important
// finding 1) — deliberately NOT the general-purpose memoryStore
// (limits.go): memoryStore's map has no size cap at all and its
// sweepLocked does a full, mutex-held O(n) scan, which is fine for the
// bounded-by-configuration scope space checkAndCount/account use (one
// entry per configured user/group) but wrong for a value with no
// configured upper bound like source IP.
//
// current is the active generation every increment/read goes to first;
// previous is the immediately-prior generation, consulted as a read-only
// fallback so an IP's count does not spuriously read as zero the instant
// after a rotation. Rotation — current becomes previous, a fresh empty
// map becomes current — is a two-pointer swap (O(1), never a scan),
// triggered on inserting a brand-new IP once current is already at
// authFailureMapCap; total memory is bounded to at most
// 2×authFailureMapCap entries. An IP whose entry is displaced by a
// rotation simply starts a fresh count on its next failure — the same
// outcome a normal authFailureWindow rollover already produces for any
// tracked IP; this is accepted, lossy-under-extreme-load behavior, the
// same trade-off limiter.spawnTokens' own doc comment (limits.go)
// accepts for a different bounded resource.
type authFailureTracker struct {
	current  map[string]*authFailureEntry
	previous map[string]*authFailureEntry
	nowFn    func() time.Time
	mu       sync.Mutex
}

// newAuthFailureTracker returns an empty tracker using nowFn for its
// clock — a func value, not a copied time.Time, so a caller that later
// swaps what nowFn points to (authStore's own fakeClock-in-tests
// convention) is honored on every subsequent call.
func newAuthFailureTracker(nowFn func() time.Time) *authFailureTracker {
	return &authFailureTracker{current: make(map[string]*authFailureEntry), nowFn: nowFn}
}

// get returns ip's current failure count, or 0 if it is untracked or its
// window has expired. Checks current first, then previous.
func (t *authFailureTracker) get(ip string) int64 {
	now := t.nowFn()
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.current[ip]; ok && now.Before(e.expiry) {
		return e.count
	}
	if e, ok := t.previous[ip]; ok && now.Before(e.expiry) {
		return e.count
	}
	return 0
}

// increment records one failure for ip and returns its new count for the
// current window, creating a fresh entry in current if ip is untracked in
// current or its own entry there already expired — a plain fixed-window
// counter, matching memoryStore.incrBy's own contract (limits.go) in
// spirit. Rotates current -> previous before inserting a brand-new ip
// once current is already at authFailureMapCap — see this type's own doc
// comment for why that bounds total memory without ever scanning.
func (t *authFailureTracker) increment(ip string) int64 {
	now := t.nowFn()
	t.mu.Lock()
	defer t.mu.Unlock()

	if e, ok := t.current[ip]; ok {
		if now.Before(e.expiry) {
			e.count++
			return e.count
		}
		e.count = 1
		e.expiry = now.Add(authFailureWindow)
		return 1
	}
	if len(t.current) >= authFailureMapCap {
		t.previous = t.current
		t.current = make(map[string]*authFailureEntry, authFailureMapCap)
	}
	t.current[ip] = &authFailureEntry{count: 1, expiry: now.Add(authFailureWindow)}
	return 1
}

// logGate rate-limits a single log line to once per authFailureLogEvery,
// counting how many calls were suppressed in between so the next actually
// -written line can report them (security review round 2, 2026-08-22,
// important finding 6): a single global rate-limit gate would otherwise
// make several DISTINCT attacker IPs failing within one interval look
// identical to a single noisy one — three separate IPs each failing once
// in a 30s window previously produced only ONE log line total, with no
// sign two more existed. Reporting the suppressed count in the next line
// restores that visibility without tracking a per-IP gate (an LRU of
// recently-logged IPs was the documented alternative; a suppressed count
// is simpler and gives an operator the same "something else happened"
// signal). Two independent zero-value-usable instances back authStore's
// routine-failure and throttle-engaged lines (below) so a burst of one
// can never suppress, or be suppressed by, the other. The very first call
// on a fresh gate always logs (lastLogAt's zero value is more than
// authFailureLogEvery before any real now).
type logGate struct {
	lastLogAt  time.Time
	suppressed int64
	mu         sync.Mutex
}

func (g *logGate) shouldLog(now time.Time) (log bool, suppressed int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.lastLogAt.IsZero() && now.Sub(g.lastLogAt) < authFailureLogEvery {
		g.suppressed++
		return false, 0
	}
	suppressed = g.suppressed
	g.suppressed = 0
	g.lastLogAt = now
	return true, suppressed
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
//
// failures is identify's own per-source-IP failed-attempt counter
// (security audit finding 3, 2026-08-22; failureLog/throttleLog added by
// security review round 2, 2026-08-22) — see identify's own doc comment
// for why this is pure observability, never enforcement, and
// authFailureTracker's own doc comment for why it is not the
// general-purpose memoryStore (limits.go). It is authStore's own
// dedicated instance: auth failures are a distinct concept from LLM
// traffic limits, and authStore must work identically whether or not a
// Gateway even has a limiter wired yet. failureLog/throttleLog back
// logAuthEvent's two distinct rate-limited lines (logger.go).
type authStore struct {
	lastModTime   time.Time
	lastCheck     time.Time
	log           gatewayLogger
	failures      *authFailureTracker
	usersFile     *usersFile
	nowFn         func() time.Time
	groups        map[string]*group
	byDigest      map[[32]byte]*authEntry
	inline        map[[32]byte]*authEntry
	failureLog    logGate
	throttleLog   logGate
	fileUserCount int
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
	// Delegate through a.nowFn rather than copying it: a test that
	// overrides a.nowFn AFTER construction (the fakeClock convention
	// newReloadableAuthStore already uses, users_file_test.go) must still
	// control failures' own clock — this closure reads a.nowFn fresh on
	// every call, so it always sees whichever func is currently assigned.
	a.failures = newAuthFailureTracker(func() time.Time { return a.nowFn() })
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
		// seenNames rejects two INLINE users sharing the same name
		// (security audit finding 5, 2026-08-22) — scoped to inline only,
		// not cross-checked against a later file-sourced set: replaceFileUsers
		// intentionally allows a file user to override an inline user of the
		// same name (its own doc comment), so "duplicate name" only ever
		// means "duplicate within the same source".
		seenNames := make(map[string]bool, len(cfg.Users.Inline))
		for _, uc := range cfg.Users.Inline {
			entry, err := a.buildEntry(uc)
			if err != nil {
				return nil, err
			}
			if seenNames[entry.user.name] {
				return nil, fmt.Errorf("llmgateway: duplicate user name %q", entry.user.name)
			}
			seenNames[entry.user.name] = true
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
//
// uc.Name must be non-empty (security audit finding 5, 2026-08-22):
// counters are keyed on a user's name (windowKey's "kind:id:..." shape,
// limits.go, called with id=u.name at routes_unified.go's own call site),
// so an empty name would collapse every such user's requests-per-minute/
// day, token, and cost counters onto the single shared key
// "llmgw:user::...", each silently overwriting the others' admission
// checks and usage — never a legitimate configuration, always a config
// mistake worth failing construction over. This check is deliberately
// NOT extended to configNamePattern's full character restriction
// (providers.go): that pattern's own doc comment ties it specifically to
// being embedded as a URL PATH SEGMENT (passthroughRoute's
// "/{provider}/*", targetRoute's "/mcp/{name}/*" and "/a2a/{name}/*") — a
// concern that does not apply to a user name, which is never used as a
// route path segment anywhere in this package, only as a counter-key
// component and a JSON field in admin views. Restricting the character
// set beyond "non-empty" would risk rejecting a real, currently-working
// operator-chosen name this round has no visibility into, for a route-
// safety reason that has no bearing on users at all — see this finding's
// own report for the full reasoning.
func (a *authStore) buildEntry(uc *UserConfig) (*authEntry, error) {
	if uc == nil {
		return nil, fmt.Errorf("llmgateway: user config entry must not be nil")
	}
	if uc.Name == "" {
		return nil, fmt.Errorf("llmgateway: user config entry must have a non-empty name")
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
	// seenFileNames rejects two FILE users sharing the same name (security
	// audit finding 5, 2026-08-22) — scoped to this call's own us slice
	// only, never checked against a.inline: a file user overriding an
	// inline user of the same name is this function's own documented,
	// intentional behavior (doc comment above), not a duplicate.
	seenFileNames := make(map[string]bool, len(us))
	for _, uc := range us {
		entry, err := a.buildEntry(uc)
		if err != nil {
			return err
		}
		if seenFileNames[entry.user.name] {
			return fmt.Errorf("llmgateway: duplicate user name %q", entry.user.name)
		}
		seenFileNames[entry.user.name] = true
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

// authThrottled reports whether ip has reached authFailureLimit failed
// identify attempts within the current authFailureWindow — a read-only
// observability signal for logAuthEvent's distinct throttle-engaged line
// (logger.go). identify itself never consults this (security review
// round 2, 2026-08-22, critical finding 1 — see identify's own doc
// comment for why).
func (a *authStore) authThrottled(ip string) bool {
	return a.failures.get(ip) >= authFailureLimit
}

// recordAuthFailure increments ip's failure count for the current
// authFailureWindow (authFailureTracker.increment's own fixed-window
// contract). Pure observability post security review round 2's critical
// finding 1: it never influences identify's own return value (see
// identify's doc comment) — only authThrottled's read, for
// logAuthEvent's distinct throttle-engaged line.
func (a *authStore) recordAuthFailure(ip string) {
	a.failures.increment(ip)
}

// shouldLogAuthFailure reports whether the next ROUTINE failed-auth log
// line (logAuthEvent, logger.go) should actually be written, and how many
// prior failure events were suppressed since the last line actually
// wrote (logGate's own doc comment covers the suppressed-count
// reasoning) — the same rate-limiting technique limiter.logStoreError
// (limits.go) already applies to store-error lines, applied here so an
// attacker driving unlimited failed-auth attempts can never drive
// unlimited synchronous stderr writes on the shared Traefik ingress
// (security audit finding 3, 2026-08-22). Successful-auth logging is
// deliberately left unbounded: it requires an already-valid API key, so
// its volume is bounded by legitimate authenticated traffic — itself
// already governed by checkAndCount's requests-per-minute/day limits
// elsewhere — not by an unauthenticated caller's attempt rate, which is
// exactly what this finding is about bounding.
func (a *authStore) shouldLogAuthFailure() (log bool, suppressed int64) {
	return a.failureLog.shouldLog(a.nowFn())
}

// shouldLogThrottleEngaged is shouldLogAuthFailure's counterpart for the
// distinct "this source IP is currently throttled" line (security review
// round 2, 2026-08-22, important finding 5) — its own separate rate-limit
// gate (logGate's own doc comment), so a burst of routine failure lines
// can never suppress, or be suppressed by, this higher-signal line.
func (a *authStore) shouldLogThrottleEngaged() (log bool, suppressed int64) {
	return a.throttleLog.shouldLog(a.nowFn())
}

// identify resolves r's presented API key to a user and their group. It
// reads "Authorization: Bearer <key>" (case-insensitive "bearer" prefix) or
// "x-api-key: <key>"; Bearer wins when both are present. The presented key
// is looked up by SHA-256 digest, then verified with a constant-time
// compare — the key itself is never retained.
//
// The presented key is verified FIRST, UNCONDITIONALLY (security review
// round 2, 2026-08-22, critical finding 1): a request carrying a VALID
// key always succeeds, before any per-source-IP throttle state is even
// read. The prior revision of this function checked authThrottled BEFORE
// verifying the key at all — but this plugin runs as a Traefik middleware
// behind a Kubernetes Service, and with externalTrafficPolicy: Cluster
// (this operator's own live deployment), r.RemoteAddr is the NODE's own
// SNAT address, shared by every external caller reaching that node, not
// a per-tenant address. Confirmed against this operator's own 10 live
// users: 20 failed requests from anywhere on the internet, sharing that
// node, locked out every one of them, including the admin dashboard's own
// poll. Verifying the key first makes that impossible — a legitimate,
// already-valid request can never be rejected by another caller's failed
// attempts, from the same address or not.
//
// Every failure path below — no key presented, key not found, or a
// (practically unreachable, see subtle.ConstantTimeCompare's own call
// site) digest mismatch — records one failure for the caller's source IP
// via recordAuthFailure; logAuthEvent (logger.go) separately decides, per
// call, whether to emit a routine failure line or a distinct throttle-
// engaged line by consulting authThrottled itself. No branch in this
// function ever changes its own return value based on throttle state —
// throttling is pure observability now (bounding the log's write volume
// and the tracker's own memory; authFailureTracker's doc comment,
// above), never enforcement: an attacker with no valid key gains nothing
// from this change (every failure path here already returned false
// either way, throttled or not), and the raw per-attempt cost this
// function pays (one SHA-256 digest, one map lookup, one
// ConstantTimeCompare) is already cheap enough — nanoseconds — that
// gating it early bought negligible protection against the cost itself,
// only the false-positive risk the paragraph above describes.
func (a *authStore) identify(r *http.Request) (*user, *group, bool) {
	key, ok := presentedKey(r)
	if ok {
		digest := sha256.Sum256([]byte(key))
		a.mu.RLock()
		entry, found := a.byDigest[digest]
		a.mu.RUnlock()
		if found && subtle.ConstantTimeCompare(digest[:], entry.digest[:]) == 1 {
			return entry.user, entry.group, true
		}
	}
	a.recordAuthFailure(clientIP(r))
	return nil, nil, false
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
