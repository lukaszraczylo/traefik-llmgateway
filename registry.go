package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultDiscoveryInterval is the refresh interval applied to a
// discovery-enabled provider whose ProviderConfig.DiscoveryInterval is
// empty.
const defaultDiscoveryInterval = time.Hour

// warmFillTimeout bounds newGateway's synchronous first discovery fill, per
// provider: a slow or unreachable upstream must not stall plugin
// construction indefinitely.
const warmFillTimeout = 5 * time.Second

// backgroundRefreshTimeout bounds one background refresh goroutine spawned
// by maybeRefresh. Not specified by the task brief; chosen generously
// enough for a provider's models-list call while still bounding a leaked
// goroutine's lifetime if an upstream hangs.
const backgroundRefreshTimeout = 30 * time.Second

// errModelUnknown is returned by modelRegistry.resolve when id does not
// match any provider's known model set (explicit config plus the last
// successful discovery fetch) — 404 semantics for the caller.
var errModelUnknown = errors.New("llmgateway: unknown model")

// errModelDenied is returned by modelRegistry.resolve when id is a known
// model but the requesting group is not authorized for it — 403 semantics
// for the caller. Distinct from errModelUnknown so the caller can tell "no
// such model" from "that model exists, you cannot use it".
var errModelDenied = errors.New("llmgateway: model access denied")

// providerState tracks one provider's known model ids and discovery
// bookkeeping. explicit is set once at construction from
// ProviderConfig.Models and never mutated afterward; discovered is
// stale-while-error — a failed refresh leaves the previous discovered set
// in place rather than clearing it. mu guards every mutable field below it.
type providerState struct {
	lastRefresh time.Time
	explicit    map[string]bool
	discovered  map[string]bool
	// discoveredContext holds discovery-captured per-model context
	// lengths (feature v0.23, ProviderConfig.MetadataPath — currently
	// only openai-type adapters ever populate this, via
	// modelMetadataFetcher), keyed by upstream model id. nil for a
	// provider with no metadataPath configured, or whose adapter does
	// not implement modelMetadataFetcher at all — contextFor treats a
	// nil map the same as an absent entry: (0, false). Stale-while-
	// error, the same convention discovered itself already uses: a
	// failed capture leaves the previous map in place rather than
	// clearing it (captureModelMetadata, below).
	discoveredContext map[string]int
	// lastErr is the most recent finishRefresh call's error message, or
	// "" when that call succeeded (or discovery is disabled and
	// finishRefresh was never called at all). Read by snapshot for the
	// admin dashboard (spec §4, v0.2) — it reflects the latest attempt's
	// outcome, not the stale-while-error discovered set: a provider can
	// show a non-empty lastErr while modelCount still reflects its last
	// successful discovery.
	lastErr          string
	interval         time.Duration
	mu               sync.Mutex
	discoveryEnabled bool
	inFlight         bool
}

// hasModel reports whether id is in this provider's explicit or discovered
// model set.
func (st *providerState) hasModel(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.explicit[id] || st.discovered[id]
}

// knownIDsLocked returns the sorted union of st.explicit and
// st.discovered. It takes no lock itself — the caller must already hold
// st.mu — so both knownIDs (below) and snapshot (which needs the
// identical merge inside its own, already-held, critical section
// alongside lastRefresh/lastErr) can share this one implementation
// without knownIDs' own st.mu.Lock() double-locking (sync.Mutex is not
// reentrant) when called from inside snapshot.
func (st *providerState) knownIDsLocked() []string {
	set := make(map[string]bool, len(st.explicit)+len(st.discovered))
	for id := range st.explicit {
		set[id] = true
	}
	for id := range st.discovered {
		set[id] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// knownIDs returns the sorted union of this provider's explicit and
// discovered model ids.
func (st *providerState) knownIDs() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.knownIDsLocked()
}

// contextFor returns id's discovery-captured context length and whether
// one is known (feature v0.23) — mirroring hasModel's own locked-read
// shape.
func (st *providerState) contextFor(id string) (int, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	n, ok := st.discoveredContext[id]
	return n, ok
}

// setDiscoveredContext replaces st's discoveredContext wholesale on a
// successful metadata capture (feature v0.23, captureModelMetadata below)
// — the same replace-on-success, stale-while-error shape finishRefresh
// already applies to discovered.
func (st *providerState) setDiscoveredContext(m map[string]int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.discoveredContext = m
}

// tryBeginRefresh reports whether now is far enough past lastRefresh (or
// this is the first refresh) to start a new one, and if so marks the
// provider inFlight so a concurrent caller cannot start a second one. The
// caller must pair a true result with a later finishRefresh call.
func (st *providerState) tryBeginRefresh(now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.inFlight || now.Sub(st.lastRefresh) < st.interval {
		return false
	}
	st.inFlight = true
	return true
}

// finishRefresh records the outcome of a refresh attempt started by
// tryBeginRefresh (or, for the synchronous warm fill, run unconditionally).
// lastRefresh always advances to now, on success or failure alike — that is
// what throttles a failing provider to one attempt per interval instead of
// retrying on every call. On success (err == nil) ids replaces the
// discovered set; on failure the previous discovered set is kept
// (stale-while-error).
func (st *providerState) finishRefresh(now time.Time, ids []string, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.inFlight = false
	st.lastRefresh = now
	if err != nil {
		st.lastErr = err.Error()
		return
	}
	st.lastErr = ""
	discovered := make(map[string]bool, len(ids))
	for _, id := range ids {
		discovered[id] = true
	}
	st.discovered = discovered
}

// snapshot returns st's read-only view for the admin dashboard (spec §4,
// v0.2): its known model ids (explicit ∪ discovered, sorted — the
// provider-model-accordion task; a caller wanting just the count uses
// len(models)), last refresh time, and last refresh error message.
//
// models comes from knownIDsLocked (this method already holds st.mu, so
// it uses the lock-free variant directly rather than the locking
// knownIDs, which would deadlock reentering the same non-reentrant
// sync.Mutex), read in the SAME critical section as lastRefresh/lastErr
// below — not two separate locked calls. That matters functionally, not
// just stylistically: a concurrent finishRefresh landing between two
// separate locked sections could otherwise hand the admin dashboard a
// models list whose length disagrees with a separately-read modelCount;
// reading all three from one locked pass makes that impossible.
//
// A provider whose discovery is mid-refresh (or has never refreshed
// since a config reload) returns exactly this stale-while-error set —
// finishRefresh's own doc comment covers why: a failed refresh keeps the
// previous discovered set rather than clearing it. This method applies
// no special handling for that case; it is simply the same set every
// other snapshot consumer (modelCount, listFor) already reads.
func (st *providerState) snapshot() (models []string, lastRefresh time.Time, lastErr string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.knownIDsLocked(), st.lastRefresh, st.lastErr
}

// modelRegistry aggregates every configured provider's explicit and
// discovered model ids into the gateway's model-resolution and /v1/models
// listing surfaces. adapters, providerNames, and states are all fixed at
// construction; only providerState's own fields mutate afterward (each
// guarded by its own mutex), so modelRegistry itself needs no lock beyond
// warnedMu for the collision-log dedup below.
type modelRegistry struct {
	adapters map[string]providerAdapter
	states   map[string]*providerState
	// log is a bound method value (g.errorf), injected rather than holding
	// a *Gateway directly. Every call site below pre-formats with
	// fmt.Sprintf and calls log("%s", msg) — never log(format, a, b, ...)
	// with two or more variadic arguments. Yaegi v0.16.1's CFG builder
	// panics ("index out of range") compiling a call to a struct FIELD of
	// variadic func type with 2+ variadic arguments; the identical call
	// shape through a method or interface method is unaffected — this is
	// specific to a field holding a func value. Verified empirically
	// against yaegi v0.16.1 (see tools/yaegi-check) and does not apply to
	// g.logf/g.errorf, which are methods, not fields.
	log func(string, ...any)
	// aliases is the validated alias->target map from Config.ModelAliases
	// (spec §5, v0.2), built once by newModelRegistry via
	// validateModelAliases and never mutated afterward — resolve checks
	// it first, ahead of splitConfiguredProvider/bareWinner (see resolve's
	// own doc comment). nil (not merely empty) when the operator
	// configured no aliases at all, so every alias-aware code path is a
	// cheap nil-map lookup (always false/zero) that adds no branching
	// cost to the v0.1 no-aliases case.
	aliases map[string]string
	// modelMeta is the validated Config.ModelMeta config-override map
	// (feature v0.23), built once by newModelRegistry via
	// validateModelMeta and never mutated afterward. nil (not merely
	// empty) when the operator configured no metadata overrides at all,
	// so resolveModelMeta's config-override layer is a cheap nil-map
	// lookup (always a miss) that adds no branching cost to the
	// no-overrides case — the same nil-vs-empty convention aliases above
	// already establishes.
	modelMeta     map[string]*ModelMetaConfig
	nowFn         func() time.Time
	warned        map[string]bool
	providerNames []string
	warnedMu      sync.Mutex
}

// newModelRegistry builds a modelRegistry from adapters and cfg's matching
// ProviderConfig entries. It does not perform discovery itself — the
// caller (newGateway) runs the synchronous first fill separately via
// warmFill, then ServeHTTP entry keeps it fresh via maybeRefresh. A
// provider's ProviderConfig.DiscoveryInterval that fails time.ParseDuration
// is a constructor error; an empty one defaults to defaultDiscoveryInterval.
func newModelRegistry(adapters map[string]providerAdapter, cfg *Config, log func(string, ...any)) (*modelRegistry, error) {
	m := &modelRegistry{
		adapters:      adapters,
		states:        make(map[string]*providerState, len(adapters)),
		log:           log,
		nowFn:         time.Now,
		warned:        make(map[string]bool),
		providerNames: make([]string, 0, len(adapters)),
	}
	for name := range adapters {
		m.providerNames = append(m.providerNames, name)
	}
	sort.Strings(m.providerNames)

	// explicitModels is the union of every configured provider's EXPLICIT
	// model ids only (never discovered ones — discovery has not run yet
	// at this point in construction) — validateModelAliases' collision
	// check needs exactly this set (spec §5: "alias must not equal an
	// explicit model id of any provider").
	explicitModels := make(map[string]bool)
	for _, name := range m.providerNames {
		pc := cfg.Providers[name]
		if pc == nil {
			pc = &ProviderConfig{}
		}

		interval := defaultDiscoveryInterval
		if pc.DiscoveryInterval != "" {
			d, err := time.ParseDuration(pc.DiscoveryInterval)
			if err != nil {
				return nil, fmt.Errorf("llmgateway: provider %q: invalid discoveryInterval %q: %w", name, pc.DiscoveryInterval, err)
			}
			interval = d
		}

		explicit := make(map[string]bool, len(pc.Models))
		for _, id := range pc.Models {
			explicit[id] = true
			explicitModels[id] = true
		}

		m.states[name] = &providerState{
			explicit:         explicit,
			discovered:       make(map[string]bool),
			discoveryEnabled: pc.Discovery,
			interval:         interval,
		}
	}

	aliases, err := validateModelAliases(cfg.ModelAliases, m.providerNames, explicitModels)
	if err != nil {
		return nil, err
	}
	m.aliases = aliases

	modelMeta, err := validateModelMeta(cfg.ModelMeta)
	if err != nil {
		return nil, err
	}
	m.modelMeta = modelMeta

	return m, nil
}

// isValidModelID reports whether id satisfies spec §5's alias model-id
// shape: non-empty, and free of ASCII control characters (0x00-0x1F and
// 0x7F) — the same class of raw value mcp_a2a.go's target-URL validation
// already rejects, applied here to a model/alias id string instead of a
// URL.
func isValidModelID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validateModelAliases validates cfg.ModelAliases per spec §5's
// validation table and returns the accepted alias->target map (nil when
// raw is empty — newGateway's default, zero-behavior-change case).
// providerNames is the registry's own sorted provider-name list (used for
// the prefix-shadow check); explicitModels is the union of every
// configured provider's EXPLICIT model ids only (used for the collision
// check) — a DISCOVERED model colliding with an alias is NOT a
// construction error (discovery is dynamic and runs after this), and is
// instead resolved in the alias's favor by resolve's own precedence; see
// resolve's warnAliasShadowsDiscoveredOnce call for the one-time runtime
// log that implies.
//
// raw is iterated in sorted alias-key order, not Go's randomized map
// order, so a config with more than one invalid alias always reports the
// same one first, deterministically, across repeated runs.
func validateModelAliases(raw map[string]string, providerNames []string, explicitModels map[string]bool) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	providerSet := make(map[string]bool, len(providerNames))
	for _, p := range providerNames {
		providerSet[p] = true
	}

	names := make([]string, 0, len(raw))
	for alias := range raw {
		names = append(names, alias)
	}
	sort.Strings(names)

	out := make(map[string]string, len(raw))
	for _, alias := range names {
		target := raw[alias]
		if !isValidModelID(alias) {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q is invalid: must be non-empty with no control characters", alias)
		}
		if prefix, _, hasSlash := strings.Cut(alias, "/"); hasSlash && providerSet[prefix] {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q is invalid: its prefix %q names a configured provider and would be shadowed by provider-prefix resolution", alias, prefix)
		}
		if explicitModels[alias] {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q collides with an explicitly configured model id of the same name", alias)
		}
		if target == "" {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q has an empty target", alias)
		}
		if _, targetIsAlias := raw[target]; targetIsAlias {
			return nil, fmt.Errorf("llmgateway: modelAliases: alias %q targets %q, which is itself an alias (alias chains are not allowed)", alias, target)
		}
		out[alias] = target
	}
	return out, nil
}

// now returns the registry's current time, via nowFn — overridable in
// tests to exercise refresh throttling deterministically.
func (m *modelRegistry) now() time.Time {
	return m.nowFn()
}

// warmFill performs newGateway's synchronous first discovery fill: for
// every discovery-enabled provider, it fetches listModels once, bounded by
// warmFillTimeout, and records the result via finishRefresh. A fetch error
// is logged and otherwise non-fatal — construction still succeeds, and the
// provider's known model set is whatever its explicit config already
// provides (empty, if it configured neither explicit models nor a
// reachable discovery endpoint) until the next maybeRefresh window.
func (m *modelRegistry) warmFill(ctx context.Context) {
	for _, name := range m.providerNames {
		st := m.states[name]
		if !st.discoveryEnabled {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, warmFillTimeout)
		ids, err := m.adapters[name].listModels(fctx)
		cancel()
		st.finishRefresh(m.now(), ids, err)
		if err != nil {
			m.log("%s", fmt.Sprintf("model registry: initial discovery for provider %q failed: %v", name, err))
		}
		// Own timeout budget, review fix (SHOULD-4): fctx above is
		// spent by listModels — reusing it here would hand the metadata
		// fetch an already-near-expired (or fully expired, on a slow/
		// timed-out listModels) context every warm fill, guaranteeing a
		// spurious deadline error and log line even for a healthy
		// provider whose model-list call merely took a while.
		mctx, mcancel := context.WithTimeout(ctx, warmFillTimeout)
		m.captureModelMetadata(mctx, name, m.adapters[name], st)
		mcancel()
	}
}

// modelMetadataFetcher is an OPTIONAL providerAdapter capability (feature
// v0.23): an adapter that can also fetch per-model discovery metadata
// (currently: context window) beyond the plain model-id list listModels
// returns. Only openai-type adapters configured with ProviderConfig.
// MetadataPath implement it (provider_openai.go's openaiAdapter) —
// anthropic/gemini adapters do not, so captureModelMetadata checks for
// this via a comma-ok type assertion rather than every providerAdapter
// implementation growing a no-op method for it.
//
// Safe under Yaegi despite the documented Trap-4 restriction
// (matchesSentinel's own doc comment, limits.go): that trap is a comma-ok
// assertion of a COMPILED concrete value (a stdlib type, e.g.
// *fmt.wrapErrors or *url.Error) against an INTERPRETER-declared
// interface, which silently returns ok=false. Every adapter's dynamic
// type behind the providerAdapter interface here (*openaiAdapter,
// *anthropicAdapter, *geminiAdapter) is itself plugin-declared and
// interpreted right alongside modelMetadataFetcher — an
// interpreted-concrete-value-against-interpreted-interface assertion,
// the opposite, unaffected direction.
type modelMetadataFetcher interface {
	// fetchModelMetadata returns discovery-captured per-model context
	// lengths, keyed by upstream model id. A nil map with a nil error
	// means the adapter has nothing to report (e.g. metadataPath left
	// unconfigured) — a fast, cheap no-op the caller can invoke
	// unconditionally on every adapter that implements this interface.
	fetchModelMetadata(ctx context.Context) (map[string]int, error)
}

// captureModelMetadata is warmFill/refreshProvider's shared, best-effort
// discovery-metadata step (feature v0.23): when adapter also implements
// modelMetadataFetcher (see that interface's own doc comment for why
// this assertion is yaegi-safe), it fetches per-model context lengths
// and records them on st. A fetch error, or an adapter that does not
// implement the interface at all, is intentionally non-fatal beyond one
// log line — per this feature's own "absent/failed = no metadata
// captured" ruling, it must never fail discovery itself, which
// listModels (warmFill/refreshProvider's own caller) already accounts
// for independently.
//
// The deferred recover (review fix, SHOULD-5) is this function's OWN,
// separate from refreshProvider's outer recover: without it, a panic
// inside fetchModelMetadata would unwind into refreshProvider's deferred
// recover instead, which sets its OWN err variable and would then report
// listModels' own, already-successful discovery fetch as failed —
// discarding a good, freshly-fetched model list over an unrelated bug in
// the metadata path. Catching it here, before it can ever reach that
// outer recover, is what actually enforces "metadata must never fail
// discovery" on the panic path, not just the plain-error-return one
// already handled below.
func (m *modelRegistry) captureModelMetadata(ctx context.Context, name string, adapter providerAdapter, st *providerState) {
	defer func() {
		if rec := recover(); rec != nil {
			m.log("%s", fmt.Sprintf("model registry: metadata capture for provider %q panicked: %v", name, rec))
		}
	}()

	mf, ok := adapter.(modelMetadataFetcher)
	if !ok {
		return
	}
	meta, err := mf.fetchModelMetadata(ctx)
	if err != nil {
		m.log("%s", fmt.Sprintf("model registry: metadata capture for provider %q failed: %v", name, err))
		return
	}
	if meta == nil {
		return
	}
	st.setDiscoveredContext(meta)
}

// maybeRefresh is called at every ServeHTTP entry. For each discovery-
// enabled provider whose last refresh is older than its interval (and that
// is not already mid-refresh), it spawns exactly one background goroutine
// to pull listModels and merge the result. The spawned goroutine
// deliberately does not inherit ctx's cancellation: ctx is the incoming
// request's context, which net/http cancels once ServeHTTP returns — using
// it here would abort the background refresh moments after it starts on
// every request, so the goroutine gets its own backgroundRefreshTimeout
// budget instead. ctx is still consulted before spawning: a request whose
// context is already done skips starting new background work for it.
func (m *modelRegistry) maybeRefresh(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := m.now()
	for _, name := range m.providerNames {
		st := m.states[name]
		if !st.discoveryEnabled || !st.tryBeginRefresh(now) {
			continue
		}
		adapter := m.adapters[name]
		go m.refreshProvider(name, st, adapter) //nolint:gosec // G118: refreshProvider intentionally uses its own context.Background()-derived timeout, not r.Context(), per this function's doc comment above — the request context is canceled once ServeHTTP returns and would abort the background fetch moments after it starts
	}
}

// refreshProvider runs one background discovery fetch for a provider
// already marked inFlight by tryBeginRefresh, and records its outcome via
// finishRefresh. Split out from maybeRefresh so the goroutine closes over
// named parameters, not loop variables.
//
// This goroutine (maybeRefresh's only "go" statement in the whole plugin)
// is a deliberate, narrow exception to memoryStore's no-background-goroutine
// rule (limits.go:144-149): unlike a ticker that would outlive a Yaegi
// middleware instance rebuilt on every config reload, this goroutine is
// self-terminating — at most one per provider per interval
// (tryBeginRefresh's inFlight guard), bounded to backgroundRefreshTimeout
// (30s). A rebuild mid-flight can transiently leak one goroutine for at
// most 30s; that is accepted, not a steady-state leak.
//
// The deferred recover below is required precisely because this runs off
// any request's goroutine: an unrecovered panic here has no ServeHTTP
// caller to unwind into and would crash the whole Traefik process, not just
// fail one request. finishRefresh still runs on the panic path (with a
// synthetic error) so inFlight is always released and the next interval
// retries, exactly as a normal fetch failure would.
func (m *modelRegistry) refreshProvider(name string, st *providerState, adapter providerAdapter) {
	var ids []string
	var err error
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
		st.finishRefresh(m.now(), ids, err)
		if err != nil {
			m.log("%s", fmt.Sprintf("model registry: discovery refresh for provider %q failed: %v", name, err))
		}
	}()

	fctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
	ids, err = adapter.listModels(fctx)
	cancel()

	// Own timeout budget, review fix (SHOULD-4) — see warmFill's
	// identical comment: reusing fctx here would hand the metadata fetch
	// an already-spent context on every refresh whose listModels call
	// ran long, not just a slow or timed-out one.
	mctx, mcancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
	m.captureModelMetadata(mctx, name, adapter, st)
	mcancel()
}

// splitConfiguredProvider reports whether id has "prefix/rest" form where
// prefix names a currently configured provider. Per ruling (a): a slash in
// id is only ever a provider-prefix separator when the part before it is a
// real, configured provider name; otherwise (including when id has no
// slash, or its prefix names no configured provider) the whole string is a
// bare model id — this is what lets an upstream model id that itself
// contains a slash (e.g. "uni/deepseek-v4-flash-0731" on a provider that
// happens not to be named "uni") resolve correctly as a bare id.
func (m *modelRegistry) splitConfiguredProvider(id string) (providerName, rest string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] != '/' {
			continue
		}
		prefix, tail := id[:i], id[i+1:]
		if _, known := m.adapters[prefix]; known {
			return prefix, tail, true
		}
		return "", "", false
	}
	return "", "", false
}

// bareWinner returns the provider that owns bare id when more than one
// provider's known model set contains it: the first, in sorted
// provider-name order (ruling (g)).
func (m *modelRegistry) bareWinner(id string) (string, bool) {
	for _, name := range m.providerNames {
		if m.states[name].hasModel(id) {
			return name, true
		}
	}
	return "", false
}

// resolve maps a client-requested model id to the adapter that serves it.
// It returns the adapter, the upstream model id to send that adapter (the
// id form the provider itself knows, with any gateway-side provider prefix
// stripped), and the canonical "provider/upstreamModel" id. The original,
// exactly-as-requested id the caller passed in is not returned separately —
// the caller already holds it, in the id argument itself.
//
// An EXACT alias match (spec §5, v0.2) wins before any other rule below —
// checked first, ahead of provider-prefix splitting, so an alias id that
// happens to look like "prefix/rest" is never mistaken for one (and
// validateModelAliases already rejects an alias whose prefix names an
// actually configured provider at construction, so this ordering never
// has to arbitrate a genuine conflict between the two forms). id also
// happening to collide with a since-DISCOVERED model of some provider
// (impossible for an explicit one; validateModelAliases already rejects
// that at construction) is resolved in the alias's favor by this same
// ordering — warnAliasShadowsDiscoveredOnce logs that once per id.
//
// id in "provider/model" form (ruling (a): only when "provider" names a
// configured provider) resolves directly against that provider. Any other
// id is a bare id, resolved against the first configured provider (sorted
// name order) whose known model set contains it — ruling (g)'s collision
// rule. Either way, authorization requires both grp.allowsModel and
// grp.allowsProvider for the resolved provider (ruling (c)); a model that
// exists but fails authorization returns errModelDenied, distinct from
// errModelUnknown for a model no configured provider knows at all.
func (m *modelRegistry) resolve(id string, grp *group) (providerAdapter, string, string, error) {
	if target, isAlias := m.aliases[id]; isAlias {
		// Warned-set checked FIRST (review fix): bareWinner is an
		// O(providerNames) scan, and every request for an already-warned
		// alias would otherwise pay it just to decide not to log again.
		// Once warned, this collapses to one cheap mutex-guarded map
		// lookup per request instead.
		if !m.aliasShadowWarned(id) {
			if _, discoveredCollision := m.bareWinner(id); discoveredCollision {
				m.warnAliasShadowsDiscoveredOnce(id)
			}
		}
		return m.resolveAliasTarget(id, target, grp)
	}
	if providerName, rest, ok := m.splitConfiguredProvider(id); ok {
		return m.resolveAgainst(providerName, rest, id, "", grp, errModelUnknown)
	}
	providerName, ok := m.bareWinner(id)
	if !ok {
		return nil, "", "", errModelUnknown
	}
	return m.resolveAgainst(providerName, id, id, "", grp, errModelUnknown)
}

// aliasTargetError is resolveAgainst's notFoundErr when the provider it
// was called against, and the upstream model id on it, both came from
// resolving an alias's TARGET (resolveAliasTarget) rather than a direct
// client request — spec §5's "Targets referencing not-yet-discovered
// models are allowed at construction and resolve lazily; an unresolvable
// target at request time → 404 whose message names the alias AND the
// missing target". writeModelResolveError (routes_unified.go) type-
// asserts for *aliasTargetError explicitly, ahead of its
// errors.Is(errModelUnknown) switch — a plain type assertion, not
// errors.As, matching this package's established yaegi-safe convention
// for a pointer error type (providers.go's providerHTTPError doc
// comment: errors.As panics under yaegi checking whether an interpreted
// pointer type implements error).
type aliasTargetError struct {
	alias, target string
}

// Error names both the alias and its unresolved target, per spec §5.
func (e *aliasTargetError) Error() string {
	return fmt.Sprintf("llmgateway: alias %q targets %q, which is not a known model", e.alias, e.target)
}

// resolveAliasTarget finishes resolve for id already known to be an exact
// alias match: target is resolved through the SAME rules as any other id
// — provider-prefixed direct, or bare-id collision winner — via the same
// resolveAgainst helper, with two differences from a direct request:
//
//   - Authorization (spec §5): "a group may use an alias when its model
//     globs match the ALIAS name OR the resolved target (either
//     grants)". alias is passed to resolveAgainst as its extraModelName,
//     so a request denied by every target-side candidate can still
//     succeed purely because the group's model glob names the alias
//     itself.
//   - A target that does not resolve to any known model at request time
//     returns *aliasTargetError (naming both ids), not the bare
//     errModelUnknown a direct request's unknown-model case returns.
func (m *modelRegistry) resolveAliasTarget(alias, target string, grp *group) (providerAdapter, string, string, error) {
	notFound := &aliasTargetError{alias: alias, target: target}
	if providerName, rest, ok := m.splitConfiguredProvider(target); ok {
		return m.resolveAgainst(providerName, rest, target, alias, grp, notFound)
	}
	providerName, ok := m.bareWinner(target)
	if !ok {
		return nil, "", "", notFound
	}
	return m.resolveAgainst(providerName, target, target, alias, grp, notFound)
}

// aliasShadowWarnKey builds the m.warned key for alias's discovered-
// collision warning — shared by aliasShadowWarned and
// warnAliasShadowsDiscoveredOnce so the two can never drift apart into
// mismatched keys. Prefixed with "alias-shadow:" so it can never collide
// with warnCollisionOnce's own plain-id keys in the same m.warned map.
func aliasShadowWarnKey(alias string) string {
	return "alias-shadow:" + alias
}

// aliasShadowWarned reports whether warnAliasShadowsDiscoveredOnce has
// already logged for alias, with no other side effect — resolve's fast
// path (its own doc comment) checks this BEFORE paying bareWinner's
// O(providerNames) scan, so a registry with many providers does not pay
// that cost on every request for an alias once its one-time warning has
// already fired.
func (m *modelRegistry) aliasShadowWarned(alias string) bool {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	return m.warned[aliasShadowWarnKey(alias)]
}

// warnAliasShadowsDiscoveredOnce logs, at most once per colliding alias id
// for this registry's lifetime, that alias also names a model some
// provider's discovery fetch found after construction — resolve's
// documented precedence (its own doc comment above): the alias always
// wins, since only an EXPLICIT collision is rejected at construction
// (validateModelAliases); discovery is dynamic and runs after that check,
// so this collision can only be caught, and only warned about, here at
// request time.
func (m *modelRegistry) warnAliasShadowsDiscoveredOnce(alias string) {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	key := aliasShadowWarnKey(alias)
	if m.warned[key] {
		return
	}
	m.warned[key] = true
	m.log("%s", fmt.Sprintf("model registry: alias %q also names a discovered model; the alias takes precedence", alias))
}

// resolveAgainst finishes resolve for a providerName already chosen (either
// the explicit prefix or the bare-id collision winner): it checks
// upstreamModel is actually known to that provider, then checks
// authorization, in that order — so a real model behind a provider the
// group cannot use reports errModelDenied, not errModelUnknown.
//
// allowsModel is an exact glob match (auth.go) with no prefix-stripping of
// its own, so resolveAgainst generates both candidate strings itself:
// requestedID (the provider-prefixed form for a direct request, e.g.
// "openai/gpt-test") and upstreamModel (its bare suffix, e.g. "gpt-test"),
// and allows if either matches — that is what lets a pattern like "gpt-*"
// reach a provider-prefixed request. For a bare-id request the caller
// passes the same string as both arguments, so the two checks collapse to
// one: no bare-suffix candidate is invented for an id that was never
// legitimately provider-prefixed in the first place (ruling: a pattern like
// "deepseek-*" must not match a bare id that merely contains a slash, e.g.
// "uni/deepseek-v4-flash-0731", when "uni" is not a configured provider).
//
// extraModelName is a further authorization candidate checked via
// grp.allowsModel, alongside requestedID and upstreamModel: empty ("") for
// a direct (non-alias) resolution, or an alias's own id when resolveAliasTarget
// (spec §5, v0.2) is resolving that alias's target — "a group may use an
// alias when its model globs match the ALIAS name OR the resolved target
// (either grants)".
//
// notFoundErr is returned, unmodified, when upstreamModel is not known to
// providerName: resolve's own two call sites pass the bare errModelUnknown
// sentinel (unchanged from before this parameter existed — checked by "err
// != errModelUnknown" equality in registry_test.go, not errors.Is, so it
// must stay the exact sentinel value there); resolveAliasTarget passes a
// *aliasTargetError naming both the alias and its unresolved target (spec
// §5's 404 message requirement).
func (m *modelRegistry) resolveAgainst(providerName, upstreamModel, requestedID, extraModelName string, grp *group, notFoundErr error) (providerAdapter, string, string, error) {
	if !m.states[providerName].hasModel(upstreamModel) {
		return nil, "", "", notFoundErr
	}
	allowed := grp.allowsModel(requestedID) || grp.allowsModel(upstreamModel)
	if extraModelName != "" {
		allowed = allowed || grp.allowsModel(extraModelName)
	}
	if !allowed || !grp.allowsProvider(providerName) {
		return nil, "", "", errModelDenied
	}
	// canonical is assigned to a local before the return, not inlined into
	// it: yaegi v0.16.1 panics ("reflect.Set: value of type
	// interp.valueInterface is not assignable to type string") building a
	// multi-value return tuple when one element is an inline string
	// concatenation of two parameters. The identical expression assigned
	// to a variable first, then returned, does not trigger it. Verified
	// empirically against yaegi v0.16.1 running this exact function under
	// real Traefik (Task 15's integration suite) — tools/yaegi-check never
	// exercises this call path (it only drives GET /v1/models), which is
	// why the existing yaegi gate never caught it.
	canonical := providerName + "/" + upstreamModel
	return m.adapters[providerName], upstreamModel, canonical, nil
}

// warnCollisionOnce logs, at most once per colliding bare id for this
// registry's lifetime, that provs (sorted) all provide id and winner owns
// its bare form.
func (m *modelRegistry) warnCollisionOnce(id, winner string, provs []string) {
	m.warnedMu.Lock()
	defer m.warnedMu.Unlock()
	if m.warned[id] {
		return
	}
	m.warned[id] = true
	m.log("%s", fmt.Sprintf("model registry: model id %q is provided by multiple providers %v; %q wins the bare id", id, provs, winner))
}

// listFor returns grp's visible model catalog as OpenAI-compatible model
// objects ({"id","object":"model","owned_by"}), sorted by id. A bare id
// owned by only one provider is listed once, bare. A bare id owned by more
// than one provider (ruling (g)/(h)) is listed once bare — under its
// collision winner — plus once more per owning provider in "provider/id"
// form, so every provider's copy stays reachable through explicit
// addressing even when it lost the bare-id collision. Each listed entry is
// independently filtered by grp.allowsModel and grp.allowsProvider for the
// provider it names.
func (m *modelRegistry) listFor(grp *group) []map[string]any {
	owners := make(map[string][]string)
	for _, name := range m.providerNames {
		for _, id := range m.states[name].knownIDs() {
			owners[id] = append(owners[id], name) // m.providerNames is sorted, so owners[id] accumulates in sorted order
		}
	}

	out := make([]map[string]any, 0, len(owners))
	// listed tracks every id actually APPENDED to out below — not merely
	// present in the raw, authorization-blind owners map — so the alias
	// dedupe loop further down only skips an alias whose real-model entry
	// this group can actually see (review fix, second pass): keying the
	// dedupe on owners itself made a group-denied collision vanish the id
	// from the listing entirely (neither the real entry nor the alias
	// appeared), even though resolve still serves it through the alias.
	listed := make(map[string]bool, len(owners))
	for id, provs := range owners {
		winner := provs[0]
		if grp.allowsModel(id) && grp.allowsProvider(winner) {
			out = append(out, modelObject(id, winner, m.resolveMetaFor(winner, id)))
			listed[id] = true
		}
		if len(provs) < 2 {
			continue
		}
		m.warnCollisionOnce(id, winner, provs)
		for _, p := range provs {
			pid := p + "/" + id
			// allowsModel does no prefix-stripping (auth.go): check both the
			// prefixed form and its bare suffix, same as resolveAgainst does
			// for the equivalent client request.
			if (grp.allowsModel(pid) || grp.allowsModel(id)) && grp.allowsProvider(p) {
				out = append(out, modelObject(pid, p, m.resolveMetaFor(p, id)))
				listed[pid] = true
			}
		}
	}

	// Aliases (spec §5, v0.2): listed alongside real models only when
	// both their target actually resolves right now (a target awaiting
	// its provider's first discovery fetch is not listed — there is
	// nothing yet to name as owned_by) and grp is authorized for it.
	// resolveAliasTarget applies the exact same "alias name OR target"
	// authorization rule request-time resolve uses (its own doc
	// comment), so this listing can never promise access resolve would
	// then deny. owned_by is recovered from the successful call's own
	// canonical return value ("provider/upstreamModel") by splitting on
	// the first "/" — exact, because a registry provider name can never
	// itself contain one (configNamePattern, providers.go).
	//
	// An alias id that also happens to collide with a since-discovered
	// model's own bare id (impossible for an EXPLICIT model —
	// validateModelAliases already rejects that at construction) is
	// SKIPPED here (review fix) only when that real model's own entry was
	// actually EMITTED above (listed[alias], not merely present in
	// owners) — otherwise adding it a second time would list it twice,
	// duplicate, order-nondeterministic-looking entries in an
	// OpenAI-shaped model list. resolve's own precedence (its doc
	// comment) still has the alias win at REQUEST time regardless of
	// which entry the listing shows; only the listing itself is deduped.
	for alias, target := range m.aliases {
		if listed[alias] {
			continue
		}
		_, _, canonical, err := m.resolveAliasTarget(alias, target, grp)
		if err != nil {
			continue
		}
		providerName, bareTarget, _ := strings.Cut(canonical, "/")
		out = append(out, modelObject(alias, providerName, m.resolveMetaForAlias(alias, providerName, bareTarget)))
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string) //nolint:forcetypeassert // modelObject always sets id to a string
	})
	return out
}

// modelObject builds one OpenAI-compatible model list entry, extended
// with two OpenAI-compat-safe extension fields (feature v0.23):
// "context_window" (int) and "pricing" ({"input_per_mtok_usd",
// "output_per_mtok_usd"} floats, USD per million tokens) — both omitted
// entirely when meta reports them unknown, never emitted as a
// misleading zero. "pricing" IS emitted with zeros for an explicitly
// free model (meta.CostKnown true, both cost fields 0) — that is a
// known, meaningful zero, not an absent one.
func modelObject(id, ownedBy string, meta resolvedModelMeta) map[string]any {
	obj := map[string]any{"id": id, "object": "model", "owned_by": ownedBy}
	if meta.ContextKnown {
		obj["context_window"] = meta.ContextTokens
	}
	if meta.CostKnown {
		obj["pricing"] = map[string]any{
			"input_per_mtok_usd":  microUSDPerMTokToUSD(meta.InputCostPerMTokMicroUSD),
			"output_per_mtok_usd": microUSDPerMTokToUSD(meta.OutputCostPerMTokMicroUSD),
		}
	}
	return obj
}

// resolveMetaFor resolves (provider, bareModel)'s metadata (feature
// v0.23), supplying this registry's config-override map and that
// provider's own discovery-captured context for bareModel.
func (m *modelRegistry) resolveMetaFor(provider, bareModel string) resolvedModelMeta {
	var ctxVal int
	var ctxKnown bool
	if st, ok := m.states[provider]; ok {
		ctxVal, ctxKnown = st.contextFor(bareModel)
	}
	return resolveModelMeta(provider, bareModel, m.modelMeta, ctxVal, ctxKnown)
}

// resolveMetaForAliasName resolves alias's metadata for the admin
// dashboard's unrestricted view (feature v0.23, hover-detail
// refinement): unlike listFor's request-authorized alias handling, this
// ignores group authorization entirely (an internal, unrestricted
// &group{} — GroupConfig's own documented "empty means all" semantics,
// llmgateway.go), matching buildAdminOverview's existing behavior of
// showing every configured provider/alias unconditionally, with no
// group filtering anywhere else in its response either. Returns an
// entirely-unknown resolvedModelMeta when alias is not a configured
// alias at all, or its target does not resolve to any known model yet.
func (m *modelRegistry) resolveMetaForAliasName(alias string) resolvedModelMeta {
	target, ok := m.aliases[alias]
	if !ok {
		return resolvedModelMeta{}
	}
	_, _, canonical, err := m.resolveAliasTarget(alias, target, &group{})
	if err != nil {
		return resolvedModelMeta{}
	}
	providerName, bareTarget, _ := strings.Cut(canonical, "/")
	return m.resolveMetaForAlias(alias, providerName, bareTarget)
}

// resolveMetaForAlias resolves alias's metadata (feature v0.23) via
// resolveAliasModelMeta: target-inherited unless the alias itself has
// its own modelMeta entry — see that function's own doc comment.
func (m *modelRegistry) resolveMetaForAlias(alias, targetProvider, targetModel string) resolvedModelMeta {
	var ctxVal int
	var ctxKnown bool
	if st, ok := m.states[targetProvider]; ok {
		ctxVal, ctxKnown = st.contextFor(targetModel)
	}
	return resolveAliasModelMeta(alias, targetProvider, targetModel, m.modelMeta, ctxVal, ctxKnown)
}

// providerSnapshot is one provider's read-only view for the admin
// dashboard (spec §4, v0.2). baseURL is not secret (the spec's NEVER-
// exposed list is API keys, digests, redis password, and users-file path
// contents only) — it is included so an operator can see which upstream
// a provider actually targets. models — the sorted explicit∪discovered
// id set (provider-model-accordion task) — is likewise not secret: a
// model id names no credential, and this same set is already public
// through /v1/models (listFor) to any authenticated caller the group
// allows.
type providerSnapshot struct {
	lastRefresh time.Time
	name        string
	typeName    string
	baseURL     string
	lastErr     string
	models      []string
	modelCount  int
	// discoveryEnabled mirrors providerState.discoveryEnabled (Feature B,
	// v0.22 last-refresh label honesty): read directly rather than through
	// providerState.snapshot(), because it is set once at construction
	// (newModelRegistry, from ProviderConfig.Discovery) and never
	// reassigned afterward — every other providerState field warmFill/
	// maybeRefresh/finishRefresh mutate only reads it, never writes it — so
	// it needs neither the mutex snapshot() takes for the fields that DO
	// mutate
	// (models/lastRefresh/lastErr) nor a place in that method's return
	// shape. The admin dashboard needs it to tell "discovery is off" apart
	// from "discovery is on but has not refreshed yet" — both currently
	// read identically as a zero LastRefresh, which GET /admin/api/overview
	// (admin.go) and the Providers tab (webui) previously rendered as the
	// misleading "refreshed never" for a provider that will never refresh
	// by design.
	discoveryEnabled bool
}

// aliasSnapshotEntry is one configured alias's read-only view for the
// admin dashboard (spec §5, v0.2): the pair exactly as an operator wrote
// it. No secrets involved. The target's actual resolved provider is
// deliberately not included — a target still awaiting its provider's
// first discovery fetch has none yet, and the configured pair is the
// meaningful, stable fact an operator wants to see either way.
type aliasSnapshotEntry struct {
	Alias  string
	Target string
}

// aliasSnapshot returns every configured alias's alias->target pair,
// sorted by alias — the admin dashboard's alias table (spec §5, v0.2).
func (m *modelRegistry) aliasSnapshot() []aliasSnapshotEntry {
	names := make([]string, 0, len(m.aliases))
	for alias := range m.aliases {
		names = append(names, alias)
	}
	sort.Strings(names)

	out := make([]aliasSnapshotEntry, len(names))
	for i, alias := range names {
		out[i] = aliasSnapshotEntry{Alias: alias, Target: m.aliases[alias]}
	}
	return out
}

// snapshot returns every configured provider's read-only view, sorted by
// name (m.providerNames is already sorted at construction) — the admin
// dashboard's provider table (spec §4, v0.2).
func (m *modelRegistry) snapshot() []providerSnapshot {
	out := make([]providerSnapshot, 0, len(m.providerNames))
	for _, name := range m.providerNames {
		adapter := m.adapters[name]
		// Three-value multi-assign straight from the call, no intermediate
		// named variables reused across a loop iteration boundary — Yaegi
		// has had multi-assign edge cases around reused/shadowed loop
		// variables in other parts of this codebase's history; this shape
		// (fresh locals every iteration, assigned once, read once) avoids
		// that class of trap entirely.
		models, lastRefresh, lastErr := m.states[name].snapshot()
		out = append(out, providerSnapshot{
			name:             adapter.name(),
			typeName:         adapter.typeName(),
			baseURL:          adapter.base(),
			models:           models,
			modelCount:       len(models),
			lastRefresh:      lastRefresh,
			lastErr:          lastErr,
			discoveryEnabled: m.states[name].discoveryEnabled,
		})
	}
	return out
}
