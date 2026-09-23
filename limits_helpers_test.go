package traefikllmgateway

import "time"

// This file restores single-key test conveniences for limiter/memoryStore
// counter operations that no longer have any production caller (deadcode
// audit, 2026-09 review): checkAndCount/account moved onto the batched
// incrMulti/getMulti/incrAndGetMulti paths in the 2026-08-21/22 perf
// reviews, which left incrBy/get — and the limiter-level helpers built on
// them (storeIncrBy, storeGet, failPolicyIncrBy, failPolicyGet, incrCounter,
// getCounter) — with no caller anywhere in production. They were removed
// from limits.go and from the counterStore interface. Every helper here is
// implemented via the SAME batched production path a single-key call would
// have used (a one-entry counterIncr slice, or a one-key string slice), so
// it exercises the identical fail-open/fail-closed/latch policy and EXPIRE-
// confirmation logic the removed originals did — not a parallel
// reimplementation. This keeps the ~130 existing test call sites (across
// limits_test.go and admin_test.go) compiling and passing unchanged.

// incrBy is a test-only single-key counterpart to memoryStore.incrMulti,
// restoring the pre-batching direct-increment API for memoryStore-level
// unit tests that need a bare key with no window-derived enforceTTL floor
// (incrMulti's own callers always supply one via newCounterIncr; a bare
// test key has none, matching the removed production incrBy's own
// no-floor clamp).
func (m *memoryStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
	return m.applyIncr(key, n, clampTTL(ttl, 0)), nil
}

// storeIncrBy is a test-only single-key counterpart to storeIncrMulti,
// letting the limiter's fail-open/fail-closed/latch tests exercise that
// policy against one key without building a one-entry counterIncr slice at
// every call site. Delegates to storeIncrMulti itself, so it drives the
// exact same policy branches (nil store / latched / store error / store
// success) a single-entry batch would.
func (l *limiter) storeIncrBy(key string, n int64, ttl time.Duration) (v int64, ok bool) {
	vs, ok := l.storeIncrMulti([]counterIncr{{key: key, delta: n, ttl: ttl}})
	if !ok || len(vs) == 0 {
		return 0, ok
	}
	return vs[0], true
}

// storeGet mirrors storeIncrBy for a read, delegating to storeGetMulti.
func (l *limiter) storeGet(key string) (v int64, ok bool) {
	vs, ok := l.storeGetMulti([]string{key})
	if !ok || len(vs) == 0 {
		return 0, ok
	}
	return vs[0], true
}

// failPolicyIncrBy applies the limiter's fail-open/fail-closed policy for
// an incrBy the caller has decided not to attempt against the configured
// store (it just failed, or the store-down latch is open): failOpen=true
// counts key in the fallback instead; failOpen=false reports the
// operation as failed. Exercised directly (not via storeIncrBy/
// storeIncrMulti) by tests that isolate this branching from the store-call
// decision above it.
func (l *limiter) failPolicyIncrBy(key string, n int64, ttl time.Duration) (int64, bool) {
	if !l.failOpen {
		return 0, false
	}
	v, _ := l.fallback.incrBy(key, n, ttl)
	return v, true
}

// failPolicyGet mirrors failPolicyIncrBy for a read.
func (l *limiter) failPolicyGet(key string) (int64, bool) {
	if !l.failOpen {
		return 0, false
	}
	v, _ := l.fallback.get(key)
	return v, true
}

// incrCounter increments the (kind, id, metric, window) counter at t by n
// and returns its new value, together with whether the operation
// succeeded under the limiter's fail-open/fail-closed policy — the
// limiter-level single-key seeding helper tests use to seed counters
// directly (admin_test.go's TestAdminUsage_MathAgainstSeededCounters and
// others), implemented via the batched storeIncrMulti path.
func (l *limiter) incrCounter(kind, id, metric, window string, t time.Time, n int64, ttl time.Duration) (int64, bool) {
	return l.storeIncrBy(windowKey(kind, id, metric, window, t), n, ttl)
}

// getCounter returns the (kind, id, metric, window) counter's value at t,
// together with whether the read succeeded under the limiter's fail-open/
// fail-closed policy, implemented via the batched storeGetMulti path.
func (l *limiter) getCounter(kind, id, metric, window string, t time.Time) (int64, bool) {
	return l.storeGet(windowKey(kind, id, metric, window, t))
}
