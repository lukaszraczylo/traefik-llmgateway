package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The admin dashboard's routes (spec §4, v0.2; adminUsageHistoryPath added
// by the v0.2 data-layer task; adminAssetsPathPrefix added by the Vue
// admin-panel task; adminTargetsPath added by Feature B, v0.21), matched
// only when adminEnabled(g.cfg) — see llmgateway.go's ServeHTTP dispatch.
// adminAssetsPathPrefix is a prefix, not one fixed path: every hashed
// filename the Vite build emits (admin_assets_gen.go) is served under it.
const (
	adminPagePath         = "/admin"
	adminAssetsPathPrefix = "/admin/assets/"
	adminOverviewPath     = "/admin/api/overview"
	adminUsagePath        = "/admin/api/usage"
	adminUsageHistoryPath = "/admin/api/usage/history"
	adminTargetsPath      = "/admin/api/targets"
)

// adminCSP is the Content-Security-Policy header served with GET /admin,
// every GET /admin/assets/{hashedname} response (serveAdminAsset), and
// every /admin/api/* JSON route — every /admin* response this
// gateway ever sends (spec §4, v0.2; extended to the JSON routes by a
// folded review item, 2026-08-20 review; extended to the asset route by
// a later review sweep — see serveAdminAsset's own doc comment for why).
// It was
// widened with `script-src 'unsafe-inline'`/`style-src 'unsafe-inline'`
// for the original vanilla-JS single-file page; the baked Vue build (Vue
// admin-panel task) needs neither: `vite build` emits index.html with
// only an external `<script type="module" src="/admin/assets/...">` and
// an external `<link rel="stylesheet">` — no inline script or style tag
// of any kind (vite.config.ts's `build.modulePreload.polyfill: false`
// specifically removes the one inline bootstrap Vite would otherwise add
// for legacy-browser module-preload support). `script-src`/`style-src`
// therefore tighten to 'self', matching every hashed asset's own origin.
// `img-src` adds `data:` for index.html's inlined favicon (a `data:` URI,
// not a separate asset route — see webui/index.html's own comment) —
// the only image this page ever loads. connect-src stays 'self': the
// panel's own fetch calls to /admin/api/* are the only network activity
// it ever performs. frame-ancestors/base-uri/form-action stay 'none'
// (v0.2 final review wave, 2026-08-20): the page carries a password-type
// key-entry input, so it must never be embeddable in another site's frame
// (clickjacking), never have its <base> href hijacked to retarget a
// relative script/fetch URL, and its auth form's own submit handler
// already intercepts and cancels the real submit, so form-action has
// nothing legitimate to allow.
const adminCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// adminEnabled reports whether cfg's Admin block is present and enabled —
// the single gate ServeHTTP checks before matching any /admin* route at
// all. A nil or disabled block means those paths are simply not
// registered, so they fall through to ServeHTTP's existing 404/
// passthroughUnknown handling, exactly like any other unrecognized path.
func adminEnabled(cfg *Config) bool {
	return cfg.Admin != nil && cfg.Admin.Enabled
}

// isAdminPath reports whether path is one of the admin dashboard's
// routes: the page, any hashed asset under adminAssetsPathPrefix, or one
// of the /admin/api/* JSON routes.
func isAdminPath(path string) bool {
	if path == adminPagePath || path == adminOverviewPath || path == adminUsagePath ||
		path == adminUsageHistoryPath || path == adminTargetsPath {
		return true
	}
	return strings.HasPrefix(path, adminAssetsPathPrefix)
}

// handleAdmin is ServeHTTP's single entry point for every /admin* route
// (spec §4, v0.2), called only when adminEnabled — ServeHTTP's dispatch
// already checked that.
//
// GET /admin itself serves the HTML shell with no authentication at all
// (controller-approved amendment to spec §4, 2026-08-20 review): a
// browser navigating straight to a URL cannot attach a custom
// Authorization or x-api-key header, so the original "401 when
// unauthenticated" gate made the dashboard unreachable from a browser in
// the first place. The shell carries zero data — every value is fetched
// client-side from /admin/api/*, which stay fully gated below — so there
// is nothing to protect by gating the page itself. Every hashed asset
// under adminAssetsPathPrefix is unauthenticated for the same reason: the
// browser must load the page's own script/style before it can ever send
// an x-api-key header, and the built JS/CSS is not sensitive (it is
// exactly what `vite build` emits from webui/, publicly inspectable by
// any admin dashboard user already). adminEnabled(g.cfg) still governs
// whether any /admin* path is reachable at all (disabled falls through to
// 404, unchanged).
func (g *Gateway) handleAdmin(sw *statusTrackingWriter, r *http.Request) {
	switch {
	case r.URL.Path == adminPagePath:
		g.serveAdminPage(sw)
	case strings.HasPrefix(r.URL.Path, adminAssetsPathPrefix):
		g.serveAdminAsset(sw, strings.TrimPrefix(r.URL.Path, adminAssetsPathPrefix))
	default:
		g.handleAdminAPI(sw, r)
	}
}

// handleAdminAPI is the gate for the /admin/api/* JSON routes, applying
// spec §4's gate order: unauthenticated → 401, authenticated non-admin →
// 403, admin → serve.
//
// None of these routes call checkAndCount (operator directive: progress
// ledger, 2026-08-20 — "admin requests must NOT touch req/min, req/day
// statistics"): admin traffic must never appear in usage statistics,
// which exist to measure real LLM traffic only. GET /admin/api/usage/
// history was built this way from the start (v0.2 data-layer task); this
// change extends the same treatment to GET /admin/api/overview and GET
// /admin/api/usage, which previously counted like any other authenticated
// route (spec §4 amended accordingly); GET /admin/api/targets (Feature B,
// v0.21) is built the same way from the start too. An admin's own req/min
// or req/day limit, if configured, is therefore never enforced against
// admin-route traffic either — the accepted trade-off the operator
// directive names: these are admin-gated, cheap reads, and an admin
// holder polling the dashboard aggressively is a self-inflicted, not a
// shared, resource cost.
func (g *Gateway) handleAdminAPI(sw *statusTrackingWriter, r *http.Request) {
	u, _, ok := g.auth.identify(r)
	g.logAuthEvent(ok, authEventUserName(u), r)
	if !ok {
		writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return
	}
	if !u.admin {
		writeOAIError(sw, http.StatusForbidden, "invalid_request_error", "admin access required")
		return
	}

	switch r.URL.Path {
	case adminOverviewPath:
		g.serveAdminOverview(sw)
	case adminUsagePath:
		g.serveAdminUsage(sw)
	case adminUsageHistoryPath:
		g.serveAdminUsageHistory(sw, r)
	case adminTargetsPath:
		g.serveAdminTargets(sw)
	}
}

// serveAdminPage writes the built Vue app's index.html
// (admin_assets_gen.go's adminIndexHTML, generated by webui/generate.mjs)
// with its Content-Security-Policy header. Unauthenticated by design —
// see handleAdmin's doc comment.
func (g *Gateway) serveAdminPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", adminCSP)
	_, _ = w.Write(adminIndexHTML)
}

// serveAdminAsset writes one hashed static asset from adminAssets
// (admin_assets_gen.go, generated by webui/generate.mjs) — name is the
// path with adminAssetsPathPrefix already stripped (handleAdmin). An
// unrecognized name (a stale bookmark, a probe) is a 404 in the same
// OpenAI-shaped error envelope every other unknown route on this gateway
// returns, not a bare empty body.
//
// Cache-Control is long-lived and `immutable`: every filename is content-
// hashed by the Vite build, so a given name's bytes never change — a
// config change or plugin upgrade that alters the built output always
// produces new filenames, never mutates one this browser may have cached.
// X-Content-Type-Options: nosniff matches the JSON routes' own header
// (setAdminJSONHeaders) — the declared Content-Type must never be
// second-guessed by the browser's MIME sniffer. Content-Security-Policy
// carries adminCSP too (review sweep): a browser only enforces CSP
// against the top-level document that names it, so this header governs
// nothing for a JS/CSS response fetched as a sub-resource of GET
// /admin — but a browser navigated STRAIGHT to an asset URL (a pasted
// link, a bookmark) treats that response as its own top-level document,
// and every other admin route already sends the identical header, so
// there is no reason for this one route to be the exception.
func (g *Gateway) serveAdminAsset(w http.ResponseWriter, name string) {
	asset, ok := adminAssets[name]
	if !ok {
		writeOAIError(w, http.StatusNotFound, "invalid_request_error", "unknown admin asset")
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", adminCSP)
	_, _ = w.Write(asset.body)
}

// setAdminJSONHeaders applies the response headers shared by both admin
// JSON endpoints (folded review item, 2026-08-20 review): the same CSP
// as the HTML page, X-Content-Type-Options: nosniff (a browser must
// never guess this response is anything other than the JSON it is
// declared as), and Cache-Control: no-store (every response is built
// fresh per request from live state and must never linger in a shared
// or disk cache).
func setAdminJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", adminCSP)
}

// sanitizeBaseURL strips a provider baseUrl's userinfo (username and
// password) and query string before admin.go ever echoes it (folded
// review item, 2026-08-20 review): an operator who embeds credentials in
// a baseUrl — "https://token@host" or "https://host?api-key=..." are
// both misconfigurations, but ones this dashboard must never amplify
// into a credential leak — must never see them reflected back. This
// strips the userinfo entirely rather than only the password the way
// net/url's own URL.Redacted() does: Redacted() would still leave a bare
// embedded username in place, and a bare username with no separate
// password is itself commonly how a token gets embedded in a URL. A raw
// value that fails to parse as a URL is returned unchanged — it names no
// scheme/host/userinfo net/url can identify, so there is nothing
// structured left to strip.
func sanitizeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// sanitizeProviderErr applies sanitizeBaseURL's userinfo/query-stripping
// treatment to lastErr before it is ever echoed by GET /admin/api/overview
// (v0.2 final review wave, 2026-08-20): a provider's discovery/refresh
// error commonly embeds the request URL it failed against (Go's
// net/http wraps a *url.Error carrying it verbatim), so a baseUrl
// misconfigured with embedded credentials would otherwise leak straight
// back out through this field, even though adminProviderView.BaseURL
// itself is already sanitized.
//
// This is best-effort, not a general URL scrubber: it looks only for
// rawBaseURL appearing verbatim inside lastErr and replaces every such
// occurrence with sanitizeBaseURL's cleaned form. An error naming some
// OTHER URL — a redirect target, a differently-formatted variant of the
// same host — is not caught; recognizing every URL shape a wrapped error
// might embed, without risking mangling ordinary error text, is not
// attempted here.
func sanitizeProviderErr(lastErr, rawBaseURL string) string {
	if lastErr == "" || rawBaseURL == "" || !strings.Contains(lastErr, rawBaseURL) {
		return lastErr
	}
	return strings.ReplaceAll(lastErr, rawBaseURL, sanitizeBaseURL(rawBaseURL))
}

// adminProviderView is one provider's read-only view in GET
// /admin/api/overview (spec §4, v0.2). baseUrl is not secret — the
// spec's NEVER-exposed list is API keys (not even digests), provider
// keys, the redis password, and users-file path contents; a provider's
// base URL names no credential by itself, and any credential-shaped
// userinfo/query an operator mistakenly embedded in it is stripped by
// sanitizeBaseURL before this view is ever built.
type adminProviderView struct {
	LastRefresh time.Time `json:"lastRefresh"`
	// OpenUntil is when this provider's breaker will next attempt a
	// half-open probe (feat/provider-health). The zero time.Time when
	// HealthState is not "open". Grouped with LastRefresh above (both
	// time.Time) for fieldalignment — see providerState's own doc
	// comment for the convention this struct follows.
	OpenUntil time.Time `json:"openUntil"`
	// ModelMeta is this provider's resolved per-model metadata (feature
	// v0.23: context window, per-token cost — resolveModelMeta,
	// modelmeta.go), keyed by upstream model id. One entry per id in
	// Models, unconditionally — never nil, mirroring ModelRates' own
	// "never nil" convention below — so the dashboard's provider
	// accordion can render a context/cost chip beside each model without
	// a second round trip. An individual entry's own fields are omitted
	// (not zeroed) when unknown; see adminModelMetaView's own doc
	// comment.
	ModelMeta map[string]adminModelMetaView `json:"modelMeta"`
	// ModelRates is this provider's per-model breakdown of the Attempts*/
	// Failures* counters below, keyed by upstream model id (Feature A,
	// v0.22) — every entry in Models gets one, even a model with zero
	// attempts, so the Providers tab's provider accordion can decide
	// "degraded, show a badge" per model without a second round trip.
	// Never nil, mirroring Models' own "never nil" convention below —
	// marshals as "{}" for a provider with no known models yet, not
	// omitted.
	ModelRates map[string]adminModelRateView `json:"modelRates"`
	// Latency is a compact per-stream ("streaming"/"non-streaming")
	// average TTFB/duration summary for this provider (feat: instrument
	// upstream latency), sourced from the SAME in-process g.latency
	// accumulator /metrics reads (metrics.go). Averages only — never a
	// full histogram or a per-model breakdown — to keep this already-
	// ~113KB, 5s-polled response from growing meaningfully; see
	// buildAdminLatencyViews' own doc comment. nil (omitted) for a
	// provider with no observations yet, or for a deployment with metrics
	// collection gated off entirely. Grouped here with the other map
	// fields above, not declared near AttemptsDay/ModelCount below,
	// because it is a pointer-shaped field (a map header) and this
	// struct's fields are ordered pointer-shaped-first, then plain
	// scalars (int64/int/bool) last — fieldalignment (govet, enabled via
	// this repo's global golangci-lint config) flags a pointer field
	// declared after the scalar block as costing extra GC pointer-scan
	// bytes. See timeout.go's watchdogBody for the identical convention.
	Latency map[string]adminLatencyView `json:"latency,omitempty"`
	Type    string                      `json:"type"`
	BaseURL string                      `json:"baseUrl"`
	LastErr string                      `json:"lastErr,omitempty"`
	// HealthState is this provider's discovery circuit breaker state
	// (feat/provider-health): "closed" (normal), "open" (backing off
	// after repeated discovery failures — maybeRefresh skips it until
	// OpenUntil above), or "half-open" (a probe refresh is in flight,
	// deciding whether to close the breaker again). A provider with
	// discovery disabled, or one that has never failed a refresh, always
	// reads "closed" — it has no health signal that could trip the
	// breaker.
	HealthState string `json:"healthState"`
	Name        string `json:"name"`
	// Models is the sorted explicit∪discovered model id set
	// (registry.go's providerSnapshot.models — provider-model-accordion
	// task, Vue admin panel): the dashboard's expandable provider row.
	// Never nil for a real provider — even zero known models marshals as
	// "[]", not omitted, so the dashboard can tell "no models yet" apart
	// from a field a stale client build does not know how to read.
	Models []string `json:"models"`
	// AttemptsDay/FailuresDay/AttemptsMinute/FailuresMinute are this
	// provider's current-window upstream-attempt counters (Feature A,
	// v0.22 — see limiter.recordProviderAttempt, limits.go): every
	// upstream HTTP attempt increments Attempts*; only a provider-fault
	// outcome (isTransient's classification, retry.go) additionally
	// increments Failures*. The Providers tab's success-rate badge is
	// (AttemptsDay-FailuresDay)/AttemptsDay; AttemptsMinute/FailuresMinute
	// exist only for the badge's "right now" title-attribute detail — no
	// day-window figure alone tells an operator whether a spike is still
	// happening. Both stay 0 for a provider with no traffic yet, which the
	// webui renders as a distinct "no traffic" state rather than a
	// (misleadingly perfect) 100% badge.
	AttemptsDay    int64 `json:"attemptsDay"`
	FailuresDay    int64 `json:"failuresDay"`
	AttemptsMinute int64 `json:"attemptsMinute"`
	FailuresMinute int64 `json:"failuresMinute"`
	ModelCount     int   `json:"modelCount"`
	// DiscoveryEnabled mirrors providerSnapshot.discoveryEnabled (Feature
	// B, v0.22): the Providers tab uses it to tell a provider whose
	// discovery is off apart from one that is on but has not completed its
	// first refresh yet — both otherwise show the same zero LastRefresh.
	// See registry.go's providerSnapshot.discoveryEnabled doc comment.
	DiscoveryEnabled bool `json:"discoveryEnabled"`
}

// adminModelRateView is one upstream model's current-window attempt/
// failure counters within its provider — adminProviderView.ModelRates'
// value type (Feature A, v0.22). Day window only, no AttemptsMinute/
// FailuresMinute (SHOULD-2, v0.22 review round — removed, not merely
// left unpopulated): a dashboard with N configured models was paying N
// wasted minute-window store reads on every 5s poll for a figure that
// only ever fed a badge's title text, never its own displayed tier — see
// limits.go's providerCounterKeys/providerScopeKeyCount for the read-side
// half of this same ruling. The Providers tab's per-model badge falls
// back to a day-window-only title when these are absent; see webui's
// lib/provider-rate.ts (dayRateTitle) and ProviderRateBadge.vue.
type adminModelRateView struct {
	AttemptsDay int64 `json:"attemptsDay"`
	FailuresDay int64 `json:"failuresDay"`
}

// adminLatencyView is one provider's one stream-state's compact latency
// summary (feat: instrument upstream latency) — adminProviderView.
// Latency's value type. Plain averages (sum/count from the same
// latencySnapshot /metrics reads, metrics.go), never a percentile: no
// interpolation-correctness question to get wrong, and "compact" (the
// task brief's own word for this admin surface) rules out shipping a
// full bucket set here anyway — that detail belongs on /metrics.
// AvgTTFBMs is 0 (omitted) when no successful read was ever observed
// for this stream state (latencySnapshot.ttfbCount == 0) — never
// fabricated from AvgDurationMs, which is a materially different
// number for a genuine stream (see llmgateway_upstream_ttfb_seconds'
// own HELP text, metrics.go, for why the two must never be conflated).
type adminLatencyView struct {
	AvgTTFBMs     float64 `json:"avgTtfbMs,omitempty"`
	AvgDurationMs float64 `json:"avgDurationMs,omitempty"`
	Count         int64   `json:"count,omitempty"`
}

// buildAdminLatencyViews groups snaps (g.latency.snapshot(), metrics.go)
// by provider, discarding the opt-in model dimension entirely (task
// brief: "compact... do not bloat" — GET /admin/api/overview already
// sits around 113 KB and is polled every 5s; a full per-model latency
// breakdown belongs on /metrics, not here). The outer map is keyed by
// provider name, the inner by "streaming"/"non-streaming"; a provider
// with no observations at all is simply absent from the outer map, so
// adminOverviewResponse's own lookup (buildAdminOverview, below) yields
// nil for it — adminProviderView.Latency's own documented "nil means no
// observations yet".
func buildAdminLatencyViews(snaps []latencySnapshot) map[string]map[string]adminLatencyView {
	out := make(map[string]map[string]adminLatencyView)
	for _, s := range snaps {
		if s.key.model != "" || s.durationCount == 0 {
			continue
		}
		streamKey := "non-streaming"
		if s.key.streaming {
			streamKey = "streaming"
		}
		v := adminLatencyView{
			AvgDurationMs: s.durationSum / float64(s.durationCount) * 1000,
			Count:         s.durationCount,
		}
		if s.ttfbCount > 0 {
			v.AvgTTFBMs = s.ttfbSum / float64(s.ttfbCount) * 1000
		}
		if out[s.key.provider] == nil {
			out[s.key.provider] = make(map[string]adminLatencyView)
		}
		out[s.key.provider][streamKey] = v
	}
	return out
}

// adminModelMetaView is one upstream model's resolved metadata (feature
// v0.23: context window, per-token cost) within its provider —
// adminProviderView.ModelMeta's value type. Every field is a pointer,
// not a plain int/float64: a plain field's "omitempty" cannot tell a
// genuinely unknown value apart from an explicit zero (a free model's
// cost IS 0, and known), so a nil pointer (omitted from the JSON
// response) means "unknown" and a non-nil pointer — even one pointing at
// 0 — means "known", mirroring resolvedModelMeta's own ContextKnown/
// CostKnown bools (modelmeta.go) one for one.
type adminModelMetaView struct {
	ContextTokens    *int     `json:"contextTokens,omitempty"`
	InputPerMTokUSD  *float64 `json:"inputPerMTokUsd,omitempty"`
	OutputPerMTokUSD *float64 `json:"outputPerMTokUsd,omitempty"`
}

// buildAdminModelMetaView converts resolveModelMeta's result into the
// admin API's pointer-based "known vs. unknown" JSON shape (see
// adminModelMetaView's own doc comment for why plain zero values cannot
// do this).
func buildAdminModelMetaView(meta resolvedModelMeta) adminModelMetaView {
	var v adminModelMetaView
	if meta.ContextKnown {
		ctx := meta.ContextTokens
		v.ContextTokens = &ctx
	}
	if meta.CostKnown {
		in := microUSDPerMTokToUSD(meta.InputCostPerMTokMicroUSD)
		out := microUSDPerMTokToUSD(meta.OutputCostPerMTokMicroUSD)
		v.InputPerMTokUSD = &in
		v.OutputPerMTokUSD = &out
	}
	return v
}

// adminRedisView is the redis status line in GET /admin/api/overview.
// LastErrAt's json tag omits "omitempty": encoding/json never treats a
// struct value (time.Time) as "empty" regardless of its fields, so the
// tag would be a silent no-op — the same caveat Config's own Retry/Cache
// field doc comments already record (llmgateway.go). Its zero value
// ("0001-01-01T00:00:00Z") is the "no error recorded yet" sentinel,
// matching adminProviderView.LastRefresh's own convention; the dashboard
// renders it as "last error (Ns ago): ..." (folded review item,
// 2026-08-20 review) so a single transient blip does not read as
// currently-broken long after it recovered.
type adminRedisView struct {
	LastErrAt  time.Time `json:"lastErrAt"`
	LastErr    string    `json:"lastErr,omitempty"`
	Configured bool      `json:"configured"`
}

// adminCacheView is the cache config summary in GET /admin/api/overview —
// enabled/ttl only, per spec §4: no live hit counters (the spec makes no
// such promise).
type adminCacheView struct {
	TTL     string `json:"ttl,omitempty"`
	Enabled bool   `json:"enabled"`
}

// adminRetryView is the retry config summary in GET /admin/api/overview
// (v0.2 final review wave, 2026-08-20): the EFFECTIVE enabled/attempts/
// backoff a `retry: {enabled: true}` block resolves to, after
// newRetryPolicy's own zero-value defaulting (retry.go) — not the raw,
// possibly-empty RetryConfig fields, so an operator sees what actually
// governs upstream calls, not just what they left unset. Attempts and
// Backoff are both omitted (zero value / empty string) when retry is
// disabled, mirroring adminCacheView's own omitempty convention for a
// disabled feature's now-meaningless detail fields.
type adminRetryView struct {
	Backoff  string `json:"backoff,omitempty"`
	Enabled  bool   `json:"enabled"`
	Attempts int    `json:"attempts,omitempty"`
}

// adminGroupView is one group's summary in GET /admin/api/overview.
type adminGroupView struct {
	Limits      *LimitsConfig `json:"limits,omitempty"`
	Name        string        `json:"name"`
	MemberCount int           `json:"memberCount"`
}

// adminAliasView is one configured model alias's row in GET
// /admin/api/overview (spec §5, v0.2): the alias->target pair exactly as
// an operator wrote it. No secrets involved.
type adminAliasView struct {
	// ModelMeta is this alias's own resolved metadata (feature v0.23,
	// hover-detail refinement): the alias's INHERITED metadata from its
	// resolved target, unless the alias itself carries its own modelMeta
	// override (resolveMetaForAliasName, registry.go — same
	// alias-inheritance rule listFor's own alias entries use for GET
	// /v1/models). All-unknown (every field omitted) when the alias's
	// target does not resolve to any known model yet.
	ModelMeta adminModelMetaView `json:"modelMeta"`
	Alias     string             `json:"alias"`
	Target    string             `json:"target"`
}

// adminOverviewResponse is the full body of GET /admin/api/overview (spec
// §4, v0.2). Every slice is sorted and built fresh per request — no
// caching of the response itself — so the dashboard's 5s poll always
// reflects current state.
type adminOverviewResponse struct {
	Version   string              `json:"version"`
	Providers []adminProviderView `json:"providers"`
	Groups    []adminGroupView    `json:"groups"`
	Aliases   []adminAliasView    `json:"aliases"`
	Redis     adminRedisView      `json:"redis"`
	Cache     adminCacheView      `json:"cache"`
	Retry     adminRetryView      `json:"retry"`
}

// buildAdminOverview assembles adminOverviewResponse from the registry,
// limiter, cache, and authStore read hooks — nothing here mutates any of
// them.
func (g *Gateway) buildAdminOverview() adminOverviewResponse {
	snaps := g.registry.snapshot()

	// Feature A (v0.22): ONE batched limiter read for every provider's and
	// every (provider, model) pair's current attempt/failure counters —
	// scopes built here in two fixed passes (every provider, then every
	// provider's models, in snaps' own order), so providerUsage's flat
	// result slices back by plain index: the first len(snaps) entries are
	// the provider-level counters, in order; the rest are the per-model
	// counters, grouped by provider in the same order and, within a
	// provider, in s.models' own order — exactly how the loop below
	// consumes them.
	scopes := make([]limitScope, 0, len(snaps))
	for _, s := range snaps {
		scopes = append(scopes, limitScope{kind: kindProvider, id: s.name})
	}
	for _, s := range snaps {
		for _, model := range s.models {
			scopes = append(scopes, limitScope{kind: kindProviderModel, id: providerModelScopeID(s.name, model)})
		}
	}
	allCounters := g.limiter.providerUsage(scopes)
	providerLevelCounters := allCounters[:len(snaps)]
	modelCounters := allCounters[len(snaps):]

	// feat: instrument upstream latency — one g.latency.snapshot() call
	// for the whole response, mirroring providerUsage's own one-batched-
	// read-for-every-provider shape above, not a per-provider read.
	latencyViews := buildAdminLatencyViews(g.latency.snapshot())

	providers := make([]adminProviderView, len(snaps))
	mi := 0
	for i, s := range snaps {
		pc := providerLevelCounters[i]
		modelRates := make(map[string]adminModelRateView, len(s.models))
		modelMeta := make(map[string]adminModelMetaView, len(s.models))
		for _, model := range s.models {
			mc := modelCounters[mi]
			mi++
			modelRates[model] = adminModelRateView{
				AttemptsDay: mc.attemptsDay,
				FailuresDay: mc.failuresDay,
			}
			modelMeta[model] = buildAdminModelMetaView(g.registry.resolveMetaFor(s.name, model))
		}
		providers[i] = adminProviderView{
			Name:             s.name,
			Type:             s.typeName,
			BaseURL:          sanitizeBaseURL(s.baseURL),
			Models:           s.models,
			ModelCount:       s.modelCount,
			LastRefresh:      s.lastRefresh,
			LastErr:          sanitizeProviderErr(s.lastErr, s.baseURL),
			DiscoveryEnabled: s.discoveryEnabled,
			HealthState:      s.health.String(),
			OpenUntil:        s.openUntil,
			AttemptsDay:      pc.attemptsDay,
			FailuresDay:      pc.failuresDay,
			AttemptsMinute:   pc.attemptsMinute,
			FailuresMinute:   pc.failuresMinute,
			ModelRates:       modelRates,
			ModelMeta:        modelMeta,
			Latency:          latencyViews[s.name],
		}
	}

	configured, redisLastErr, redisLastErrAt := g.limiter.redisStatus()

	cacheEnabled := g.cache != nil
	var ttl string
	if cacheEnabled {
		ttl = g.cache.ttl.String()
	}

	// newRetryPolicy is pure (no I/O) and already ran once, successfully,
	// when this Gateway was constructed (providers.go's buildAdapters) —
	// g.cfg.Retry cannot have changed since a running Gateway never
	// mutates its own config, so this second call is guaranteed to return
	// the identical result with a nil error; the error return is
	// intentionally discarded rather than propagated.
	rp, _ := newRetryPolicy(g.cfg.Retry)
	retryView := adminRetryView{Enabled: rp.enabled}
	if rp.enabled {
		retryView.Attempts = rp.attempts
		retryView.Backoff = rp.backoff.String()
	}

	_, groupSummaries := g.auth.snapshot()
	groups := make([]adminGroupView, len(groupSummaries))
	for i, gs := range groupSummaries {
		groups[i] = adminGroupView{Name: gs.name, Limits: gs.limits, MemberCount: gs.memberCount}
	}

	aliasSnaps := g.registry.aliasSnapshot()
	aliases := make([]adminAliasView, len(aliasSnaps))
	for i, a := range aliasSnaps {
		aliases[i] = adminAliasView{
			Alias:     a.Alias,
			Target:    a.Target,
			ModelMeta: buildAdminModelMetaView(g.registry.resolveMetaForAliasName(a.Alias)),
		}
	}

	return adminOverviewResponse{
		Providers: providers,
		Redis:     adminRedisView{Configured: configured, LastErr: redisLastErr, LastErrAt: redisLastErrAt},
		Cache:     adminCacheView{Enabled: cacheEnabled, TTL: ttl},
		Retry:     retryView,
		Groups:    groups,
		Aliases:   aliases,
		Version:   pluginVersion,
	}
}

// serveAdminOverview writes buildAdminOverview's result as JSON.
func (g *Gateway) serveAdminOverview(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminOverview())
}

// adminUsageEntryView is one user's, group's, or the total scope's row in
// GET /admin/api/usage (spec §4, v0.2): its current-window counter
// values, with its configured limit echoed beside each. GroupName is set
// only for a user entry (empty for a group or the total entry) — the
// group a user currently belongs to, for display; it carries no
// access-control meaning of its own. TokensInPerDay/TokensOutPerDay and
// their per-month counterparts replace a single combined
// tokensPerDay/tokensPerMonth pair (v0.2 data-layer task, tokens-in/
// tokens-out split) — a client wanting the combined total sums the two
// itself.
//
// Providers/Models/MCPServers/Agents (group-access-display task) are set
// only for a group entry — a user or the total entry never populates them,
// same convention as GroupName's own "user-only" field above, mirrored.
// Each carries the group's GroupConfig list exactly as configured: omitted
// (nil) means every provider/model/MCP server/agent is allowed
// (group.allowsX's own empty-list-matches-anything contract, auth.go) —
// this is never resolved/expanded to the full catalog here, so the
// dashboard can tell "explicitly restricted to these N" apart from
// "unrestricted" without the payload growing with catalog size.
type adminUsageEntryView struct {
	Limits               *LimitsConfig `json:"limits,omitempty"`
	Kind                 string        `json:"kind"`
	ID                   string        `json:"id"`
	GroupName            string        `json:"groupName,omitempty"`
	Providers            []string      `json:"providers,omitempty"`
	Models               []string      `json:"models,omitempty"`
	MCPServers           []string      `json:"mcpServers,omitempty"`
	Agents               []string      `json:"agents,omitempty"`
	RequestsPerMinute    int64         `json:"requestsPerMinute"`
	RequestsPerDay       int64         `json:"requestsPerDay"`
	TokensInPerDay       int64         `json:"tokensInPerDay"`
	TokensOutPerDay      int64         `json:"tokensOutPerDay"`
	TokensInPerMonth     int64         `json:"tokensInPerMonth"`
	TokensOutPerMonth    int64         `json:"tokensOutPerMonth"`
	CostPerDayMicroUSD   int64         `json:"costPerDayMicroUsd"`
	CostPerMonthMicroUSD int64         `json:"costPerMonthMicroUsd"`
	StoreDown            bool          `json:"storeDown,omitempty"`
}

// adminUsageResponse is the full body of GET /admin/api/usage: every
// currently active user, and every configured group, each with its own
// usage+limits row, plus the synthetic total scope's own row (v0.2
// data-layer task) — usage summed across every user and group combined,
// limits always nil (see totalScopeKind). Users and Groups are both
// sorted (authStore.snapshot's own contract) and built fresh per request.
type adminUsageResponse struct {
	Users  []adminUsageEntryView `json:"users"`
	Groups []adminUsageEntryView `json:"groups"`
	Total  adminUsageEntryView   `json:"total"`
}

// usageEntryView converts one limiter.currentUsage result plus its
// matching limit into the JSON view.
func usageEntryView(su scopeUsage, limits *LimitsConfig) adminUsageEntryView {
	return adminUsageEntryView{
		Kind:                 su.kind,
		ID:                   su.id,
		Limits:               limits,
		RequestsPerMinute:    su.requestsPerMinute,
		RequestsPerDay:       su.requestsPerDay,
		TokensInPerDay:       su.tokensInPerDay,
		TokensOutPerDay:      su.tokensOutPerDay,
		TokensInPerMonth:     su.tokensInPerMonth,
		TokensOutPerMonth:    su.tokensOutPerMonth,
		CostPerDayMicroUSD:   su.costPerDayMicros,
		CostPerMonthMicroUSD: su.costPerMonthMicros,
		StoreDown:            su.storeDown,
	}
}

// buildAdminUsage assembles adminUsageResponse: authStore.snapshot lists
// every currently active user and every configured group (names, group
// membership, and limits only — never a key or its digest), and
// chunkedCurrentUsage (above) reads every one's current-window counters —
// plus the synthetic total scope's own (v0.2 data-layer task) — in
// batches of at most adminUsageChunkScopes scopes per storeGetMulti round
// trip (security audit finding 4, 2026-08-22, capping the v0.2 final
// review wave's own "fold everything into ONE round trip" design — see
// adminUsageChunkScopes' own doc comment for why) — users, groups, and the
// total scope are concatenated into a single scopes slice before
// chunking, then the flat result is sliced back into the three response
// sections at the same split points, order preserved. Unlike
// buildLimitScopes (routes_unified.go), this never omits an entity for
// having nil limits — the dashboard shows usage for every user and group,
// limited or not.
// adminUsageChunkScopes bounds how many scopes buildAdminUsage sends to
// limiter.currentUsage in a single storeGetMulti round trip (security
// audit finding 4, 2026-08-22). currentUsage's own doc comment (limits.go)
// already explains why every scope was folded into ONE round trip rather
// than one per scope — but "one round trip" for the WHOLE catalog means
// that round trip's own pipeline size, and how long it holds the shared,
// mutex-guarded Redis connection (respClient, resp.go), now scales with
// how many users and groups are configured. With 1,000 users that is
// ~8,000 GETs (usageKeysPerScope=8) in one pipelined call, during which
// every concurrent checkAndCount from live LLM traffic queues behind the
// same connection mutex — and the admin dashboard polls this endpoint
// every 5s. chunkedCurrentUsage (below) keeps each individual round trip's
// key count bounded, letting live traffic interleave between chunks,
// while a chunk still batches every scope inside it exactly as before,
// keeping most of currentUsage's own round-trip-collapsing win. 200
// scopes (1,600 keys per chunk) comfortably covers the live cluster's 5
// friends + 4 home users in a single chunk, so today's real deployment
// sees no behavior change at all — chunking only engages once a
// deployment's user+group count actually grows past it. One accepted,
// disclosed side effect: currentUsage's own doc comment notes a transient
// store error marks every scope in ONE call storeDown together; chunking
// narrows that blast radius to one chunk's worth of scopes instead of the
// whole response, which is a strict improvement, not a new risk.
const adminUsageChunkScopes = 200

// chunkedCurrentUsage calls limiter.currentUsage in batches of at most
// adminUsageChunkScopes scopes at a time, concatenating the results in
// the SAME order scopes was given — buildAdminUsage's own slicing of the
// flat result back into users/groups/total (below) is unaffected by, and
// unaware of, the chunking underneath. A scopes slice at or under the
// chunk size makes exactly one call, byte-for-byte what calling
// currentUsage directly would have done.
func (g *Gateway) chunkedCurrentUsage(scopes []limitScope) []scopeUsage {
	out := make([]scopeUsage, 0, len(scopes))
	for len(scopes) > 0 {
		n := adminUsageChunkScopes
		if n > len(scopes) {
			n = len(scopes)
		}
		out = append(out, g.limiter.currentUsage(scopes[:n])...)
		scopes = scopes[n:]
	}
	return out
}

func (g *Gateway) buildAdminUsage() adminUsageResponse {
	userSummaries, groupSummaries := g.auth.snapshot()

	scopes := make([]limitScope, 0, len(userSummaries)+len(groupSummaries)+1)
	for _, us := range userSummaries {
		scopes = append(scopes, limitScope{kind: "user", id: us.name, limits: us.limits})
	}
	for _, gs := range groupSummaries {
		scopes = append(scopes, limitScope{kind: "group", id: gs.name, limits: gs.limits})
	}
	scopes = append(scopes, limitScope{kind: totalScopeKind, id: totalScopeID, limits: nil})

	allUsage := g.chunkedCurrentUsage(scopes)
	userUsage := allUsage[:len(userSummaries)]
	groupUsage := allUsage[len(userSummaries) : len(userSummaries)+len(groupSummaries)]
	totalUsage := allUsage[len(userSummaries)+len(groupSummaries)]

	users := make([]adminUsageEntryView, len(userUsage))
	for i, su := range userUsage {
		users[i] = usageEntryView(su, userSummaries[i].limits)
		users[i].GroupName = userSummaries[i].groupName
	}
	groups := make([]adminUsageEntryView, len(groupUsage))
	for i, su := range groupUsage {
		groups[i] = usageEntryView(su, groupSummaries[i].limits)
		groups[i].Providers = groupSummaries[i].providers
		groups[i].Models = groupSummaries[i].models
		groups[i].MCPServers = groupSummaries[i].mcpServers
		groups[i].Agents = groupSummaries[i].agents
	}
	total := usageEntryView(totalUsage, nil)

	return adminUsageResponse{Users: users, Groups: groups, Total: total}
}

// serveAdminUsage writes buildAdminUsage's result as JSON.
func (g *Gateway) serveAdminUsage(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminUsage())
}

// historyMaxSpan caps GET /admin/api/usage/history's span query parameter
// per window (v0.2 data-layer task), matching each window's own retention
// (limits.go: hourWindowTTL/dayWindowTTL/monthWindowTTL) rounded down to
// whole buckets: 48 hourly buckets, 35 daily buckets, 13 monthly buckets
// (roughly a 4-day margin, not a full month: 13 average-length ~30.4-day
// months is ~396 days, against monthWindowTTL's 400*24h = 400 days —
// review sweep, 2026-08-21, corrects an earlier "one-month margin"
// overclaim here). A span beyond what its window's TTL could ever have
// kept alive would only echo zeros for the missing tail, so the cap keeps
// every requested bucket meaningful. window is assumed already validated
// by validHistoryWindow — the default case is a programming error, not a
// client input, mirroring windowKey/windowEnd's own panic convention.
func historyMaxSpan(window string) int {
	switch window {
	case windowHour:
		return 48
	case windowDay:
		return 35
	case windowMonth:
		return 13
	default:
		panic(fmt.Sprintf("llmgateway: historyMaxSpan: unknown window %q", window))
	}
}

// validHistoryMetric reports whether metric is one of GET
// /admin/api/usage/history's four accepted metric values.
func validHistoryMetric(metric string) bool {
	switch metric {
	case metricReq, metricTokIn, metricTokOut, metricCost:
		return true
	default:
		return false
	}
}

// validHistoryWindow reports whether window is one of GET
// /admin/api/usage/history's three accepted window values.
func validHistoryWindow(window string) bool {
	switch window {
	case windowHour, windowDay, windowMonth:
		return true
	default:
		return false
	}
}

// parseHistoryScope parses GET /admin/api/usage/history's "scope" query
// parameter: "user:{id}", "group:{id}", or the literal "total". ok is
// false for anything else, including a bare "total:{id}" form — the total
// scope carries no id component of its own, it is always totalScopeID
// (limits.go).
func parseHistoryScope(raw string) (kind, id string, ok bool) {
	if raw == totalScopeKind {
		return totalScopeKind, totalScopeID, true
	}
	k, i, found := strings.Cut(raw, ":")
	if !found || i == "" {
		return "", "", false
	}
	if k != "user" && k != "group" {
		return "", "", false
	}
	return k, i, true
}

// parseHistorySpan parses GET /admin/api/usage/history's "span" query
// parameter for window: an empty raw value defaults to window's own
// historyMaxSpan; otherwise raw must parse as an integer between 1 and
// that max inclusive. ok is false for anything else — a non-integer, a
// value below 1, or a value beyond window's max.
func parseHistorySpan(raw, window string) (span int, ok bool) {
	max := historyMaxSpan(window)
	if raw == "" {
		return max, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// scopeExists reports whether kind/id names a currently active user or a
// configured group — GET /admin/api/usage/history's 404 check for an
// unknown scope id. authStore.snapshot's own listing is the same
// membership buildAdminUsage already trusts for "every currently active
// user and every configured group"; kind is assumed already restricted to
// "user" or "group" by parseHistoryScope (the total scope's id is never
// checked against it — totalScopeID always exists).
func (g *Gateway) scopeExists(kind, id string) bool {
	users, groups := g.auth.snapshot()
	switch kind {
	case "user":
		for _, u := range users {
			if u.name == id {
				return true
			}
		}
	case "group":
		for _, gr := range groups {
			if gr.name == id {
				return true
			}
		}
	}
	return false
}

// usageHistoryPointView is one bucket in GET /admin/api/usage/history's
// points array.
type usageHistoryPointView struct {
	Bucket string `json:"bucket"`
	Value  int64  `json:"value"`
}

// usageHistoryResponse is the full body of GET /admin/api/usage/history
// (v0.2 data-layer task): scope/metric/window echoed back exactly as
// validated (scope in its raw "user:{id}"/"group:{id}"/"total" query
// form), plus span buckets oldest-first — Points[len-1] is the current,
// possibly partial, bucket.
type usageHistoryResponse struct {
	Scope  string                  `json:"scope"`
	Metric string                  `json:"metric"`
	Window string                  `json:"window"`
	Points []usageHistoryPointView `json:"points"`
}

// serveAdminUsageHistory writes GET /admin/api/usage/history's bucketed
// series for one scope/metric/window (v0.2 data-layer task, the Vue admin
// panel's chart data source): "scope"=user:{id}|group:{id}|total,
// "metric"=req|tokin|tokout|cost, "window"=hour|day|month, optional
// "span"=N (default and max per historyMaxSpan). Validates every
// parameter before touching the store — 400 for an unrecognized scope
// kind/metric/window or an out-of-range span, 404 for a user/group id
// that names no configured entity — then reads the series via
// limiter.history, ONE storeGetMulti round trip for the whole span. A
// storeDown read answers 503, not a silently-zero series: a chart built
// on this API must never mistake "the store was unreachable" for "usage
// was genuinely zero". checkAndCount is deliberately never called here —
// see handleAdminAPI's own doc comment.
func (g *Gateway) serveAdminUsageHistory(sw *statusTrackingWriter, r *http.Request) {
	q := r.URL.Query()

	rawScope := q.Get("scope")
	kind, id, ok := parseHistoryScope(rawScope)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown scope")
		return
	}
	metric := q.Get("metric")
	if !validHistoryMetric(metric) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown metric")
		return
	}
	window := q.Get("window")
	if !validHistoryWindow(window) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown window")
		return
	}
	span, ok := parseHistorySpan(q.Get("span"), window)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid span")
		return
	}
	if kind != totalScopeKind && !g.scopeExists(kind, id) {
		writeOAIError(sw, http.StatusNotFound, "invalid_request_error", "unknown "+kind+" id")
		return
	}

	points, storeOK := g.limiter.history(kind, id, metric, window, g.limiter.now(), span)
	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage history store unavailable")
		return
	}

	view := make([]usageHistoryPointView, len(points))
	for i, p := range points {
		view[i] = usageHistoryPointView{Bucket: p.bucket, Value: p.value}
	}

	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(usageHistoryResponse{Scope: rawScope, Metric: metric, Window: window, Points: view})
}

// adminTargetCountersView is one target's current-window request counters
// in GET /admin/api/targets (Feature B, v0.21) — requests only, mirroring
// limiter.targetCounters (limits.go) field for field; a target never
// accumulates tokens or cost (handleTargetProxy's own doc comment,
// mcp_a2a.go), so there is nothing else to report here.
type adminTargetCountersView struct {
	RequestsPerMinute int64 `json:"requestsPerMinute"`
	RequestsPerDay    int64 `json:"requestsPerDay"`
	RequestsPerMonth  int64 `json:"requestsPerMonth"`
}

// adminTargetView is one configured MCP server's or A2A agent's row in GET
// /admin/api/targets. Access lists the group names actually allowed to
// reach this target — computed by running the SAME matchesGlob call
// group.allowsMCP/allowsAgent themselves delegate to (auth.go) against
// each group's own cloned mcpServers/agents pattern list (groupSummary,
// authStore.snapshot), so this view can never disagree with what
// handleTargetProxy actually enforces. Access is omitted (nil) when every
// configured group can reach this target — "empty meaning all", mirroring
// groupSummary's own providers/models/mcpServers/agents omitempty
// convention (adminUsageEntryView) — rather than always listing every
// group name, which would grow with the group catalog for no reason once
// nothing is actually restricted.
type adminTargetView struct {
	Name     string                  `json:"name"`
	URL      string                  `json:"url"`
	Access   []string                `json:"access,omitempty"`
	Counters adminTargetCountersView `json:"counters"`
}

// adminTargetsResponse is the full body of GET /admin/api/targets (Feature
// B, v0.21): every configured MCP server and every configured agent, each
// with its own access list and current-window request counters. Both
// slices are sorted by name and built fresh per request, matching every
// other admin response's "no caching of the response itself" convention.
type adminTargetsResponse struct {
	MCPServers []adminTargetView `json:"mcpServers"`
	Agents     []adminTargetView `json:"agents"`
}

// adminTargetAccess computes one target's access list (adminTargetView.Access's
// own doc comment): the sorted names of every group in groupSummaries whose
// own glob pattern list — read via patternsOf, so one function serves both
// MCPServers and Agents without duplicating this loop — matches name
// (matchesGlob, auth.go: the exact function group.allowsMCP/allowsAgent
// themselves call). groupSummaries is already sorted by name
// (authStore.snapshot's own contract), so the result stays sorted too. A
// match set covering every configured group collapses to nil ("empty
// meaning all") rather than echoing the full group list back.
func adminTargetAccess(groupSummaries []groupSummary, patternsOf func(groupSummary) []string, name string) []string {
	matched := make([]string, 0, len(groupSummaries))
	for _, gs := range groupSummaries {
		if matchesGlob(patternsOf(gs), name) {
			matched = append(matched, gs.name)
		}
	}
	if len(matched) == len(groupSummaries) {
		return nil
	}
	return matched
}

// sortedMCPServerNames and sortedAgentNames return the sorted key list of
// Config.MCPServers/Config.Agents — the deterministic response order every
// admin listing in this file already promises (authStore.snapshot's own
// sorted contract), and the identical pattern handleMCPServers/
// handleAgents (mcp_a2a.go) already use for their own group-filtered
// listings. Two near-identical functions, not one generic helper, over a
// map[string]*T: this plugin runs interpreted under Yaegi, whose stdlib/
// type-parameter support does not extend to generic code the plugin
// itself defines (tools/yaegi-check; see auth.go's cloneStringSlice for
// the same constraint applied to a generic slices.Clone call instead).
func sortedMCPServerNames(m map[string]*TargetConfig) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedAgentNames(m map[string]*AgentConfig) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// targetCountersView converts one limiter.targetCounters (limits.go) into
// its JSON view. Not a plain type conversion (unlike adminAliasView's own
// aliasSnapshotEntry conversion above): targetCounters' fields are
// unexported (limiter-internal), so its field names do not match
// adminTargetCountersView's exported ones and Go's identical-underlying-
// type conversion rule does not apply here.
func targetCountersView(c targetCounters) adminTargetCountersView {
	return adminTargetCountersView{
		RequestsPerMinute: c.requestsPerMinute,
		RequestsPerDay:    c.requestsPerDay,
		RequestsPerMonth:  c.requestsPerMonth,
	}
}

// buildAdminTargets assembles adminTargetsResponse: every configured MCP
// server and agent, their access lists (adminTargetAccess), and their
// current-window request counters — ONE limiter.targetUsage round trip
// for every target combined (mirroring buildAdminUsage's own
// single-batch discipline), not one per target.
func (g *Gateway) buildAdminTargets() adminTargetsResponse {
	_, groupSummaries := g.auth.snapshot()

	mcpNames := sortedMCPServerNames(g.cfg.MCPServers)
	agentNames := sortedAgentNames(g.cfg.Agents)

	scopes := make([]limitScope, 0, len(mcpNames)+len(agentNames))
	for _, name := range mcpNames {
		scopes = append(scopes, limitScope{kind: targetKindMCP, id: name})
	}
	for _, name := range agentNames {
		scopes = append(scopes, limitScope{kind: scopeKindAgent, id: name})
	}
	counters := g.limiter.targetUsage(scopes)

	mcpServers := make([]adminTargetView, len(mcpNames))
	for i, name := range mcpNames {
		mcpServers[i] = adminTargetView{
			Name:     name,
			URL:      sanitizeBaseURL(g.cfg.MCPServers[name].URL),
			Access:   adminTargetAccess(groupSummaries, func(gs groupSummary) []string { return gs.mcpServers }, name),
			Counters: targetCountersView(counters[i]),
		}
	}
	agents := make([]adminTargetView, len(agentNames))
	for i, name := range agentNames {
		agents[i] = adminTargetView{
			Name:     name,
			URL:      sanitizeBaseURL(g.cfg.Agents[name].URL),
			Access:   adminTargetAccess(groupSummaries, func(gs groupSummary) []string { return gs.agents }, name),
			Counters: targetCountersView(counters[len(mcpNames)+i]),
		}
	}

	return adminTargetsResponse{MCPServers: mcpServers, Agents: agents}
}

// serveAdminTargets writes buildAdminTargets's result as JSON.
func (g *Gateway) serveAdminTargets(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminTargets())
}
