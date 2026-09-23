package traefikllmgateway

import (
	"fmt"
	"time"
)

// This file holds test-only helpers for limits.go functions that
// admin-redesign WP-G's deadcode cleanup removed from production: every
// production call site was rewritten to call the more general
// replacement directly (accountWith, countTargetRequestsBy/
// countTargetRequestBy, admin.go's chunkedModelSpanTotals/
// chunkedModelSpanTotalsMulti), leaving these exact shapes reachable only
// from tests. Moving them here — same signatures, same bodies — keeps
// `deadcode ./...` clean (they are gone from the production build graph)
// while `deadcode -test ./...` stays clean too (tests still call them
// directly) and every pre-existing test keeps its original coverage of
// the behavior these names document.
//
// storeIncrBy/storeGet/incrCounter below are a separate, older instance
// of the identical pattern (counterStore's own doc comment, limits.go:
// "counterStore had single-key incrBy/get methods until the 2026-09
// deadcode audit ... the tests that needed a single-key call now build a
// one-entry incrMulti batch or a one-key getMulti read instead (see
// limits_helpers_test.go ...)"), naming this exact file as their home;
// this pass fills that gap.

// enforceTTLFor returns the minimum TTL window's own counter must stay
// alive for to enforce correctly — see clampTTL's own doc comment
// (limits.go) for why the floor must always win a conflict with the
// ceiling. Production code builds all four windows' floors at once via
// enforceTTLsFor (limits.go) instead of calling this per window; the
// tests below pin enforceTTLsFor's four values against this switch
// directly, and clampTTL's own tests use it to build per-window floor
// values as clampTTL inputs.
func enforceTTLFor(window string) time.Duration {
	switch window {
	case windowMin:
		return enforceTTLMinWindow
	case windowHour:
		return enforceTTLHourWindow
	case windowDay:
		return enforceTTLDayWindow
	case windowMonth:
		return enforceTTLMonthWindow
	default:
		panic(fmt.Sprintf("llmgateway: enforceTTLFor: unknown window %q", window))
	}
}

// account records u's prompt/completion tokens and costMicros with no
// extra counter families — accountWith(scopes, u, costMicros,
// accountExtras{}). Every production call site now calls accountWith
// directly; this wrapper keeps every pre-existing test's three-argument
// call site compiling and behaving byte-for-byte the same as before
// accountExtras existed.
func (l *limiter) account(scopes []limitScope, u usage, costMicros int64) {
	l.accountWith(scopes, u, costMicros, accountExtras{})
}

// countTargetRequests increments MULTIPLE target scopes' per-target
// request counters with no caller identity attached —
// countTargetRequestsBy(limits.go) with caller="". Every production call
// site now names a caller and calls countTargetRequestsBy directly; this
// wrapper keeps tests exercising the caller-less shape unchanged.
func (l *limiter) countTargetRequests(scopes []limitScope) {
	l.countTargetRequestsBy("", scopes)
}

// countTargetRequest is countTargetRequests for exactly one target scope.
func (l *limiter) countTargetRequest(kind, id string) {
	l.countTargetRequests([]limitScope{{kind: kind, id: id}})
}

// modelSpanTotals reads span counter buckets per id, positionally
// (result[i] is the sum for ids[i]) — a single-metric convenience over
// the production spanTotalsMulti reader (limits.go; P12 fix, admin
// dashboard redesign verify round: this used to be its own separate
// unchunked implementation, exercising nothing chunkedModelSpanTotals
// (admin.go) actually runs in production — spanTotalsMulti is now the
// ONE production reader both call). Kept here as a test convenience
// purely so limits_test.go's many single-metric call sites did not all
// need rewriting to unpack spanTotalsMulti's [][]int64 by hand.
func (l *limiter) modelSpanTotals(ids []string, metric, window string, now time.Time, span int) ([]int64, bool) {
	out, ok := l.spanTotalsMulti(kindModel, ids, []string{metric}, window, now, span, 0)
	if !ok {
		return nil, false
	}
	return out[0], true
}

// modelTotals reads ONE counter per id — metric at window's CURRENT
// bucket — the modelSpanTotals span=1 special case.
func (l *limiter) modelTotals(ids []string, metric, window string) ([]int64, bool) {
	return l.modelSpanTotals(ids, metric, window, l.now(), 1)
}

// incrBy restores memoryStore's single-key increment API removed from
// production in the 2026-09 deadcode audit (applyIncr's own doc comment,
// limits.go): checkAndCount/accountWith moved onto the batched incrMulti
// in the 2026-08-21 perf review, leaving incrBy with no production
// caller. Clamped with no enforcement floor (clampTTL(ttl, 0)) — a bare
// key carries no (kind, id, metric, window) tuple to derive one from,
// unlike incrMulti's own per-entry enforceTTL floor.
func (m *memoryStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
	return m.applyIncr(key, n, clampTTL(ttl, 0)), nil
}

// failPolicyGet is failPolicyGetMulti's single-key special case, isolating
// the fail-open/fail-closed branch itself (TestLimiter_FailPolicyGet) from
// storeGet's own error/latched paths that already drive it indirectly.
func (l *limiter) failPolicyGet(key string) (int64, bool) {
	v, ok := l.failPolicyGetMulti([]string{key})
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// failPolicyIncrBy mirrors failPolicyGet for failPolicyIncrMulti
// (TestLimiter_FailPolicyIncrBy).
func (l *limiter) failPolicyIncrBy(key string, delta int64, ttl time.Duration) (int64, bool) {
	v, ok := l.failPolicyIncrMulti([]counterIncr{{key: key, delta: delta, ttl: ttl}})
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// storeIncrBy is storeIncrMulti's single-entry special case: build one
// counterIncr{key, delta, ttl} (enforceTTL left at its zero value —
// "ceiling only, no floor", clampTTL's own doc comment) and apply the
// identical fail-open/fail-closed/latched policy storeIncrMulti already
// applies to a batch. No production caller needs a single-key increment
// on its own (checkAndCount/accountWith/countTargetRequestsBy/
// recordProviderAttempt all batch); this exists purely so
// TestLimiter_StoreIncrBy_DirectPaths and incrCounter (below) can drive
// and seed one counter at a time without building a one-element slice
// literal at every call site.
func (l *limiter) storeIncrBy(key string, delta int64, ttl time.Duration) (int64, bool) {
	v, ok := l.storeIncrMulti([]counterIncr{{key: key, delta: delta, ttl: ttl}})
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// storeGet is storeGetMulti's single-key special case, mirroring
// storeIncrBy for the read side — TestLimiter_StoreGet_DirectPaths' own
// subject.
func (l *limiter) storeGet(key string) (int64, bool) {
	v, ok := l.storeGetMulti([]string{key})
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// incrCounter seeds exactly one (kind, id, metric, window) counter's
// bucket for the instant now falls in, incrementing it by n with TTL
// ttl — windowKey (limits.go) plus a single storeIncrBy call. This is
// every test file's own direct counter-seeding primitive (admin_test.go,
// limits_test.go, metrics_test.go): production code never seeds a
// counter outside of a real accountWith/checkAndCount/
// countTargetRequestsBy/recordProviderAttempt call, so there is nothing
// for this to mirror there — it exists purely to put a known value at a
// known key without a test reaching past the limiter into windowKey and
// storeIncrBy itself at every call site. The store-failure return
// storeIncrBy carries is intentionally discarded: every caller seeds
// against a nil-store or fixedStore limiter where the write cannot fail,
// and a seeding helper silently doing nothing on an unexpected failure
// would only surface as a confusing assertion mismatch two lines later,
// not here.
func (l *limiter) incrCounter(kind, id, metric, window string, now time.Time, n int64, ttl time.Duration) {
	key := windowKey(kind, id, metric, window, now)
	_, _ = l.storeIncrBy(key, n, ttl)
}

// getCounter reads exactly one (kind, id, metric, window) counter's
// bucket for the instant now falls in — windowKey (limits.go) plus a
// single storeGet call, incrCounter's read-side mirror and this whole
// test suite's dominant assertion primitive: "did the right counter end
// up with the right value after this request". ok is false only in the
// fail-closed case, exactly like storeGet/storeGetMulti's own contract;
// every real assertion above passes ok through require.True/assert
// rather than ignoring it, the same discipline production callers
// already apply to storeGetMulti's own ok.
func (l *limiter) getCounter(kind, id, metric, window string, now time.Time) (int64, bool) {
	key := windowKey(kind, id, metric, window, now)
	return l.storeGet(key)
}
