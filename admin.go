package traefikllmgateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// pluginVersion is the gateway's own build/plugin version string, surfaced
// read-only in GET /admin/api/overview (spec §4, v0.2). Bumped alongside
// the module's own release process — not read from anywhere else in the
// package, since Yaegi interpretation has no build-info mechanism to pull
// it from automatically. Keep in sync with the release tag: bump this
// alongside `git tag`, not before or after it.
const pluginVersion = "0.2.0"

// The three routes the read-only admin dashboard registers (spec §4,
// v0.2), matched only when adminEnabled(g.cfg) — see llmgateway.go's
// ServeHTTP dispatch.
const (
	adminPagePath     = "/admin"
	adminOverviewPath = "/admin/api/overview"
	adminUsagePath    = "/admin/api/usage"
)

// adminCSP is the Content-Security-Policy header served with all three
// admin routes (spec §4, v0.2; extended to the two JSON routes by a
// folded review item, 2026-08-20 review): no external assets of any
// kind, only this page's own inline script/style, and fetch calls
// restricted to same-origin — the dashboard works air-gapped and cannot
// be coerced into loading anything off-host.
const adminCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"

// adminEnabled reports whether cfg's Admin block is present and enabled —
// the single gate ServeHTTP checks before matching any /admin* route at
// all. A nil or disabled block means those paths are simply not
// registered, so they fall through to ServeHTTP's existing 404/
// passthroughUnknown handling, exactly like any other unrecognized path.
func adminEnabled(cfg *Config) bool {
	return cfg.Admin != nil && cfg.Admin.Enabled
}

// isAdminPath reports whether path is one of the three admin routes.
func isAdminPath(path string) bool {
	return path == adminPagePath || path == adminOverviewPath || path == adminUsagePath
}

// handleAdmin is ServeHTTP's single entry point for all three /admin*
// routes (spec §4, v0.2), called only when adminEnabled — ServeHTTP's
// dispatch already checked that.
//
// GET /admin itself serves the HTML shell with no authentication at all
// (controller-approved amendment to spec §4, 2026-08-20 review): a
// browser navigating straight to a URL cannot attach a custom
// Authorization or x-api-key header, so the original "401 when
// unauthenticated" gate made the dashboard unreachable from a browser in
// the first place. The shell carries zero data — every value is fetched
// client-side from /admin/api/*, which stay fully gated below — so there
// is nothing to protect by gating the page itself. adminEnabled(g.cfg)
// still governs whether GET /admin is reachable at all (disabled falls
// through to 404, unchanged).
func (g *Gateway) handleAdmin(sw *statusTrackingWriter, r *http.Request) {
	if r.URL.Path == adminPagePath {
		g.serveAdminPage(sw)
		return
	}
	g.handleAdminAPI(sw, r)
}

// handleAdminAPI is the gate for the two /admin/api/* JSON routes,
// applying spec §4's gate order: unauthenticated → 401, authenticated
// non-admin → 403, admin → serve. Like every other authenticated route
// it counts request counters via checkAndCount before serving: an admin
// over their own req/min limit gets a 429 here exactly as they would on
// any other route (spec §4's "Accounting: admin routes count request
// counters like any authed route").
func (g *Gateway) handleAdminAPI(sw *statusTrackingWriter, r *http.Request) {
	u, grp, ok := g.auth.identify(r)
	g.logAuthEvent(ok, authEventUserName(u), r)
	if !ok {
		writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return
	}
	if !u.admin {
		writeOAIError(sw, http.StatusForbidden, "invalid_request_error", "admin access required")
		return
	}
	if violation := g.limiter.checkAndCount(buildLimitScopes(u, grp)); violation != nil {
		writeLimitViolation(sw, violation)
		return
	}

	switch r.URL.Path {
	case adminOverviewPath:
		g.serveAdminOverview(sw)
	case adminUsagePath:
		g.serveAdminUsage(sw)
	}
}

// serveAdminPage writes the single-page dashboard (adminPageHTML, below)
// with its Content-Security-Policy header. Unauthenticated by design —
// see handleAdmin's doc comment.
func (g *Gateway) serveAdminPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", adminCSP)
	_, _ = io.WriteString(w, adminPageHTML)
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

// adminGroupView is one group's summary in GET /admin/api/overview.
type adminGroupView struct {
	Limits      *LimitsConfig `json:"limits,omitempty"`
	Name        string        `json:"name"`
	MemberCount int           `json:"memberCount"`
}

// adminOverviewResponse is the full body of GET /admin/api/overview (spec
// §4, v0.2). Every slice is sorted and built fresh per request — no
// caching of the response itself — so the dashboard's 5s poll always
// reflects current state.
type adminOverviewResponse struct {
	Version   string              `json:"version"`
	Providers []adminProviderView `json:"providers"`
	Groups    []adminGroupView    `json:"groups"`
	Redis     adminRedisView      `json:"redis"`
	Cache     adminCacheView      `json:"cache"`
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
			LastErr:     s.lastErr,
		}
	}

	configured, redisLastErr, redisLastErrAt := g.limiter.redisStatus()

	cacheEnabled := g.cache != nil
	var ttl string
	if cacheEnabled {
		ttl = g.cache.ttl.String()
	}

	_, groupSummaries := g.auth.snapshot()
	groups := make([]adminGroupView, len(groupSummaries))
	for i, gs := range groupSummaries {
		groups[i] = adminGroupView{Name: gs.name, Limits: gs.limits, MemberCount: gs.memberCount}
	}

	return adminOverviewResponse{
		Providers: providers,
		Redis:     adminRedisView{Configured: configured, LastErr: redisLastErr, LastErrAt: redisLastErrAt},
		Cache:     adminCacheView{Enabled: cacheEnabled, TTL: ttl},
		Groups:    groups,
		Version:   pluginVersion,
	}
}

// serveAdminOverview writes buildAdminOverview's result as JSON.
func (g *Gateway) serveAdminOverview(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminOverview())
}

// adminUsageEntryView is one user's or group's row in GET
// /admin/api/usage (spec §4, v0.2): its current-window counter values,
// with its configured limit echoed beside each. GroupName is set only for
// a user entry (empty for a group entry) — the group a user currently
// belongs to, for display; it carries no access-control meaning of its
// own.
type adminUsageEntryView struct {
	Limits               *LimitsConfig `json:"limits,omitempty"`
	Kind                 string        `json:"kind"`
	ID                   string        `json:"id"`
	GroupName            string        `json:"groupName,omitempty"`
	RequestsPerMinute    int64         `json:"requestsPerMinute"`
	RequestsPerDay       int64         `json:"requestsPerDay"`
	TokensPerDay         int64         `json:"tokensPerDay"`
	TokensPerMonth       int64         `json:"tokensPerMonth"`
	CostPerDayMicroUSD   int64         `json:"costPerDayMicroUsd"`
	CostPerMonthMicroUSD int64         `json:"costPerMonthMicroUsd"`
	StoreDown            bool          `json:"storeDown,omitempty"`
}

// adminUsageResponse is the full body of GET /admin/api/usage: every
// currently active user, and every configured group, each with its own
// usage+limits row. Both slices are sorted (authStore.snapshot's own
// contract) and built fresh per request.
type adminUsageResponse struct {
	Users  []adminUsageEntryView `json:"users"`
	Groups []adminUsageEntryView `json:"groups"`
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
		TokensPerDay:         su.tokensPerDay,
		TokensPerMonth:       su.tokensPerMonth,
		CostPerDayMicroUSD:   su.costPerDayMicros,
		CostPerMonthMicroUSD: su.costPerMonthMicros,
		StoreDown:            su.storeDown,
	}
}

// buildAdminUsage assembles adminUsageResponse: authStore.snapshot lists
// every currently active user and every configured group (names, group
// membership, and limits only — never a key or its digest), and
// limiter.currentUsage reads each one's six current-window counters —
// one batched round trip per scope (limits.go's storeGetMulti) rather
// than six separate ones. Unlike buildLimitScopes (routes_unified.go),
// this never omits an entity for having nil limits — the dashboard shows
// usage for every user and group, limited or not.
func (g *Gateway) buildAdminUsage() adminUsageResponse {
	userSummaries, groupSummaries := g.auth.snapshot()

	userScopes := make([]limitScope, len(userSummaries))
	for i, us := range userSummaries {
		userScopes[i] = limitScope{kind: "user", id: us.name, limits: us.limits}
	}
	groupScopes := make([]limitScope, len(groupSummaries))
	for i, gs := range groupSummaries {
		groupScopes[i] = limitScope{kind: "group", id: gs.name, limits: gs.limits}
	}

	userUsage := g.limiter.currentUsage(userScopes)
	groupUsage := g.limiter.currentUsage(groupScopes)

	users := make([]adminUsageEntryView, len(userUsage))
	for i, su := range userUsage {
		users[i] = usageEntryView(su, userSummaries[i].limits)
		users[i].GroupName = userSummaries[i].groupName
	}
	groups := make([]adminUsageEntryView, len(groupUsage))
	for i, su := range groupUsage {
		groups[i] = usageEntryView(su, groupSummaries[i].limits)
	}

	return adminUsageResponse{Users: users, Groups: groups}
}

// serveAdminUsage writes buildAdminUsage's result as JSON.
func (g *Gateway) serveAdminUsage(w http.ResponseWriter) {
	setAdminJSONHeaders(w)
	_ = json.NewEncoder(w).Encode(g.buildAdminUsage())
}

// adminPageHTML is the entire /admin single-page dashboard: markup, CSS,
// and vanilla JS in one Go raw-string const. Yaegi interpretation forbids
// the embed directive (no filesystem access from an interpreted plugin),
// so the page ships as source, exactly like every other Yaegi-compatible
// plugin's static assets. It polls adminOverviewPath and adminUsagePath
// every 5s via fetch, loads no external asset of any kind (matching
// adminCSP's default-src 'none'), and never uses innerHTML with
// server-provided strings — every dynamic value is written via
// textContent, so nothing the API returns is ever interpreted as markup.
//
// Browser key entry (controller-approved amendment, 2026-08-20 review):
// the page itself carries no auth (see handleAdmin), so its script reads
// an admin API key from this tab's sessionStorage and sends it as
// "x-api-key" on every /admin/api/* fetch. Absent a stored key, or on any
// 401/403 response, it shows an inline key-entry form instead of the
// dashboard; a submitted key is kept only in sessionStorage — never in a
// cookie, localStorage, or any persistent store — so it disappears when
// the tab closes and is never sent anywhere but this page's own fetches.
const adminPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LLM Gateway - Admin</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #ffffff;
    --fg: #1a1a1a;
    --muted: #666666;
    --border: #d0d0d0;
    --head-bg: #f2f2f2;
    --err: #b91c1c;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #111214;
      --fg: #e6e6e6;
      --muted: #9a9a9a;
      --border: #33363a;
      --head-bg: #1c1e21;
      --err: #f87171;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 1.5rem;
    background: var(--bg);
    color: var(--fg);
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  h2 { font-size: 1rem; margin: 1.5rem 0 .5rem; }
  #status-line { color: var(--muted); margin-bottom: 1rem; font-size: .85rem; }
  table { border-collapse: collapse; width: 100%; margin-bottom: .5rem; }
  th, td {
    border: 1px solid var(--border);
    padding: .4rem .6rem;
    text-align: left;
    font-size: .85rem;
    vertical-align: top;
  }
  th { background: var(--head-bg); font-weight: 600; }
  .muted { color: var(--muted); }
  .err { color: var(--err); }
  .hidden { display: none; }
  .infra-line { margin: .25rem 0; font-size: .85rem; }
  section { margin-bottom: 1.5rem; }
  input, button {
    font: inherit;
    padding: .4rem .6rem;
    border: 1px solid var(--border);
    border-radius: .25rem;
    background: var(--bg);
    color: var(--fg);
  }
  button { cursor: pointer; }
</style>
</head>
<body>
<h1>LLM Gateway - Admin</h1>
<div id="status-line">loading...</div>

<section id="auth-gate">
  <h2>Admin key required</h2>
  <p class="muted">Enter an admin API key. The browser keeps it only in this tab's sessionStorage: it is never written to disk and is sent only to this page's own /admin/api/* requests.</p>
  <form id="auth-form">
    <input type="password" id="auth-key-input" autocomplete="off" placeholder="API key">
    <button type="submit">Continue</button>
  </form>
  <div id="auth-error" class="err"></div>
</section>

<div id="dashboard" class="hidden">

<section>
  <h2>Providers</h2>
  <table id="providers"><thead><tr>
    <th>Name</th><th>Type</th><th>Base URL</th><th>Models</th><th>Last refresh</th><th>Last error</th>
  </tr></thead><tbody></tbody></table>
</section>

<section>
  <h2>Infrastructure</h2>
  <div id="infra"></div>
</section>

<section>
  <h2>Groups</h2>
  <table id="groups"><thead><tr>
    <th>Name</th><th>Members</th><th>Limits</th><th>req/min</th><th>req/day</th><th>tok/day</th><th>tok/month</th><th>cost/day</th><th>cost/month</th>
  </tr></thead><tbody></tbody></table>
</section>

<section>
  <h2>Users</h2>
  <table id="users"><thead><tr>
    <th>Name</th><th>Group</th><th>Limits</th><th>req/min</th><th>req/day</th><th>tok/day</th><th>tok/month</th><th>cost/day</th><th>cost/month</th>
  </tr></thead><tbody></tbody></table>
</section>

</div>

<script>
(function () {
  "use strict";

  var POLL_MS = 5000;
  var OVERVIEW_URL = "/admin/api/overview";
  var USAGE_URL = "/admin/api/usage";
  var ZERO_TIME = "0001-01-01T00:00:00Z";
  var KEY_STORAGE = "llmgwAdminKey";
  var groupMeta = {};

  function getStoredKey() {
    try { return sessionStorage.getItem(KEY_STORAGE) || ""; } catch (e) { return ""; }
  }
  function setStoredKey(k) {
    try { sessionStorage.setItem(KEY_STORAGE, k); } catch (e) { /* storage unavailable */ }
  }
  function clearStoredKey() {
    try { sessionStorage.removeItem(KEY_STORAGE); } catch (e) { /* storage unavailable */ }
  }

  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) e.textContent = text;
    if (cls) e.className = cls;
    return e;
  }

  function fmtCost(micros) {
    return "$" + (micros / 1000000).toFixed(4);
  }

  function fmtAgo(iso) {
    if (!iso || iso === ZERO_TIME) return "";
    var then = new Date(iso).getTime();
    if (isNaN(then)) return "";
    var secs = Math.max(0, Math.round((Date.now() - then) / 1000));
    return " (" + secs + "s ago)";
  }

  function fmtLimits(l) {
    if (!l) return "none";
    var parts = [];
    if (l.requestsPerMinute) parts.push("req/min " + l.requestsPerMinute);
    if (l.requestsPerDay) parts.push("req/day " + l.requestsPerDay);
    if (l.tokensPerDay) parts.push("tok/day " + l.tokensPerDay);
    if (l.tokensPerMonth) parts.push("tok/month " + l.tokensPerMonth);
    if (l.costPerDayUSD) parts.push("cost/day $" + l.costPerDayUSD);
    if (l.costPerMonthUSD) parts.push("cost/month $" + l.costPerMonthUSD);
    return parts.length ? parts.join(", ") : "none";
  }

  function setRows(tableId, rows, buildRow) {
    var tbody = document.querySelector("#" + tableId + " tbody");
    while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
    if (!rows || rows.length === 0) {
      var tr = document.createElement("tr");
      tr.appendChild(el("td", "none", "muted"));
      tbody.appendChild(tr);
      return;
    }
    for (var i = 0; i < rows.length; i++) {
      tbody.appendChild(buildRow(rows[i]));
    }
  }

  function renderOverview(data) {
    setRows("providers", data.providers, function (p) {
      var tr = document.createElement("tr");
      tr.appendChild(el("td", p.name));
      tr.appendChild(el("td", p.type));
      tr.appendChild(el("td", p.baseUrl));
      tr.appendChild(el("td", String(p.modelCount)));
      tr.appendChild(el("td", p.lastRefresh && p.lastRefresh !== ZERO_TIME ? p.lastRefresh : "never"));
      tr.appendChild(el("td", p.lastErr || "", p.lastErr ? "err" : "muted"));
      return tr;
    });

    var infra = document.getElementById("infra");
    while (infra.firstChild) infra.removeChild(infra.firstChild);
    var redisText = "Redis: " + (data.redis.configured ? "configured" : "not configured");
    if (data.redis.lastErr) redisText += " - last error" + fmtAgo(data.redis.lastErrAt) + ": " + data.redis.lastErr;
    infra.appendChild(el("div", redisText, "infra-line"));
    var cacheText = "Cache: " + (data.cache.enabled ? "enabled (ttl " + data.cache.ttl + ")" : "disabled");
    infra.appendChild(el("div", cacheText, "infra-line"));
    infra.appendChild(el("div", "Version: " + data.version, "infra-line muted"));

    groupMeta = {};
    (data.groups || []).forEach(function (g) {
      groupMeta[g.name] = g;
    });
  }

  function renderUsageTable(tableId, entries, withGroupColumn) {
    setRows(tableId, entries, function (entry) {
      var tr = document.createElement("tr");
      if (withGroupColumn) {
        tr.appendChild(el("td", entry.id));
        tr.appendChild(el("td", entry.groupName || ""));
      } else {
        var meta = groupMeta[entry.id] || {};
        tr.appendChild(el("td", entry.id));
        tr.appendChild(el("td", meta.memberCount !== undefined ? String(meta.memberCount) : ""));
      }
      tr.appendChild(el("td", fmtLimits(entry.limits)));
      tr.appendChild(el("td", entry.storeDown ? "?" : String(entry.requestsPerMinute)));
      tr.appendChild(el("td", entry.storeDown ? "?" : String(entry.requestsPerDay)));
      tr.appendChild(el("td", entry.storeDown ? "?" : String(entry.tokensPerDay)));
      tr.appendChild(el("td", entry.storeDown ? "?" : String(entry.tokensPerMonth)));
      tr.appendChild(el("td", entry.storeDown ? "?" : fmtCost(entry.costPerDayMicroUsd)));
      tr.appendChild(el("td", entry.storeDown ? "?" : fmtCost(entry.costPerMonthMicroUsd)));
      if (entry.storeDown) tr.className = "err";
      return tr;
    });
  }

  function renderUsage(data) {
    renderUsageTable("groups", data.groups || [], false);
    renderUsageTable("users", data.users || [], true);
  }

  function setStatus(text, isErr) {
    var s = document.getElementById("status-line");
    s.textContent = text;
    s.className = isErr ? "err" : "";
  }

  // showAuthGate switches to the key-entry view. message is OPTIONAL
  // (review sweep, 2026-08-20): passed with a string, it replaces the
  // auth-error text; passed as undefined (refresh's own no-stored-key
  // branch), it leaves whatever error text is already there untouched —
  // otherwise a 5s poll running while no key is stored (the state right
  // after a 401 already showed "invalid key, or not an admin") would call
  // showAuthGate() on every tick and silently wipe that message back to
  // empty before the user ever reads it.
  function showAuthGate(message) {
    document.getElementById("dashboard").classList.add("hidden");
    document.getElementById("auth-gate").classList.remove("hidden");
    if (message !== undefined) {
      document.getElementById("auth-error").textContent = message;
    }
  }

  function showDashboard() {
    document.getElementById("auth-gate").classList.add("hidden");
    document.getElementById("dashboard").classList.remove("hidden");
    document.getElementById("auth-error").textContent = "";
  }

  // fetchJSON captures the key it was actually sent with (key) onto a
  // thrown auth error, rather than reading getStoredKey() again at the
  // catch site: refresh's own Promise.all can still have this request's
  // 401 in flight after the user has already submitted a newer key
  // (setStoredKey then a fresh refresh() call) — capturing at throw time
  // lets the catch handler tell that late, stale-key failure apart from a
  // genuine rejection of the key currently stored.
  function fetchJSON(url) {
    var key = getStoredKey();
    var headers = key ? { "x-api-key": key } : {};
    return fetch(url, { credentials: "same-origin", headers: headers }).then(function (resp) {
      if (resp.status === 401 || resp.status === 403) {
        var err = new Error("admin key rejected: HTTP " + resp.status);
        err.authFailed = true;
        err.key = key;
        throw err;
      }
      if (!resp.ok) throw new Error(url + ": HTTP " + resp.status);
      return resp.json();
    });
  }

  function refresh() {
    if (!getStoredKey()) {
      showAuthGate();
      return;
    }
    Promise.all([fetchJSON(OVERVIEW_URL), fetchJSON(USAGE_URL)]).then(function (results) {
      showDashboard();
      renderOverview(results[0]);
      renderUsage(results[1]);
      setStatus("last updated " + new Date().toLocaleTimeString(), false);
    }).catch(function (err) {
      if (err.authFailed) {
        // Guard against the late-401 race (review sweep, 2026-08-20): only
        // clear the stored key and show the rejection when the FAILING
        // request's own key still matches what is currently stored. A
        // stale in-flight request for a key the user has since replaced
        // must not clobber the newer key that may well be valid.
        if (err.key === getStoredKey()) {
          clearStoredKey();
          showAuthGate("invalid key, or not an admin");
        }
        return;
      }
      setStatus("refresh failed: " + err.message, true);
    });
  }

  document.getElementById("auth-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var input = document.getElementById("auth-key-input");
    var val = input.value;
    input.value = "";
    if (!val) return;
    document.getElementById("auth-error").textContent = "";
    setStoredKey(val);
    refresh();
  });

  refresh();
  setInterval(refresh, POLL_MS);
})();
</script>
</body>
</html>
`
