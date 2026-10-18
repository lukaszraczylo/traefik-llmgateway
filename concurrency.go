package traefikllmgateway

import (
	"context"
	"net/http"
	"strconv"
	"sync"
)

// rateLimitPolicyHeader carries the concurrency limit as an RFC 8941
// structured field, per draft-ietf-httpapi-ratelimit-headers.
const rateLimitPolicyHeader = "RateLimit-Policy"

const (
	concurrencyPolicyName = "concurrency"
	concurrencyUnit       = "concurrent-requests"
	// successesPerRegrow is how many consecutive upstream successes raise
	// the learned limit by one.
	successesPerRegrow = 4
	// learnedCeiling is the learned value at which the limit counts as
	// unknown again.
	learnedCeiling = 64
)

// concurrencyPolicyValue renders n as a RateLimit-Policy list member.
func concurrencyPolicyValue(n int) string {
	return `"` + concurrencyPolicyName + `";q=` + strconv.Itoa(n) + `;qu="` + concurrencyUnit + `"`
}

// providerConcurrency is one provider's learned state.
type providerConcurrency struct {
	inFlight  int
	learned   int // 0 means unknown
	successes int
}

// concurrencyLearner learns, per configured provider, how many requests the
// upstream serves at once. Keys are configured provider names only, so
// memory stays bounded.
type concurrencyLearner struct {
	byProvider map[string]*providerConcurrency
	mu         sync.Mutex
	explicit   int
}

func newConcurrencyLearner(explicit int) *concurrencyLearner {
	return &concurrencyLearner{byProvider: map[string]*providerConcurrency{}, explicit: explicit}
}

func (c *concurrencyLearner) state(provider string) *providerConcurrency {
	s := c.byProvider[provider]
	if s == nil {
		s = &providerConcurrency{}
		c.byProvider[provider] = s
	}
	return s
}

// begin counts one in-flight upstream request; the returned func ends it.
func (c *concurrencyLearner) begin(provider string) func() {
	c.mu.Lock()
	c.state(provider).inFlight++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.state(provider).inFlight--
			c.mu.Unlock()
		})
	}
}

// observe feeds one upstream attempt outcome into the provider's state.
func (c *concurrencyLearner) observe(provider string, resp *http.Response, err error) {
	overload := false
	success := false
	switch {
	case resp != nil:
		code := resp.StatusCode
		overload = code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
		success = code < http.StatusBadRequest
	case err != nil:
		overload = isDeadlineExceeded(err)
	}
	if !overload && !success {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state(provider)
	if overload {
		s.successes = 0
		next := s.inFlight / 2
		if next < 1 {
			next = 1
		}
		if s.learned == 0 || next < s.learned {
			s.learned = next
		}
		return
	}
	if s.learned == 0 {
		return
	}
	s.successes++
	if s.successes < successesPerRegrow {
		return
	}
	s.successes = 0
	s.learned++
	if s.learned >= learnedCeiling {
		s.learned = 0
	}
}

// limit returns the effective advertised limit for provider: the smaller of
// the explicit and learned values, or 0 when neither is known.
func (c *concurrencyLearner) limit(provider string) int {
	c.mu.Lock()
	learned := 0
	if s := c.byProvider[provider]; s != nil {
		learned = s.learned
	}
	c.mu.Unlock()
	switch {
	case c.explicit > 0 && (learned == 0 || c.explicit < learned):
		return c.explicit
	default:
		return learned
	}
}

// setConcurrencyHeader sets or clears the RateLimit-Policy header on h.
func setConcurrencyHeader(h http.Header, n int) {
	if n > 0 {
		h.Set(rateLimitPolicyHeader, concurrencyPolicyValue(n))
		return
	}
	h.Del(rateLimitPolicyHeader)
}

// trackUpstream wires per-provider accounting into ctx: the limiter's
// attempt record and, when concurrencyAutodetect is on, in-flight tracking,
// learning and the RateLimit-Policy header on w. The returned func must run
// when the upstream exchange ends.
func (g *Gateway) trackUpstream(ctx context.Context, w http.ResponseWriter, provider, model string) (context.Context, func()) {
	c := g.concurrency
	if c == nil {
		return withAttemptRecorder(ctx, func(resp *http.Response, attemptErr error) {
			g.limiter.recordProviderAttempt(provider, model, resp, attemptErr)
		}), func() {}
	}
	end := c.begin(provider)
	setConcurrencyHeader(w.Header(), c.limit(provider))
	return withAttemptRecorder(ctx, func(resp *http.Response, attemptErr error) {
		g.limiter.recordProviderAttempt(provider, model, resp, attemptErr)
		c.observe(provider, resp, attemptErr)
		// The watchdog also calls this from a timer goroutine with a nil
		// resp; only a real response means the request goroutine still owns w.
		if resp != nil {
			setConcurrencyHeader(w.Header(), c.limit(provider))
		}
	}), end
}
