package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// maxRetryWait caps every wait retryPolicy.do performs between attempts,
// whether computed from exponential backoff or read from an upstream's
// Retry-After header — spec §1's "capped at 2s per wait".
const maxRetryWait = 2 * time.Second

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
// error). Only a 429 response carrying a Retry-After header that parses
// as a non-negative whole-second count no greater than maxRetryWait wins
// outright: the upstream said exactly how long to back off. Every other
// case falls back to exponential backoff (p.backoff * 2^(attempt-1)),
// capped at maxRetryWait — that covers a 429 with no Retry-After header,
// one that fails to parse as whole seconds (for example the HTTP-date
// form), one that parses negative, one whose value exceeds maxRetryWait,
// a Retry-After header present on a non-429 5xx (ignored: Retry-After is
// only meaningful on 429 here), and every network-error retry.
func (p *retryPolicy) waitBefore(attempt int, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				if wait := time.Duration(secs) * time.Second; wait <= maxRetryWait {
					return wait
				}
			}
		}
	}

	wait := p.backoff * time.Duration(int64(1)<<uint(attempt-1))
	if wait > maxRetryWait {
		wait = maxRetryWait
	}
	return wait
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
// last attempt's (resp, err) as-is either way: on success, on a
// still-transient failure once tries are exhausted, on a non-transient
// failure at any attempt, or on a wait ctx aborted early.
func (p *retryPolicy) do(ctx context.Context, call func() (*http.Response, error)) (*http.Response, error) {
	tries := p.totalTries()

	var resp *http.Response
	var err error
	for attempt := 1; attempt <= tries; attempt++ {
		resp, err = call()
		if !isTransient(resp, err) || attempt == tries {
			return resp, err
		}

		wait := p.waitBefore(attempt, resp)
		drainAndClose(resp)
		if !p.waitFn(ctx, wait) {
			return resp, err
		}
	}
	return resp, err
}
