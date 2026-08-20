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
const (
	defaultRetryAttempts = 1
	defaultRetryBackoff  = "250ms"
)

// maxRetryAttempts is the highest RetryConfig.Attempts newRetryPolicy
// accepts — spec §1's "attempts: int (default 1, max 3)".
const maxRetryAttempts = 3

// maxRetryWait caps every wait retryPolicy.do performs between attempts,
// whether computed from exponential backoff or read from an upstream's
// Retry-After header — spec §1's "capped at 2s per wait".
const maxRetryWait = 2 * time.Second

// retryPolicy implements spec §1's same-provider retry: at most
// p.attempts total tries per upstream call, exponential backoff between
// them (capped at maxRetryWait), honoring a qualifying 429 Retry-After
// header outright. A nil *retryPolicy, or one with enabled=false, makes
// do try call exactly once — the same shape every v0.1 adapter
// constructor leaves its adapter's retry field in, since none of them set
// it. sleepFn is a field (not a direct time.Sleep call) so tests can
// inject a non-blocking spy instead of paying real wall-clock time.
type retryPolicy struct {
	sleepFn  func(time.Duration)
	backoff  time.Duration
	attempts int
	enabled  bool
}

// newRetryPolicy validates rc and returns the retryPolicy buildAdapters
// attaches to every adapter it constructs. Disabled config (the v0.1
// default) skips validation and defaulting entirely — an unset
// Attempts/Backoff on a disabled block is never an error, since neither
// value is ever read. Enabled config defaults an unset Attempts to
// defaultRetryAttempts and an unset Backoff to defaultRetryBackoff before
// validating: Attempts must fall within 1..maxRetryAttempts, and Backoff
// must parse via time.ParseDuration.
func newRetryPolicy(rc RetryConfig) (*retryPolicy, error) {
	if !rc.Enabled {
		return &retryPolicy{enabled: false, sleepFn: time.Sleep}, nil
	}

	attempts := rc.Attempts
	if attempts == 0 {
		attempts = defaultRetryAttempts
	}
	if attempts < 1 || attempts > maxRetryAttempts {
		return nil, fmt.Errorf("llmgateway: retry.attempts must be between 1 and %d, got %d", maxRetryAttempts, attempts)
	}

	backoffStr := rc.Backoff
	if backoffStr == "" {
		backoffStr = defaultRetryBackoff
	}
	backoff, err := time.ParseDuration(backoffStr)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: retry.backoff: %w", err)
	}

	return &retryPolicy{enabled: true, attempts: attempts, backoff: backoff, sleepFn: time.Sleep}, nil
}

// isTransient reports whether resp/err — the result of one upstream
// attempt, mutually exclusive per http.Client's own contract (a non-nil
// err always means resp is nil) — is a failure retryPolicy.do should
// retry: any error (a dial/EOF/reset failure reaching upstreamJSON's
// client.Do) other than a context cancellation or deadline, or a
// response carrying HTTP 429 or a 5xx status. Every other outcome —
// success, or a non-429 4xx — is not transient, per spec §1's "Never on
// 4xx≠429, translate errors, or context cancellation".
func isTransient(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
}

// waitBefore returns how long do should wait before the attempt after
// attempt (1-based) — the one that just produced resp (nil on a network
// error). A 429 carrying a Retry-After header parseable as whole seconds,
// and no greater than maxRetryWait, wins outright: the upstream said
// exactly how long to back off. Every other transient failure falls back
// to exponential backoff (p.backoff * 2^(attempt-1)), capped at
// maxRetryWait either way.
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
// (capped at maxResponseBytes, defensively) and closes it, so the
// underlying connection is eligible for reuse on the next attempt instead
// of being forced closed by an un-drained body.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
}

// do executes call, retrying while isTransient(resp, err) reports a
// transient failure, up to p.attempts total tries. A nil p, or one with
// enabled=false, tries call exactly once — the disabled/unwired shape.
// Between attempts it drains and closes the failed attempt's response
// body, then waits per waitBefore via p.sleepFn. do never starts a wait
// once ctx is already done: at that point it returns the last attempt's
// (resp, err) as-is, the same value it would return had attempts run out
// naturally. do returns the final attempt's result either way — success,
// a still-transient failure once attempts are exhausted, or a
// non-transient failure on any attempt.
func (p *retryPolicy) do(ctx context.Context, call func() (*http.Response, error)) (*http.Response, error) {
	attempts := 1
	if p != nil && p.enabled {
		attempts = p.attempts
	}

	var resp *http.Response
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err = call()
		if !isTransient(resp, err) || attempt == attempts {
			return resp, err
		}
		if ctx.Err() != nil {
			return resp, err
		}

		wait := p.waitBefore(attempt, resp)
		drainAndClose(resp)
		p.sleepFn(wait)
	}
	return resp, err
}
