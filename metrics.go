package traefikllmgateway

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
)

// MetricsConfig configures the Prometheus text-exposition endpoint
// (default path /metrics, overridable via Path). nil or Enabled false
// means the metrics route is not registered at all, matching Admin's own
// nil-disables convention (AdminConfig, llmgateway.go).
//
// This endpoint exposes per-user and per-group cost and usage, so it must
// never be reachable unauthenticated by default. Auth reuses the SAME
// admin-bearer-token check GET /admin/api/* already applies (auth.go's
// identify, gated on user.admin) — handleMetrics below is not a second
// credential path. AllowedCIDRs is an additional, OPTIONAL bypass for a
// caller whose SOURCE ADDRESS is trusted enough to skip the key entirely
// — a co-located Prometheus server that should not need to carry a
// credential — checked against the request's source address exactly as
// clientIP reads it elsewhere in this package (net/http's own
// RemoteAddr, never a client-supplied header, auth.go's own doc comment).
// This is only meaningful for a caller whose real source address
// actually survives to this gateway: behind a Kubernetes Service or a
// second reverse proxy that does not preserve the original address,
// every caller looks identical to this check, and the allowlist becomes
// either "everyone" or "no one" depending on what address arrives.
// Confirm RemoteAddr reflects the scraper's own address in your
// deployment before relying on this instead of a key.
//
// THE FAILURE MODE TO AVOID IS NOT "the allowlist does not work" — it is
// "the allowlist works exactly as configured, and what survives to
// RemoteAddr is an address you never intended to trust" (review fix,
// adversarial verification 2026-08-23, reproduced live): a whole RFC1918
// block such as 10.0.0.0/8 is never a correct entry, because a SNAT-ing
// router or load balancer makes EXTERNAL traffic arrive from an address
// inside that same private range — on the reproduction cluster,
// AllowedCIDRs: ["10.0.0.0/8"] paired with the router's own SNAT address
// returned full per-user cost data to an external caller with no key at
// all. Always scope this to the NARROWEST block that covers only your
// actual scrapers — a Prometheus/VictoriaMetrics pod CIDR such as
// 10.42.0.0/16, not the broad private range it happens to sit inside.
type MetricsConfig struct {
	// Path overrides the served path. Empty (the default) serves at
	// metricsPathDefault ("/metrics").
	Path string `json:"path,omitempty"`
	// AllowedCIDRs lists CIDR blocks whose source address may scrape
	// without an admin key — e.g. "10.42.0.0/16" for a cluster's pod
	// network, never a whole RFC1918 range like "10.0.0.0/8". Empty
	// means every scrape must carry a valid admin bearer token — see
	// this type's own doc comment for the trust assumption this relies
	// on, and for why the narrowest-possible block matters here.
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`
	Enabled      bool     `json:"enabled,omitempty"`
	// ModelLabel adds a `model` label to the provider attempt/failure
	// counters (llmgateway_provider_model_attempts_total/
	// llmgateway_provider_model_failures_total), breaking them out per
	// (provider, model) pair in addition to the always-on per-provider
	// totals. Defaults to false: a deployment with roughly 1,000
	// discovered model ids across several providers is a series-count
	// explosion waiting to happen, so this stays opt-in — enable only
	// when your Prometheus retention and cardinality budget can absorb
	// one series per (provider, model) pair actually seen.
	ModelLabel bool `json:"modelLabel,omitempty"`
}

// metricsPathDefault is where the Prometheus endpoint is served when
// MetricsConfig.Path is left empty.
const metricsPathDefault = "/metrics"

// metricsEnabled reports whether cfg's Metrics block is present and
// enabled — the single gate ServeHTTP checks before matching the metrics
// route at all, mirroring adminEnabled (admin.go).
func metricsEnabled(cfg *Config) bool {
	return cfg.Metrics != nil && cfg.Metrics.Enabled
}

// metricsPath returns the path the metrics route is served at: cfg.
// Metrics.Path when set, metricsPathDefault otherwise. Only meaningful
// when metricsEnabled(cfg).
func metricsPath(cfg *Config) string {
	if cfg.Metrics != nil && cfg.Metrics.Path != "" {
		return cfg.Metrics.Path
	}
	return metricsPathDefault
}

// parseMetricsCIDRs parses mc's AllowedCIDRs once, at construction, so
// the request path never re-parses a CIDR string per scrape. A malformed
// entry is a constructor error (validate-at-construction, matching
// LimitsConfig.validate's own convention, limits.go) rather than a
// silently-ignored allowlist entry an operator would only discover was
// never applied by testing it.
func parseMetricsCIDRs(mc *MetricsConfig) ([]*net.IPNet, error) {
	if mc == nil || len(mc.AllowedCIDRs) == 0 {
		return nil, nil
	}
	nets := make([]*net.IPNet, 0, len(mc.AllowedCIDRs))
	for _, raw := range mc.AllowedCIDRs {
		_, ipNet, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("llmgateway: metrics: invalid allowedCIDRs entry %q: %w", raw, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// handleMetrics implements GET <metrics path>: the Prometheus text-
// exposition endpoint. Gate order mirrors handleAdminAPI (admin.go)
// exactly — reusing the SAME admin bearer-token check, never a second
// credential path — with one addition checked FIRST: a source address
// inside Config.Metrics.AllowedCIDRs skips the key requirement entirely,
// so a co-located Prometheus server needs no credential. Checking the
// CIDR allowlist before ever looking at a presented key also means a
// keyless, allowlisted scrape never touches authStore's failed-attempt
// tracking (auth.go's identify records a failure on every call with no
// valid key) — an allowlisted scraper polling every 15s must not look
// like a sustained attack against itself.
func (g *Gateway) handleMetrics(sw *statusTrackingWriter, r *http.Request) {
	if g.metricsSourceAllowed(r) {
		g.serveMetrics(sw)
		return
	}
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
	g.serveMetrics(sw)
}

// metricsSourceAllowed reports whether r's source address (clientIP,
// auth.go) falls inside any of g.metricsNets, parsed once at
// construction from Config.Metrics.AllowedCIDRs (parseMetricsCIDRs).
// Always false when no CIDRs are configured.
func (g *Gateway) metricsSourceAllowed(r *http.Request) bool {
	if len(g.metricsNets) == 0 {
		return false
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	for _, n := range g.metricsNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// serveMetrics writes renderMetrics' result with the Prometheus text-
// exposition Content-Type. X-Content-Type-Options/Cache-Control mirror
// setAdminJSONHeaders' own reasoning (admin.go): this response is built
// fresh per request from live state and must never linger in a shared or
// disk cache, and its declared Content-Type must never be second-guessed
// by a client's MIME sniffer.
func (g *Gateway) serveMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(g.renderMetrics()) // headers already committed; nothing useful to do on write failure
}

// escapeLabelValue escapes s for use as a Prometheus label value inside
// double quotes in the text exposition format: backslash becomes \\,
// double quote becomes \", and newline becomes \n — the exact rule the
// format requires. An unescaped quote or embedded newline in a label
// value (a user or group name, a model id, an operator picks these) would
// otherwise emit an unparseable exposition document — and Prometheus
// reports an unparseable scrape as the WHOLE target being DOWN, not as
// one bad line, so this is a correctness requirement, not a formatting
// nicety.
//
// This walks s BYTE by byte and classifies each one independently, never
// a sequential find-and-replace pass: replacing quotes first and
// backslashes second (or vice versa) would double-escape a backslash
// this function itself just inserted — a real, classic bug class for
// this exact kind of escaping. A single forward pass has no such
// ordering hazard.
//
// Byte-oriented, deliberately NOT `for _, r := range s` (review fix,
// adversarial verification 2026-08-23): ranging over a string decodes
// UTF-8 and substitutes U+FFFD for every invalid byte, which makes the
// function NOT injective — two distinct configured names that both
// contain invalid UTF-8 (a byte sequence auth.go's buildEntry never
// rejects; it validates non-empty only, see its own doc comment) can
// decode to the identical U+FFFD run and render as the SAME escaped
// label value. Two scope_id values colliding onto one series is a
// duplicate-series scrape failure — confirmed against a real Prometheus
// scrape parser: the whole target reports DOWN, and promtool alone will
// not catch it, since expfmt silently dedupes. Escaping byte-for-byte
// passes every raw byte through unchanged except the three ASCII
// characters requiring escaping ('\\', '"', '\n') — none of which can
// ever appear as a UTF-8 continuation byte (continuation bytes are
// always >= 0x80), so this never misinterprets a multi-byte sequence's
// internal bytes as one of these three, and every distinct input byte
// string still maps to a distinct output. It also avoids WriteRune's
// per-rune decode cost on a large value.
func escapeLabelValue(s string) string {
	var b bytes.Buffer
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// metricLabel is one label name/value pair a metricWriter sample carries.
type metricLabel struct {
	name  string
	value string
}

// metricWriter accumulates one Prometheus text-exposition document.
// Every value written through it is escaped via escapeLabelValue — no
// call site below is responsible for escaping its own label values.
type metricWriter struct {
	buf bytes.Buffer
}

// family writes name's HELP and TYPE lines. Every metric name this file
// emits is a package-level string literal (never derived from request or
// config input), so no runtime validation of the name character set
// ([a-zA-Z_:][a-zA-Z0-9_:]*) is needed here — the literals below are
// already compliant.
func (m *metricWriter) family(name, kind, help string) {
	m.buf.WriteString("# HELP ")
	m.buf.WriteString(name)
	m.buf.WriteByte(' ')
	m.buf.WriteString(help)
	m.buf.WriteByte('\n')
	m.buf.WriteString("# TYPE ")
	m.buf.WriteString(name)
	m.buf.WriteByte(' ')
	m.buf.WriteString(kind)
	m.buf.WriteByte('\n')
}

// sample writes one series: name{label="value",...} value\n. Every label
// value is escaped via escapeLabelValue.
func (m *metricWriter) sample(name string, labels []metricLabel, value string) {
	m.buf.WriteString(name)
	if len(labels) > 0 {
		m.buf.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				m.buf.WriteByte(',')
			}
			m.buf.WriteString(l.name)
			m.buf.WriteString(`="`)
			m.buf.WriteString(escapeLabelValue(l.value))
			m.buf.WriteByte('"')
		}
		m.buf.WriteByte('}')
	}
	m.buf.WriteByte(' ')
	m.buf.WriteString(value)
	m.buf.WriteByte('\n')
}

// sampleInt writes one integer-valued series (every counter and the
// provider-healthy gauge below).
func (m *metricWriter) sampleInt(name string, labels []metricLabel, v int64) {
	m.sample(name, labels, strconv.FormatInt(v, 10))
}

// sampleFloat writes one float-valued series (the budget-consumed-ratio
// gauge below, which is rarely a whole number).
func (m *metricWriter) sampleFloat(name string, labels []metricLabel, v float64) {
	m.sample(name, labels, strconv.FormatFloat(v, 'g', -1, 64))
}

// aggregationNoteStoreBacked is appended to every store-backed family's
// HELP text below (item 1, adversarial verification 2026-08-23,
// reproduced against a 3-replica Traefik deployment): windowKey
// (limits.go) carries no per-instance component, so with Config.Redis
// configured every replica's counterStore read returns the SAME
// gateway-wide value — Prometheus scrapes each replica as its own
// target, so sum(rate(...)) or sum(...) across replicas TRIPLE-COUNTS
// on 3 replicas (a cost alert fires at a third of its intended
// threshold). The correct cross-replica aggregation is max(), since
// every replica already reports the true total on its own. Without
// Config.Redis (the in-memory memoryStore fallback), the IDENTICAL
// family becomes genuinely per-process instead — each replica counts
// only the traffic it personally handled — and sum() becomes the
// correct aggregation instead of max(). This flips on that one config
// bit; know which one a deployment is running before wiring an alert.
const aggregationNoteStoreBacked = " AGGREGATION ACROSS REPLICAS: with Redis configured, this value is the gateway-wide total read from shared limit state and is IDENTICAL on every replica's own /metrics response — aggregate with max(), never sum(), or a multi-replica deployment overcounts by the replica count. Without Redis (the in-memory fallback), this SAME family becomes genuinely per-process instead, and sum() becomes the correct aggregation. Know which one you are running."

// aggregationNotePerProcess is appended to llmgateway_rate_limit_
// rejections_total's HELP text: limiter.rejections (limits.go) is a
// bare in-process map, never written to or read from Redis regardless
// of Config.Redis — unlike every store-backed family above, this one
// never flips, and sum() is always the right cross-replica aggregation.
const aggregationNotePerProcess = " AGGREGATION ACROSS REPLICAS: this counter lives only in this process's memory, never in Redis, regardless of Config.Redis — aggregate with sum(), which is always correct for it, unlike the store-backed families above."

// aggregationNoteProviderHealthy is appended to llmgateway_provider_
// healthy's HELP text: the discovery circuit breaker (registry.go's
// providerState.health) is per-process state, set by each replica's own
// discovery polling loop, and is never written to Redis — two replicas
// can legitimately disagree about one provider's health at the same
// instant (e.g. one mid-backoff, one already recovered).
const aggregationNoteProviderHealthy = " AGGREGATION ACROSS REPLICAS: each replica runs its own discovery circuit breaker in-process, never shared via Redis, so this can legitimately differ per replica. Read it per-instance where possible; if you must aggregate, use min() to surface \"at least one replica sees this provider as unhealthy\" — sum() is meaningless for a 0/1 gauge."

// renderMetrics builds the full Prometheus text-exposition document for
// GET <metrics path>. Every read below goes through the SAME batched,
// chunked accessors GET /admin/api/overview and GET /admin/api/usage
// already use (admin.go's buildAdminOverview/chunkedCurrentUsage) — no
// per-series store round trip, and no second counting path: every number
// here is read from accounting limits.go already maintains, with one
// exception (llmgateway_rate_limit_rejections_total) that has no
// existing counter to read at all — see limiter.rejections' own doc
// comment (limits.go) for why that one is a new, deliberately minimal,
// in-process-only counter rather than a store round trip.
func (g *Gateway) renderMetrics() []byte {
	var m metricWriter
	g.writeUsageMetrics(&m)
	g.writeProviderMetrics(&m)
	g.writeRejectionMetrics(&m)
	return m.buf.Bytes()
}

// writeUsageMetrics emits llmgateway_requests_total, llmgateway_tokens_total,
// llmgateway_cost_micro_usd_total, and llmgateway_budget_consumed_ratio —
// one batched, chunked limiter.currentUsage read (via chunkedCurrentUsage,
// admin.go) covering every active user, every configured group, and the
// synthetic total scope, exactly mirroring buildAdminUsage's own scope
// construction so this pays no additional store round trip beyond what
// GET /admin/api/usage already pays.
//
// A scope whose read failed closed (scopeUsage.storeDown) is skipped
// entirely for every counter/gauge in this function, rather than reported
// as a false zero — the same "must not present as confirmed zero usage"
// rule scopeUsage's own doc comment states (limits.go). Prometheus treats
// a temporarily missing series as a gap, which is the honest signal here;
// a fabricated zero would not be.
func (g *Gateway) writeUsageMetrics(m *metricWriter) {
	userSummaries, groupSummaries := g.auth.snapshot()

	scopes := make([]limitScope, 0, len(userSummaries)+len(groupSummaries)+1)
	for _, us := range userSummaries {
		scopes = append(scopes, limitScope{kind: "user", id: us.name, limits: us.limits})
	}
	for _, gs := range groupSummaries {
		scopes = append(scopes, limitScope{kind: "group", id: gs.name, limits: gs.limits})
	}
	scopes = append(scopes, limitScope{kind: totalScopeKind, id: totalScopeID, limits: nil})

	usage := g.chunkedCurrentUsage(scopes)

	m.family("llmgateway_requests_total", "counter",
		"Total requests admitted for accounting today (UTC calendar day; resets at UTC midnight), by scope."+aggregationNoteStoreBacked)
	for _, su := range usage {
		if su.storeDown {
			continue
		}
		m.sampleInt("llmgateway_requests_total",
			[]metricLabel{{"scope_kind", su.kind}, {"scope_id", su.id}}, su.requestsPerDay)
	}

	m.family("llmgateway_tokens_total", "counter",
		"Total tokens accounted today (UTC calendar day; resets at UTC midnight), by scope and direction."+aggregationNoteStoreBacked)
	for _, su := range usage {
		if su.storeDown {
			continue
		}
		m.sampleInt("llmgateway_tokens_total",
			[]metricLabel{{"scope_kind", su.kind}, {"scope_id", su.id}, {"direction", "prompt"}}, su.tokensInPerDay)
		m.sampleInt("llmgateway_tokens_total",
			[]metricLabel{{"scope_kind", su.kind}, {"scope_id", su.id}, {"direction", "completion"}}, su.tokensOutPerDay)
	}

	m.family("llmgateway_cost_micro_usd_total", "counter",
		"Total cost accounted today (UTC calendar day; resets at UTC midnight), in micro-USD (1000000 = $1), by scope."+aggregationNoteStoreBacked)
	for _, su := range usage {
		if su.storeDown {
			continue
		}
		m.sampleInt("llmgateway_cost_micro_usd_total",
			[]metricLabel{{"scope_kind", su.kind}, {"scope_id", su.id}}, su.costPerDayMicros)
	}

	m.family("llmgateway_budget_consumed_ratio", "gauge",
		"Fraction of a configured limit already consumed in its own window; 1.0 means fully consumed, above 1.0 means the limit has been breached. Emitted only for a scope with that particular limit configured."+aggregationNoteStoreBacked)
	for i, su := range usage {
		if su.storeDown {
			continue
		}
		for _, br := range budgetRatios(su, scopes[i].limits) {
			m.sampleFloat("llmgateway_budget_consumed_ratio",
				[]metricLabel{{"scope_kind", su.kind}, {"scope_id", su.id}, {"budget", br.name}},
				float64(br.used)/float64(br.limit))
		}
	}
}

// budgetRatioSpec is one configured limit's usage-vs-limit pair,
// budgetRatios' own output — name matches requestLimitViolation's and
// buildBudgetProbes' own name strings exactly (limits.go), so the label
// value an operator sees here is the identical vocabulary a 429's own
// error message already uses.
type budgetRatioSpec struct {
	name  string
	used  int64
	limit int64
}

// budgetRatios returns one budgetRatioSpec per limit lc has actually
// configured (limit<=0 means unlimited and is skipped — the same
// short-circuit buildBudgetProbes applies, limits.go), pairing each
// against su's own current usage. lc nil (no limits configured for this
// scope, e.g. the synthetic total scope) returns nil. tokens-per-day/
// -month sum tokensIn+tokensOut, matching checkAndCount's own probe loop
// — a TokensPerDay/Month limit enforces a combined budget across both
// directions, not either alone.
func budgetRatios(su scopeUsage, lc *LimitsConfig) []budgetRatioSpec {
	if lc == nil {
		return nil
	}
	var out []budgetRatioSpec
	add := func(name string, used, limit int64) {
		if limit <= 0 {
			return
		}
		out = append(out, budgetRatioSpec{name: name, used: used, limit: limit})
	}
	add("requests-per-minute", su.requestsPerMinute, lc.RequestsPerMinute)
	add("requests-per-day", su.requestsPerDay, lc.RequestsPerDay)
	add("tokens-per-day", su.tokensInPerDay+su.tokensOutPerDay, lc.TokensPerDay)
	add("tokens-per-month", su.tokensInPerMonth+su.tokensOutPerMonth, lc.TokensPerMonth)
	add("cost-per-day", su.costPerDayMicros, usdToMicros(lc.CostPerDayUSD))
	add("cost-per-month", su.costPerMonthMicros, usdToMicros(lc.CostPerMonthUSD))
	return out
}

// writeProviderMetrics emits llmgateway_provider_attempts_total,
// llmgateway_provider_failures_total, and llmgateway_provider_healthy —
// always, for every configured provider — plus
// llmgateway_provider_model_attempts_total/-_failures_total, only when
// Config.Metrics.ModelLabel opts into the higher-cardinality per-model
// breakdown. Provider-level and (opt-in) per-model counters ride the SAME
// batched limiter.providerUsage round trip buildAdminOverview already
// pays (admin.go) — this adds no additional store call, on or off.
func (g *Gateway) writeProviderMetrics(m *metricWriter) {
	snaps := g.registry.snapshot()

	includeModel := g.cfg.Metrics != nil && g.cfg.Metrics.ModelLabel

	scopes := make([]limitScope, 0, len(snaps))
	for _, s := range snaps {
		scopes = append(scopes, limitScope{kind: kindProvider, id: s.name})
	}
	if includeModel {
		for _, s := range snaps {
			for _, model := range s.models {
				scopes = append(scopes, limitScope{kind: kindProviderModel, id: providerModelScopeID(s.name, model)})
			}
		}
	}
	counters := g.limiter.providerUsage(scopes)
	providerCounts := counters[:len(snaps)]

	m.family("llmgateway_provider_attempts_total", "counter",
		"Total upstream attempts today (UTC calendar day; resets at UTC midnight), by provider."+aggregationNoteStoreBacked)
	m.family("llmgateway_provider_failures_total", "counter",
		"Total upstream attempts today classified as a provider fault (transport error, 429, or 5xx — the same classification recordProviderAttempt/isTransient apply), by provider."+aggregationNoteStoreBacked)
	for i, s := range snaps {
		// A storeDown counter is a failed-closed read (limits.go's
		// providerUsage), not a real zero (review fix, adversarial
		// verification 2026-08-23): emitting it as a hard 0 here is
		// exactly the bug writeUsageMetrics' own storeDown guard above
		// already avoids for the request/token/cost families — a Redis
		// blip would otherwise paint a phantom counter-reset spike the
		// instant the store recovers and this value jumps back to its
		// real total. Skip the series entirely; a temporary gap is the
		// honest signal, a fabricated 0 is not.
		if providerCounts[i].storeDown {
			continue
		}
		labels := []metricLabel{{"provider", s.name}}
		m.sampleInt("llmgateway_provider_attempts_total", labels, providerCounts[i].attemptsDay)
		m.sampleInt("llmgateway_provider_failures_total", labels, providerCounts[i].failuresDay)
	}

	m.family("llmgateway_provider_healthy", "gauge",
		"1 when the provider's discovery circuit breaker is not open (closed or half-open), 0 when open (discoveryHealthy, registry.go)."+aggregationNoteProviderHealthy)
	for _, s := range snaps {
		healthy := int64(1)
		if s.health == breakerOpen {
			healthy = 0
		}
		m.sampleInt("llmgateway_provider_healthy", []metricLabel{{"provider", s.name}}, healthy)
	}

	if !includeModel {
		return
	}
	modelCounts := counters[len(snaps):]

	m.family("llmgateway_provider_model_attempts_total", "counter",
		"Total upstream attempts today (UTC calendar day; resets at UTC midnight), by provider and model. Opt-in via metrics.modelLabel — see MetricsConfig's own doc comment for the cardinality trade-off."+aggregationNoteStoreBacked)
	m.family("llmgateway_provider_model_failures_total", "counter",
		"Total upstream attempts today classified as a provider fault, by provider and model. Opt-in via metrics.modelLabel."+aggregationNoteStoreBacked)
	mi := 0
	for _, s := range snaps {
		for _, model := range s.models {
			mc := modelCounts[mi]
			mi++
			if mc.storeDown { // same fabricated-reset hazard as the provider-level loop above
				continue
			}
			labels := []metricLabel{{"provider", s.name}, {"model", model}}
			m.sampleInt("llmgateway_provider_model_attempts_total", labels, mc.attemptsDay)
			m.sampleInt("llmgateway_provider_model_failures_total", labels, mc.failuresDay)
		}
	}
}

// writeRejectionMetrics emits llmgateway_rate_limit_rejections_total from
// limiter.rejections (limits.go) — the one metric family in this file
// with no existing counterStore-backed accounting to read: checkAndCount
// increments/reads req:min/req:day for EVERY admission attempt regardless
// of outcome, so "how many were actually rejected" is not otherwise
// derivable without a second, redundant read against the store. See
// limiter.rejections' own doc comment for why this is a cheap, in-process
// -only, per-process-lifetime counter rather than a new windowed
// counterStore key.
func (g *Gateway) writeRejectionMetrics(m *metricWriter) {
	snaps := g.limiter.rejectionSnapshot()
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].kind != snaps[j].kind {
			return snaps[i].kind < snaps[j].kind
		}
		return snaps[i].id < snaps[j].id
	})

	m.family("llmgateway_rate_limit_rejections_total", "counter",
		"Total requests refused by a rate or budget limit since process start, by scope. Unlike the request/token/cost counters above, this is a process-lifetime counter, not UTC-day-windowed — it resets only on restart."+aggregationNotePerProcess)
	for _, s := range snaps {
		m.sampleInt("llmgateway_rate_limit_rejections_total",
			[]metricLabel{{"scope_kind", s.kind}, {"scope_id", s.id}}, s.count)
	}
}

// pruneRejectionsAfterReload evicts every "user"-kind rejection scope
// (limiter.pruneRejections, limits.go) whose name is no longer among
// g.auth's currently active users — ServeHTTP's own call site
// (llmgateway.go), gated on authStore.maybeReload just having performed
// a REAL file-sourced reload, never run unconditionally (see
// maybeReload's own doc comment, users_file.go, for why). Without this,
// a user removed from a hot-reloaded users file would keep exporting
// llmgateway_rate_limit_rejections_total for the rest of the process's
// life (review fix, adversarial verification 2026-08-23).
func (g *Gateway) pruneRejectionsAfterReload() {
	userSummaries, _ := g.auth.snapshot()
	keep := make(map[string]bool, len(userSummaries))
	for _, us := range userSummaries {
		keep[us.name] = true
	}
	g.limiter.pruneRejections(keep)
}
