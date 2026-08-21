package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The admin dashboard's routes (spec §4, v0.2; adminUsageHistoryPath added
// by the v0.2 data-layer task; adminAssetsPathPrefix added by the Vue
// admin-panel task), matched only when adminEnabled(g.cfg) — see
// llmgateway.go's ServeHTTP dispatch. adminAssetsPathPrefix is a prefix,
// not one fixed path: every hashed filename the Vite build emits
// (admin_assets_gen.go) is served under it.
const (
	adminPagePath         = "/admin"
	adminAssetsPathPrefix = "/admin/assets/"
	adminOverviewPath     = "/admin/api/overview"
	adminUsagePath        = "/admin/api/usage"
	adminUsageHistoryPath = "/admin/api/usage/history"
)

// adminCSP is the Content-Security-Policy header served with GET /admin,
// every GET /admin/assets/{hashedname} response (serveAdminAsset), and
// the three /admin/api/* JSON routes — every /admin* response this
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
// of the three /admin/api/* JSON routes.
func isAdminPath(path string) bool {
	if path == adminPagePath || path == adminOverviewPath || path == adminUsagePath || path == adminUsageHistoryPath {
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

// handleAdminAPI is the gate for the three /admin/api/* JSON routes,
// applying spec §4's gate order: unauthenticated → 401, authenticated
// non-admin → 403, admin → serve.
//
// None of the three routes call checkAndCount (operator directive:
// progress ledger, 2026-08-20 — "admin requests must NOT touch req/min,
// req/day statistics"): admin traffic must never appear in usage
// statistics, which exist to measure real LLM traffic only. GET
// /admin/api/usage/history was built this way from the start (v0.2
// data-layer task); this change extends the same treatment to GET
// /admin/api/overview and GET /admin/api/usage, which previously counted
// like any other authenticated route (spec §4 amended accordingly). An
// admin's own req/min or req/day limit, if configured, is therefore never
// enforced against admin-route traffic either — the accepted trade-off
// the operator directive names: these are admin-gated, cheap reads, and
// an admin holder polling the dashboard aggressively is a self-inflicted,
// not a shared, resource cost.
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
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	BaseURL     string    `json:"baseUrl"`
	LastRefresh time.Time `json:"lastRefresh"`
	LastErr     string    `json:"lastErr,omitempty"`
	ModelCount  int       `json:"modelCount"`
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
	Alias  string `json:"alias"`
	Target string `json:"target"`
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
	providers := make([]adminProviderView, len(snaps))
	for i, s := range snaps {
		providers[i] = adminProviderView{
			Name:        s.name,
			Type:        s.typeName,
			BaseURL:     sanitizeBaseURL(s.baseURL),
			ModelCount:  s.modelCount,
			LastRefresh: s.lastRefresh,
			LastErr:     sanitizeProviderErr(s.lastErr, s.baseURL),
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
		aliases[i] = adminAliasView(a) // identical underlying field shape (alias, target string), differing only in json tags
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
type adminUsageEntryView struct {
	Limits               *LimitsConfig `json:"limits,omitempty"`
	Kind                 string        `json:"kind"`
	ID                   string        `json:"id"`
	GroupName            string        `json:"groupName,omitempty"`
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
// limiter.currentUsage reads every one's current-window counters — plus
// the synthetic total scope's own (v0.2 data-layer task) — in ONE
// storeGetMulti round trip total (v0.2 final review wave, 2026-08-20; see
// limiter.currentUsage's own doc comment for the tradeoff this
// supersedes) — users, groups, and the total scope are concatenated into
// a single scopes slice before that one call, then the flat result is
// sliced back into the three response sections at the same split points,
// order preserved. Unlike buildLimitScopes (routes_unified.go), this
// never omits an entity for having nil limits — the dashboard shows usage
// for every user and group, limited or not.
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

	allUsage := g.limiter.currentUsage(scopes)
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
