package traefikllmgateway

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// user is an authenticated API-key holder resolved by authStore.identify.
type user struct {
	limits          *LimitsConfig
	name, groupName string
	// groupNames is this user's full, de-duplicated, order-preserving
	// member group list (effectiveGroupNames, below) — groupName above
	// keeps only the first, for back-compat display; groupNames is the
	// complete membership, read by snapshot (below) for the admin
	// dashboard's per-user "groups" listing and for counting this user
	// once in EACH member group's own memberCount.
	groupNames []string
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
	cache *bool
	// personalGrant is the same grant as grants' own LAST element,
	// whenever a personal grant is present (effectiveGroup, below) — kept
	// as its own field, distinct from indexing into grants, so
	// allowsPassthroughPathForProvider (below, HIGH-1 fix, review round
	// 2) can tell "the personal grant" apart from "the last member
	// group's grant" directly, rather than inferring it from
	// len(grants) vs len(memberGroups). nil for an ordinary group and for
	// a multi-group user with no personal grant alike.
	personalGrant *grant
	name          string
	providers     []string
	models        []string
	mcpServers    []string
	agents        []string
	// passthroughPaths is GroupConfig.PassthroughPaths carried through
	// unchanged (security+performance audit, 2026-08-22) — see
	// allowsPassthroughPath below.
	passthroughPaths []string
	// grants is the ordered list of (providers, models) authorization
	// pairs this principal grants THROUGH — one per member group plus an
	// optional personal grant appended last (effectiveGroup, below). nil
	// for an ordinary, single-membership group with no personal grant
	// (the overwhelmingly common case, including every group literal
	// this package's own tests construct directly): allowsProviderModel
	// (below) then falls back to treating grp itself — via its own
	// providers/models fields above — as the sole implicit grant, so
	// every existing group keeps behaving exactly as before this field
	// existed.
	grants []grant
	// memberGroups is the list of actual, named *group values a
	// synthetic multi-membership/personal-grant principal
	// (effectiveGroup) was built from — nil for an ordinary group, which
	// is its own sole member (memberScopeGroups, below, used by
	// buildLimitScopes, routes_unified.go, for one limit scope per
	// member group).
	memberGroups []*group
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

// grant is one (providers, models) authorization pair a principal (a
// *group, possibly synthetic — effectiveGroup, below) grants access
// through. A principal's authorization is the union of one grant per
// member group plus an optional personal grant, but a request must match
// a SINGLE grant's providers AND models — never a provider one grant
// allows combined with a model only some OTHER grant allows
// (allowsProviderModel, below).
type grant struct {
	providers []string
	models    []string
}

// allowsProvider reports whether name matches one of the group's provider
// glob patterns. An empty pattern list allows every provider.
func (grp *group) allowsProvider(name string) bool {
	return matchesGlob(grp.providers, name)
}

// hasModelRestriction reports whether grp's Models glob is non-empty —
// whether there is anything for allowsProviderModel to actually reject on
// the model side (security review fix, 2026-08-22, round 2 — updated
// review-auth finding F6, 2026-09 audit: the standalone allowsModel this
// comment originally referenced was removed as dead code; every
// production model check goes through allowsProviderModel). handlePassthrough
// (routes_passthrough.go) checks this BEFORE peeking a passthrough
// request's body at all: a group with an empty Models list (matchesGlob's
// own empty-means-all contract — the live cluster's "home" group and
// every other group that has not opted into model restrictions) has
// nothing to deny on the model side, so there is no reason to read,
// buffer, or even look at the request body for model enforcement — zero
// risk, and exactly today's behavior for the overwhelming majority of
// traffic.
func (grp *group) hasModelRestriction() bool {
	return len(grp.models) > 0
}

// grantCount reports how many grants grp carries — len(grp.grants) for a
// synthetic multi-membership/personal-grant principal, or 1 for grp's
// own single-implicit-grant case (grp.grants nil) — WITHOUT allocating
// (review round 2, NIT fix): callers that only need to know "single
// grant or not" to pick a fast/slow path (bareEntryWinner, registry.go;
// failoverCandidates, failover.go) used to call
// len(grp.effectiveGrants()), which built and threw away a fresh
// []grant{...} on every call, including every ordinary single-group
// request — the overwhelmingly common case, on the request-path hot
// loop.
func (grp *group) grantCount() int {
	if grp.grants != nil {
		return len(grp.grants)
	}
	return 1
}

// allowsProviderModel reports whether SOME SINGLE grant of grp's own
// allows provider AND at least one of ids — the multi-group/personal-
// grant feature's "no cross-grant leak" rule: a provider one grant
// allows combined with a model only a DIFFERENT grant allows must never
// authorize a request neither grant alone would. For an ordinary group
// with exactly one implicit grant (the overwhelmingly common case,
// grp.grants nil), this is exactly the pre-existing combined "model glob
// matches AND provider glob matches" check every caller below switched
// to this method FROM — registry.go's resolveAgainst/listFor,
// routes_passthrough.go's allowsPassthroughModel, and failover.go's
// failoverCandidates for a multi-grant principal — specifically so
// single-grant behavior is preserved byte-for-byte while the multi-grant
// case gets this stricter, per-grant rule.
//
// The single-grant case is inlined directly against grp's own
// providers/models fields (review round 2, NIT fix), rather than
// wrapping them in a one-element []grant and looping that: this method
// runs at least once per request through every metered route
// (resolveAgainst, allowsPassthroughModel) and the wrapping slice was a
// real per-request heap allocation for every ordinary, single-group
// caller — the multi-grant loop below is unchanged and still used for
// grp.grants != nil.
func (grp *group) allowsProviderModel(provider string, ids ...string) bool {
	if grp.grants == nil {
		if !matchesGlob(grp.providers, provider) {
			return false
		}
		for _, id := range ids {
			if matchesGlob(grp.models, id) {
				return true
			}
		}
		return false
	}
	for _, g := range grp.grants {
		if !matchesGlob(g.providers, provider) {
			continue
		}
		for _, id := range ids {
			if matchesGlob(g.models, id) {
				return true
			}
		}
	}
	return false
}

// memberScopeGroups returns the *group values buildLimitScopes (routes_
// unified.go) builds one "group" limit scope per: grp.memberGroups for a
// synthetic multi-membership/personal-grant principal (effectiveGroup,
// below), or grp itself — its own sole member — for the overwhelmingly
// common ordinary-group case (memberGroups nil), preserving today's
// single "group" scope exactly.
func (grp *group) memberScopeGroups() []*group {
	if grp.memberGroups != nil {
		return grp.memberGroups
	}
	return []*group{grp}
}

// writeLengthPrefixed appends s to b as "<decimal length>:<s>" (a
// netstring — a well-known unambiguous, prefix-free encoding): a reader
// that always consumes digits up to the first ':' as a byte count, then
// consumes exactly that many further bytes as the value, can never
// misinterpret where one component ends and the next begins, regardless
// of what bytes s itself contains (including further digits or ':'
// characters) — unlike a fixed separator byte/string, which a value
// containing that exact separator can spoof. Used by modelsCacheKey,
// below (NIT fix, review round 3): a plain "\x00"-joined encoding with
// bare "p:"/"m:" marker literals let a group literally named "p:" (group
// names carry no character-set validation — effectiveGroup's own doc
// comment, above) produce a signature byte-for-byte identical to an
// unrelated (membership, personal grant) combination.
func writeLengthPrefixed(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}

// modelsCacheKey returns grp's stable STRING key for modelRegistry's
// modelsCache (MEDIUM-3 fix, review round 2) — see modelsJSON's own doc
// comment (registry.go) for why a *group pointer is unsafe to key that
// cache on. A real, named group (grp.memberGroups nil) is keyed on its
// own name, netstring-encoded (writeLengthPrefixed, above) behind a "g"
// discriminator byte that can never collide with a synthetic key's own
// "s" discriminator, regardless of what an operator names a group. A
// synthetic principal (effectiveGroup) is keyed on its member group
// names, in EFFECTIVE ORDER — each length-prefixed, preceded by their own
// count, so the reader never needs a marker byte to know when the member
// list ends — followed by a single 'Y'/'N' byte for "personal grant
// present", and, when present, its own providers then models, each list
// as a count followed by that many length-prefixed entries. Every
// component of every list is length-prefixed (review round 3, NIT fix —
// the pre-fix version separated components with a bare "\x00" byte and
// bare "p:"/"m:" marker strings, both of which a sufficiently-adversarial
// group/provider/model NAME could itself contain, producing a genuine
// collision between two different, unrelated principals — see this
// function's own regression test, registry_test.go, for the exact
// reviewer-reported pair). Two DIFFERENT users with IDENTICAL membership
// therefore still share one cache entry — including the SAME principal
// rebuilt, as a brand-new *group, across a users-file hot reload
// (effectiveGroup allocates fresh every call) — closing the reload-driven
// unbounded growth MEDIUM-3 (review round 2) fixed, without reintroducing
// this round's own ambiguity.
func (grp *group) modelsCacheKey() string {
	var b strings.Builder
	if grp.memberGroups == nil {
		b.WriteString("g")
		writeLengthPrefixed(&b, grp.name)
		return b.String()
	}
	b.WriteString("s")
	b.WriteString(strconv.Itoa(len(grp.memberGroups)))
	b.WriteByte(':')
	for _, m := range grp.memberGroups {
		writeLengthPrefixed(&b, m.name)
	}
	if grp.personalGrant == nil {
		b.WriteByte('N')
		return b.String()
	}
	b.WriteByte('Y')
	b.WriteString(strconv.Itoa(len(grp.personalGrant.providers)))
	b.WriteByte(':')
	for _, p := range grp.personalGrant.providers {
		writeLengthPrefixed(&b, p)
	}
	b.WriteString(strconv.Itoa(len(grp.personalGrant.models)))
	b.WriteByte(':')
	for _, mo := range grp.personalGrant.models {
		writeLengthPrefixed(&b, mo)
	}
	return b.String()
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

// personalGrantAllowsPath reports whether grp's personal grant (if any)
// authorizes path — the "personal grant borrows the primary group's own
// paths" rule (HIGH-1 fix, review round 2; extracted into its own,
// independently testable function in review round 3 so the rule cannot
// be silently collapsed to "always allow" without a dedicated test
// catching it — see auth_test.go's own mutation-checked test). The
// personal grant carries no passthroughPaths of its own (UserConfig has
// no such field on Providers/Models), so it borrows
// grp.memberGroups[0] — the PRIMARY member group, "first in effective
// order" (UserConfig.Group before Groups, effectiveGroupNames) — the
// same one that already governs every other provider this principal can
// reach without a more specific member group's own restriction. false
// when grp has no personal grant at all, or (defensively) no member
// groups to borrow from — every real caller with a non-nil
// personalGrant also has a non-empty memberGroups (effectiveGroup, below,
// never builds one without the other).
func (grp *group) personalGrantAllowsPath(path string) bool {
	if grp.personalGrant == nil || len(grp.memberGroups) == 0 {
		return false
	}
	return grp.memberGroups[0].allowsPassthroughPath(path)
}

// allowsPassthroughPathForProvider reports whether grp authorizes a
// native passthrough request to provider AND path TOGETHER (HIGH-1 fix,
// review round 2). passthroughPaths is deliberately NOT unioned across
// member groups the way providers/models/mcpServers/agents are
// (unionOrAll, effectiveGroup below): unioning it let joining ANY
// further group silently strip a DIFFERENT member group's own path
// restriction — group "a" {providers:[alpha]}, no path restriction,
// joined with group "b" {providers:[beta], passthroughPaths:[v1/chat/
// completions]}, would otherwise let "a"'s own unrestricted paths
// authorize "/beta/v1/files" even though NEITHER group alone ever
// granted that combination.
//
// The rule: some MEMBER group g has g.allowsProvider(provider) AND
// g.allowsPassthroughPath(path) — its OWN, un-merged path list — OR the
// personal grant (if any) allows provider (matchesGlob against its own
// providers; MEDIUM-5's constructor validation, buildEntry below,
// guarantees a personal grant's providers is never empty when the grant
// exists at all) AND personalGrantAllowsPath (above).
//
// This method alone does NOT couple the MODEL check to the same grant —
// see hasModelRestrictionForProviderPath/allowsPassthroughModelForPath
// below (HIGH fix, review round 3) for that: this function answers
// "is provider+path authorized at all", used both by a single-grant
// principal's own path gate (independent of its own, separately-checked
// model gate — safe with only one grant to combine from) and by a
// multi-grant principal's own path gate (handlePassthrough,
// routes_passthrough.go), which then re-derives which SPECIFIC grants
// matched provider+path before ever consulting a model.
//
// For an ordinary, single-grant group (grp.memberGroups nil), this
// collapses to grp itself as its own sole member (memberScopeGroups):
// grp.allowsProvider(provider) && grp.allowsPassthroughPath(path) —
// byte-identical to the two independent checks handlePassthrough
// (routes_passthrough.go) ran before this method existed.
func (grp *group) allowsPassthroughPathForProvider(provider, path string) bool {
	for _, m := range grp.memberScopeGroups() {
		if m.allowsProvider(provider) && m.allowsPassthroughPath(path) {
			return true
		}
	}
	if grp.personalGrant != nil && matchesGlob(grp.personalGrant.providers, provider) && grp.personalGrantAllowsPath(path) {
		return true
	}
	return false
}

// hasModelRestrictionForProviderPath reports whether EVERY grant that
// allows BOTH provider AND path also restricts models (a non-empty
// Models glob) — the HIGH fix's (review round 3) multi-grant, path-
// coupled counterpart to the single-grant hasModelRestriction() above:
// peeking the body is pointless once some grant that ALREADY covers this
// EXACT provider+path combination also grants blanket model access on
// it. Mirrors allowsPassthroughPathForProvider's own member-then-personal
// structure exactly (down to reusing personalGrantAllowsPath), so the
// two can never disagree about which grants "match" provider+path.
//
// Used only for a multi-grant principal (handlePassthrough,
// routes_passthrough.go, gates on grp.grantCount() > 1 itself) — a
// single-grant principal keeps using hasModelRestriction() directly,
// unchanged, since a lone grant can never combine with itself to produce
// the cross-grant leak this exists to prevent.
func (grp *group) hasModelRestrictionForProviderPath(provider, path string) bool {
	for _, m := range grp.memberScopeGroups() {
		if m.allowsProvider(provider) && m.allowsPassthroughPath(path) && len(m.models) == 0 {
			return false
		}
	}
	if grp.personalGrant != nil && matchesGlob(grp.personalGrant.providers, provider) && grp.personalGrantAllowsPath(path) && len(grp.personalGrant.models) == 0 {
		return false
	}
	return true
}

// allowsPassthroughModelForPath reports whether SOME SINGLE grant — a
// member group's own, or the personal grant's own (borrowing the
// primary's own paths via personalGrantAllowsPath) — authorizes provider
// AND path AND at least one of ids, ALL from that SAME grant (HIGH fix,
// review round 3): the defect P1/P2 reproduce is exactly a provider+path
// match from one grant combined with a model match from a DIFFERENT
// one — round 2's fix coupled provider+model per grant
// (allowsProviderModel) and provider+path per grant
// (allowsPassthroughPathForProvider) SEPARATELY, which still let the two
// checks succeed via two DIFFERENT grants for the same request. Used
// only for a multi-grant principal, mirroring
// hasModelRestrictionForProviderPath's own gating.
func (grp *group) allowsPassthroughModelForPath(provider, path string, ids ...string) bool {
	for _, m := range grp.memberScopeGroups() {
		if !m.allowsProvider(provider) || !m.allowsPassthroughPath(path) {
			continue
		}
		for _, id := range ids {
			if matchesGlob(m.models, id) {
				return true
			}
		}
	}
	if grp.personalGrant != nil && matchesGlob(grp.personalGrant.providers, provider) && grp.personalGrantAllowsPath(path) {
		for _, id := range ids {
			if matchesGlob(grp.personalGrant.models, id) {
				return true
			}
		}
	}
	return false
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
	lastModTime time.Time
	lastCheck   time.Time
	// lastReloadErrModTime and lastReloadErrMsg dedup maybeReload's own
	// error logging (users_file.go — review-auth finding F11, 2026-09
	// audit): a users file left broken (bad JSON, a stat failure, a
	// rejected replaceFileUsers) never advances lastModTime, so without
	// this a broken file gets re-attempted AND re-logged every
	// reloadEvery for as long as it stays broken — roughly 17k lines/day
	// at the default 5s throttle. A later log fires again only when the
	// error TEXT changes, or the mtime the attempt was made against
	// changes (an operator rewrote the file, even to an identically
	// broken state) — "once per distinct error / mtime change". Both are
	// touched only from inside maybeReload, which its own TryLock already
	// serializes to at most one call in flight, so no separate mutex
	// guards them.
	lastReloadErrModTime time.Time
	log                  gatewayLogger
	failures             *authFailureTracker
	usersFile            *usersFile
	nowFn                func() time.Time
	groups               map[string]*group
	byDigest             map[[32]byte]*authEntry
	inline               map[[32]byte]*authEntry
	lastReloadErrMsg     string
	failureLog           logGate
	throttleLog          logGate
	fileUserCount        int
	mu                   sync.RWMutex
	buildMu              sync.Mutex
	reloadMu             sync.Mutex
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

// unionOrAll returns the union of every list in lists' own glob patterns,
// EXCEPT when some list is itself empty: matchesGlob's own contract (this
// file) already treats an empty pattern list as "matches anything," so
// one empty list among several already allows everything on its own —
// the union must too, returning nil (matchesGlob's own empty fast path)
// rather than a literal concatenation of the OTHER lists, which would
// lose that all-allowing meaning entirely. Used by effectiveGroup, below,
// for a synthetic principal's providers/models/mcpServers/agents fields:
// "if ANY member's list is empty, the effective list is empty" is the
// identical rule for every one of them. NOT used for passthroughPaths
// (HIGH-1 fix, review round 2) — see allowsPassthroughPathForProvider's
// own doc comment above for why a union is actively wrong for that one
// field specifically: unlike providers/models/mcpServers/agents,
// passthroughPaths is scoped to a SPECIFIC provider, so merging it
// across member groups can strip a narrower member's own restriction the
// instant a second, broader member group is joined.
func unionOrAll(lists ...[]string) []string {
	for _, l := range lists {
		if len(l) == 0 {
			return nil
		}
	}
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// effectiveGroupNames returns uc's de-duplicated, order-preserving member
// group name list: primary (UserConfig.Group) first when non-empty, then
// extra (UserConfig.Groups) in order, skipping any name already seen —
// UserConfig.Groups' own doc comment (llmgateway.go) has the full
// contract this implements.
func effectiveGroupNames(primary string, extra []string) []string {
	seen := make(map[string]bool, len(extra)+1)
	var names []string
	if primary != "" {
		seen[primary] = true
		names = append(names, primary)
	}
	for _, name := range extra {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// effectiveGroup returns the *group a user belonging to every group in
// members, plus an optional personal grant, is authorized through —
// UserConfig.Groups' own doc comment (llmgateway.go) has the full
// authorization/limits/cache contract this builds.
//
// Identity is preserved for the overwhelmingly common case — exactly one
// member group and no personal grant — by returning that member's own
// *group pointer completely unchanged: name, limit-scope id, every
// field, all identical to before this feature existed (multi-group
// back-compat rule). Every other case builds a fresh, synthetic *group:
// its name joins every member group's own name with "+", for display/log
// use only — this synthetic group is never looked up by name (grp.name's
// only reader outside tests, registry.go's modelsJSON, uses it purely to
// label a log line), and Config.Groups keys go through no character-set
// validation the way provider/mcpServers/agents names do
// (validateConfigName, providers.go), so a real, operator-chosen group
// name COULD textually collide with a synthetic one; that collision is
// harmless (an ambiguous-looking log line, never a lookup) precisely
// because nothing resolves a *group by this name. grants holds one
// (providers, models) pair per member plus the personal grant (if any)
// appended last (personalGrant, below, keeps that same last grant under
// its own name too, for allowsPassthroughPathForProvider's own use), and
// memberGroups keeps the actual member *group pointers, in EFFECTIVE
// ORDER (primary first), for memberScopeGroups' own per-member-group
// limit scope and for allowsPassthroughPathForProvider's own "primary
// group" rule.
//
// passthroughPaths is deliberately left at its zero value (nil) here,
// UNLIKE providers/models/mcpServers/agents: it is not a simple union
// (HIGH-1 fix, review round 2) — allowsPassthroughPathForProvider reads
// memberGroups/personalGrant directly instead, so a synthetic
// principal's own .passthroughPaths field would be dead, and
// potentially misleading, state if this function still populated it.
func effectiveGroup(members []*group, personal *grant) *group {
	if len(members) == 1 && personal == nil {
		return members[0]
	}

	names := make([]string, len(members))
	grants := make([]grant, 0, len(members)+1)
	var providerLists, modelLists, mcpLists, agentLists [][]string
	var sawCacheFalse, sawCacheTrue bool
	var minTTL time.Duration
	for i, m := range members {
		names[i] = m.name
		grants = append(grants, grant{providers: m.providers, models: m.models})
		providerLists = append(providerLists, m.providers)
		modelLists = append(modelLists, m.models)
		mcpLists = append(mcpLists, m.mcpServers)
		agentLists = append(agentLists, m.agents)
		if m.cache != nil {
			if *m.cache {
				sawCacheTrue = true
			} else {
				sawCacheFalse = true
			}
		}
		if m.cacheTTL > 0 && (minTTL == 0 || m.cacheTTL < minTTL) {
			minTTL = m.cacheTTL
		}
	}
	var personalGrant *grant
	if personal != nil {
		grants = append(grants, *personal)
		providerLists = append(providerLists, personal.providers)
		modelLists = append(modelLists, personal.models)
		personalGrant = personal
	}

	var cache *bool
	switch {
	case sawCacheFalse:
		f := false
		cache = &f
	case sawCacheTrue:
		t := true
		cache = &t
	}

	return &group{
		name:          strings.Join(names, "+"),
		providers:     unionOrAll(providerLists...),
		models:        unionOrAll(modelLists...),
		mcpServers:    unionOrAll(mcpLists...),
		agents:        unionOrAll(agentLists...),
		cache:         cache,
		cacheTTL:      minTTL,
		grants:        grants,
		memberGroups:  members,
		personalGrant: personalGrant,
	}
}

// buildEntry resolves uc's groups (Group/Groups, effectiveGroupNames),
// its optional personal grant (Providers/Models), and its API key into an
// authEntry. The API key goes through resolveSecret before digesting, so
// env:/file: references work the same as for provider keys.
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
	groupNames := effectiveGroupNames(uc.Group, uc.Groups)
	if len(groupNames) == 0 {
		return nil, fmt.Errorf("llmgateway: user %q must belong to at least one group", uc.Name)
	}
	members := make([]*group, len(groupNames))
	for i, name := range groupNames {
		grp, ok := a.groups[name]
		if !ok {
			return nil, fmt.Errorf("llmgateway: user %q references unknown group %q", uc.Name, name)
		}
		members[i] = grp
	}
	// personal is uc's own Providers/Models grant — present only when at
	// least one of the two lists is non-empty (UserConfig.Providers/
	// Models' own doc comment, llmgateway.go): an empty personal grant
	// can never mean "allow everything" on its own.
	//
	// A MODELS-ONLY personal grant (Models set, Providers empty) is
	// rejected here (MEDIUM-5 fix, review round 2): empty Providers means
	// "any provider" (matchesGlob's own empty-means-all contract), so a
	// models-only grant would silently reach every configured provider —
	// including ones no member group of this user covers at all — and
	// would pass allowsPassthroughPathForProvider's own provider gate for
	// any provider too. A Providers-only grant (Models empty) is valid:
	// it means "every model on those providers," the same empty-means-all
	// contract applied the other way round.
	var personal *grant
	if len(uc.Providers) > 0 || len(uc.Models) > 0 {
		if len(uc.Providers) == 0 {
			return nil, fmt.Errorf("llmgateway: user %q: personal \"models\" requires personal \"providers\"", uc.Name)
		}
		personal = &grant{providers: uc.Providers, models: uc.Models}
	}
	effective := effectiveGroup(members, personal)

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
		user: &user{
			limits:     uc.Limits,
			groupNames: groupNames,
			name:       uc.Name,
			groupName:  groupNames[0],
			admin:      uc.Admin,
		},
		group: effective,
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
// is looked up by its SHA-256 digest — the key itself is never retained.
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
// Every failure path below — no key presented, or key not found — records
// one failure for the caller's source IP via recordAuthFailure;
// logAuthEvent (logger.go) separately decides, per call, whether to emit
// a routine failure line or a distinct throttle-engaged line by
// consulting authThrottled itself. No branch in this function ever
// changes its own return value based on throttle state — throttling is
// pure observability now (bounding the log's write volume and the
// tracker's own memory; authFailureTracker's doc comment, above), never
// enforcement: an attacker with no valid key gains nothing from this
// change (every failure path here already returned false either way,
// throttled or not), and the raw per-attempt cost this function pays (one
// SHA-256 digest, one map lookup) is already cheap enough — nanoseconds —
// that gating it early bought negligible protection against the cost
// itself, only the false-positive risk the paragraph above describes.
//
// No constant-time compare runs here (review-auth finding F7, 2026-09
// audit — a prior revision ran subtle.ConstantTimeCompare(digest[:],
// entry.digest[:]) after this exact lookup, but byDigest is populated
// ONLY as a.byDigest[entry.digest] = entry (newAuthStore/
// replaceFileUsers), so a map hit's entry.digest is the map key itself by
// construction: the compare was a tautology, always true when found is
// true, comparing digest against a value guaranteed identical to it. The
// real secret-matching step already happened inside the Go runtime's own
// map lookup, on the presented key's SHA-256 digest, not the key itself
// — there is no separate secret-bytes compare here for a non-constant-time
// implementation to leak timing from).
func (a *authStore) identify(r *http.Request) (*user, *group, bool) {
	key, ok := presentedKey(r)
	if ok {
		digest := sha256.Sum256([]byte(key))
		a.mu.RLock()
		entry, found := a.byDigest[digest]
		a.mu.RUnlock()
		if found {
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
	// groups is the user's full, de-duplicated member group list
	// (user.groupNames) — groupName above keeps only the first, for
	// back-compat display (admin.go's adminUsageEntryView.GroupName).
	groups []string
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
		users = append(users, userSummary{
			limits:    entry.user.limits,
			name:      entry.user.name,
			groupName: entry.user.groupName,
			groups:    cloneStringSlice(entry.user.groupNames),
		})
		// A multi-group user counts once in EACH member group's own
		// memberCount (UserConfig.Groups' own doc comment, llmgateway.go)
		// — entry.user.groupNames is always non-empty (buildEntry rejects
		// a user with none), so this is exactly one increment per member
		// group, matching the single-group case's own one-increment
		// behavior before this field existed.
		for _, name := range entry.user.groupNames {
			memberCounts[name]++
		}
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
