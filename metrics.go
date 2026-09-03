package traefikllmgateway

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	g.maybeSweepTargetHealth()
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
// otherwise emit an unparseable exposition document, which is fatal to a
// real scrape — see the CORRECTNESS section below for exactly how, and
// for a claim this comment previously made that turned out to be wrong.
//
// Two independent correctness properties, both required, are in tension
// here and this function's whole shape exists to satisfy both at once:
//
//  1. INJECTIVE: two distinct inputs must never produce the same escaped
//     output. A collision means two distinct scope_id/model/provider
//     values render as the identical label value — a duplicate series.
//  2. VALID UTF-8 OUT: the output must always be valid UTF-8, regardless
//     of whether s is.
//
// CORRECTNESS (review fixes, adversarial verification 2026-08-23, two
// rounds): round 1 walked s byte-by-byte and copied every non-special
// byte through verbatim, including an invalid one — injective (property
// 1 held), but an invalid byte copied through means the OUTPUT can itself
// be invalid UTF-8, which fails property 2. Measured this round against
// prometheus/prometheus's real scrape path (promparse.go): an invalid-
// UTF-8 label value is FATAL — the parser errors, the scrape loop breaks,
// and Prometheus marks the target up=0 (DOWN). That is a strictly worse
// failure than the one round 1 was fixing: the doc comment at the time
// claimed a duplicate series is ALSO fatal ("the whole target reports
// DOWN"); measured properly this round against scrape.go, that claim was
// wrong — checkAddError treats a duplicate sample as non-fatal (returns
// false, nil), bumps a counter, and the scrape still succeeds with
// up=1. So the ORIGINAL rune-based version (pre-round-1) was lossy but
// safe: colliding invalid-UTF-8 inputs onto one U+FFFD-bearing series
// dropped data but never took the target down. Round 1's byte-oriented
// version was lossless but unsafe: correct in the one dimension it
// measured, worse in the one it did not.
//
// This version keeps both: valid runes (including a genuine, already-
// valid 3-byte-encoded U+FFFD that was actually present in s) pass
// through unchanged via utf8.DecodeRuneInString, which reports both the
// rune and how many bytes it consumed; only a TRULY undecodable single
// byte (RuneError with a reported width of 1 — DecodeRuneInString's own
// documented signal that byte could not start any valid encoding, as
// opposed to width 3 for a real U+FFFD) is replaced with \xHH, HH being
// that byte's own hex value — recoverable, so distinct invalid bytes can
// never collide, and it can never appear from the normal escape path
// either: every literal backslash in s is ASCII and therefore always
// decodes as its own valid 1-byte rune, which the switch below always
// doubles to \\ — a lone, undoubled backslash in the output can
// therefore only ever be this marker, never user-typed text. Reachability
// caveat: no live config path is known to reach the invalid-UTF-8 branch
// today (encoding/json, which every user/group name currently decodes
// through, coerces invalid UTF-8 to U+FFFD before this function ever
// sees it) — this is closing a latent gap, not a currently-exploitable
// one, but the SAME property (injective, always-valid-UTF-8 output) is
// what a future caller — an upstream-reported model id read some other
// way, say — would need without re-deriving this reasoning.
//
// Fast path (item 5, adversarial verification round 2, benchmarked):
// nearly every real user/group/model name needs none of the above — no
// backslash/quote/newline, already valid UTF-8 (true of every Go string
// literal and every encoding/json-decoded value by construction) — so
// skip the walk and its allocation entirely when s is already its own
// answer. Benchmarked over 7 representative values: the byte-oriented
// slow path alone cost 318.8 ns/op, 560 B/op, 14 allocs/op (worse than
// the original rune-based version's 344.7 ns, 120 B, 7 allocs on
// allocations specifically, from bytes.Buffer.String() copying where
// strings.Builder.String() does not — fixed below by switching to
// strings.Builder too); the fast path measures 84.4 ns/op, 0 B, 0
// allocs/op. At 1,000 configured users that is the difference between
// roughly 20,000 allocations and 800 KB of garbage per scrape, inside
// the shared Traefik process, and zero.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") && utf8.ValidString(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// A genuinely undecodable byte — see this function's own
			// CORRECTNESS section above for why \xHH, not � and not
			// a raw copy, is the only choice that keeps both required
			// properties.
			b.WriteString(`\x`)
			b.WriteByte(hexDigit(s[i] >> 4))
			b.WriteByte(hexDigit(s[i] & 0x0f))
			i++
			continue
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			// A valid rune, 1-4 bytes: none of the three special ASCII
			// characters above can appear as any byte of a valid
			// multi-byte encoding (every byte of one is >= 0x80), so a
			// verbatim copy of s[i:i+size] can never hide an unescaped
			// backslash/quote/newline inside it.
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// hexDigit returns the lowercase hex digit for the low nibble of n
// (0x0-0xf) — escapeLabelValue's own \xHH marker for an undecodable
// byte, written without fmt.Sprintf to keep that (already-rare) branch
// allocation-free too.
func hexDigit(n byte) byte {
	const digits = "0123456789abcdef"
	return digits[n&0x0f]
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

// histogram writes one Prometheus histogram family's samples for one
// label set (feat: instrument upstream latency): cumulative `name+
// "_bucket"` series in ascending `le` order, ending with `le="+Inf"`,
// followed by `name+"_sum"` then `name+"_count"` — the exact shape a
// real scrape parser requires (see escapeLabelValue's own doc comment
// for what getting exposition format wrong costs in general: this is
// not "one bad metric gets dropped", it is "the whole scrape fails to
// parse and the target reports DOWN"). Getting a histogram wrong is
// worse than getting a label escape wrong: a non-cumulative or
// out-of-order bucket set, or a missing +Inf terminator, are ALSO
// spec violations a strict parser rejects outright.
//
// buckets is DENSE, one entry per latencyBucketBounds index — index i
// counts every observation in (latencyBucketBounds[i-1],
// latencyBucketBounds[i]] — converted to the CUMULATIVE form the
// exposition format requires here, once, at render time
// (observeLatencyBucket, this file's own accumulation-side counterpart,
// only ever needs to bump one dense bucket per observation, never
// rewrite every bucket at or above its own). overflow is the +Inf
// bucket's own dense count: every observation past the largest
// configured bound. _count is derived from the SAME cumulative running
// total the last +Inf bucket line already computed, rather than a
// separately-tracked counter passed in alongside sum — by construction,
// these can never diverge from what the bucket lines above them show,
// closing off exactly the "buckets and _count silently disagree" failure
// mode a real parser would also reject.
//
// labels is never mutated or its backing array reused across calls: a
// fresh []metricLabel is built per bucket line instead of append(labels,
// ...), so this is safe regardless of what spare capacity the caller's
// own labels slice happens to have.
func (m *metricWriter) histogram(name string, labels []metricLabel, buckets []int64, overflow int64, sum float64) {
	cumulative := int64(0)
	for i, bound := range latencyBucketBounds {
		cumulative += buckets[i]
		bucketLabels := make([]metricLabel, len(labels)+1)
		copy(bucketLabels, labels)
		bucketLabels[len(labels)] = metricLabel{"le", strconv.FormatFloat(bound, 'g', -1, 64)}
		m.sampleInt(name+"_bucket", bucketLabels, cumulative)
	}
	cumulative += overflow
	infLabels := make([]metricLabel, len(labels)+1)
	copy(infLabels, labels)
	infLabels[len(labels)] = metricLabel{"le", "+Inf"}
	m.sampleInt(name+"_bucket", infLabels, cumulative)

	m.sampleFloat(name+"_sum", labels, sum)
	m.sampleInt(name+"_count", labels, cumulative)
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
// rejections_total's HELP text, and to both usage-provenance counter
// families' HELP text below (llmgateway_usage_provenance_requests_total/
// -_tokens_total, feat: expose token-accounting provenance): each is a
// bare in-process store — limiter.rejections and g.provenance
// respectively — never written to or read from Redis regardless of
// Config.Redis. Unlike every store-backed family above, none of these
// ever flips, and sum() is always the right cross-replica aggregation.
const aggregationNotePerProcess = " AGGREGATION ACROSS REPLICAS: this counter lives only in this process's memory, never in Redis, regardless of Config.Redis — aggregate with sum(), which is always correct for it, unlike the store-backed families above."

// aggregationNoteProviderHealthy is appended to llmgateway_provider_
// healthy's HELP text: the discovery circuit breaker (registry.go's
// providerState.health) is per-process state, set by each replica's own
// discovery polling loop, and is never written to Redis — two replicas
// can legitimately disagree about one provider's health at the same
// instant (e.g. one mid-backoff, one already recovered).
const aggregationNoteProviderHealthy = " AGGREGATION ACROSS REPLICAS: each replica runs its own discovery circuit breaker in-process, never shared via Redis, so this can legitimately differ per replica. Read it per-instance where possible; if you must aggregate, use min() to surface \"at least one replica sees this provider as unhealthy\" — sum() is meaningless for a 0/1 gauge."

// aggregationNoteTargetHealthy is appended to llmgateway_target_healthy's
// HELP text (feat/target-health): the tracker behind this gauge
// (target_health.go) is per-process, in-memory state, fed by each
// replica's own passive traffic and, when enabled, its own active probe
// sweep — never shared via Redis. A sibling to
// aggregationNoteProviderHealthy above, not a reuse of it: this
// tracker has no discovery step of its own, so that note's own
// reference to "discovery circuit breaker" would not apply here.
const aggregationNoteTargetHealthy = " AGGREGATION ACROSS REPLICAS: each replica tracks this target's health independently from its own passive traffic and, if enabled, its own active probes — never shared via Redis — so this can legitimately differ per replica. Read it per-instance where possible; if you must aggregate, use min() to surface \"at least one replica sees this target as unhealthy\" — sum() is meaningless for a 0/1 gauge."

// aggregationNoteStoreHealth is appended to llmgateway_limit_store_up's
// HELP text: whether THIS replica's own connection to the configured
// limit store is currently healthy is, by definition, discovered
// locally (limiter.storeLatched, limits.go) — it cannot itself be read
// from the store being described, so — like provider health above —
// it is inherently per-replica, never shared via Redis.
const aggregationNoteStoreHealth = " AGGREGATION ACROSS REPLICAS: each replica discovers its own connection health to the configured store independently — this cannot itself be read from the store being described — so it can legitimately differ per replica. Read it per-instance where possible; if you must aggregate, use min() to surface \"at least one replica currently sees the store as down\" — sum() is meaningless for a 0/1 gauge."

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
	g.writeTargetMetrics(&m)
	g.writeLatencyMetrics(&m)
	g.writeProvenanceMetrics(&m)
	g.writeRejectionMetrics(&m)
	g.writeStoreHealthMetrics(&m)
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

// writeTargetMetrics emits llmgateway_target_healthy (feat/target-health):
// 1 for a healthy MCP server or A2A agent, 0 for an unhealthy one, and NO
// sample at all for a target this tracker has never observed — the
// identical "a gap is honest, a fabricated value is not" convention
// writeUsageMetrics/writeProviderMetrics already apply to a storeDown
// read, applied here to "never yet observed" instead of "currently
// unreadable". Iterates every CONFIGURED MCP server and agent (not just
// ones the tracker happens to have an entry for), so a target removed
// from config since its last observation contributes no stale series.
func (g *Gateway) writeTargetMetrics(m *metricWriter) {
	m.family("llmgateway_target_healthy", "gauge",
		"1 when an MCP server or A2A agent's health (last passive observation or active probe) is healthy, 0 when its consecutive-failure count has reached targetHealth.failureThreshold. No sample while never yet observed."+aggregationNoteTargetHealthy)

	for _, name := range sortedMCPServerNames(g.cfg.MCPServers) {
		writeTargetHealthySample(m, g.targetHealth.snapshot(targetKindMCP, name), targetScopeKind(targetKindMCP), name)
	}
	for _, name := range sortedAgentNames(g.cfg.Agents) {
		writeTargetHealthySample(m, g.targetHealth.snapshot(targetKindAgent, name), targetScopeKind(targetKindAgent), name)
	}
}

// writeTargetHealthySample writes llmgateway_target_healthy's one sample
// for (kindLabel, name), skipped entirely while snap's state is unknown
// — see writeTargetMetrics' own doc comment for why. kindLabel is the
// external "mcp"/"agent" spelling (targetScopeKind), never the internal
// routing kind ("a2a") targetHealthTracker itself keys on.
func writeTargetHealthySample(m *metricWriter, snap targetHealthSnapshot, kindLabel, name string) {
	if snap.state == targetHealthUnknown {
		return
	}
	healthy := int64(1)
	if snap.state == targetHealthUnhealthy {
		healthy = 0
	}
	m.sampleInt("llmgateway_target_healthy", []metricLabel{{"kind", kindLabel}, {"target", name}}, healthy)
}

// --- upstream latency (feat: instrument upstream latency) ---
//
// The task: "priority-based throttling that engages when providers are
// under strain" needs real data on what strain looks like first — this
// section adds the measurement and nothing else. No throttling, no
// priorities, no control logic reads any of it; it exists purely to be
// scraped and looked at.

// latencyBucketBounds are the shared histogram bucket upper bounds, in
// seconds, for both llmgateway_upstream_ttfb_seconds and
// llmgateway_upstream_duration_seconds below.
//
// Bucket choice, reasoned from how upstream LLM latency actually
// distributes: total upstream duration is roughly TTFB + output_tokens/
// tokens_per_sec (task brief), so the useful range spans sub-second
// (a fast TTFB, or a very short completion) through several minutes (a
// long generation, or a request approaching defaultRequestTimeout,
// timeout.go — 5 minutes, the point this gateway aborts an idle
// upstream outright, regardless of Config.RequestTimeout overrides).
//
//   - 0.1/0.25/0.5/1s: resolves a fast TTFB and short completions at
//     useful granularity — most of a healthy provider's TTFB population
//     lives in this range.
//   - 2/5/10/15/30/60s: the bulk of real chat completions, where most
//     observations should land; finer-grained here than at the extremes
//     because this is where an operator needs to tell "a bit slower"
//     from "a lot slower".
//   - 120/300s: brackets defaultRequestTimeout itself (300s = 5
//     minutes), so mass building up in this range is visibly "close to
//     where this gateway would abort the request outright", not just
//     "slow".
//   - 600s (10 minutes): headroom for a deployment that configures a
//     longer RequestTimeout override; anything past it falls into the
//     mandatory +Inf bucket.
//
// Both histograms share this one bound set deliberately, not two
// independently tuned ones: it lets TTFB and duration be read off the
// same axis on one dashboard, and one set is one less thing to keep in
// sync as this feature evolves.
var latencyBucketBounds = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600}

// aggregationNoteLatency is appended to both upstream-latency histogram
// families' HELP text: g.latency (latencyStore, below) is in-process
// only, like limiter.rejections (aggregationNotePerProcess above) —
// NEVER written to or read from Redis regardless of Config.Redis, unlike
// the store-backed request/token/cost/provider-attempt families
// elsewhere in this file, which flip between sum() and max() depending
// on whether Redis is configured (aggregationNoteStoreBacked's own doc
// comment). Each replica reports only the upstream traffic it personally
// handled, so sum() (or, for a real percentile, Prometheus's own
// histogram_quantile over a sum()-aggregated set) is always the correct
// cross-replica aggregation here, unconditionally.
const aggregationNoteLatency = " AGGREGATION ACROSS REPLICAS: this histogram lives only in this process's memory, never in Redis, regardless of Config.Redis — each replica reports only the upstream traffic it personally handled, so aggregate with sum() (histogram_quantile over a sum()-aggregated set, for a real percentile), which is always correct for it, unlike the store-backed families above that flip between sum() and max() depending on whether Redis is configured."

// latencySample is what one completed upstream watchdogBody (timeout.go)
// hands its latencyRecorder exactly once, from Close: duration always
// (the whole body's lifetime, start to Close); ttfb only when hasTTFB —
// a body that never delivered one successful byte (e.g. the watchdog
// fired before any read succeeded) has nothing meaningful to report
// there, and must not fabricate a zero. streaming is the distinguishing
// label the task brief requires, derived once by the caller from the
// response's own Content-Type (isEventStreamResponse, timeout.go).
type latencySample struct {
	duration  time.Duration
	ttfb      time.Duration
	hasTTFB   bool
	streaming bool
}

// latencyKey identifies one label set's histogram pair within
// latencyStore: provider and streaming are always populated (the task
// brief's "always" cardinality); model is empty unless Config.Metrics.
// ModelLabel opted into the per-model breakdown AND the caller actually
// knew the upstream model at record time (recordLatency below —
// native passthrough never does, mirroring recordProviderAttempt's own
// identical limitation, limits.go).
type latencyKey struct {
	provider  string
	model     string
	streaming bool
}

// latencyHistogram accumulates one latencyKey's two histograms (TTFB,
// duration): DENSE per-bucket occurrence counts (observeLatencyBucket
// below converts to the exposition format's required CUMULATIVE form
// only at render time, metricWriter.histogram above), each with its own
// running sum/count, plus overflow — the +Inf bucket's own dense count,
// every observation past the largest configured bound.
type latencyHistogram struct {
	ttfbBuckets      []int64
	durationBuckets  []int64
	ttfbSum          float64
	durationSum      float64
	ttfbCount        int64
	ttfbOverflow     int64
	durationCount    int64
	durationOverflow int64
}

// newLatencyHistogram allocates one latencyHistogram with dense bucket
// slices sized to latencyBucketBounds, freshly zeroed — called at most
// once per DISTINCT latencyKey ever observed (latencyStore.record
// below), never per observation.
func newLatencyHistogram() *latencyHistogram {
	return &latencyHistogram{
		ttfbBuckets:     make([]int64, len(latencyBucketBounds)),
		durationBuckets: make([]int64, len(latencyBucketBounds)),
	}
}

// observeLatencyBucket bumps the correct dense bucket (or overflow, for
// a value past every configured bound) for one observation, and updates
// the running sum/count alongside it — shared by latencyHistogram's own
// two observations (TTFB, duration) so the bucket-selection rule can
// never drift between them. latencyBucketBounds is ascending, so the
// first bound the observation is <= is its bucket, matching Prometheus's
// own "le" (less-than-or-equal) histogram semantics exactly.
func observeLatencyBucket(buckets []int64, overflow *int64, sum *float64, count *int64, d time.Duration) {
	v := d.Seconds()
	*sum += v
	*count++
	for i, bound := range latencyBucketBounds {
		if v <= bound {
			buckets[i]++
			return
		}
	}
	*overflow++
}

// latencyStore is the Gateway's in-process, per-replica upstream-latency
// accumulator (feat: instrument upstream latency) — see
// aggregationNoteLatency above for why sum(), never max(), is always the
// correct cross-replica aggregation for it.
//
// Every method is nil-receiver-safe, mirroring requestHealthTracker's
// own convention (failoverHealth, llmgateway.go): a Gateway assembled
// directly as a bare &Gateway{} literal (bypassing newGateway, as a few
// older tests do) degrades to "no latency observed", rather than a
// nil-pointer panic, even though newGateway itself always constructs a
// real one.
type latencyStore struct {
	// data declared before mu (fieldalignment, govet): a map header is
	// fully pointer-shaped, while sync.Mutex is a plain int32+uint32 pair
	// with no pointer at all — grouping the pointer-shaped field first
	// keeps the GC's pointer-scan span as short as possible, the same
	// convention Gateway and adminProviderView follow (llmgateway.go,
	// admin.go).
	data map[latencyKey]*latencyHistogram
	mu   sync.Mutex
}

// newLatencyStore returns an empty, ready-to-use latencyStore.
func newLatencyStore() *latencyStore {
	return &latencyStore{data: make(map[latencyKey]*latencyHistogram)}
}

// record accumulates sample into key's histogram, allocating a fresh
// latencyHistogram the first time key is seen. Called at most twice per
// completed upstream body — recordLatency below's own dual-write, once
// for the always-on provider+streaming key and once more for the opt-in
// provider+streaming+model key — nowhere near the hot per-Read path
// watchdogBody.Read/Close themselves stay off (armLatency's and Read's
// own doc comments, timeout.go, make that constraint explicit): this
// lock is paid once per REQUEST, never once per chunk of a stream.
func (s *latencyStore) record(key latencyKey, sample latencySample) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.data[key]
	if h == nil {
		h = newLatencyHistogram()
		s.data[key] = h
	}
	if sample.hasTTFB {
		observeLatencyBucket(h.ttfbBuckets, &h.ttfbOverflow, &h.ttfbSum, &h.ttfbCount, sample.ttfb)
	}
	observeLatencyBucket(h.durationBuckets, &h.durationOverflow, &h.durationSum, &h.durationCount, sample.duration)
}

// latencySnapshot is one latencyKey's fully-copied histogram state, safe
// to read after latencyStore.snapshot returns without holding its lock.
type latencySnapshot struct {
	key              latencyKey
	ttfbBuckets      []int64
	durationBuckets  []int64
	ttfbSum          float64
	durationSum      float64
	ttfbCount        int64
	ttfbOverflow     int64
	durationCount    int64
	durationOverflow int64
}

// snapshot returns a fully-copied view of every latencyKey s currently
// holds, safe to read without s's lock — writeLatencyMetrics and
// buildAdminLatencyViews (admin.go) both read through this rather than
// s.data directly.
func (s *latencyStore) snapshot() []latencySnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]latencySnapshot, 0, len(s.data))
	for k, h := range s.data {
		out = append(out, latencySnapshot{
			key:              k,
			ttfbBuckets:      append([]int64(nil), h.ttfbBuckets...),
			durationBuckets:  append([]int64(nil), h.durationBuckets...),
			ttfbSum:          h.ttfbSum,
			durationSum:      h.durationSum,
			ttfbCount:        h.ttfbCount,
			ttfbOverflow:     h.ttfbOverflow,
			durationCount:    h.durationCount,
			durationOverflow: h.durationOverflow,
		})
	}
	return out
}

// recordLatency accumulates sample into g.latency: always under
// (provider, streaming) — the always-on dimension the task brief
// requires — and additionally under (provider, streaming, model) when
// both Config.Metrics.ModelLabel is enabled and model is known,
// mirroring limiter.recordProviderAttempt's own dual-write rule (limits.
// go) for the identical opt-in-cardinality reason.
//
// This is the ONLY write path into g.latency, and it is reached only
// when a latencyRecorder was ever wired onto a request's context in the
// first place — every withLatencyRecorder call site (routes_unified.go,
// routes_media.go, routes_passthrough.go) gates that wiring on
// metricsEnabled(g.cfg) — so a deployment with metrics disabled calls
// this exactly zero times, and g.latency's map simply never grows.
func (g *Gateway) recordLatency(provider, model string, sample latencySample) {
	g.latency.record(latencyKey{provider: provider, streaming: sample.streaming}, sample)
	if model != "" && g.cfg.Metrics != nil && g.cfg.Metrics.ModelLabel {
		g.latency.record(latencyKey{provider: provider, streaming: sample.streaming, model: model}, sample)
	}
}

// writeLatencyMetrics emits llmgateway_upstream_ttfb_seconds and
// llmgateway_upstream_duration_seconds — this feature's own two
// histograms. Absent entirely (no # HELP/TYPE lines even emitted) when
// g.latency has recorded nothing yet — a fresh process, or a deployment
// with metrics collection gated off at every wiring site — mirroring how
// a brand-new histogram with zero observations would look on any real
// Prometheus exporter too: no series until the first observation, not a
// family of all-zero ones.
//
// snaps is sorted (provider, then streaming, then model) purely for
// deterministic output across repeated scrapes of identical underlying
// state — Prometheus does not require sample ordering, but a stable
// scrape body makes diffing two scrapes by eye meaningful, matching
// writeRejectionMetrics' own sort.Slice above.
func (g *Gateway) writeLatencyMetrics(m *metricWriter) {
	snaps := g.latency.snapshot()
	if len(snaps) == 0 {
		return
	}
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i].key, snaps[j].key
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.streaming != b.streaming {
			return !a.streaming // "false" (non-streaming) sorts before "true"
		}
		return a.model < b.model
	})

	m.family("llmgateway_upstream_ttfb_seconds", "histogram",
		"Time to first successful byte read from an upstream response body, in seconds, measured from just before the request was sent (watchdogBody, timeout.go). Meaningful as a distinct \"how fast did the provider start responding\" signal only for stream=\"true\": a non-streaming provider buffers its whole completion before sending anything, so its own TTFB is approximately equal to its own llmgateway_upstream_duration_seconds and carries the same output-length contamination duration does — always split by the stream label before comparing across requests of different lengths, never averaged across it."+aggregationNoteLatency)
	m.family("llmgateway_upstream_duration_seconds", "histogram",
		"Total upstream response body duration, in seconds, from just before the request was sent to the last successful read or Close (watchdogBody, timeout.go) — covers the whole body, not just headers. Roughly TTFB + output_tokens/tokens_per_sec: a long generation legitimately takes longer than a short one on an equally healthy provider, so a raw average across requests of very different output length is not by itself a \"provider degraded\" signal — llmgateway_tokens_total's completion direction (this file) gives the token side of that ratio for the non-streaming population, where duration approximates total generation time."+aggregationNoteLatency)

	for _, s := range snaps {
		labels := []metricLabel{{"provider", s.key.provider}, {"stream", strconv.FormatBool(s.key.streaming)}}
		if s.key.model != "" {
			labels = append(labels, metricLabel{"model", s.key.model})
		}
		if s.ttfbCount > 0 {
			m.histogram("llmgateway_upstream_ttfb_seconds", labels, s.ttfbBuckets, s.ttfbOverflow, s.ttfbSum)
		}
		if s.durationCount > 0 {
			m.histogram("llmgateway_upstream_duration_seconds", labels, s.durationBuckets, s.durationOverflow, s.durationSum)
		}
	}
}

// --- usage provenance (feat: expose token-accounting provenance) ---
//
// Token accounting has three provenances (usage.estimated's own doc
// comment, limits.go), and before this section nothing on any read
// surface distinguished them: REPORTED (the provider returned real
// usage — the normal, accurate case), ESTIMATED (a non-streaming
// response carried none, so prompt is substituted with
// ceil(len(body)/4) and completion is billed as zero — runUnified's own
// recording call site, routes_unified.go), and UNBILLED (a streaming
// response carried none, so the request is counted but zero tokens are
// billed). This section adds the metric surface only; billing itself is
// unchanged — see runUnified's own doc comment at its recording call
// site for the guarantee that no value recorded here ever feeds back
// into what gets billed.
//
// ESTIMATED and UNBILLED are deliberately never merged into one "not
// reported" signal: estimated substitutes an approximation (a nonzero
// prompt count is still billed), unbilled charges nothing at all. A
// provider silently ignoring stream_options.include_usage serves
// completions entirely free against every configured budget, and that
// is the one fact this feature exists to make visible — collapsing the
// two would hide it again.

const (
	provenanceReported  = "reported"
	provenanceEstimated = "estimated"
	provenanceUnbilled  = "unbilled"
)

// provenanceKey identifies one (provider, provenance) series pair
// provenanceStore accumulates under. provider is always one of this
// deployment's configured provider names (config-bounded, never user
// input, exactly like latencyKey.provider above), and provenance is
// always one of the three package-level constants above — the total key
// space is bounded by (configured providers) x 3, so no rotation/cap
// logic is needed here, unlike rejectionCounter's own user/group-name
// keys (limits.go), which ARE operator/end-user controlled and can churn
// without bound.
type provenanceKey struct {
	provider   string
	provenance string
}

// provenanceCounts is one provenanceKey's accumulated totals: requests
// (how many accounted candidate outcomes were recorded under this
// provenance) and tokens (their summed result.total() — the SAME number
// runUnified already passed to accounting, never a second count),
// answering the task brief's "what fraction of billed tokens came from
// real reported usage" question directly: sum(tokens where provenance ==
// reported) / sum(tokens across every provenance).
type provenanceCounts struct {
	requests int64
	tokens   int64
}

// provenanceStore is the Gateway's in-process, per-replica usage-
// provenance accumulator — mirrors latencyStore's own shape and
// nil-receiver-safe convention above for the identical reason: a Gateway
// assembled directly as a bare &Gateway{} literal (bypassing newGateway,
// as a few older tests do) degrades to "no provenance observed" rather
// than a nil-pointer panic.
type provenanceStore struct {
	data map[provenanceKey]*provenanceCounts
	mu   sync.Mutex
}

// newProvenanceStore returns an empty, ready-to-use provenanceStore.
func newProvenanceStore() *provenanceStore {
	return &provenanceStore{data: make(map[provenanceKey]*provenanceCounts)}
}

// record accumulates one candidate outcome under (provider, provenance):
// one request, plus tokens — allocating a fresh provenanceCounts the
// first time key is seen.
func (s *provenanceStore) record(provider, provenance string, tokens int64) {
	if s == nil {
		return
	}
	key := provenanceKey{provider: provider, provenance: provenance}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data[key]
	if c == nil {
		c = &provenanceCounts{}
		s.data[key] = c
	}
	c.requests++
	c.tokens += tokens
}

// provenanceSnapshot is one provenanceKey's fully-copied counts, safe to
// read after provenanceStore.snapshot returns without holding its lock.
type provenanceSnapshot struct {
	key      provenanceKey
	requests int64
	tokens   int64
}

// snapshot returns a fully-copied view of every provenanceKey s
// currently holds, safe to read without s's lock — writeProvenanceMetrics
// and buildAdminProvenanceViews (admin.go) both read through this rather
// than s.data directly.
func (s *provenanceStore) snapshot() []provenanceSnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]provenanceSnapshot, 0, len(s.data))
	for k, c := range s.data {
		out = append(out, provenanceSnapshot{key: k, requests: c.requests, tokens: c.tokens})
	}
	return out
}

// recordUsageProvenance accumulates one candidate outcome into
// g.provenance — runUnified's (routes_unified.go) only write path into
// it, reached only for a candidate whose own response actually completed
// (callErr == nil), gated on metricsEnabled exactly like
// withLatencyRecorder's own wiring (routes_unified.go): a deployment
// with metrics off calls this exactly zero times.
func (g *Gateway) recordUsageProvenance(provider, provenance string, tokens int64) {
	g.provenance.record(provider, provenance, tokens)
}

// writeProvenanceMetrics emits llmgateway_usage_provenance_requests_total
// and llmgateway_usage_provenance_tokens_total — this feature's own two
// counter families. Absent entirely (no # HELP/TYPE lines emitted) when
// g.provenance has recorded nothing yet, mirroring writeLatencyMetrics'
// own "no series until the first observation" convention above.
//
// snaps is sorted (provider, then provenance) purely for a deterministic
// scrape body across repeated scrapes of identical state, matching
// writeLatencyMetrics' own reasoning.
func (g *Gateway) writeProvenanceMetrics(m *metricWriter) {
	snaps := g.provenance.snapshot()
	if len(snaps) == 0 {
		return
	}
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i].key, snaps[j].key
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		return a.provenance < b.provenance
	})

	m.family("llmgateway_usage_provenance_requests_total", "counter",
		"Total accounted candidate outcomes (a successful upstream response, callErr == nil, that reached usage accounting in runUnified, routes_unified.go), by provider and provenance. provenance is \"reported\" (the provider returned real usage), \"estimated\" (a non-streaming response carried none, so prompt was substituted with ceil(len(body)/4) and completion billed as zero — usage.estimated, limits.go), or \"unbilled\" (a streaming response carried none, so the request is counted but zero tokens are billed). This family only reports how an already-billed number was arrived at; it never changes what gets billed."+aggregationNotePerProcess)
	m.family("llmgateway_usage_provenance_tokens_total", "counter",
		"Total tokens billed under each provenance (the SAME result.total() runUnified already passed to accounting, routes_unified.go — never a second, independent count), by provider and provenance. sum(...{provenance=\"reported\"}) / sum(...) across every provenance is the fraction of billed tokens that came from real provider-reported usage."+aggregationNotePerProcess)
	for _, s := range snaps {
		labels := []metricLabel{{"provider", s.key.provider}, {"provenance", s.key.provenance}}
		m.sampleInt("llmgateway_usage_provenance_requests_total", labels, s.requests)
		m.sampleInt("llmgateway_usage_provenance_tokens_total", labels, s.tokens)
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
		fmt.Sprintf(
			"Total requests refused by a rate or budget limit since process start, by scope. Unlike the request/token/cost counters above, this is a process-lifetime counter, not UTC-day-windowed, but it does NOT reset only on restart: a scope's own count resets to 0 when that user is removed via a hot-reloaded users file, and every tracked scope resets together, in one bulk cliff (not a graceful per-scope decay), the instant more than %d distinct scopes have ever been seen — a value that drops is one of these events, not a scrape anomaly.",
			rejectionCounterMapCap,
		)+aggregationNotePerProcess)
	for _, s := range snaps {
		m.sampleInt("llmgateway_rate_limit_rejections_total",
			[]metricLabel{{"scope_kind", s.kind}, {"scope_id", s.id}}, s.count)
	}
}

// writeStoreHealthMetrics emits llmgateway_limit_store_up (item 7,
// adversarial verification 2026-08-23): every store-backed family above
// simply goes silent for the scopes it cannot read during an outage
// (writeUsageMetrics/writeProviderMetrics's own storeDown skip) — the
// honest choice for THOSE families (see their own doc comments), but it
// means a store outage has NO signal of its own on this endpoint: with
// failOpen false and a store flapping faster than the fail latch
// (storeDownLatchFor, limits.go), every store-backed family can simply
// be ABSENT, scrape after scrape, with nothing on /metrics itself
// saying why — an operator needs live traffic AND a fail-closed config
// to even notice. This gauge exists to be the "why are my other
// families missing" signal on its own, independent of traffic volume or
// failOpen.
//
// Absent entirely when no limit store is configured at all (a fallback-
// only deployment, limiter.redisStatus' own "configured" return) —
// there is no store health to report there; the fallback is always
// "up" by definition, a trivial, uninteresting reading not worth a
// series.
func (g *Gateway) writeStoreHealthMetrics(m *metricWriter) {
	configured, _, _ := g.limiter.redisStatus()
	if !configured {
		return
	}
	up := int64(1)
	if g.limiter.configuredStoreDown() {
		up = 0
	}
	m.family("llmgateway_limit_store_up", "gauge",
		"1 when this replica's own connection to the configured limit store (Redis) is currently healthy, 0 when it is latched down after a recent failure (limiter.storeLatched, limits.go). A store-backed family (llmgateway_requests_total and the rest) going silent for one or more scrapes in a row, with this gauge at 0, is the store outage those families' own storeDown skip is deliberately quiet about."+aggregationNoteStoreHealth)
	m.sampleInt("llmgateway_limit_store_up", nil, up)
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
