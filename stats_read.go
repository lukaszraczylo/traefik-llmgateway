package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// adminUsageSeriesPath, adminUsageTotalsPath and adminPerformancePath are
// GET /admin/api/usage/series, GET /admin/api/usage/totals and GET
// /admin/api/performance's routes (admin dashboard redesign, WP-B) —
// defined here, alongside the rest of these features' own constants,
// mirroring adminEventsPath's own precedent (events.go).
const (
	adminUsageSeriesPath = "/admin/api/usage/series"
	adminUsageTotalsPath = "/admin/api/usage/totals"
	adminPerformancePath = "/admin/api/performance"
)

// adminMaxKeysPerRequest is the hard cap on how many counterStore keys any
// single stats_read.go endpoint may read in one request (DECISIONS Q8): a
// caller whose scope/span/offset/metric combination would read more than
// this many keys gets 400 "range too large; narrow span or filter"
// instead of an unbounded pipeline against the shared store.
const adminMaxKeysPerRequest = 64000

// adminSeriesMaxScopes bounds GET /admin/api/usage/series' repeated
// "scope" query parameter (plan §1.3(iv): "1..100").
const adminSeriesMaxScopes = 100

// parseStatsOffset parses an optional "offset" query parameter shared by
// GET /admin/api/usage/models (admin.go), .../usage/series, .../usage/
// totals and .../performance: empty means 0 (the current, most-recent
// span — every endpoint's own pre-offset behavior), otherwise an integer
// between 0 and window's historyMaxSpan minus the already-validated span,
// inclusive — span+offset never exceeds what that window's TTL could ever
// have kept alive (historyMaxSpan's own doc comment, admin.go). ok is
// false for anything else.
func parseStatsOffset(raw, window string, span int) (offset int, ok bool) {
	if raw == "" {
		return 0, true
	}
	max := historyMaxSpan(window) - span
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > max {
		return 0, false
	}
	return n, true
}

// readSpanTotals reads, for every id in ids (all sharing kind), each
// metric in metrics summed over span buckets ending offset window-steps
// back from now, keyed by id then metric name. P6/P12 fix (admin
// dashboard redesign verify round): this used to build its own single
// unbounded storeGetMulti pipeline for the whole batch; it now delegates
// to limits.go's spanTotalsMulti — the ONE production span-sum reader
// admin.go's chunkedModelSpanTotals/chunkedModelSpanTotalsMulti also
// call — which chunks every read at usageModelsChunkKeys keys per round
// trip. ok is false on any store read failure or a fail-open outage in
// progress (configuredStoreDown), exactly like every other admin stats
// reader in this package — spanTotalsMulti itself already applies that
// check per chunk.
func (g *Gateway) readSpanTotals(kind string, ids, metrics []string, window string, now time.Time, span, offset int) (map[string]map[string]int64, bool) {
	out := make(map[string]map[string]int64, len(ids))
	if len(ids) == 0 || len(metrics) == 0 {
		return out, true
	}
	totalsByMetric, ok := g.limiter.spanTotalsMulti(kind, ids, metrics, window, now, span, offset)
	if !ok {
		return nil, false
	}
	for i, id := range ids {
		m := make(map[string]int64, len(metrics))
		for mi, metric := range metrics {
			m[metric] = totalsByMetric[mi][i]
		}
		out[id] = m
	}
	return out, true
}

// ---------------------------------------------------------------------
// GET /admin/api/usage/series
// ---------------------------------------------------------------------

// adminSeriesView is one requested scope's bucketed series in GET
// /admin/api/usage/series.
type adminSeriesView struct {
	Scope  string  `json:"scope"`
	Points []int64 `json:"points"`
}

// adminSeriesResponse is the full body of GET /admin/api/usage/series.
type adminSeriesResponse struct {
	Metric  string            `json:"metric"`
	Window  string            `json:"window"`
	Buckets []string          `json:"buckets"` // oldest first
	Series  []adminSeriesView `json:"series"`
	Unknown []string          `json:"unknown"` // never nil
	Span    int               `json:"span"`
	Offset  int               `json:"offset"`
}

// parseSeriesScope parses one GET /admin/api/usage/series "scope" value:
// "total", "user:{id}", "group:{id}", "model:{canonical}",
// "provider:{name}" or "provmodel:{canonical}". ok is false for anything
// else.
func parseSeriesScope(raw string) (kind, id string, ok bool) {
	if raw == totalScopeKind {
		return totalScopeKind, totalScopeID, true
	}
	prefix, rest, found := strings.Cut(raw, ":")
	if !found || rest == "" {
		return "", "", false
	}
	switch prefix {
	case "user", "group":
		return prefix, rest, true
	case "model":
		return kindModel, rest, true
	case "provider":
		return kindProvider, rest, true
	case "provmodel":
		return kindProviderModel, rest, true
	default:
		return "", "", false
	}
}

// seriesScopeExists extends scopeExists (admin.go) to also validate a
// kindProvider or kindProviderModel scope against the live registry —
// the two additional scope kinds GET /admin/api/usage/series accepts
// that GET /admin/api/usage/history (scopeExists' only other caller)
// never did.
func (g *Gateway) seriesScopeExists(kind, id string) bool {
	switch kind {
	case kindProvider:
		for _, s := range g.registry.snapshot() {
			if s.name == id {
				return true
			}
		}
		return false
	case kindProviderModel:
		return g.scopeExists(kindModel, id)
	default:
		return g.scopeExists(kind, id)
	}
}

// seriesValidMetric reports whether metric is a valid reading for
// scopeKind at window — plan §1.3(iv)'s metric x scope-kind matrix.
func seriesValidMetric(scopeKind, metric, window string) bool {
	hourOrDay := window == windowHour || window == windowDay
	switch scopeKind {
	case "user", "group", totalScopeKind:
		switch metric {
		case metricReq, metricTokIn, metricTokOut, metricCost:
			return true
		case metricRej:
			return hourOrDay
		}
		if scopeKind == totalScopeKind {
			switch metric {
			case metricChit, metricCmiss, metricCsave, metricR402:
				return hourOrDay
			}
		}
		return false
	case kindModel:
		switch metric {
		case metricReq, metricTokIn, metricTokOut, metricCost:
			return true
		case metricChit, metricCsave, metricR402:
			return window == windowDay
		}
		return false
	case kindProvider:
		switch metric {
		case metricProvAttempt, metricProvFail, metricProvTimeout, metricFover:
			return hourOrDay
		}
		return false
	case kindProviderModel:
		switch metric {
		case metricProvAttempt, metricProvFail:
			return hourOrDay
		}
		return false
	default:
		return false
	}
}

// serveAdminUsageSeries writes GET /admin/api/usage/series: one or more
// scopes' bucketed history for a single metric/window/span/offset, in one
// batched store read. Every parameter is validated before the store is
// touched (400 for anything malformed or out of range); an unknown scope
// id is skipped and listed in Unknown rather than failing the whole
// request, mirroring the free-models ranking's own "skip, don't fail"
// convention for an individual bad element within an otherwise valid
// request.
func (g *Gateway) serveAdminUsageSeries(sw *statusTrackingWriter, r *http.Request) {
	q := r.URL.Query()

	rawScopes := q["scope"]
	if len(rawScopes) == 0 {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "at least one scope is required")
		return
	}
	if len(rawScopes) > adminSeriesMaxScopes {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "too many scopes")
		return
	}

	window := q.Get("window")
	if !validHistoryWindow(window) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown window")
		return
	}
	metric := q.Get("metric")
	if !validHistoryMetric(metric) && !isKnownStatsMetric(metric) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown metric")
		return
	}
	span, ok := parseHistorySpan(q.Get("span"), window)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid span")
		return
	}
	offset, ok := parseStatsOffset(q.Get("offset"), window, span)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid offset")
		return
	}

	type parsedScope struct {
		raw, kind, id string
	}
	parsed := make([]parsedScope, 0, len(rawScopes))
	unknown := []string{}
	for _, raw := range rawScopes {
		kind, id, ok := parseSeriesScope(raw)
		if !ok {
			unknown = append(unknown, raw)
			continue
		}
		if !seriesValidMetric(kind, metric, window) {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown metric for scope kind")
			return
		}
		if kind != totalScopeKind && !g.seriesScopeExists(kind, id) {
			unknown = append(unknown, raw)
			continue
		}
		parsed = append(parsed, parsedScope{raw: raw, kind: kind, id: id})
	}

	if len(parsed)*span > adminMaxKeysPerRequest {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
		return
	}

	now := g.limiter.now()
	var buckets []string
	series := make([]adminSeriesView, 0, len(parsed))
	allKeys := make([]string, 0, len(parsed)*span)
	for _, p := range parsed {
		keys, bkts := historyBucketKeysAt(p.kind, p.id, metric, window, now, span, offset)
		if buckets == nil {
			buckets = bkts
		}
		allKeys = append(allKeys, keys...)
	}

	// P6 fix (admin dashboard redesign verify round): chunked at
	// usageModelsChunkKeys keys per round trip (chunkedGetMulti, limits.go)
	// instead of one unbounded pipeline covering every requested scope's
	// whole span at once — a 100-scope request at span=48 used to build a
	// single 4,800-key pipeline in one call.
	vals, storeOK := g.limiter.chunkedGetMulti(allKeys, usageModelsChunkKeys)
	if !storeOK || len(vals) != len(allKeys) {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
		return
	}
	for i, p := range parsed {
		points := append([]int64(nil), vals[i*span:i*span+span]...)
		series = append(series, adminSeriesView{Scope: p.raw, Points: points})
	}
	if buckets == nil {
		buckets = []string{}
	}

	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminSeriesResponse{
		Metric: metric, Window: window, Span: span, Offset: offset,
		Buckets: buckets, Series: series, Unknown: unknown,
	})
}

// isKnownStatsMetric reports whether metric is one of the admin-redesign
// counter families' metric names (fover/timeout/r402/chit/cmiss/csave/
// attempt/fail) — validHistoryMetric (admin.go) only ever knew the four
// original req/tokin/tokout/cost metrics, so GET /admin/api/usage/series
// and GET /admin/api/usage/totals — the two endpoints that also read
// these newer families — check both.
func isKnownStatsMetric(metric string) bool {
	switch metric {
	case metricRej, metricProvAttempt, metricProvFail, metricProvTimeout, metricFover,
		metricR402, metricChit, metricCmiss, metricCsave:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------
// GET /admin/api/usage/totals
// ---------------------------------------------------------------------

// adminTotalsRow is one entity's metric values in GET
// /admin/api/usage/totals. ID's shape depends on Kind: a plain name for
// user/group, a canonical "provider/model" id for usermodel, a user name
// for modeluser, or "mcp/{target}/{caller}"/"agent/{target}/{caller}"
// for targetcaller (targetCallerScopeID's own format, limits.go).
type adminTotalsRow struct {
	Values map[string]int64 `json:"values"`
	ID     string           `json:"id"`
}

// adminTotalsResponse is the full body of GET /admin/api/usage/totals.
type adminTotalsResponse struct {
	Kind      string           `json:"kind"`
	Window    string           `json:"window"`
	Metrics   []string         `json:"metrics"`
	Rows      []adminTotalsRow `json:"rows"`
	Span      int              `json:"span"`
	Offset    int              `json:"offset"`
	Truncated bool             `json:"truncated,omitempty"`
}

const (
	adminTotalsDefaultLimit = 200
	adminTotalsMaxLimit     = 1000
)

// totalsKindMetrics enumerates every metric allowed for kind, in a fixed
// order — the default `metrics` list when a request omits that query
// parameter, and the set a caller's own comma list is validated against
// (plan §1.3(v): "usermodel/modeluser/targetcaller: day/month only;
// req/tokin/tokout/cost (targetcaller req only)").
func totalsKindMetrics(kind string) []string {
	switch kind {
	case "user", "group", "usermodel", "modeluser":
		return []string{metricReq, metricTokIn, metricTokOut, metricCost}
	case "targetcaller":
		return []string{metricReq}
	default:
		return nil
	}
}

// totalsValidWindow reports whether window is valid for kind: usermodel/
// modeluser/targetcaller accept only day/month (plan §1.3(v)); user/group
// accept any of hour/day/month (validHistoryWindow, admin.go).
func totalsValidWindow(kind, window string) bool {
	switch kind {
	case "usermodel", "modeluser", "targetcaller":
		return window == windowDay || window == windowMonth
	default:
		return validHistoryWindow(window)
	}
}

// parseTotalsMetrics parses GET /admin/api/usage/totals' optional
// "metrics" query parameter: empty means every metric totalsKindMetrics
// allows for kind, in that fixed order; otherwise a comma-separated list,
// each entry one of those allowed metrics, no duplicates. ok is false for
// an unknown metric, a duplicate, or an empty list.
func parseTotalsMetrics(raw, kind string) (metrics []string, ok bool) {
	allowed := totalsKindMetrics(kind)
	if raw == "" {
		return allowed, true
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, m := range allowed {
		allowedSet[m] = true
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !allowedSet[p] || seen[p] {
			return nil, false
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, true
}

// parseTotalsLimit parses GET /admin/api/usage/totals' optional "limit"
// query parameter: empty means adminTotalsDefaultLimit, otherwise an
// integer between 1 and adminTotalsMaxLimit inclusive.
func parseTotalsLimit(raw string) (limit int, ok bool) {
	if raw == "" {
		return adminTotalsDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > adminTotalsMaxLimit {
		return 0, false
	}
	return n, true
}

// parseTargetRef parses a "target" query value ("mcp/{name}" or
// "agent/{name}") into its kind ("mcp"|"agent") and name.
func parseTargetRef(raw string) (kind, name string, ok bool) {
	k, n, found := strings.Cut(raw, "/")
	if !found || n == "" {
		return "", "", false
	}
	if k != "mcp" && k != "agent" {
		return "", "", false
	}
	return k, n, true
}

// stringSliceContains reports whether s contains v.
func stringSliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// totalsCandidate is one row-to-be, before the drop-all-zero/sort/limit
// pass buildTotalsRows applies.
type totalsCandidate struct {
	values map[string]int64
	id     string
}

// buildTotalsRows drops every candidate whose every requested metric is
// zero, sorts the rest by metrics[0] descending then id ascending (plan
// §1.3(v): "Drop all-zero rows; sort by first metric desc then id"), and
// truncates to limit, reporting whether it did.
func buildTotalsRows(candidates []totalsCandidate, metrics []string, limit int) ([]adminTotalsRow, bool) {
	rows := make([]adminTotalsRow, 0, len(candidates))
	for _, c := range candidates {
		allZero := true
		for _, m := range metrics {
			if c.values[m] != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			continue
		}
		rows = append(rows, adminTotalsRow{ID: c.id, Values: c.values})
	}
	first := metrics[0]
	sort.Slice(rows, func(i, j int) bool {
		vi, vj := rows[i].Values[first], rows[j].Values[first]
		if vi != vj {
			return vi > vj
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return rows, truncated
}

// serveAdminUsageTotals writes GET /admin/api/usage/totals: ranked
// per-entity metric totals for one of five kinds (plan §1.3(v)).
// usermodel/modeluser additionally 404 when admin.stats.userModel is off
// (parity with GET /admin/api/usage/models' own detail extras).
func (g *Gateway) serveAdminUsageTotals(sw *statusTrackingWriter, r *http.Request) {
	q := r.URL.Query()

	kind := q.Get("kind")
	switch kind {
	case "user", "group", "usermodel", "modeluser", "targetcaller":
	default:
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown kind")
		return
	}

	window := q.Get("window")
	if window == "" {
		window = windowDay
	}
	if !totalsValidWindow(kind, window) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown window")
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
	metrics, ok := parseTotalsMetrics(q.Get("metrics"), kind)
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid metrics")
		return
	}
	limit, ok := parseTotalsLimit(q.Get("limit"))
	if !ok {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid limit")
		return
	}

	userParam := q.Get("user")
	if kind == "usermodel" && userParam == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "user is required for kind=usermodel")
		return
	}
	modelParam := q.Get("model")
	if kind == "modeluser" && modelParam == "" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "model is required for kind=modeluser")
		return
	}
	var targetKind, targetName string
	if raw := q.Get("target"); kind == "targetcaller" && raw != "" {
		targetKind, targetName, ok = parseTargetRef(raw)
		if !ok {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid target")
			return
		}
	}

	// Feature gate checked LAST, after every parameter is already known
	// valid (plan §1.3's intro paragraph: "validate params first... 404
	// ...statistics are not enabled" — a malformed request must always
	// answer 400 for the thing that is actually wrong with it, never a
	// 404 that reads as "this whole kind doesn't exist" when the real
	// problem is a missing/invalid parameter the caller can fix on its
	// own regardless of whether the feature is enabled).
	if (kind == "usermodel" || kind == "modeluser") && !g.buildAdminFeatures().UserModelStats {
		writeOAIError(sw, http.StatusNotFound, "invalid_request_error", "per-user-model statistics are not enabled (admin.stats.userModel)")
		return
	}

	now := g.limiter.now()
	userSummaries, groupSummaries := g.auth.snapshot()

	var candidates []totalsCandidate
	var storeOK bool

	switch kind {
	case "user":
		ids := make([]string, 0, len(userSummaries))
		for _, us := range userSummaries {
			if grp := q.Get("group"); grp != "" && !stringSliceContains(us.groups, grp) {
				continue
			}
			ids = append(ids, us.name)
		}
		if len(ids)*len(metrics)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		var raw map[string]map[string]int64
		raw, storeOK = g.readSpanTotals("user", ids, metrics, window, now, span, offset)
		candidates = make([]totalsCandidate, len(ids))
		for i, id := range ids {
			candidates[i] = totalsCandidate{id: id, values: raw[id]}
		}

	case "group":
		ids := make([]string, len(groupSummaries))
		for i, gs := range groupSummaries {
			ids[i] = gs.name
		}
		if len(ids)*len(metrics)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		var raw map[string]map[string]int64
		raw, storeOK = g.readSpanTotals("group", ids, metrics, window, now, span, offset)
		candidates = make([]totalsCandidate, len(ids))
		for i, id := range ids {
			candidates[i] = totalsCandidate{id: id, values: raw[id]}
		}

	case "usermodel":
		// Cheap phase (plan §1.3(v)): narrow the full catalog down to
		// models with ANY recent usage at all, via a single-metric read
		// over covering month buckets, before ever touching a per-user
		// umodel counter — the catalog can run to hundreds of models,
		// while a real user typically used only a handful of them.
		modelIDs := g.catalogModelScopeIDs()
		coverSpan := historyMaxSpan(windowMonth)
		if len(modelIDs)*coverSpan > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		cheap, cheapOK := g.readSpanTotals(kindModel, modelIDs, []string{metricReq}, windowMonth, now, coverSpan, 0)
		if !cheapOK {
			writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
			return
		}
		var nonzero []string
		for _, id := range modelIDs {
			if cheap[id][metricReq] != 0 {
				nonzero = append(nonzero, id)
			}
		}
		umodelIDs := make([]string, len(nonzero))
		for i, canonical := range nonzero {
			umodelIDs[i] = userModelScopeID(userParam, canonical)
		}
		if len(umodelIDs)*len(metrics)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		var raw map[string]map[string]int64
		raw, storeOK = g.readSpanTotals(kindUserModel, umodelIDs, metrics, window, now, span, offset)
		candidates = make([]totalsCandidate, len(nonzero))
		for i, canonical := range nonzero {
			candidates[i] = totalsCandidate{id: canonical, values: raw[umodelIDs[i]]}
		}

	case "modeluser":
		umodelIDs := make([]string, len(userSummaries))
		for i, us := range userSummaries {
			umodelIDs[i] = userModelScopeID(us.name, modelParam)
		}
		if len(umodelIDs)*len(metrics)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		var raw map[string]map[string]int64
		raw, storeOK = g.readSpanTotals(kindUserModel, umodelIDs, metrics, window, now, span, offset)
		candidates = make([]totalsCandidate, len(userSummaries))
		for i, us := range userSummaries {
			candidates[i] = totalsCandidate{id: us.name, values: raw[umodelIDs[i]]}
		}

	case "targetcaller":
		type targetRef struct{ kind, name string }
		var targets []targetRef
		if targetName != "" {
			targets = []targetRef{{targetKind, targetName}}
		} else {
			for _, name := range sortedMCPServerNames(g.cfg.MCPServers) {
				targets = append(targets, targetRef{"mcp", name})
			}
			for _, name := range sortedAgentNames(g.cfg.Agents) {
				targets = append(targets, targetRef{"agent", name})
			}
		}
		ids := make([]string, 0, len(targets)*len(userSummaries))
		for _, t := range targets {
			for _, us := range userSummaries {
				ids = append(ids, targetCallerScopeID(t.kind, t.name, us.name))
			}
		}
		if len(ids)*len(metrics)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		var raw map[string]map[string]int64
		raw, storeOK = g.readSpanTotals(kindTargetCaller, ids, metrics, window, now, span, offset)
		candidates = make([]totalsCandidate, len(ids))
		for i, id := range ids {
			candidates[i] = totalsCandidate{id: id, values: raw[id]}
		}
	}

	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
		return
	}

	rows, truncated := buildTotalsRows(candidates, metrics, limit)
	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminTotalsResponse{
		Kind: kind, Window: window, Metrics: metrics, Rows: rows,
		Span: span, Offset: offset, Truncated: truncated,
	})
}

// ---------------------------------------------------------------------
// GET /admin/api/performance
// ---------------------------------------------------------------------

// adminPerfMaxIDs bounds GET /admin/api/performance's repeated "id" query
// parameter (plan §1.3(vi): "<=50").
const adminPerfMaxIDs = 50

// adminPerfRow is one entity's (provider's or model's) availability and
// latency summary in GET /admin/api/performance, over the requested span.
// The five *Ms percentile fields and Count are all latency-derived and
// stay at their zero value (nil pointers, Count 0) when latency stats are
// disabled (LatencyEnabled false) or this entity has no latency
// observations at all in the span; Attempts/Failures[/Timeouts/
// Failovers] are always populated regardless.
type adminPerfRow struct {
	P50Ms     *float64 `json:"p50Ms,omitempty"`
	P95Ms     *float64 `json:"p95Ms,omitempty"`
	P99Ms     *float64 `json:"p99Ms,omitempty"`
	TTFBP50Ms *float64 `json:"ttfbP50Ms,omitempty"`
	TTFBP95Ms *float64 `json:"ttfbP95Ms,omitempty"`
	ID        string   `json:"id"`
	Count     int64    `json:"count"`
	Attempts  int64    `json:"attempts"`
	Failures  int64    `json:"failures"`
	// Timeouts and Failovers are populated only for kind=provider — a
	// per-(provider,model) scope tracks neither (plan §1.2's family
	// table: prov/{name} only) — so they stay 0 (omitted) for kind=model.
	Timeouts  int64 `json:"timeouts,omitempty"`
	Failovers int64 `json:"failovers,omitempty"`
	Overflow  bool  `json:"overflow,omitempty"`
}

// adminPerfResponse is the full body of GET /admin/api/performance.
// Rows is populated for the default (series=0) aggregate mode — one row
// per entity, summed over the whole span. Buckets/Points are populated
// only for series=1 (exactly one id) instead: one adminPerfRow per
// bucket, ID set to that bucket's own label. Rows is ALWAYS present and
// NEVER nil (an empty array `[]`, never `null`, in series=1 mode or when
// there are zero rows to report) — every other admin stats endpoint's own
// array-typed field (adminSeriesResponse.Unknown, adminUsageModelsResponse
// .Models, ...) already carries this same "never nil" contract, and a
// client that always does `for (const r of resp.rows)` without a null
// check must never see this one field be the exception.
type adminPerfResponse struct {
	Kind           string         `json:"kind"`
	Window         string         `json:"window"`
	Rows           []adminPerfRow `json:"rows"`
	Buckets        []string       `json:"buckets,omitempty"`
	Points         []adminPerfRow `json:"points,omitempty"`
	Span           int            `json:"span"`
	Offset         int            `json:"offset"`
	LatencyEnabled bool           `json:"latencyEnabled"`
}

// performanceValidWindow reports whether window is one of GET
// /admin/api/performance's two accepted values (plan §1.3(vi): "window
// hour|day" — no month, unlike most other stats endpoints: a monthly
// latency percentile would smear together far too much variance to be
// actionable).
func performanceValidWindow(window string) bool {
	return window == windowHour || window == windowDay
}

// latencyMetricNames returns every latencyDurationMetric/
// latencyTTFBMetric name (limits.go) for the full bucket range: indices
// 0..len(latencyBucketBounds)-1 for each real bound, plus
// len(latencyBucketBounds) itself for the overflow bucket — matching
// latencyBucketIndex's own return range (metrics.go, WP-A).
func latencyMetricNames() []string {
	n := len(latencyBucketBounds) + 1
	names := make([]string, 0, 2*n)
	for i := 0; i < n; i++ {
		names = append(names, latencyDurationMetric(i))
	}
	for i := 0; i < n; i++ {
		names = append(names, latencyTTFBMetric(i))
	}
	return names
}

// histogramQuantile computes the linearly-interpolated q-quantile (0..1)
// of a dense latency histogram: bounds are latencyBucketBounds' own
// ascending bucket upper bounds, in SECONDS; counts has len(bounds)+1
// entries — counts[i] is bounds[i]'s own dense occurrence count for
// i < len(bounds), and counts[len(bounds)] is the overflow bucket (every
// observation past the largest bound). Returns ms (the quantile value in
// MILLISECONDS) and overflow=true when the computed rank falls in the
// overflow bucket, in which case ms is the largest bound in ms — the best
// lower bound available, since an overflow observation's true value is
// unknown past that point (plan §1.3(vi): "overflow bucket -> 600000 +
// overflow:true"). (0, false) when counts holds no observations at all.
func histogramQuantile(bounds []float64, counts []int64, q float64) (ms float64, overflow bool) {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0, false
	}
	target := q * float64(total)
	var cum float64
	lower := 0.0
	for i, bound := range bounds {
		c := float64(counts[i])
		if cum+c >= target {
			if c <= 0 {
				return bound * 1000, false
			}
			frac := (target - cum) / c
			return (lower + frac*(bound-lower)) * 1000, false
		}
		cum += c
		lower = bound
	}
	return bounds[len(bounds)-1] * 1000, true
}

// perfScopeKinds resolves the counterStore scope kind for a performance
// entity's core availability counters (attempt/fail[/timeout/fover]) and
// its latency buckets, given the endpoint's own "kind" query value
// ("provider"|"model"): a provider entity reads both families under
// kindProvider (id = provider name); a model entity reads its core
// counters under kindProviderModel (attempt/fail only — plan §1.2's
// family table has no per-model timeout/fover) and its latency buckets
// under kindModel (id = canonical "provider/model" either way).
func perfScopeKinds(kind string) (coreKind, latencyKind string) {
	if kind == "model" {
		return kindProviderModel, kindModel
	}
	return kindProvider, kindProvider
}

// buildPerfRow assembles one adminPerfRow from its already-read core
// counters (core, never nil) and latency bucket counts (lat, nil when
// latency stats are disabled or unread).
func buildPerfRow(id, coreKind string, core, lat map[string]int64, latencyEnabled bool) adminPerfRow {
	row := adminPerfRow{
		ID:       id,
		Attempts: core[metricProvAttempt],
		Failures: core[metricProvFail],
	}
	if coreKind == kindProvider {
		row.Timeouts = core[metricProvTimeout]
		row.Failovers = core[metricFover]
	}
	if !latencyEnabled || lat == nil {
		return row
	}

	n := len(latencyBucketBounds) + 1
	durationCounts := make([]int64, n)
	ttfbCounts := make([]int64, n)
	var durTotal, ttfbTotal int64
	for i := 0; i < n; i++ {
		durationCounts[i] = lat[latencyDurationMetric(i)]
		ttfbCounts[i] = lat[latencyTTFBMetric(i)]
		durTotal += durationCounts[i]
		ttfbTotal += ttfbCounts[i]
	}
	row.Count = durTotal

	var overflow bool
	if durTotal > 0 {
		p50, of := histogramQuantile(latencyBucketBounds, durationCounts, 0.5)
		row.P50Ms = &p50
		overflow = overflow || of
		p95, of := histogramQuantile(latencyBucketBounds, durationCounts, 0.95)
		row.P95Ms = &p95
		overflow = overflow || of
		p99, of := histogramQuantile(latencyBucketBounds, durationCounts, 0.99)
		row.P99Ms = &p99
		overflow = overflow || of
	}
	if ttfbTotal > 0 {
		t50, of := histogramQuantile(latencyBucketBounds, ttfbCounts, 0.5)
		row.TTFBP50Ms = &t50
		overflow = overflow || of
		t95, of := histogramQuantile(latencyBucketBounds, ttfbCounts, 0.95)
		row.TTFBP95Ms = &t95
		overflow = overflow || of
	}
	row.Overflow = overflow
	return row
}

// serveAdminPerformance writes GET /admin/api/performance: provider or
// model availability + (opt-in) latency percentiles, aggregated over the
// requested span (series=0, the default) or bucket-by-bucket for exactly
// one id (series=1).
func (g *Gateway) serveAdminPerformance(sw *statusTrackingWriter, r *http.Request) {
	q := r.URL.Query()

	kind := q.Get("kind")
	if kind != "provider" && kind != "model" {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown kind")
		return
	}
	window := q.Get("window")
	if !performanceValidWindow(window) {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "unknown window")
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
	rawIDs := q["id"]
	if len(rawIDs) > adminPerfMaxIDs {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "too many ids")
		return
	}
	seriesRaw := q.Get("series")
	var series bool
	switch seriesRaw {
	case "", "0":
		series = false
	case "1":
		series = true
	default:
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "invalid series")
		return
	}
	if series && len(rawIDs) != 1 {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "series requires exactly one id")
		return
	}

	now := g.limiter.now()
	coreKind, latencyKind := perfScopeKinds(kind)
	latencyEnabled := g.buildAdminFeatures().LatencyStats

	var ids []string
	if len(rawIDs) > 0 {
		for _, id := range rawIDs {
			if kind == "provider" && g.seriesScopeExists(kindProvider, id) {
				ids = append(ids, id)
			} else if kind == "model" && g.scopeExists(kindModel, id) {
				ids = append(ids, id)
			}
		}
	} else if kind == "provider" {
		for _, s := range g.registry.snapshot() {
			ids = append(ids, s.name)
		}
	} else {
		// Default: top 50 by request count in span (plan §1.3(vi)).
		catalog := g.catalogModelScopeIDs()
		// P7 fix (admin dashboard redesign verify round): this cap used to
		// check len(catalog) alone, ignoring span entirely — the actual
		// read below is len(catalog)*span keys (one metricReq bucket per
		// model per span step), so a wide catalog combined with a large
		// span could read far more than adminMaxKeysPerRequest ever meant
		// to allow through before this 400 ever triggered.
		if len(catalog)*span > adminMaxKeysPerRequest {
			writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
			return
		}
		totals, storeOK := g.chunkedModelSpanTotals(catalog, metricReq, window, now, span, offset)
		if !storeOK {
			writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
			return
		}
		type ranked struct {
			id  string
			req int64
		}
		var nonzero []ranked
		for i, id := range catalog {
			if totals[i] > 0 {
				nonzero = append(nonzero, ranked{id: id, req: totals[i]})
			}
		}
		sort.Slice(nonzero, func(i, j int) bool {
			if nonzero[i].req != nonzero[j].req {
				return nonzero[i].req > nonzero[j].req
			}
			return nonzero[i].id < nonzero[j].id
		})
		if len(nonzero) > 50 {
			nonzero = nonzero[:50]
		}
		for _, rk := range nonzero {
			ids = append(ids, rk.id)
		}
	}

	// P1 fix (admin dashboard redesign verify round): the earlier
	// `len(rawIDs) != 1` check only bounds how many id= parameters the
	// CALLER sent — it says nothing about how many of them actually
	// resolved to a real provider/model above. An unknown or renamed id
	// (`series=1&id=nope`) resolves ids to an EMPTY slice, and ids[0]
	// below panicked (recovered as a 500 "internal error" by errors.go's
	// recoverPanic, but still a logged panic on every dashboard click
	// against a stale id) instead of the 400 this endpoint's own README
	// section already promised.
	if series && len(ids) != 1 {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "series requires exactly one id")
		return
	}

	if series {
		buckets, points, storeOK := g.perfSeries(coreKind, latencyKind, ids[0], window, now, span, offset, latencyEnabled)
		if !storeOK {
			writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
			return
		}
		setAdminJSONHeaders(sw)
		_ = json.NewEncoder(sw).Encode(adminPerfResponse{
			Kind: kind, Window: window, Span: span, Offset: offset,
			// Rows stays non-nil even though series=1 never populates it —
			// see adminPerfResponse's own "never nil" doc comment.
			Rows: []adminPerfRow{}, Buckets: buckets, Points: points, LatencyEnabled: latencyEnabled,
		})
		return
	}

	coreMetrics := []string{metricProvAttempt, metricProvFail}
	if coreKind == kindProvider {
		coreMetrics = append(coreMetrics, metricProvTimeout, metricFover)
	}
	estimateKeys := len(ids) * len(coreMetrics) * span
	if latencyEnabled {
		estimateKeys += len(ids) * len(latencyMetricNames()) * span
	}
	if estimateKeys > adminMaxKeysPerRequest {
		writeOAIError(sw, http.StatusBadRequest, "invalid_request_error", "range too large; narrow span or filter")
		return
	}

	coreVals, storeOK := g.readSpanTotals(coreKind, ids, coreMetrics, window, now, span, offset)
	if !storeOK {
		writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
		return
	}
	var latVals map[string]map[string]int64
	if latencyEnabled {
		latVals, storeOK = g.readSpanTotals(latencyKind, ids, latencyMetricNames(), window, now, span, offset)
		if !storeOK {
			writeOAIError(sw, http.StatusServiceUnavailable, "server_error", "usage store unavailable")
			return
		}
	}

	rows := make([]adminPerfRow, len(ids))
	for i, id := range ids {
		rows[i] = buildPerfRow(id, coreKind, coreVals[id], latVals[id], latencyEnabled)
	}

	setAdminJSONHeaders(sw)
	_ = json.NewEncoder(sw).Encode(adminPerfResponse{
		Kind: kind, Window: window, Span: span, Offset: offset,
		Rows: rows, LatencyEnabled: latencyEnabled,
	})
}

// perfSeries reads bucket-by-bucket (never summed) core+latency counters
// for exactly one entity — GET /admin/api/performance's series=1 mode —
// returning span bucket labels and one adminPerfRow per bucket, ID set to
// that bucket's own label.
func (g *Gateway) perfSeries(coreKind, latencyKind, id, window string, now time.Time, span, offset int, latencyEnabled bool) (buckets []string, points []adminPerfRow, ok bool) {
	coreMetrics := []string{metricProvAttempt, metricProvFail}
	if coreKind == kindProvider {
		coreMetrics = append(coreMetrics, metricProvTimeout, metricFover)
	}
	var latMetrics []string
	if latencyEnabled {
		latMetrics = latencyMetricNames()
	}

	// P6 fix (admin dashboard redesign verify round): every metric used to
	// read via its own separate storeGetMulti round trip — a provider's
	// series=1 with latency on cost 4 core + 28 latency metrics = 32
	// sequential round trips. Every metric's keys are built up front into
	// ONE flat list instead, read through chunkedGetMulti's own bounded
	// pipelining (limits.go), and sliced back apart per metric below —
	// buckets come from the first metric's own key build since every
	// metric shares identical bucket labels for the same kind/id/window/
	// span/offset.
	allKeys := make([]string, 0, (len(coreMetrics)+len(latMetrics))*span)
	for _, m := range coreMetrics {
		keys, bkts := historyBucketKeysAt(coreKind, id, m, window, now, span, offset)
		if buckets == nil {
			buckets = bkts
		}
		allKeys = append(allKeys, keys...)
	}
	for _, m := range latMetrics {
		keys, _ := historyBucketKeysAt(latencyKind, id, m, window, now, span, offset)
		allKeys = append(allKeys, keys...)
	}

	vals, storeOK := g.limiter.chunkedGetMulti(allKeys, usageModelsChunkKeys)
	if !storeOK || len(vals) != len(allKeys) {
		return nil, nil, false
	}

	coreVals := make(map[string][]int64, len(coreMetrics))
	idx := 0
	for _, m := range coreMetrics {
		coreVals[m] = vals[idx : idx+span]
		idx += span
	}
	latVals := make(map[string][]int64, len(latMetrics))
	for _, m := range latMetrics {
		latVals[m] = vals[idx : idx+span]
		idx += span
	}

	points = make([]adminPerfRow, span)
	for b := 0; b < span; b++ {
		core := make(map[string]int64, len(coreMetrics))
		for _, m := range coreMetrics {
			core[m] = coreVals[m][b]
		}
		var lat map[string]int64
		if latencyEnabled {
			lat = make(map[string]int64, len(latMetrics))
			for _, m := range latMetrics {
				lat[m] = latVals[m][b]
			}
		}
		row := buildPerfRow(buckets[b], coreKind, core, lat, latencyEnabled)
		points[b] = row
	}
	return buckets, points, true
}
