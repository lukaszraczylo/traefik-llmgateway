package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	lastRefresh      time.Time
	explicit         map[string]bool
	discovered       map[string]bool
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

// knownIDs returns the sorted union of this provider's explicit and
// discovered model ids.
func (st *providerState) knownIDs() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
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
		return
	}
	discovered := make(map[string]bool, len(ids))
	for _, id := range ids {
		discovered[id] = true
	}
	st.discovered = discovered
}

// modelRegistry aggregates every configured provider's explicit and
// discovered model ids into the gateway's model-resolution and /v1/models
// listing surfaces. adapters, providerNames, and states are all fixed at
// construction; only providerState's own fields mutate afterward (each
// guarded by its own mutex), so modelRegistry itself needs no lock beyond
// warnedMu for the collision-log dedup below.
type modelRegistry struct {
	adapters      map[string]providerAdapter
	states        map[string]*providerState
	log           func(string, ...any)
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
		}

		m.states[name] = &providerState{
			explicit:         explicit,
			discovered:       make(map[string]bool),
			discoveryEnabled: pc.Discovery,
			interval:         interval,
		}
	}
	return m, nil
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
			m.log("model registry: initial discovery for provider %q failed: %v", name, err)
		}
	}
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
			m.log("model registry: discovery refresh for provider %q failed: %v", name, err)
		}
	}()

	fctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
	defer cancel()
	ids, err = adapter.listModels(fctx)
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
// id in "provider/model" form (ruling (a): only when "provider" names a
// configured provider) resolves directly against that provider. Any other
// id is a bare id, resolved against the first configured provider (sorted
// name order) whose known model set contains it — ruling (g)'s collision
// rule. Either way, authorization requires both grp.allowsModel and
// grp.allowsProvider for the resolved provider (ruling (c)); a model that
// exists but fails authorization returns errModelDenied, distinct from
// errModelUnknown for a model no configured provider knows at all.
func (m *modelRegistry) resolve(id string, grp *group) (providerAdapter, string, string, error) {
	if providerName, rest, ok := m.splitConfiguredProvider(id); ok {
		return m.resolveAgainst(providerName, rest, id, grp)
	}
	providerName, ok := m.bareWinner(id)
	if !ok {
		return nil, "", "", errModelUnknown
	}
	return m.resolveAgainst(providerName, id, id, grp)
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
func (m *modelRegistry) resolveAgainst(providerName, upstreamModel, requestedID string, grp *group) (providerAdapter, string, string, error) {
	if !m.states[providerName].hasModel(upstreamModel) {
		return nil, "", "", errModelUnknown
	}
	allowed := grp.allowsModel(requestedID) || grp.allowsModel(upstreamModel)
	if !allowed || !grp.allowsProvider(providerName) {
		return nil, "", "", errModelDenied
	}
	return m.adapters[providerName], upstreamModel, providerName + "/" + upstreamModel, nil
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
	m.log("model registry: model id %q is provided by multiple providers %v; %q wins the bare id", id, provs, winner)
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
	for id, provs := range owners {
		winner := provs[0]
		if grp.allowsModel(id) && grp.allowsProvider(winner) {
			out = append(out, modelObject(id, winner))
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
				out = append(out, modelObject(pid, p))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string) //nolint:forcetypeassert // modelObject always sets id to a string
	})
	return out
}

// modelObject builds one OpenAI-compatible model list entry.
func modelObject(id, ownedBy string) map[string]any {
	return map[string]any{"id": id, "object": "model", "owned_by": ownedBy}
}
