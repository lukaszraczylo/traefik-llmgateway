package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// defaultRetryAttempts and defaultRetryBackoff are the values
// newRetryPolicy applies when a Retry config block is Enabled but leaves
// Attempts or Backoff at its zero value — spec §1's stated defaults.
// defaultRetryAttempts counts RETRIES performed after the initial try
// (see retryPolicy's doc comment), so the default of 1 means one retry —
// two tries total — not one try total.
const (
	defaultRetryAttempts = 1
	defaultRetryBackoff  = "250ms"
)

// maxRetryAttempts is the highest RetryConfig.Attempts newRetryPolicy
// accepts — spec §1's "attempts: int (default 1, max 3)". Like
// defaultRetryAttempts, this counts retries after the initial try: the
// maximum makes four tries total, not three.
const maxRetryAttempts = 3

// maxRetryWait caps the wait retryPolicy.do computes for ITSELF — the
// exponential backoff ladder — between attempts. spec §1's "capped at 2s
// per wait". An upstream's own explicit Retry-After request is a
// separate case with its own, much larger cap (finding 12 fix,
// review-routes.md: this comment previously and incorrectly claimed
// maxRetryWait also caps a Retry-After-derived wait) — see
// maxRetryAfterWait, below.
const maxRetryWait = 2 * time.Second

// maxRetryAfterWait caps a wait this gateway will honour when the
// UPSTREAM itself asked for it via a 429's Retry-After header. It is
// deliberately far larger than maxRetryWait, which caps only the wait
// this gateway computes for itself.
//
// Security audit run-1 (retry/failover fan-out record): previously a
// Retry-After larger than maxRetryWait was DISCARDED and replaced by the
// <=2s exponential ladder, so a saturated provider could not tell this
// gateway to back off for longer than two seconds — the gateway answered
// an explicit overload signal by retrying sooner than asked. Honouring it
// up to this ceiling can only ever make the gateway wait LONGER, never
// sooner, so it strictly reduces outbound pressure. p.waitFn is
// ctx-aware, so a client disconnect or request deadline still aborts the
// wait immediately rather than blocking out the full duration.
const maxRetryAfterWait = 30 * time.Second

// retryJitterFn spreads a COMPUTED backoff so that many requests failing
// at the same instant do not retry in lockstep against the same upstream
// — the synchronized-retry shape that turns one upstream blip into a
// self-sustaining thundering herd (security audit run-1). It applies
// EQUAL jitter: half the computed wait, plus a uniform random draw over
// the other half, so a jittered wait is always at least d/2 and never
// collapses to an immediate retry the way full jitter can.
//
// Finding 4 fix (review-routes.md): do, below, applies this ONLY to
// waitBefore's exponential-backoff branch, never to a wait that came
// from an upstream's own Retry-After header. Jittering a Retry-After
// value can shorten it (equal jitter draws anywhere in [d/2, d]), which
// would retry a saturated provider SOONER than it explicitly asked —
// exactly the backwards-under-overload behavior maxRetryAfterWait's own
// doc comment says honouring Retry-After exists to prevent ("can only
// ever make the gateway wait LONGER, never sooner"). waitBefore's second
// return value tells do which branch produced the wait, so this
// function itself needs no such distinction — it always jitters
// unconditionally; the caller decides whether to call it at all.
//
// A package-level var, mirroring retryPolicy.waitFn's own convention, so
// tests can substitute an identity function and keep asserting exact
// durations. It is applied in do, not in waitBefore, so waitBefore stays
// a pure function of (attempt, resp) and its own unit tests keep testing
// the backoff ladder itself rather than the randomness on top of it.
var retryJitterFn = func(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	// #nosec G404 -- this randomness spreads retry timing to break up a
	// synchronized herd; it guards no secret and gates no decision, so a
	// predictable draw grants an attacker nothing. crypto/rand would add
	// a syscall per retry wait and an error path, for no security gain.
	return half + time.Duration(rand.Int63n(int64(d-half)+1))
}

// retryDrainLimit caps how much of a retried-away response's body
// drainAndClose reads before closing it. It is generous enough to let
// the underlying connection settle for reuse, but far below
// maxResponseBytes (32MiB): a failed attempt's body is discarded, never
// parsed, so there is no reason to pay for reading a large one in full.
const retryDrainLimit = 64 << 10

// retryPolicy implements spec §1's same-provider retry.
//
// attempts counts RETRIES performed after the initial try, not the
// total try count — ruling (controller review, 2026-08-20): the earlier
// "attempts = total tries" reading made the documented default of 1
// perform zero retries, silently defeating retry:{enabled:true}'s own
// defaults. With this policy, attempts=1 (the default) means one retry —
// two tries total; attempts=3 (the max) means three retries — four
// tries total. totalTries() is the single place that turns attempts
// into the actual try count do's loop uses.
//
// A nil *retryPolicy, or one with enabled=false, makes do try call
// exactly once — the same shape every v0.1 adapter constructor leaves
// its adapter's retry field in, since none of them set it. waitFn is a
// field (not a direct call to a wait implementation) so tests can inject
// a non-blocking spy instead of paying real wall-clock time; the
// production default (defaultWaitFn) is ctx-aware.
type retryPolicy struct {
	waitFn   func(context.Context, time.Duration) bool
	backoff  time.Duration
	attempts int
	enabled  bool
}

// totalTries returns how many times do should call call() in total: 1
// for a nil or disabled policy (no retry — v0.1 behavior), or
// p.attempts+1 for an enabled one, per retryPolicy's own doc comment. A
// malformed attempts value below 1 — reachable only by constructing a
// *retryPolicy directly rather than through newRetryPolicy, which
// already rejects it — is clamped to 1 retry rather than trusted as-is,
// so this can never return less than 1: a totalTries of 0 or less would
// make do's loop skip calling call() entirely and return the disallowed
// (nil, nil) shape.
func (p *retryPolicy) totalTries() int {
	if p == nil || !p.enabled {
		return 1
	}
	retries := p.attempts
	if retries < 1 {
		retries = 1
	}
	return retries + 1
}

// defaultWaitFn is the production (*retryPolicy).waitFn: it blocks for
// d, or returns early the moment ctx is done, whichever comes first. A
// context canceled, or a deadline reached, while do is backing off
// aborts the remaining wait promptly instead of blocking out the full
// duration for a retry the caller no longer wants — this also bounds how
// long newGateway's synchronous warmFill discovery fill can be held up
// by a retrying listModels call. Returns true when the wait completed
// normally (do should proceed with the next attempt), false when ctx
// ended it early (do should stop retrying and return the last result).
func defaultWaitFn(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// newRetryPolicy validates rc and returns the retryPolicy buildAdapters
// attaches to every adapter it constructs. Disabled config (the v0.1
// default) skips validation and defaulting entirely — an unset
// Attempts/Backoff on a disabled block is never an error, since neither
// value is ever read.
//
// rc.Attempts is the number of retries performed after the initial try,
// not the total try count (see retryPolicy's doc comment): the default,
// applied when rc.Attempts is left at its zero value, is 1 retry (two
// tries total); the maximum, maxRetryAttempts, is 3 retries (four tries
// total). A value outside 1..maxRetryAttempts is a config error.
// rc.Backoff defaults to defaultRetryBackoff when left empty, and must
// parse via time.ParseDuration either way.
func newRetryPolicy(rc RetryConfig) (*retryPolicy, error) {
	if !rc.Enabled {
		return &retryPolicy{enabled: false, waitFn: defaultWaitFn}, nil
	}

	attempts := rc.Attempts
	if attempts == 0 {
		attempts = defaultRetryAttempts
	}
	if attempts < 1 || attempts > maxRetryAttempts {
		return nil, fmt.Errorf("llmgateway: retry.attempts (retries after the first try) must be between 1 and %d, got %d", maxRetryAttempts, attempts)
	}

	backoffStr := rc.Backoff
	if backoffStr == "" {
		backoffStr = defaultRetryBackoff
	}
	backoff, err := time.ParseDuration(backoffStr)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: retry.backoff: %w", err)
	}

	return &retryPolicy{enabled: true, attempts: attempts, backoff: backoff, waitFn: defaultWaitFn}, nil
}

// isTransient reports whether resp/err — the result of one upstream
// attempt, mutually exclusive per http.Client's own contract (a non-nil
// err always means resp is nil) — is a failure retryPolicy.do should
// retry: any error reaching upstreamJSON's client.Do (dial/EOF/reset),
// other than a context cancellation, a deadline, or a failure to build
// the outgoing *http.Request in the first place (errRequestBuildFailed —
// a malformed method or URL fails identically on every attempt, so
// retrying it only wastes attempts) — or a response carrying HTTP 429 or
// a 5xx status. Every other outcome — success, or a non-429 4xx — is not
// transient, per spec §1's "Never on 4xx≠429, translate errors, or
// context cancellation".
func isTransient(resp *http.Response, err error) bool {
	if err != nil {
		if errors.Is(err, errRequestBuildFailed) {
			return false
		}
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
}

// waitBefore returns how long do should wait before the attempt after
// attempt (1-based) — the one that just produced resp (nil on a network
// error) — and whether that wait came from the upstream's own
// Retry-After header (fromRetryAfter) rather than this gateway's own
// exponential backoff ladder. do uses fromRetryAfter to decide whether
// retryJitterFn may touch the wait at all (finding 4 fix, review-
// routes.md — see retryJitterFn's own doc comment for why jittering a
// Retry-After value is wrong).
//
// A 429 response carrying a Retry-After header that parses as a
// non-negative whole-second count wins outright, clamped to
// maxRetryAfterWait (NOT maxRetryWait — an upstream's own explicit
// cool-down request is honoured far longer than this gateway's own
// ladder; see maxRetryAfterWait's own doc comment): the upstream said
// exactly how long to back off, and this gateway waits at least that
// long, up to the 30s ceiling. Every other case falls back to
// exponential backoff (p.backoff * 2^(attempt-1)), capped at
// maxRetryWait — that covers a 429 with no Retry-After header, one that
// fails to parse as whole seconds (for example the HTTP-date form), one
// that parses negative, a Retry-After header present on a non-429 5xx
// (ignored: Retry-After is only meaningful on 429 here), and every
// network-error retry.
func (p *retryPolicy) waitBefore(attempt int, resp *http.Response) (wait time.Duration, fromRetryAfter bool) {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				// An upstream-requested cool-down is honoured up to
				// maxRetryAfterWait, and CLAMPED to it rather than
				// discarded, when it asks for longer (security audit
				// run-1). Discarding it — the previous behavior — fell
				// back to the <=2s ladder and so retried a provider that
				// had explicitly asked for a longer pause sooner than it
				// asked, which is backwards under overload.
				raWait := time.Duration(secs) * time.Second
				if raWait > maxRetryAfterWait {
					raWait = maxRetryAfterWait
				}
				return raWait, true
			}
		}
	}

	wait = p.backoff * time.Duration(int64(1)<<uint(attempt-1))
	if wait > maxRetryWait {
		wait = maxRetryWait
	}
	return wait, false
}

// drainAndClose discards a retried-away response's remaining body
// (capped at retryDrainLimit, defensively) and closes it, so the
// underlying connection is eligible for reuse on the next attempt
// instead of being forced closed by an un-drained body.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, retryDrainLimit))
	_ = resp.Body.Close()
}

// do executes call, retrying while isTransient(resp, err) reports a
// transient failure, up to totalTries() tries in total (see its doc for
// the retries-vs-total-tries distinction). A nil p, or one with
// enabled=false, tries call exactly once — the disabled/unwired shape.
// Between attempts it drains and closes the failed attempt's response
// body, then waits per waitBefore via p.waitFn — ctx-aware, so a context
// canceled or timed out during that wait aborts the remaining attempts
// immediately rather than blocking out the wait in full. do returns the
// last attempt's (resp, err) as-is on success, on a still-transient
// failure once tries are exhausted, or on a non-transient failure at any
// attempt.
//
// Finding 9 fix (review-routes.md): when the wait itself is aborted
// early (p.waitFn returns false — ctx was canceled or its deadline
// passed while do was backing off, most commonly the CLIENT
// disconnecting during a now-up-to-30s Retry-After wait), do returns
// ctx.Err() instead of the just-drained-and-closed resp/err pair. The
// previous behavior returned that resp as-is: drainAndClose (below) had
// already fully read and closed its body by that point, so a caller
// building an error from it (newProviderHTTPError, providers.go) read a
// closed body and silently produced an error with an EMPTY upstream
// body — the real failure (the wait was aborted, not that the upstream
// answered with nothing) was lost. ctx.Err() is always non-nil here:
// waitFn returning false is p.waitFn's own documented contract for "ctx
// ended the wait early" (defaultWaitFn's doc comment), so this never
// masks a wait that genuinely completed.
//
// Feature A (v0.22) attempt-accounting: every call() invocation — not just
// the final one — is reported to ctx's attemptRecorder, if it carries one
// (attemptRecorderFromContext, providers.go), before the transient check
// above decides whether to retry. A request retried twice before
// succeeding therefore reports THREE attempts, not one — the caller
// (limiter.recordProviderAttempt, limits.go) classifies each with the
// identical isTransient(resp, err) this loop already uses, so "attempt"
// and "failure" here always agree with what actually got retried.
func (p *retryPolicy) do(ctx context.Context, call func() (*http.Response, error)) (*http.Response, error) {
	tries := p.totalTries()
	rec := attemptRecorderFromContext(ctx)

	var resp *http.Response
	var err error
	for attempt := 1; attempt <= tries; attempt++ {
		resp, err = call()
		if rec != nil {
			rec(resp, err)
		}
		if !isTransient(resp, err) || attempt == tries {
			return resp, err
		}

		// Finding 4 fix (review-routes.md): jitter is applied ONLY to the
		// exponential-backoff branch (fromRetryAfter == false), never to a
		// wait the upstream itself requested via Retry-After — see
		// retryJitterFn's own doc comment for why jittering that value
		// would be backwards under overload.
		wait, fromRetryAfter := p.waitBefore(attempt, resp)
		if !fromRetryAfter {
			wait = retryJitterFn(wait)
		}
		drainAndClose(resp)
		if !p.waitFn(ctx, wait) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return resp, err
		}
	}
	return resp, err
}
