package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Event kind vocabulary GET /admin/api/events' gatewayEvent.Kind field
// takes (F3, v0.3 dashboard task) — every hook below sets exactly one of
// these. eventRingCap bounds both the local eventRing's capacity and the
// Redis-backed list's own LTRIM cap (Q2, coordinator decision: "no
// expiry, bounded by LTRIM to 200 entries" — no separate TTL, the list is
// self-bounding by size alone). eventsRedisKey is that Redis list's key.
// eventPushPerSecond is the per-replica push-rate cap (Q3: "push throttle
// 50/s/replica: accepted") — the local ring is never throttled, only the
// best-effort Redis mirror. adminEventsDefaultLimit/adminEventsMaxLimit
// bound GET /admin/api/events' own "limit" query parameter, mirroring
// parseUsageModelsLimit's own default/max pair (admin.go).
const (
	eventKindRateLimit = "rate_limit"
	eventKindBudget    = "budget"
	eventKindStoreDown = "store_down"
	eventKindUpstream  = "upstream"
	eventKindTimeout   = "timeout"
	eventKindUnpriced  = "unpriced"
	eventKindCapacity  = "capacity"

	eventRingCap       = 200
	eventsRedisKey     = "llmgw:events"
	eventPushPerSecond = 50

	adminEventsPath         = "/admin/api/events"
	adminEventsDefaultLimit = 50
	adminEventsMaxLimit     = eventRingCap
)

// Route vocabulary gatewayEvent.Route takes (plan §1.4): one fixed string
// per metered entry point, independent of cacheKey's own cacheEndpoint*
// constants (cache.go), which exist for a different purpose (collision
// avoidance between differently-shaped cache keys) and must never be
// repurposed here. targetKindMCP/targetKindAgent (mcp_a2a.go) already
// equal "mcp"/"a2a" and are used directly at their own call site rather
// than duplicated as a second pair of constants here.
const (
	routeChatCompletions     = "chat/completions"
	routeEmbeddings          = "embeddings"
	routeMessages            = "messages"
	routeImages              = "images"
	routeAudioSpeech         = "audio/speech"
	routeAudioTranscriptions = "audio/transcriptions"
	routePassthrough         = "passthrough"
	routeMCPFederated        = "mcp-federated"
	routePricing             = "pricing"
	// routeCapacity is NOT part of plan §1.4's route vocabulary: the
	// capacity hook (acquireBodyAdmission, routes_unified.go) is a
	// route-agnostic shared semaphore shared with routes_messages.go
	// (unowned this round — see acquireBodyAdmission's own doc comment),
	// so it cannot cheaply name which endpoint hit capacity without a
	// signature change that would ripple into that unowned file. This
	// fixed value is the documented, deliberate simplification.
	routeCapacity = "capacity"
)

// eventRouteForEndpoint maps runMeteredCall's own endpoint argument
// (cacheEndpointChat/cacheEndpointEmbeddings/cacheEndpointMessages,
// cache.go/routes_messages.go) to the Route vocabulary above.
// cacheEndpointEmbeddings/cacheEndpointMessages already equal
// "embeddings"/"messages" verbatim; only cacheEndpointChat ("chat") needs
// remapping to "chat/completions".
func eventRouteForEndpoint(endpoint string) string {
	if endpoint == cacheEndpointChat {
		return routeChatCompletions
	}
	return endpoint
}

// gatewayEvent is one entry in GET /admin/api/events' feed — the exact
// shape stored as JSON in both the local ring and the Redis-backed list,
// so read (below) can unmarshal a Redis entry straight into this type
// with no translation step. Route values are the vocabulary above; Kind
// is the eventKind* vocabulary; User/Group/Model/Provider/Instance/Status
// are omitted when not applicable to the hook that recorded this event.
//
// Risk (plan §6): Message is always either a fixed string or
// sanitizeProviderErr's scrubbed output — never a raw response body,
// header, or key. Every hook below is written to that constraint; see
// events_test.go's own credential-scrubbing assertion.
type gatewayEvent struct {
	Time     time.Time `json:"time"`
	Replica  string    `json:"replica"`
	Instance string    `json:"instance,omitempty"`
	User     string    `json:"user,omitempty"`
	Group    string    `json:"group,omitempty"`
	Model    string    `json:"model,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Route    string    `json:"route"`
	Kind     string    `json:"kind"`
	Message  string    `json:"message"`
	Status   int       `json:"status,omitempty"`
}

// adminEventsResponse is the full body of GET /admin/api/events.
type adminEventsResponse struct {
	Source   string         `json:"source"`
	Replica  string         `json:"replica"`
	Events   []gatewayEvent `json:"events"`
	Capacity int            `json:"capacity"`
	Degraded bool           `json:"degraded,omitempty"`
}

// eventRing is a fixed-capacity, newest-overwrites-oldest ring buffer of
// gatewayEvent — this process's own local fallback event feed (F3, v0.3
// dashboard task), always able to answer GET /admin/api/events even when
// Redis is absent or unreachable (fail-open: recordEvent's own doc
// comment). Guarded by mu: add runs on every metered request's error
// path, snapshot on every admin poll — both must be safe for concurrent
// use across goroutines.
type eventRing struct {
	buf  [eventRingCap]gatewayEvent
	mu   sync.Mutex
	next int // index the NEXT add writes to
	n    int // number of valid entries so far, capped at eventRingCap
}

// add appends ev, overwriting the oldest entry once the ring is full.
func (r *eventRing) add(ev gatewayEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = ev
	r.next = (r.next + 1) % eventRingCap
	if r.n < eventRingCap {
		r.n++
	}
}

// snapshot returns up to limit of the ring's most recently added entries,
// newest first, as a fresh copy the caller may retain or mutate freely —
// never sharing the ring's own backing array.
func (r *eventRing) snapshot(limit int) []gatewayEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.n
	if limit < n {
		n = limit
	}
	out := make([]gatewayEvent, n)
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + eventRingCap) % eventRingCap
		out[i] = r.buf[idx]
	}
	return out
}

// eventLog is the Gateway-level owner of the event feed: the local ring
// (always answers) plus a best-effort mirror of every event onto a
// Redis-backed capped list, so a dashboard reading through ANY replica in
// a multi-instance deployment sees every replica's events, not just its
// own. Built once in newGateway, after g.redisClient/g.limiter — client
// is the SAME shared respClient the limiter's redisStore uses (spec §2's
// "one connection, one config"), and storeLatched (recordEvent) borrows
// the limiter's own store-health latch rather than maintaining a second,
// independent one for the identical underlying connection.
type eventLog struct {
	ring     *eventRing
	client   *respClient // nil when Redis is not configured
	replica  string
	instance string
	nowFn    func() time.Time
	spawn    func(func())
	logf     func(format string, args ...any)
	logGate  logGate

	throttleMu     sync.Mutex
	throttleSecond int64 // unix second of the current push-rate window
	throttleCount  int   // pushes already spawned within throttleSecond
}

// allowPush reports whether one more Redis push may be spawned this
// second, enforcing eventPushPerSecond (Q3, coordinator ruling: "push
// throttle 50/s/replica: accepted") — a construction-warning storm or a
// sustained rejection flood must never turn recordEvent's best-effort
// Redis mirror into an unbounded write amplifier against the shared
// store. The local ring (recordEvent's own fail-open path) is never
// throttled: every event always lands there regardless of this limit.
func (el *eventLog) allowPush(now time.Time) bool {
	sec := now.Unix()
	el.throttleMu.Lock()
	defer el.throttleMu.Unlock()
	if sec != el.throttleSecond {
		el.throttleSecond = sec
		el.throttleCount = 0
	}
	if el.throttleCount >= eventPushPerSecond {
		return false
	}
	el.throttleCount++
	return true
}

// push mirrors ev to Redis's capped event list in one pipelined round
// trip: LPUSH the newest entry, LTRIM to eventRingCap-1 immediately after
// — never a separate call, so the list can never be observed to grow
// past eventRingCap between the two. Runs inside el.spawn's own goroutine
// (recordEvent's own doc comment), off the request's hot path. Its own
// failure is logged through el.logGate (rate-limited, mirroring
// limiter.logStoreError's identical shape) and NEVER calls
// limiter.recordStoreFailure — a lost event mirror is not a limit-store
// outage and must not open the limiter's fail-open/fail-closed latch for
// unrelated request traffic.
func (el *eventLog) push(ev gatewayEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return // a gatewayEvent literal always marshals; defensive only
	}
	_, err = el.client.pipeline([][]string{
		{"LPUSH", eventsRedisKey, string(payload)},
		{"LTRIM", eventsRedisKey, "0", strconv.Itoa(eventRingCap - 1)},
	})
	if err != nil {
		if log, suppressed := el.logGate.shouldLog(el.nowFn()); log {
			// Yaegi trap (limiter.logf's own doc comment, limits.go): a
			// struct field of variadic func type crashes Yaegi v0.16.1's
			// CFG builder past ONE variadic argument. el.logf is exactly
			// such a field, so the message is pre-formatted here and
			// passed through as the single variadic argument, mirroring
			// limits.go's own l.logf("%s", fmt.Sprintf(...)) pattern.
			el.logf("%s", fmt.Sprintf("events: push to redis failed%s: %v", suppressedSuffix(suppressed), err))
		}
	}
}

// read answers GET /admin/api/events: up to limit events from the
// Redis-backed list when el has a configured, reachable client (source
// "redis"), or this replica's own local ring otherwise (source
// "replica"). degraded is true only when a store IS configured but this
// particular read failed — never for "Redis not configured at all", which
// is expected, ordinary operation for a single-replica or Redis-less
// deployment, not degradation.
//
// storeLatched is the limiter's own storeLatched() (limits.go), passed in
// by the caller (serveAdminEvents, admin.go) rather than read from a
// stored reference here — mirrors recordEvent's identical check, just
// evaluated by the caller instead of el itself. While latched, LRANGE is
// SKIPPED, not attempted: recordEvent already skips the Redis mirror
// push for the same reason (its own doc comment), so an LRANGE issued
// here would fail identically, just after paying respCallTimeout to find
// out — on every admin poll, for the whole latch window. Going straight
// to the ring is the same "store IS configured but this particular read
// failed" outcome a failed LRANGE already produces below (source
// "replica", degraded true), at zero network cost.
func (el *eventLog) read(limit int, storeLatched bool) (events []gatewayEvent, source string, degraded bool) {
	if el == nil {
		return []gatewayEvent{}, "replica", false
	}
	if el.client == nil {
		return el.ring.snapshot(limit), "replica", false
	}
	if storeLatched {
		return el.ring.snapshot(limit), "replica", true
	}
	reply, err := el.client.do("LRANGE", eventsRedisKey, "0", strconv.Itoa(limit-1))
	if err != nil {
		return el.ring.snapshot(limit), "replica", true
	}
	arr, ok := reply.([]any)
	if !ok {
		return el.ring.snapshot(limit), "replica", true
	}
	out := make([]gatewayEvent, 0, len(arr))
	for _, item := range arr {
		b, ok := item.([]byte)
		if !ok {
			continue // a malformed or error-typed entry — skip it, never fail the whole read for one bad element
		}
		var ev gatewayEvent
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, "redis", false
}

// recordEvent stamps ev's Time/Replica/Instance, appends it to the local
// ring unconditionally — fail-open: the ring always answers GET
// /admin/api/events regardless of Redis — and, only when a store is
// configured, currently reachable (per the limiter's own storeLatched),
// and the push-rate throttle allows it, mirrors ev to Redis via el.spawn,
// off the request's own hot path — exactly like recordProviderAttempt's
// identical telemetry-only spawn (limits.go). Every call site below is on
// an ERROR path only; a successful request never reaches here, so this
// costs nothing on the hot path.
func (g *Gateway) recordEvent(ev gatewayEvent) {
	el := g.events
	if el == nil {
		return
	}
	ev.Time = el.nowFn()
	ev.Replica = el.replica
	ev.Instance = el.instance
	el.ring.add(ev)

	if el.client == nil || g.limiter.storeLatched() || !el.allowPush(ev.Time) {
		return
	}
	el.spawn(func() { el.push(ev) })
}

// applyScopeAttribution sets ev.User/ev.Group from the first "user"/
// "group"-kind scope in scopes (buildLimitScopes' own user-then-group
// order) — shared by every F3 hook below that records an event alongside
// a limitScope slice.
func applyScopeAttribution(ev *gatewayEvent, scopes []limitScope) {
	for _, sc := range scopes {
		switch sc.kind {
		case "user":
			if ev.User == "" {
				ev.User = sc.id
			}
		case "group":
			if ev.Group == "" {
				ev.Group = sc.id
			}
		}
	}
}

// recordLimitEvent records a rate_limit/budget/store_down event for a
// checkAndCount rejection (F3 hook 1) — scopes is the SAME slice
// checkAndCount was called with, so applyScopeAttribution finds the
// caller's own User/Group; v.kind/v.message/v.storeDown (limits.go)
// already carry everything else. Status mirrors
// writeLimitViolationEnvelope's own HTTP mapping (routes_unified.go):
// 503 for a store-down violation, 429 otherwise.
func (g *Gateway) recordLimitEvent(scopes []limitScope, route string, v *limitViolation) {
	status := http.StatusTooManyRequests
	if v.storeDown {
		status = http.StatusServiceUnavailable
	}
	ev := gatewayEvent{Route: route, Kind: v.kind, Message: v.message, Status: status}
	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
}

// providerBaseURL returns name's configured base URL from the current
// registry snapshot, or "" when no provider by that name is configured —
// recordUpstreamEvent's own lookup for sanitizeProviderErr (admin.go),
// which needs the RAW base URL to scrub, not just the provider's name.
// Error-path only (never called from a successful request), so the
// linear scan over the small, operator-configured provider list costs
// nothing hot-path-relevant.
func (g *Gateway) providerBaseURL(name string) string {
	for _, s := range g.registry.snapshot() {
		if s.name == name {
			return s.baseURL
		}
	}
	return ""
}

// recordUpstreamEvent classifies err — an adapter or failover-loop
// failure, from a runMeteredCall/routes_media.go call site — into a
// timeout/upstream event and records it, or records nothing for an error
// class the dashboard has no use for (F3 hook 2):
//
//   - matchesSentinel(err, errProviderTimeout) or isDeadlineExceeded:
//     eventKindTimeout, HTTP 504.
//   - *providerHTTPError: eventKindUpstream at the provider's own status,
//     but ONLY when that status is 429 or >=500 — an ordinary 4xx is the
//     CLIENT's own request being rejected, not a provider/gateway
//     problem worth surfacing here.
//   - *translateError or context.Canceled: no event — neither says
//     anything about upstream health.
//   - anything else: eventKindUpstream, HTTP 502, message scrubbed via
//     sanitizeProviderErr (never the raw err text unscrubbed).
//
// model is the canonical "provider/model" id being attempted; provider is
// the provider's own configured name.
func (g *Gateway) recordUpstreamEvent(scopes []limitScope, model, provider, route string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if _, ok := err.(*translateError); ok {
		return
	}

	// ev's fields are set ONE AT A TIME below, never through a single
	// "kind, status, message = a, b, c()" tuple assignment (the shape
	// this switch used before this fix): yaegi v0.16.1 silently keeps
	// only the LAST value of a tuple assignment whose right-hand side
	// mixes a plain expression with a function/method call — every
	// earlier target is left at its zero value, with no panic and no
	// compile error, so a compiled `go test` run can never catch it
	// (verify-dash-backend.md's probe against scratchpad/yrepro is what
	// did: kind/status stayed "" and 0 for every upstream and timeout
	// event recorded under the real interpreter). This is a DIFFERENT
	// symptom from the multi-value-assignment trap translate_gemini.go's
	// hasMaxTokens comment documents (mixing a variable and a literal
	// there panics outright); this one fails silently, which is why every
	// assignment below — and every other tuple assignment introduced in
	// this round — is written as separate statements instead.
	//
	// The timeout branch also now runs through sanitizeProviderErr, like
	// the generic branch below: a watchdog or context-deadline error can
	// embed the same dialed URL (and any credentials/query on it) a
	// generic connection error does, and only the generic branch used to
	// scrub it.
	ev := gatewayEvent{Model: model, Provider: provider, Route: route}
	switch {
	case matchesSentinel(err, errProviderTimeout) || isDeadlineExceeded(err):
		ev.Kind = eventKindTimeout
		ev.Status = http.StatusGatewayTimeout
		ev.Message = sanitizeProviderErr(err.Error(), g.providerBaseURL(provider))
	default:
		if perr, ok := err.(*providerHTTPError); ok {
			if perr.status != http.StatusTooManyRequests && perr.status < http.StatusInternalServerError {
				return
			}
			ev.Kind = eventKindUpstream
			ev.Status = perr.status
			ev.Message = perr.Error()
		} else {
			ev.Kind = eventKindUpstream
			ev.Status = http.StatusBadGateway
			ev.Message = sanitizeProviderErr(err.Error(), g.providerBaseURL(provider))
		}
	}

	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
}

// recordUnpricedRefusalEvent records the 402 "no configured price, and a
// cost budget applies" refusal (F3 hook 5, item 5 this round) shared by
// routes_unified.go's own F-1 gate and routes_passthrough.go's identical
// guard (review-routes.md, finding 6) — the one place in each file that
// refuses the request outright rather than billing it 0. Reuses
// eventKindUnpriced, the SAME kind hook 4 (pricingWarn, logger.go)
// already records for the "billed 0" case; Status is what tells the two
// apart in the feed (402 here, unset/0 there — a billed-0 event carries
// no HTTP status of its own, since nothing was refused).
func (g *Gateway) recordUnpricedRefusalEvent(scopes []limitScope, model, provider, route string) {
	ev := gatewayEvent{
		Model: model, Provider: provider, Route: route, Kind: eventKindUnpriced, Status: http.StatusPaymentRequired,
		Message: fmt.Sprintf("model %q refused: no configured price, so the cost budget that applies to this caller cannot be enforced", model),
	}
	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
	// r402 (always-on-with-admin, DECISIONS) — the one chokepoint both
	// routes_unified.go's and routes_passthrough.go's identical 402 guard
	// already share, so wiring the counter here covers both call sites
	// without touching either route file again. Fire-and-forget
	// (recordUnpriced402's own countAsync/l.spawn); no-op when admin
	// stats are off.
	g.limiter.recordUnpriced402(model)
}

// recordProxyEvent records an upstream event for a proxyUpstream result
// that failed at 429/>=500, or never produced any response at all
// (result.status == 0) — F3 hook 3 (routes_passthrough.go's
// handlePassthrough, mcp_a2a.go's handleTargetProxy). Unlike
// recordUpstreamEvent (hook 2), proxyUpstream reports only a status code,
// not the underlying error (already logged at its own call site) — there
// is nothing for sanitizeProviderErr to scrub here, so the message is a
// fixed, route-scoped string, never upstream body/header/key content. A
// client-canceled result records nothing, matching hook 2's identical
// context.Canceled exclusion.
func (g *Gateway) recordProxyEvent(scopes []limitScope, provider, route string, result proxyResult) {
	if result.clientCanceled {
		return
	}
	if result.status != 0 && result.status != http.StatusTooManyRequests && result.status < http.StatusInternalServerError {
		return
	}
	status := result.status
	message := fmt.Sprintf("%s upstream returned status %d", route, result.status)
	if status == 0 {
		status = http.StatusBadGateway
		message = "upstream connection error"
	}
	ev := gatewayEvent{Provider: provider, Route: route, Kind: eventKindUpstream, Message: message, Status: status}
	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
}
