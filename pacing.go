package traefikllmgateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxPacingDelay keeps a paced request inside a CDN's time-to-first-byte
	// budget.
	maxPacingDelay = 20 * time.Second
	// maxConcurrentPaced caps the requests one user can have waiting in a
	// pacing delay at once.
	maxConcurrentPaced = 4
	// pacedHeader reports the pacing delay applied to a request.
	pacedHeader = "X-Llmgw-Paced-Ms"
	// eventKindPaced tags the rate-gated pacing event.
	eventKindPaced = "paced"
	// pacedEventScope keys the pacing event throttle in limiter.lastSeen.
	pacedEventScope = "paced"
)

// validate reports an error for a malformed pacing block. Nil is valid.
func (p *PacingConfig) validate() error {
	if p == nil {
		return nil
	}
	if p.StartPct < 1 || p.StartPct > 99 {
		return fmt.Errorf("llmgateway: limits: pacing.startPct must be between 1 and 99, got %d", p.StartPct)
	}
	d, err := time.ParseDuration(p.MaxDelay)
	if err != nil {
		return fmt.Errorf("llmgateway: limits: pacing.maxDelay %q is not a duration: %v", p.MaxDelay, err)
	}
	if d <= 0 || d > maxPacingDelay {
		return fmt.Errorf("llmgateway: limits: pacing.maxDelay must be greater than 0 and at most %s, got %s", maxPacingDelay, d)
	}
	p.maxDelay = d
	return nil
}

// maxDelayDuration returns the parsed MaxDelay, parsing it when validate has
// not run (a config built directly in code). It returns 0 for a bad value.
func (p *PacingConfig) maxDelayDuration() time.Duration {
	if p.maxDelay > 0 {
		return p.maxDelay
	}
	d, err := time.ParseDuration(p.MaxDelay)
	if err != nil || d <= 0 {
		return 0
	}
	if d > maxPacingDelay {
		return maxPacingDelay
	}
	return d
}

// delay returns the wait for a request when the usage fraction is frac
// (1.0 is the limit): 0 up to StartPct, then linear up to MaxDelay at 100%.
func (p *PacingConfig) delay(frac float64) time.Duration {
	if p == nil {
		return 0
	}
	maxD := p.maxDelayDuration()
	start := float64(p.StartPct) / 100
	if maxD <= 0 || frac <= start {
		return 0
	}
	if frac >= 1 {
		return maxD
	}
	d := time.Duration(float64(maxD) * (frac - start) / (1 - start))
	if d > maxD {
		return maxD
	}
	return d
}

// pacedSlotGate counts each user's requests currently waiting in a pacing
// delay.
type pacedSlotGate struct {
	waiting map[string]int
	mu      sync.Mutex
}

// acquire reserves a pacing slot for user. It reports false at the cap.
func (p *pacedSlotGate) acquire(user string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[string]int{}
	}
	if p.waiting[user] >= maxConcurrentPaced {
		return false
	}
	p.waiting[user]++
	return true
}

func (p *pacedSlotGate) release(user string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting[user] <= 1 {
		delete(p.waiting, user)
		return
	}
	p.waiting[user]--
}

// rateInfo is one admitted-or-refused request's view of the user scope's
// minute windows, taken from the counters checkAndCount already read.
type rateInfo struct {
	reset        time.Time
	pacing       *PacingConfig
	reqLimit     int64
	reqRemaining int64
	tokLimit     int64
	tokRemaining int64
	frac         float64
}

func remainingOf(limit, used int64) int64 {
	if used >= limit {
		return 0
	}
	return limit - used
}

// buildRateInfo derives the minute-window view for one user scope. It
// returns nil when the scope sets neither a minute limit nor pacing.
func buildRateInfo(sc limitScope, minCount, tokUsed int64, now time.Time) *rateInfo {
	lim := sc.limits
	if lim == nil || (lim.RequestsPerMinute <= 0 && lim.TokensPerMinute <= 0) {
		return nil
	}
	info := &rateInfo{reset: windowEnd(now, windowMin), pacing: lim.Pacing}
	if lim.RequestsPerMinute > 0 {
		info.reqLimit = lim.RequestsPerMinute
		info.reqRemaining = remainingOf(lim.RequestsPerMinute, minCount)
		info.frac = float64(minCount) / float64(lim.RequestsPerMinute)
	}
	if lim.TokensPerMinute > 0 {
		info.tokLimit = lim.TokensPerMinute
		info.tokRemaining = remainingOf(lim.TokensPerMinute, tokUsed)
		if f := float64(tokUsed) / float64(lim.TokensPerMinute); f > info.frac {
			info.frac = f
		}
	}
	return info
}

// refuse makes the view agree with a 429: the refused window reports zero
// remaining. A refusal on a window the headers do not describe (a day limit,
// another scope's limit) zeroes every reported window and moves the reset to
// the refused window's end, matching Retry-After.
func (info *rateInfo) refuse(v *limitViolation, now time.Time) {
	if info == nil || v == nil {
		return
	}
	switch {
	case v.limit == "requests-per-minute" && info.reqLimit > 0:
		info.reqRemaining = 0
		return
	case v.limit == "tokens-per-minute" && info.tokLimit > 0:
		info.tokRemaining = 0
		return
	}
	info.reqRemaining = 0
	info.tokRemaining = 0
	if v.retryAfter > 0 {
		info.reset = now.Add(time.Duration(v.retryAfter) * time.Second)
	}
}

func resetSeconds(reset, now time.Time) int64 {
	s := int64(reset.Sub(now).Seconds() + 0.999)
	if s < 0 {
		return 0
	}
	return s
}

// removePolicyMember drops the list member named name from an RFC 8941 list.
func removePolicyMember(list, name string) string {
	if list == "" {
		return ""
	}
	prefix := `"` + name + `"`
	var kept []string
	for _, m := range strings.Split(list, ", ") {
		if !strings.HasPrefix(m, prefix) {
			kept = append(kept, m)
		}
	}
	return strings.Join(kept, ", ")
}

func appendPolicyMember(h http.Header, member string) {
	cur := h.Get(rateLimitPolicyHeader)
	if cur != "" {
		member = cur + ", " + member
	}
	h.Set(rateLimitPolicyHeader, member)
}

// writeRateLimitHeaders sets the OpenAI, Anthropic and IETF draft headers
// for the configured minute windows. The RateLimit-Policy header keeps any
// concurrency member already present.
func writeRateLimitHeaders(h http.Header, info *rateInfo, now time.Time) {
	if info == nil {
		return
	}
	secs := resetSeconds(info.reset, now)
	resetDur := strconv.FormatInt(secs, 10) + "s"
	resetAt := info.reset.UTC().Format(time.RFC3339)
	var quota, policy []string
	if info.reqLimit > 0 {
		h.Set("x-ratelimit-limit-requests", strconv.FormatInt(info.reqLimit, 10))
		h.Set("x-ratelimit-remaining-requests", strconv.FormatInt(info.reqRemaining, 10))
		h.Set("x-ratelimit-reset-requests", resetDur)
		h.Set("anthropic-ratelimit-requests-limit", strconv.FormatInt(info.reqLimit, 10))
		h.Set("anthropic-ratelimit-requests-remaining", strconv.FormatInt(info.reqRemaining, 10))
		h.Set("anthropic-ratelimit-requests-reset", resetAt)
		quota = append(quota, `"requests";r=`+strconv.FormatInt(info.reqRemaining, 10)+";t="+strconv.FormatInt(secs, 10))
		policy = append(policy, `"requests";q=`+strconv.FormatInt(info.reqLimit, 10)+";w=60")
	}
	if info.tokLimit > 0 {
		h.Set("x-ratelimit-limit-tokens", strconv.FormatInt(info.tokLimit, 10))
		h.Set("x-ratelimit-remaining-tokens", strconv.FormatInt(info.tokRemaining, 10))
		h.Set("x-ratelimit-reset-tokens", resetDur)
		h.Set("anthropic-ratelimit-tokens-limit", strconv.FormatInt(info.tokLimit, 10))
		h.Set("anthropic-ratelimit-tokens-remaining", strconv.FormatInt(info.tokRemaining, 10))
		h.Set("anthropic-ratelimit-tokens-reset", resetAt)
		quota = append(quota, `"tokens";r=`+strconv.FormatInt(info.tokRemaining, 10)+";t="+strconv.FormatInt(secs, 10))
		policy = append(policy, `"tokens";q=`+strconv.FormatInt(info.tokLimit, 10)+";w=60")
	}
	if len(quota) == 0 {
		return
	}
	h.Set("RateLimit", strings.Join(quota, ", "))
	appendPolicyMember(h, strings.Join(policy, ", "))
}

// isRateLimitHeader reports whether canonical header key k is a rate-limit
// header in any dialect the gateway writes.
func isRateLimitHeader(k string) bool {
	lk := strings.ToLower(k)
	return strings.HasPrefix(lk, "x-ratelimit-") || strings.HasPrefix(lk, "anthropic-ratelimit-") || lk == "ratelimit" || lk == "ratelimit-policy"
}

// snapshotRateLimitHeaders returns a copy of the rate-limit headers in h, or
// nil when h carries none.
func snapshotRateLimitHeaders(h http.Header) http.Header {
	var snap http.Header
	for k, vs := range h {
		if !isRateLimitHeader(k) {
			continue
		}
		if snap == nil {
			snap = http.Header{}
		}
		snap[k] = append([]string(nil), vs...)
	}
	return snap
}

// restoreRateLimitHeaders replaces every rate-limit header in h with snap, so
// an upstream's own quota headers never override the gateway's. A nil snap
// leaves h untouched.
func restoreRateLimitHeaders(h, snap http.Header) {
	if snap == nil {
		return
	}
	for k := range h {
		if isRateLimitHeader(k) {
			delete(h, k)
		}
	}
	for k, vs := range snap {
		h[k] = vs
	}
}

// pace waits for the pacing delay unless ctx ends first. It reports false
// when the client went away during the wait.
func pace(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// admitCounted runs checkAndCount for scopes, writes the rate-limit headers
// on w for the user's effective limits, and on admission waits out the
// pacing delay. Unified and passthrough routes wait before reading the body;
// /v1/messages waits after, because it must decode the body first to reject
// streaming without charging the limit. Validation therefore requires
// requestsPerMinute for pacing, which bounds how many requests can wait, and
// each user has at most maxConcurrentPaced waiting: further requests proceed
// without delay. gone is true when the client cancelled during the wait; the
// caller must then write nothing.
func (g *Gateway) admitCounted(ctx context.Context, w http.ResponseWriter, scopes []limitScope, reserve int64, exempt bool) (*limitViolation, bool) {
	if exempt {
		g.limiter.countExempt(scopes)
		return nil, false
	}
	violation, info := g.limiter.checkAndCountInfo(scopes, reserve)
	now := g.limiter.now()
	if g.cfg.RateLimitHeaders {
		writeRateLimitHeaders(w.Header(), info, now)
	}
	if violation != nil || info == nil {
		return violation, false
	}
	if info.pacing == nil {
		return nil, false
	}
	d := info.pacing.delay(info.frac)
	if d <= 0 {
		return nil, false
	}
	user := userScopeName(scopes)
	if !g.limiter.pacedSlots.acquire(user) {
		return nil, false
	}
	defer g.limiter.pacedSlots.release(user)
	w.Header().Set(pacedHeader, strconv.FormatInt(d.Milliseconds(), 10))
	g.recordPacedEvent(scopes, d)
	if !pace(ctx, d) {
		g.limiter.releaseReservations(scopes)
		return nil, true
	}
	return nil, false
}

// recordPacedEvent records at most one pacing event per user per throttle
// window.
func (g *Gateway) recordPacedEvent(scopes []limitScope, d time.Duration) {
	ev := gatewayEvent{Route: "pacing", Kind: eventKindPaced, Message: "request paced " + d.String()}
	applyScopeAttribution(&ev, scopes)
	if ev.User == "" || !g.limiter.lastSeen.due(pacedEventScope, ev.User, g.limiter.now()) {
		return
	}
	g.recordEvent(ev)
}

func userScopeName(scopes []limitScope) string {
	for _, sc := range scopes {
		if sc.kind == "user" {
			return sc.id
		}
	}
	return ""
}
