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
	adminUsageModelsPath  = "/admin/api/usage/models"
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
// of the /admin/api/* JSON routes. adminEventsPath (F3, v0.3 dashboard
// task) is defined in events.go, alongside the rest of that feature's own
// constants; adminUsageSeriesPath/adminUsageTotalsPath/
// adminPerformancePath (stats_read.go), adminCatalogPath/
// adminConsumersPath (admin_catalog.go), and adminConfigPath
// (admin_config.go) — admin dashboard redesign, WP-B — follow the same
// convention.
func isAdminPath(path string) bool {
	switch path {
	case adminPagePath, adminOverviewPath, adminUsagePath, adminUsageHistoryPath, adminUsageModelsPath,
		adminUsageSeriesPath, adminUsageTotalsPath, adminPerformancePath,
		adminCatalogPath, adminConsumersPath, adminConfigPath,
		adminTargetsPath, adminEventsPath:
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
	case adminUsageModelsPath:
		g.serveAdminUsageModels(sw, r)
	case adminUsageSeriesPath:
		g.serveAdminUsageSeries(sw, r)
	case adminUsageTotalsPath:
		g.serveAdminUsageTotals(sw, r)
	case adminPerformancePath:
		g.serveAdminPerformance(sw, r)
	case adminCatalogPath:
		g.serveAdminCatalog(sw)
	case adminConsumersPath:
		g.serveAdminConsumers(sw)
	case adminConfigPath:
		g.serveAdminConfig(sw)
	case adminTargetsPath:
		g.serveAdminTargets(sw)
	case adminEventsPath:
		g.serveAdminEvents(sw, r)
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
// value that fails to parse as a URL returns unparseableBaseURLPlaceholder
// instead (P5 fix, admin dashboard redesign verify round: returning raw
// unchanged here defeated this whole function's purpose the moment a
// misconfigured baseUrl failed to parse, and url.Parse fails far more
// permissively than "not a URL at all" suggests — a password containing
// an unescaped '/' reads as an invalid port on the host, and a malformed
// '%' escape anywhere in the string is also a parse error; either way
// whatever credential was embedded survived verbatim in BOTH GET
// /admin/api/config's redaction, admin_config.go, and GET /admin/api/
// overview's provider view). A value that fails to parse has nothing
// structured left to strip selectively, so the only safe answer is to
// withhold it entirely.
func sanitizeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return unparseableBaseURLPlaceholder
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// unparseableBaseURLPlaceholder is what sanitizeBaseURL (above) returns in
// place of a raw value that failed url.Parse — named so every call site
// and test refers to the identical literal instead of restating it.
const unparseableBaseURLPlaceholder = "[unparseable url]"

// sanitizeProviderErr applies sanitizeBaseURL's userinfo/query-stripping
// treatment to lastErr before it is ever echoed by GET /admin/api/overview
// (v0.2 final review wave, 2026-08-20): a provider's discovery/refresh
// error commonly embeds the request URL it failed against (Go's
// net/http wraps a *url.Error carrying it verbatim), so a baseUrl
// misconfigured with embedded credentials would otherwise leak straight
// back out through this field, even though adminProviderView.BaseURL
// itself is already sanitized.
//
// It scrubs rawBaseURL's credentials in BOTH forms a real error from a
// call against rawBaseURL can carry them (security audit run-1, finding
// F-2): the verbatim URL, and net/http's own password-masked rewrite of
// it. Go's net/http builds a *url.Error via its unexported stripPassword
// (net/http/client.go) whenever the dialed URL's userinfo reports a
// password SET — which net/url's parseAuthority makes true for ANY
// userinfo containing a colon, including the token-as-username form
// "https://TOKEN:@host" whose password is empty. stripPassword rewrites
// "user:pass@" to "user:***@" IN THE URL BEFORE the error string is ever
// built, so for such a URL the error text never contains rawBaseURL
// verbatim; a verbatim-only match then finds nothing to scrub, leaving
// the USERNAME (commonly the token itself) and any query-string
// credential riding the same URL in place.
//
// sanitizeTargetErr (target_health.go) delegates here rather than
// carrying its own copy of this logic: the private copy it used to hold
// is exactly what left this provider-side path unprotected between the
// two changes that introduced them, so the two must never diverge again.
//
// Still best-effort for URLs OTHER than rawBaseURL: an error naming a
// redirect target is not caught. Rejecting a credential-bearing baseUrl
// at construction (providers.go) is the only complete fix; this is the
// containment that changes no behavior.
func sanitizeProviderErr(lastErr, rawBaseURL string) string {
	if lastErr == "" || rawBaseURL == "" {
		return lastErr
	}
	out := replaceURLForm(lastErr, rawBaseURL)
	u, parseErr := url.Parse(rawBaseURL)
	if parseErr != nil || u.User == nil {
		return out
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		// No password set means net/http passed the URL through
		// unmasked, so the verbatim pass above already matched it.
		return out
	}
	// net/http's exact stripPassword output, mirrored call for call.
	masked := strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	return replaceURLForm(out, masked)
}

// replaceURLForm replaces every occurrence of one rendering of a URL in
// msg with sanitizeBaseURL's cleaned form of that same rendering.
func replaceURLForm(msg, form string) string {
	if form == "" || !strings.Contains(msg, form) {
		return msg
	}
	return strings.ReplaceAll(msg, form, sanitizeBaseURL(form))
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
	// Provenance summarizes non-reported usage accounting for this
	// provider (feat: expose token-accounting provenance) — keyed by
	// provenance kind, "estimated" or "unbilled" only (see
	// adminProvenanceView's own doc comment for why "reported" is never a
	// key here). Sourced from the SAME in-process g.provenance
	// accumulator /metrics reads (metrics.go). nil (omitted) for a
	// provider with no estimated/unbilled outcome yet, or for a
	// deployment with metrics collection gated off entirely — mirroring
	// Latency's identical "nil means no observations yet" convention
	// immediately above.
	Provenance map[string]adminProvenanceView `json:"provenance,omitempty"`
	Type       string                         `json:"type"`
	BaseURL    string                         `json:"baseUrl"`
	LastErr    string                         `json:"lastErr,omitempty"`
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

// adminProvenanceView is one provider's one non-reported provenance
// kind's compact usage-accounting summary (feat: expose token-accounting
// provenance) — adminProviderView.Provenance's value type, keyed by
// provenance kind ("estimated" or "unbilled" — provenanceEstimated/
// provenanceUnbilled, metrics.go) in adminProviderView.Provenance's outer
// map. "reported" is deliberately never a key here: it is the default,
// healthy case AttemptsDay/FailuresDay above already partially describe,
// and duplicating its own count would not answer any operator question
// this compact surface (buildAdminLatencyViews' own "compact, no
// per-model breakdown" ruling, GET /admin/api/overview's ~113KB/5s-poll
// budget) is not already better placed to answer — the full reported/
// estimated/unbilled breakdown, by request AND by token, lives on
// /metrics (llmgateway_usage_provenance_requests_total/-_tokens_total)
// for an operator who wants the precise fraction.
type adminProvenanceView struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

// buildAdminProvenanceViews groups snaps (g.provenance.snapshot(),
// metrics.go) by provider, discarding the "reported" provenance entirely
// (adminProvenanceView's own doc comment) — mirroring
// buildAdminLatencyViews' identical "compact, provider-keyed outer map"
// shape immediately above. A provider with no estimated/unbilled
// observations at all is simply absent from the outer map, so
// adminOverviewResponse's own lookup (buildAdminOverview, below) yields
// nil for it — adminProviderView.Provenance's own documented "nil means
// no estimated/unbilled accounting yet".
func buildAdminProvenanceViews(snaps []provenanceSnapshot) map[string]map[string]adminProvenanceView {
	out := make(map[string]map[string]adminProvenanceView)
	for _, s := range snaps {
		if s.key.provenance == provenanceReported {
			continue
		}
		if out[s.key.provider] == nil {
			out[s.key.provider] = make(map[string]adminProvenanceView)
		}
		out[s.key.provider][s.key.provenance] = adminProvenanceView{Requests: s.requests, Tokens: s.tokens}
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
	Replica   string              `json:"replica"` // F4/F9, v0.3 dashboard task: os.Hostname() -> $HOSTNAME -> "unknown" (replicaID, llmgateway.go)
	Instance  string              `json:"instance"`
	Warnings  []string            `json:"warnings"` // F10, v0.3 dashboard task: collected construction warnings, never nil
	Providers []adminProviderView `json:"providers"`
	Groups    []adminGroupView    `json:"groups"`
	Aliases   []adminAliasView    `json:"aliases"`
	Redis     adminRedisView      `json:"redis"`
	Cache     adminCacheView      `json:"cache"`
	Retry     adminRetryView      `json:"retry"`
	// Features (admin dashboard redesign, WP-B) reports which of the
	// new opt-in/always-on-with-admin statistics families this Gateway
	// currently exposes — see adminFeaturesView's own doc comment.
	Features adminFeaturesView `json:"features"`
	// Pricing summarizes the live catalog's billing-price-source mix
	// (adminPricingSummaryView's own doc comment) — a one-glance "how
	// much of my fleet is actually priced" figure for the Home page,
	// without a second round trip to GET /admin/api/catalog.
	Pricing adminPricingSummaryView `json:"pricing"`
	// WarningsDropped counts construction warnings that did not fit within
	// configWarningsCap (logger.go) — omitted (0) means nothing was dropped.
	WarningsDropped int `json:"warningsDropped,omitempty"`
}

// adminFeaturesView reports which of the admin dashboard redesign's new
// statistics families this Gateway currently exposes — GET
// /admin/api/overview, GET /admin/api/config, and every stats_read.go
// endpoint's own 404 gate all echo the SAME struct, built by the SAME
// buildAdminFeatures (below), so the webui can gate a whole page section
// on one flag without guessing at a second endpoint's own availability.
//
// UserModelStats and LatencyStats are opt-in (DECISIONS Q4: both default
// OFF), read from Config.Admin.Stats. CacheStats, LastSeen and Failover
// are "always-on-with-admin" (DECISIONS: zero/near-zero cost, no new
// round trips on the request path) — CacheStats additionally requires
// the response cache itself to be configured (nothing to report
// otherwise); LastSeen and Failover need only adminEnabled, and are
// reported true whenever this endpoint answers at all, kept as their own
// explicit flags (rather than assumed true) so a future version gate
// never has to change this struct's shape to turn one off.
type adminFeaturesView struct {
	UserModelStats bool `json:"userModelStats"`
	LatencyStats   bool `json:"latencyStats"`
	CacheStats     bool `json:"cacheStats"`
	LastSeen       bool `json:"lastSeen"`
	Failover       bool `json:"failover"`
}

// buildAdminFeatures reads Config.Admin.Stats (nil-safe: an admin block
// with no stats sub-block, or predating this feature, means both opt-in
// flags stay off — DECISIONS Q4's own defaults) and g.cache's presence to
// assemble adminFeaturesView. Shared by GET /admin/api/overview
// (buildAdminOverview, below), GET /admin/api/config (admin_config.go),
// and every stats_read.go endpoint's own feature-gated 404 check, so
// they can never disagree about which families are live.
func (g *Gateway) buildAdminFeatures() adminFeaturesView {
	var userModel, latency bool
	if g.cfg.Admin != nil && g.cfg.Admin.Stats != nil {
		userModel = g.cfg.Admin.Stats.UserModel
		latency = g.cfg.Admin.Stats.Latency
	}
	return adminFeaturesView{
		UserModelStats: userModel,
		LatencyStats:   latency,
		CacheStats:     g.cache != nil,
		LastSeen:       true,
		Failover:       true,
	}
}

// adminPricingSummaryView counts the live model catalog's rows by
// billing price source (billingPriceSource, pricing.go) — Override,
// Builtin, Litellm, Free and Unpriced sum to the catalog's total model
// count. GET /admin/api/overview's own Pricing field (buildAdminOverview,
// below); GET /admin/api/catalog's per-model PriceSource field
// (admin_catalog.go) is this same classification, unaggregated.
type adminPricingSummaryView struct {
	Override int `json:"override"`
	Builtin  int `json:"builtin"`
	Litellm  int `json:"litellm"`
	Free     int `json:"free"`
	Unpriced int `json:"unpriced"`
}

// buildAdminPricingSummary counts every catalogued (provider, model)
// pair in snaps by billingPriceSource's own result — the SAME resolution
// GET /admin/api/catalog applies per model (admin_catalog.go), summed
// here rather than read from a second registry snapshot, so the two
// views can never disagree about the fleet-wide mix even though they run
// against independently-taken snapshots.
func buildAdminPricingSummary(snaps []providerSnapshot, overrides map[string]*ModelPricing, meta map[string]*ModelMetaConfig) adminPricingSummaryView {
	var summary adminPricingSummaryView
	for _, s := range snaps {
		for _, model := range s.models {
			canonical := providerModelScopeID(s.name, model)
			switch source, _ := billingPriceSource(canonical, model, overrides, meta); source {
			case priceSourceOverride:
				summary.Override++
			case priceSourceBuiltin:
				summary.Builtin++
			case priceSourceLitellm:
				summary.Litellm++
			case priceSourceFree:
				summary.Free++
			default:
				summary.Unpriced++
			}
		}
	}
	return summary
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
	// feat: expose token-accounting provenance — one g.provenance.
	// snapshot() call for the whole response, mirroring g.latency.
	// snapshot()'s own one-batched-read-for-every-provider shape above.
	provenanceViews := buildAdminProvenanceViews(g.provenance.snapshot())

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
			Provenance:       provenanceViews[s.name],
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

	aliases := g.buildAdminAliasViews()

	warnings, warningsDropped := g.configWarningsSnapshot()
	return adminOverviewResponse{
		Providers:       providers,
		Redis:           adminRedisView{Configured: configured, LastErr: redisLastErr, LastErrAt: redisLastErrAt},
		Cache:           adminCacheView{Enabled: cacheEnabled, TTL: ttl},
		Retry:           retryView,
		Groups:          groups,
		Aliases:         aliases,
		Features:        g.buildAdminFeatures(),
		Pricing:         buildAdminPricingSummary(snaps, g.cfg.Pricing, g.cfg.ModelMeta),
		Version:         pluginVersion,
		Replica:         g.replica,
		Instance:        g.name,
		Warnings:        warnings,
		WarningsDropped: warningsDropped,
	}
}

// buildAdminAliasViews returns every configured model alias's read-only
// view (adminAliasView) — shared by GET /admin/api/overview's own
// Aliases field (buildAdminOverview, above) and GET /admin/api/catalog's
// identical field (admin_catalog.go), so the two can never list a
// different alias set or disagree about one alias's resolved metadata.
func (g *Gateway) buildAdminAliasViews() []adminAliasView {
	aliasSnaps := g.registry.aliasSnapshot()
	aliases := make([]adminAliasView, len(aliasSnaps))
	for i, a := range aliasSnaps {
		aliases[i] = adminAliasView{
			Alias:     a.Alias,
			Target:    a.Target,
			ModelMeta: buildAdminModelMetaView(g.registry.resolveMetaForAliasName(a.Alias)),
		}
	}
	return aliases
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
	Limits    *LimitsConfig `json:"limits,omitempty"`
	Kind      string        `json:"kind"`
	ID        string        `json:"id"`
	GroupName string        `json:"groupName,omitempty"`
	// Groups is the user's full member group list (multi-group support)
	// — set only for a user entry, same "user-only" convention GroupName
	// above already follows. GroupName keeps the first group for
	// back-compat; Groups is the complete membership.
	Groups               []string `json:"groups,omitempty"`
	Providers            []string `json:"providers,omitempty"`
	Models               []string `json:"models,omitempty"`
	MCPServers           []string `json:"mcpServers,omitempty"`
	Agents               []string `json:"agents,omitempty"`
	RequestsPerMinute    int64    `json:"requestsPerMinute"`
	RequestsPerDay       int64    `json:"requestsPerDay"`
	TokensInPerDay       int64    `json:"tokensInPerDay"`
	TokensOutPerDay      int64    `json:"tokensOutPerDay"`
	TokensInPerMonth     int64    `json:"tokensInPerMonth"`
	TokensOutPerMonth    int64    `json:"tokensOutPerMonth"`
	CostPerDayMicroUSD   int64    `json:"costPerDayMicroUsd"`
	CostPerMonthMicroUSD int64    `json:"costPerMonthMicroUsd"`
	// RejectionsPerDay is the fleet-wide count of checkAndCount rejections
	// attributed to this scope today (F4, v0.3 dashboard task) — read via
	// windowKey's metric "rej", window "day" (limits.go's metricRej).
	RejectionsPerDay int64 `json:"rejectionsPerDay"`
	// LastSeen is this scope's most recent admitted-request instant, unix
	// seconds (admin dashboard redesign, WP-B; DECISIONS Q6) — 0
	// (omitted) means never observed, or unknown (a storeDown read).
	// Sourced from scopeUsage.lastSeen (limits.go, WP-A step 1), the
	// usageKeysPerScope's 10th key.
	LastSeen  int64 `json:"lastSeen,omitempty"`
	StoreDown bool  `json:"storeDown,omitempty"`
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
		RejectionsPerDay:     su.rejectionsPerDay,
		LastSeen:             su.lastSeen,
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
// ~9,000 GETs (usageKeysPerScope=9, F4 v0.3 dashboard task's rej:day
// counter) in one pipelined call, during which
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
//
// Lowered from 200 to 160 (admin dashboard redesign, WP-B) when
// usageKeysPerScope grew from 9 to 10 for the new lastSeen counter
// (limits.go, WP-A step 1): 160 scopes * 10 keys/scope = 1,600 keys per
// chunk, keeping this endpoint's per-round-trip key count at the same
// order of magnitude this constant has always targeted, rather than
// letting it grow every time a future per-scope counter is added here.
const adminUsageChunkScopes = 160

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
		users[i].Groups = userSummaries[i].groups
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
// parameter: "user:{id}", "group:{id}", "model:{provider}/{model}", or the
// literal "total". ok is false for anything else, including a bare
// "total:{id}" form — the total scope carries no id component of its own,
// it is always totalScopeID (limits.go).
//
// strings.Cut splits on the FIRST colon only, so a model id that itself
// contains one (an ":free"-suffixed upstream id, for instance) survives
// intact in the returned id.
func parseHistoryScope(raw string) (kind, id string, ok bool) {
	if raw == totalScopeKind {
		return totalScopeKind, totalScopeID, true
	}
	k, i, found := strings.Cut(raw, ":")
	if !found || i == "" {
		return "", "", false
	}
	if k != "user" && k != "group" && k != kindModel {
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

// scopeExists reports whether kind/id names a currently active user, a
// configured group, or a currently catalogued (provider, model) pair —
// GET /admin/api/usage/history's 404 check for an unknown scope id.
// authStore.snapshot's own listing is the same membership buildAdminUsage
// already trusts for "every currently active user and every configured
// group"; kind is assumed already restricted to "user", "group" or
// kindModel by parseHistoryScope (the total scope's id is never checked
// against it — totalScopeID always exists).
//
// A kindModel id is checked against the live registry, so a model that
// accumulated usage and was later dropped from the catalog answers 404
// rather than a series nothing can reach through the UI. That matches
// buildAdminUsageModels, which enumerates the same catalog: the picker
// never offers an id this check would then reject.
func (g *Gateway) scopeExists(kind, id string) bool {
	if kind == kindModel {
		for _, s := range g.registry.snapshot() {
			for _, model := range s.models {
				if providerModelScopeID(s.name, model) == id {
					return true
				}
			}
		}
		return false
	}
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

// usageModelsDefaultLimit and usageModelsMaxLimit bound GET
// /admin/api/usage/models' "limit" parameter — how many ranked models the
// response carries at most. The default is what the Charts view's own
// ranking renders without asking for a limit at all.
const (
	usageModelsDefaultLimit = 20
	usageModelsMaxLimit     = 100
)

// parseUsageModelsLimit parses GET /admin/api/usage/models' "limit" query
// parameter: empty means usageModelsDefaultLimit, otherwise an integer
// between 1 and usageModelsMaxLimit inclusive. ok is false for anything
// else, mirroring parseHistorySpan's own validate-before-reading shape.
func parseUsageModelsLimit(raw string) (limit int, ok bool) {
	if raw == "" {
		return usageModelsDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > usageModelsMaxLimit {
		return 0, false
	}
	return n, true
}

// usageModelsPrefixMaxLen bounds GET /admin/api/usage/models' optional
// "prefix" query parameter (item 6, v0.3 dashboard task round 2): an
// operator-typed search string from the Charts view's model filter box,
// not a value with any legitimate reason to run long — a real model id
// this endpoint ranks over is always far shorter. Bounding it keeps a
// malformed or hostile query cheap to reject before catalogModelScopeIDs
// is even walked.
const usageModelsPrefixMaxLen = 256

// parseUsageModelsPrefix parses GET /admin/api/usage/models' optional
// "prefix" query parameter: empty means no filtering (every catalogued
// model ranked, this endpoint's original, pre-prefix behavior), otherwise
// the raw string unchanged, so long as it is at most
// usageModelsPrefixMaxLen bytes. ok is false for anything longer,
// mirroring parseUsageModelsLimit/parseUsageModelsSpan's own
// validate-before-reading shape. No further validation: a prefix that
// matches nothing in the catalog is not an error, just an empty (still
// 200) ranking — the same "no non-zero models" shape an ordinary,
// unfiltered ranking already answers when nothing was used yet.
func parseUsageModelsPrefix(raw string) (prefix string, ok bool) {
	if len(raw) > usageModelsPrefixMaxLen {
		return "", false
	}
	return raw, true
}

// parseUsageModelsSpan parses GET /admin/api/usage/models' optional
// "span" query parameter (F2, v0.3 dashboard task): empty means 1 —
// today's own bucket, this endpoint's original, pre-span behavior —
// otherwise an integer between 1 and window's own historyMaxSpan
// inclusive, mirroring parseHistorySpan's own validate-before-reading
// shape and identical range. window is assumed already validated by
// validHistoryWindow, exactly like parseHistorySpan's own precondition.
func parseUsageModelsSpan(raw, window string) (span int, ok bool) {
	if raw == "" {
		return 1, true
	}
	max := historyMaxSpan(window)
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// usageModelsDetailOn and usageModelsDetailOff enumerate GET
// /admin/api/usage/models' optional "detail" query parameter's accepted
// values (free-models feature): "1"/"true" turn detail on, ""/"0"/
// "false" leave it off (the byte-identical legacy response shape).
// Anything else is a 400, mirroring this endpoint's own
// validate-before-reading shape for every other parameter.
const (
	usageModelsDetailOn1    = "1"
	usageModelsDetailOn2    = "true"
	usageModelsDetailOff0   = "0"
	usageModelsDetailOff1   = "false"
	usageModelsDetailOffRaw = ""
)

// parseUsageModelsDetail parses GET /admin/api/usage/models' optional
// "detail" query parameter: empty, "0" or "false" means off (today's
// byte-identical response — no Requests/TokensIn/TokensOut/CostMicroUSD/
// Free fields, no "detail" key at all thanks to its own omitempty); "1"
// or "true" means on. ok is false for anything else.
func parseUsageModelsDetail(raw string) (detail bool, ok bool) {
	switch raw {
	case usageModelsDetailOffRaw, usageModelsDetailOff0, usageModelsDetailOff1:
		return false, true
	case usageModelsDetailOn1, usageModelsDetailOn2:
		return true, true
	default:
		return false, false
	}
}

// catalogModelScopeIDs returns every catalogued (provider, model) pair's
// canonical kindModel scope id, in the registry snapshot's own order. It
// is the enumeration GET /admin/api/usage/models ranks over, and the same
// one scopeExists validates a model scope against, so the two can never
// disagree about which model ids exist.
func (g *Gateway) catalogModelScopeIDs() []string {
	snaps := g.registry.snapshot()
	ids := make([]string, 0, len(snaps))
	for _, s := range snaps {
		for _, model := range s.models {
			ids = append(ids, providerModelScopeID(s.name, model))
		}
	}
	return ids
}

// adminUsageModelEntryView is one ranked model in GET
// /admin/api/usage/models: its canonical "provider/model" id and its
// total for the requested metric over the requested window's CURRENT
// bucket. detail=1 (free-models feature) additionally populates
// Requests/TokensIn/TokensOut/CostMicroUSD — pointers so a genuinely
// zero total is still emitted rather than indistinguishable from "not
// requested" — and Free. Every detail-only field is omitted (not
// zeroed) in the non-detail response, keeping it byte-identical to
// before this feature existed.
type adminUsageModelEntryView struct {
	// Requests, TokensIn, TokensOut and CostMicroUSD are populated only
	// by serveAdminUsageModelsDetail (detail=1); the non-detail path
	// never sets them, so they stay nil and omitempty drops them.
	// Ordered ahead of ID/Value/Free for fieldalignment (govet: minimal
	// GC pointer-scan prefix) — JSON names are what's binding, not Go
	// field order or the JSON key order it produces (the plan's own
	// ruling, free-models plan's "Response" section).
	Requests     *int64 `json:"requests,omitempty"`
	TokensIn     *int64 `json:"tokensIn,omitempty"`
	TokensOut    *int64 `json:"tokensOut,omitempty"`
	CostMicroUSD *int64 `json:"costMicroUsd,omitempty"`
	// CacheHits and CacheSavedMicroUsd (admin dashboard redesign, WP-B)
	// are populated only in detail mode, only when features.cacheStats
	// (the response cache is configured), AND only when window is "day"
	// or "month" — nil for a window="hour" request too (P2 fix,
	// serveAdminUsageModelsDetail's own doc comment): a model's chit/csave
	// only ever have a DAY bucket, and there is no exact way to recover an
	// hour-range value from whole-day sums.
	CacheHits          *int64 `json:"cacheHits,omitempty"`
	CacheSavedMicroUSD *int64 `json:"cacheSavedMicroUsd,omitempty"`
	// R402 (admin dashboard redesign, WP-B) is populated only in detail
	// mode: how many requests for this model were refused as unpriced
	// under a caller's cost budget (metricR402) over the requested span.
	R402 *int64 `json:"r402,omitempty"`
	ID   string `json:"id"`
	// PriceSource (admin dashboard redesign, WP-B) is populated only in
	// detail mode: billingPriceSource's own result (pricing.go) for this
	// model — the same "override"|"builtin"|"litellm"|"free"|"unpriced"
	// vocabulary GET /admin/api/catalog's adminCatalogModel.PriceSource
	// already uses.
	PriceSource string `json:"priceSource,omitempty"`
	Value       int64  `json:"value"`
	// Free is populated only by serveAdminUsageModelsDetail too; false
	// (the non-detail zero value) is dropped by its own omitempty.
	Free bool `json:"free,omitempty"`
}

// adminUsageModelsResponse is GET /admin/api/usage/models' body. Span
// (F2, v0.3 dashboard task) echoes the resolved span (1 when the request
// carried no ?span= parameter — parseUsageModelsSpan's own default).
// Detail (free-models feature) echoes whether the request carried
// detail=1 (parseUsageModelsDetail) — omitted (false) via its own
// omitempty for the legacy, byte-identical detail=0/absent response.
type adminUsageModelsResponse struct {
	Metric string                     `json:"metric"`
	Window string                     `json:"window"`
	Models []adminUsageModelEntryView `json:"models"`
	Span   int                        `json:"span"`
	// Offset (admin dashboard redesign, WP-B) echoes the resolved offset
	// (0 when the request carried no ?offset= parameter —
	// parseStatsOffset's own default) — how many whole window-steps back
	// from the current bucket this ranking's span starts.
	Offset int  `json:"offset"`
	Detail bool `json:"detail,omitempty"`
}

// usageModelsChunkKeys bounds how many counterStore keys
// chunkedModelSpanTotals (below) reads in a single storeGetMulti round
// trip — adminUsageChunkScopes' own reasoning (buildAdminUsage, above)
// applied to the catalog-sized, now span-multiplied read GET
// /admin/api/usage/models performs once a caller asks for span > 1: with
// ?span=24 on an hour window and a 400-model catalog, an unchunked read
// would pipeline 400*24 = 9,600 keys in one call, holding the shared
// Redis connection mutex (respClient, resp.go) for that whole pipeline
// while live traffic's own checkAndCount/account calls queue behind it.
const usageModelsChunkKeys = 1600

// chunkedModelSpanTotals calls limiter.modelSpanTotals in batches sized
// so each batch's own key count (len(chunk)*span) stays at or under
// usageModelsChunkKeys — mirroring chunkedCurrentUsage's chunking
// (above), sized for span instead of the fixed usageKeysPerScope stride.
// now is resolved once by the caller and threaded through every chunk, so
// every id's span ends at the identical instant regardless of how many
// chunks the catalog splits into. Any chunk that fails (storeDown or a
// mismatched read) fails the whole call: a ranking silently missing an
// arbitrary subset of models is worse than a 503, so this never returns a
// partial ranking.
//
// P12 fix (admin dashboard redesign verify round): now a thin
// kind=kindModel wrapper over limits.go's spanTotalsMulti — the ONE
// production span-sum reader this function's own chunking logic used to
// duplicate (byte-for-byte) against admin.go's own separate
// chunkedModelSpanTotalsMulti below AND limits_helpers_test.go's
// test-only modelSpanTotals/modelSpanTotalsMulti. See spanTotalsMulti's
// own doc comment for the chunking contract this preserves unchanged.
func (g *Gateway) chunkedModelSpanTotals(ids []string, metric, window string, now time.Time, span, offset int) ([]int64, bool) {
	out, ok := g.limiter.spanTotalsMulti(kindModel, ids, []string{metric}, window, now, span, offset)
	if !ok {
		return nil, false
	}
	return out[0], true
}

// chunkedModelSpanTotalsMulti is chunkedModelSpanTotals generalized to
// several metrics read together (GET /admin/api/usage/models' detail=1
// mode, free-models feature: it needs all four metrics —
// req/tokin/tokout/cost — for every candidate id, not just the one the
// ranking sorts by). P12 fix (admin dashboard redesign verify round): now
// a thin kind=kindModel wrapper over limits.go's spanTotalsMulti — see
// chunkedModelSpanTotals' own doc comment, immediately above, for what
// this replaced.
func (g *Gateway) chunkedModelSpanTotalsMulti(ids, metrics []string, window string, now time.Time, span, offset int) ([][]int64, bool) {
	return g.limiter.spanTotalsMulti(kindModel, ids, metrics, window, now, span, offset)
}

// serveAdminUsageModels writes GET /admin/api/usage/models' ranking of the
// most-used models for one metric/window (the Charts view's "Models"
// tab): "metric"=req|tokin|tokout|cost, "window"=hour|day|month, optional
// "limit"=N (default and max per the constants above), optional
// "prefix"=STRING (item 6, v0.3 dashboard task round 2: the Charts
// view's model search box) narrowing the catalog to ids that start with
// it before anything is ranked — see below, optional "detail"=1|true
// (free-models feature; parseUsageModelsDetail) switching to
// serveAdminUsageModelsDetail below. Every parameter is validated before
// the store is touched — 400 for an unrecognized metric or window, an
// out-of-range limit, a prefix over usageModelsPrefixMaxLen, or an
// unrecognized detail value.
//
// ONLY NON-ZERO models are returned, by explicit operator requirement: the
// catalog runs to hundreds of models and a ranking padded with zeroes is
// unreadable. The filter also keeps the response small even though the
// READ is catalog-sized — one counter per configured model in a single
// storeGetMulti (limiter.modelTotals), which is the same
// one-batched-read-per-poll shape serveAdminOverview already uses for
// per-provider counters.
//
// prefix narrows catalogModelScopeIDs' own output BEFORE
// chunkedModelSpanTotals ever reads a counter for it, and before limit is
// applied — not a post-hoc filter over an already-ranked, already-limited
// result. A caller searching the Charts view down to one provider's
// models (a common id shape here is "provider/model") must not still pay
// a catalog-sized store read for ids its own prefix already excludes, and
// limit must cap the FILTERED set's own top-N, not silently return fewer
// than limit matches because the unfiltered ranking's top N happened to
// fall outside the prefix.
//
// Ties break on the id, ascending, so a ranking of equal values is stable
// across polls rather than reshuffling under the reader. A storeDown read
// answers 503, never a silently-empty ranking — the same rule
// serveAdminUsageHistory applies, and for the same reason: "the store was
// unreachable" must never render as "no model was used".
//
// detail=0 (or an absent detail parameter) is byte-identical to this
// endpoint's behavior before the free-models feature existed — the
// branch below never runs, and adminUsageModelsResponse's own Detail
// field and every adminUsageModelEntryView detail-only field stay at
// their zero value, which their shared omitempty tag drops from the
// JSON entirely.
func (g *Gateway) serveAdminUsageModels(sw *statusTrackingWriter, r *http.Request) {
	q := r.URL.Query()

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
	limit, ok := parseUsageModelsLimit(q.Get("limit"))
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid limit")
		return
	}
	span, ok := parseUsageModelsSpan(q.Get("span"), window)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid span")
		return
	}
	offset, ok := parseStatsOffset(q.Get("offset"), window, span)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid offset")
		return
	}
	prefix, ok := parseUsageModelsPrefix(q.Get("prefix"))
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid prefix")
		return
	}
	detail, ok := parseUsageModelsDetail(q.Get("detail"))
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid detail")
		return
	}

	ids := g.catalogModelScopeIDs()
	if prefix != "" {
		filtered := make([]string, 0, len(ids))
		for _, id := range ids {
			if strings.HasPrefix(id, prefix) {
				filtered = append(filtered, id)
			}
		}
		ids = filtered
	}
	// P7 fix (admin dashboard redesign verify round): this cap used to
	// check len(ids)*span regardless of detail mode, but detail mode
	// reads up to 4 base metrics (req/tokin/tokout/cost) PLUS r402
	// (always, P2 fix) PLUS chit/csave (when features.cacheStats and the
	// window isn't hour) per id — up to 7x the single-metric estimate
	// this check assumed. keyMultiplier over-estimates when a month
	// window's cache/r402 reads convert to a smaller, retention-bounded
	// day span (P2 fix, monthRangeToDayBuckets) — that only ever rejects
	// a request earlier than strictly necessary, never lets an oversized
	// one through.
	keyMultiplier := 1
	if detail {
		keyMultiplier = len(usageModelsDetailMetrics()) + 1 // +1: r402, always read
		if g.buildAdminFeatures().CacheStats {
			keyMultiplier += 2 // chit, csave
		}
	}
	if len(ids)*span*keyMultiplier > adminMaxKeysPerRequest {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
		return
	}

	if detail {
		g.serveAdminUsageModelsDetail(sw, ids, metric, window, span, offset, limit)
		return
	}

	totals, storeOK := g.chunkedModelSpanTotals(ids, metric, window, g.limiter.now(), span, offset)
	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage history store unavailable")
		return
	}

	models := make([]adminUsageModelEntryView, 0, len(ids))
	for i, id := range ids {
		if totals[i] == 0 {
			continue
		}
		models = append(models, adminUsageModelEntryView{ID: id, Value: totals[i]})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Value != models[j].Value {
			return models[i].Value > models[j].Value
		}
		return models[i].ID < models[j].ID
	})
	if len(models) > limit {
		models = models[:limit]
	}

	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminUsageModelsResponse{Metric: metric, Window: window, Span: span, Offset: offset, Models: models})
}

// usageModelsDetailMetrics is the fixed metric read/unpack order
// serveAdminUsageModelsDetail uses: req, tokin, tokout, cost — matching
// adminUsageModelEntryView's own field order (Requests, TokensIn,
// TokensOut, CostMicroUSD). Built fresh per call rather than as a
// package-level var (go.md: no mutable package-level state) — the cost
// of one 4-element slice literal per detail request is immaterial next
// to the storeGetMulti round trip it drives.
func usageModelsDetailMetrics() []string {
	return []string{metricReq, metricTokIn, metricTokOut, metricCost}
}

// usageModelsMetricIndex maps metric — already validated by
// validHistoryMetric, so always one of the four cases below — to its
// position in usageModelsDetailMetrics, picking out the "value" column
// GET /admin/api/usage/models' detail=1 mode ranks by: whichever metric
// the caller actually asked for, even though detail mode always reads
// all four.
func usageModelsMetricIndex(metric string) int {
	switch metric {
	case metricReq:
		return 0
	case metricTokIn:
		return 1
	case metricTokOut:
		return 2
	default: // metricCost: the only case validHistoryMetric still allows through
		return 3
	}
}

// adminUsageModelFree reports whether canonical (a catalogued
// "provider/model" id, bare its part after the first "/") is free for
// GET /admin/api/usage/models' detail=1 mode: either an operator
// modelMeta entry marks it Free (modelMetaFree, modelmeta.go — the SAME
// two-key lookup billing itself uses, so this view and actual billing
// can never disagree), or its resolved price (resolveUnifiedPricing,
// failover.go — the identical resolution unifiedCostMicros itself uses)
// is KNOWN and both per-token prices are exactly 0. The second check
// matters on its own: a configured or built-in price table entry that
// happens to bill nothing is just as "free" to an operator reading this
// ranking as an explicit modelMeta:{free:true}, but a genuinely UNKNOWN
// price must never render as free — resolveUnifiedPricing's own ok
// return keeps the two cases apart.
func adminUsageModelFree(canonical, bare string, overrides map[string]*ModelPricing, meta map[string]*ModelMetaConfig) bool {
	if modelMetaFree(canonical, bare, meta) {
		return true
	}
	price, known := resolveUnifiedPricing(canonical, bare, overrides)
	return known && price.InputPerM == 0 && price.OutputPerM == 0
}

// serveAdminUsageModelsDetail is serveAdminUsageModels' detail=1 branch
// (free-models feature): ids (already prefix-filtered), metric, window,
// span and limit are all already validated by the caller. Unlike the
// non-detail path, an id is included when ANY of its four metrics is
// nonzero, not just the requested one — so a free model with real
// request traffic but zero cost still appears in a metric=cost ranking,
// at Value=0, instead of being dropped the way the non-detail path would
// drop it. Ties break on value desc, then requests desc, then id asc:
// once two free models tie on the requested metric (typically cost, both
// zero), requests is the natural next signal, ahead of the id fallback
// every ranking already uses to stay stable across polls.
// monthRangeToDayBuckets converts a windowMonth span/offset (relative to
// now) into the windowDay span/offset covering the EXACT SAME calendar
// range — P2 fix, admin dashboard redesign verify round: GET
// /admin/api/usage/models' detail=1 mode needs this for the counter
// families that have no month bucket of their own (r402) or only ever
// have a DAY bucket at the model scope (chit/csave — plan §1.2's "model
// day only" row). Month and day buckets both align on calendar
// boundaries (historyStepBack's own month case always anchors on the 1st
// of a month, limits.go), so this conversion is exact, not an
// approximation: every day it names falls entirely inside the requested
// month range, and every day in that range is named. A day outside the
// day family's own retention (historyMaxSpan(windowDay), 35) simply reads
// back 0 through the store, which is the correct answer for data that
// was never kept that long — the returned span is capped at that
// retention purely to avoid building a key list for months of days none
// of which could possibly hold live data.
func monthRangeToDayBuckets(now time.Time, span, offset int) (daySpan, dayOffset int) {
	u := now.UTC()
	today := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	monthAnchor := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonth := monthAnchor.AddDate(0, -offset, 0)
	firstMonth := lastMonth.AddDate(0, -(span - 1), 0)
	lastDay := today
	if offset > 0 {
		lastDay = lastMonth.AddDate(0, 1, -1) // last calendar day of lastMonth
	}
	dayOffset = int(today.Sub(lastDay).Hours() / 24)
	daySpan = int(lastDay.Sub(firstMonth).Hours()/24) + 1
	if max := historyMaxSpan(windowDay) - dayOffset; daySpan > max {
		if max < 1 {
			max = 1
		}
		daySpan = max
	}
	return daySpan, dayOffset
}

func (g *Gateway) serveAdminUsageModelsDetail(sw *statusTrackingWriter, ids []string, metric, window string, span, offset, limit int) {
	now := g.limiter.now()
	metrics := usageModelsDetailMetrics()
	totalsByMetric, storeOK := g.chunkedModelSpanTotalsMulti(ids, metrics, window, now, span, offset)
	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage history store unavailable")
		return
	}
	valueIdx := usageModelsMetricIndex(metric)

	// r402 (admin dashboard redesign, WP-B; P2 fix, verify round): ALWAYS
	// read, independent of features.cacheStats — r402 counts refused-
	// unpriced-under-a-cost-budget requests, unrelated to the response
	// cache. It used to be folded into the SAME cache-gated read as
	// chit/csave, forced to windowDay regardless of the caller's own
	// window — wrong on two counts: gated on cache config at all, and
	// accountWith's own recordUnpriced402 (limits.go) writes a model's
	// r402 at BOTH hour and day, so an hour-window request silently read
	// 24-120x too many day buckets while a month-window request read only
	// 1 day per month instead of the whole month. Read at the caller's
	// own window when r402 has a bucket there (hour or day, exact);
	// convert to the covering day buckets when it is month (r402 has no
	// month bucket — monthRangeToDayBuckets, above, is exact for this).
	r402Window, r402Span, r402Offset := window, span, offset
	if window == windowMonth {
		r402Window = windowDay
		r402Span, r402Offset = monthRangeToDayBuckets(now, span, offset)
	}
	r402Multi, storeOK := g.chunkedModelSpanTotalsMulti(ids, []string{metricR402}, r402Window, now, r402Span, r402Offset)
	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage history store unavailable")
		return
	}
	r402ByID := r402Multi[0]

	// chit/csave (admin dashboard redesign, WP-B): only when
	// features.cacheStats (the response cache is configured) — nil
	// otherwise, as before. Unlike r402, a model's chit/csave are ONLY
	// ever written at windowDay (recordCacheHit, limits.go — plan §1.2's
	// "model day only" row), so there is no hour bucket to read at all:
	// window==day reads directly; window==month converts to the exact
	// covering day buckets, same as r402 above; window==hour is left
	// OMITTED (both fields stay nil, not a fabricated or over-counted
	// number) — an hour range's true chit/csave cannot be recovered from
	// whole-day sums without also counting every hour of those days OUTSIDE
	// the requested range, so there is no exact answer to give (documented
	// in README.md's GET /admin/api/usage/models section).
	cacheEnabled := g.buildAdminFeatures().CacheStats
	var chitByID, csaveByID []int64
	if cacheEnabled && window != windowHour {
		cacheWindow, cacheSpan, cacheOffset := window, span, offset
		if window == windowMonth {
			cacheWindow = windowDay
			cacheSpan, cacheOffset = monthRangeToDayBuckets(now, span, offset)
		}
		var cacheMulti [][]int64
		cacheMulti, storeOK = g.chunkedModelSpanTotalsMulti(ids, []string{metricChit, metricCsave}, cacheWindow, now, cacheSpan, cacheOffset)
		if !storeOK {
			writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage history store unavailable")
			return
		}
		chitByID, csaveByID = cacheMulti[0], cacheMulti[1]
	}

	models := make([]adminUsageModelEntryView, 0, len(ids))
	for i, id := range ids {
		req := totalsByMetric[0][i]
		tokIn := totalsByMetric[1][i]
		tokOut := totalsByMetric[2][i]
		cost := totalsByMetric[3][i]
		if req == 0 && tokIn == 0 && tokOut == 0 && cost == 0 {
			continue
		}
		_, bare, _ := strings.Cut(id, "/")
		source, _ := billingPriceSource(id, bare, g.cfg.Pricing, g.cfg.ModelMeta)
		r402 := r402ByID[i]
		entry := adminUsageModelEntryView{
			ID:           id,
			Value:        totalsByMetric[valueIdx][i],
			Requests:     &req,
			TokensIn:     &tokIn,
			TokensOut:    &tokOut,
			CostMicroUSD: &cost,
			Free:         adminUsageModelFree(id, bare, g.cfg.Pricing, g.cfg.ModelMeta),
			PriceSource:  source,
			R402:         &r402,
		}
		if chitByID != nil {
			chit := chitByID[i]
			csave := csaveByID[i]
			entry.CacheHits = &chit
			entry.CacheSavedMicroUSD = &csave
		}
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Value != models[j].Value {
			return models[i].Value > models[j].Value
		}
		if *models[i].Requests != *models[j].Requests {
			return *models[i].Requests > *models[j].Requests
		}
		return models[i].ID < models[j].ID
	})
	if len(models) > limit {
		models = models[:limit]
	}

	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminUsageModelsResponse{Metric: metric, Window: window, Span: span, Offset: offset, Models: models, Detail: true})
}

// parseEventsLimit parses GET /admin/api/events' "limit" query parameter
// (F3, v0.3 dashboard task): empty means adminEventsDefaultLimit,
// otherwise an integer between 1 and adminEventsMaxLimit inclusive,
// mirroring parseUsageModelsLimit's own validate-before-reading shape.
func parseEventsLimit(raw string) (limit int, ok bool) {
	if raw == "" {
		return adminEventsDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > adminEventsMaxLimit {
		return 0, false
	}
	return n, true
}

// serveAdminEvents writes GET /admin/api/events' most recent gateway
// events (F3, v0.3 dashboard task): rate-limit/budget/store-down
// admission rejections and upstream/timeout/unpriced/capacity error-path
// signals, newest first. "limit" is optional (adminEventsDefaultLimit),
// 1..adminEventsMaxLimit inclusive otherwise 400. Never calls
// checkAndCount — the same admin-traffic-must-not-move-usage-statistics
// rule every other /admin/api/* route already follows (handleAdminAPI's
// own doc comment). Answers from the shared Redis-backed list when
// reachable (source "redis"), falling back to this replica's own local
// ring otherwise (source "replica", Degraded true when Redis is
// configured but this particular read failed) — fail-open: the local
// ring always answers (eventLog.read's own doc comment, events.go).
func (g *Gateway) serveAdminEvents(sw *statusTrackingWriter, r *http.Request) {
	limit, ok := parseEventsLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid limit")
		return
	}
	events, source, degraded := g.events.read(limit, g.limiter.storeLatched())
	if events == nil {
		events = []gatewayEvent{}
	}
	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminEventsResponse{
		Events: events, Source: source, Replica: g.replica, Capacity: eventRingCap, Degraded: degraded,
	})
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
// handleTargetProxy actually enforces.
//
// Access's JSON contract (review-auth finding F2, 2026-09 review — fixed
// from a prior "omitempty" tag): "access":null means every configured
// group can reach this target ("empty meaning all" — adminTargetAccess
// returns a nil slice for that case); "access":[] (present, zero-length)
// means NO configured group can reach it; "access":["g1",...] lists the
// groups that can. The prior omitempty tag made the first two cases
// serialize identically (both simply absent from the JSON body), so a
// target restricted to zero groups rendered in the webui exactly like an
// unrestricted one — the opposite of what its own access rule enforces.
// No omitempty here specifically because that distinction depends on nil
// vs. non-nil-but-empty, which omitempty collapses. adminTargetAccess's
// own return value is unchanged by this fix (it already returned nil for
// "all" and a non-nil, possibly zero-length, slice otherwise) — only this
// struct's serialization of that value changes. The webui (webui/src/
// types/api.ts's AdminTargetView.access, webui/src/lib/target-columns.ts)
// must be updated to treat null/undefined and a zero-length array as
// DIFFERENT states, distinct from today's "!access?.length means All
// groups" check that treats both alike; report this contract to whoever
// owns that file.
//
// Health is feat/target-health's own readout (targetHealthView, below).
// Field order (Health first, the struct-typed field, then the strings/
// slice, then Counters last) is fieldalignment-sensitive, derived
// against a scratch copy the same way this package's other structs
// already document (Config's own doc comment, llmgateway.go).
type adminTargetView struct {
	Health   adminTargetHealthView   `json:"health"`
	Name     string                  `json:"name"`
	URL      string                  `json:"url"`
	Access   []string                `json:"access"`
	Counters adminTargetCountersView `json:"counters"`
}

// adminTargetHealthView is one target's feat/target-health readout in
// GET /admin/api/targets — the exact JSON contract the webui's MCP &
// Agents panel codes against. State and ConsecutiveFailures are always
// present. State "unknown" (targetHealthUnknown) covers two different
// situations (F4, feat/target-health review) — see that constant's own
// doc comment — and this view tells them apart by whether the tracker
// has ever observed the target at all, NOT by State alone:
//   - never observed: every other field is omitted, since none of them
//     carry a meaningful value yet.
//   - observed, but never once succeeded (still below
//     targetHealth.failureThreshold): every field below is still
//     present, exactly as for "healthy"/"unhealthy" — there IS a real
//     lastCheck/lastError/source/latencyMs to show, the tracker simply
//     has not seen this target succeed yet.
//
// LatencyMs is a pointer so a genuine 0ms observation still serializes
// as "latencyMs":0 rather than being indistinguishable from "omitted".
// Field order is fieldalignment-derived, the same convention
// adminTargetView's own doc comment above explains.
type adminTargetHealthView struct {
	LatencyMs           *int64            `json:"latencyMs,omitempty"`
	State               targetHealthState `json:"state"`
	LastCheck           string            `json:"lastCheck,omitempty"`
	LastError           string            `json:"lastError,omitempty"`
	Source              string            `json:"source,omitempty"`
	ConsecutiveFailures int               `json:"consecutiveFailures"`
}

// targetHealthView converts one targetHealthTracker.snapshot result
// (target_health.go) into its JSON view. Whether to omit the non-always-
// present fields is decided by snap.observed, NOT snap.state ==
// targetHealthUnknown (adminTargetHealthView's own doc comment explains
// why those are different questions since F4).
func targetHealthView(snap targetHealthSnapshot) adminTargetHealthView {
	view := adminTargetHealthView{State: snap.state, ConsecutiveFailures: snap.consecutiveFailures}
	if !snap.observed {
		return view
	}
	view.LastCheck = snap.lastCheck.UTC().Format(time.RFC3339)
	view.LastError = snap.lastError
	view.Source = snap.source
	ms := snap.latency.Milliseconds()
	view.LatencyMs = &ms
	return view
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
			Health:   targetHealthView(g.targetHealth.snapshot(targetKindMCP, name)),
			Name:     name,
			URL:      sanitizeBaseURL(g.cfg.MCPServers[name].URL),
			Access:   adminTargetAccess(groupSummaries, func(gs groupSummary) []string { return gs.mcpServers }, name),
			Counters: targetCountersView(counters[i]),
		}
	}
	agents := make([]adminTargetView, len(agentNames))
	for i, name := range agentNames {
		agents[i] = adminTargetView{
			Health:   targetHealthView(g.targetHealth.snapshot(targetKindAgent, name)),
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
	g.maybeSweepTargetHealth()
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminTargets())
}
