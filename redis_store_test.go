package traefikllmgateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisStore_IncrBy_PipelinesIncrbyAndExpire is the brief's Step-1
// pipeline case: incrBy sends INCRBY and EXPIRE as one pipeline and
// returns INCRBY's reply value.
func TestRedisStore_IncrBy_PipelinesIncrbyAndExpire(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:user:a:req:day:20260820", "1"}, reply: []byte(":5\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:user:a:req:day:20260820", "60"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	v, err := store.incrBy("llmgw:user:a:req:day:20260820", 1, 60*time.Second)
	if err != nil {
		t.Fatalf("incrBy: %v", err)
	}
	if v != 5 {
		t.Errorf("incrBy = %d, want 5", v)
	}
}

// TestRedisStore_IncrBy_RoundsSubSecondTTLUp asserts a ttl under one
// second is never sent to EXPIRE as 0 (which would delete the key
// immediately) — it is rounded up to a 1s floor.
func TestRedisStore_IncrBy_RoundsSubSecondTTLUp(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "1"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	if _, err := store.incrBy("k", 1, 10*time.Millisecond); err != nil {
		t.Fatalf("incrBy: %v", err)
	}
}

// TestRedisStore_Get_MissingKeyReturnsZero is the brief's Step-1 GET case:
// a null bulk reply ("$-1") decodes to 0 with no error.
func TestRedisStore_Get_MissingKeyReturnsZero(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "llmgw:user:a:req:day:20260820"}, reply: []byte("$-1\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	v, err := store.get("llmgw:user:a:req:day:20260820")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != 0 {
		t.Errorf("get = %d, want 0", v)
	}
}

// TestRedisStore_Get_ParsesIntegerValue asserts a present key's bulk-string
// value is parsed as a base-10 integer.
func TestRedisStore_Get_ParsesIntegerValue(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$3\r\n123\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	v, err := store.get("k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != 123 {
		t.Errorf("get = %d, want 123", v)
	}
}

// TestLimiter_FailOpenFalse_DeadRedisAddress_ReturnsStoreDownViolation is
// the brief's Step-1 case: a limiter backed by a redisStore pointed at an
// address nothing listens on, with failOpen=false, must refuse the
// request with a storeDown violation rather than silently falling back to
// an in-process counter or panicking.
func TestLimiter_FailOpenFalse_DeadRedisAddress_ReturnsStoreDownViolation(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	} // nothing listens at deadAddr from here on

	store := newRedisStore(newRESPClient(deadAddr, "", 0))
	l := newLimiter(store, false)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 100}}}

	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("want a violation when the store is unreachable and failOpen is false")
	}
	if !v.storeDown {
		t.Errorf("v.storeDown = false, want true")
	}
	if v.message != "limit store unavailable" {
		t.Errorf("v.message = %q, want %q", v.message, "limit store unavailable")
	}
}

// TestLimiter_FailOpenTrue_DeadRedisAddress_FallsBackAndAllows mirrors the
// fail-closed case above with failOpen=true: the same dead address must
// not block the request — the limiter transparently counts against its
// in-process fallback instead.
func TestLimiter_FailOpenTrue_DeadRedisAddress_FallsBackAndAllows(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store := newRedisStore(newRESPClient(deadAddr, "", 0))
	l := newLimiter(store, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 100}}}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation with failOpen=true and a dead store, got %+v", v)
	}

	got, err := l.fallback.get(windowKey("user", "u", metricReq, windowDay, l.now()))
	if err != nil {
		t.Fatalf("fallback.get: %v", err)
	}
	if got != 1 {
		t.Errorf("fallback req:day counter = %d, want 1 (the request must have been counted in the fallback)", got)
	}
}

// TestLimiter_FailOpenTrue_HungRedisServer_FallsBackQuickly is review item
// 1's limiter-level proof (round 1), tightened by the round-3 store-down
// latch: a hung (accepts the connection, never replies) Redis-compatible
// server must not make a failOpen=true request wait out several stacked
// multi-second timeouts (the pre-fix behavior for a checkAndCount call
// that performs several store operations). checkAndCount's two store ops
// here (req:min, req:day) would each independently be bounded by one
// respCallTimeout — already a large improvement over the old
// per-operation-multiplied stall — but the store-down latch (limits.go)
// goes further: the first op's failure latches the store down, so the
// second op skips the network call entirely and the whole call costs
// only ~one respCallTimeout, not two.
func TestLimiter_FailOpenTrue_HungRedisServer_FallsBackQuickly(t *testing.T) {
	ln := newHungListener(t)
	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	l := newLimiter(store, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 100}}}

	start := time.Now()
	v := l.checkAndCount(scopes)
	elapsed := time.Since(start)

	if v != nil {
		t.Fatalf("want no violation with failOpen=true and a hung store, got %+v", v)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("elapsed = %v, want < 3s (one respCallTimeout for the first op, the second latched and skipping the network entirely)", elapsed)
	}
}

// TestRedisStore_Get_DownServer_ReturnsError asserts get surfaces a
// transport failure (dead address) as an error, mirroring incrBy's own
// contract for the same failure — the limiter's storeGet relies on this to
// treat a Redis outage as a store error rather than a false zero reading.
func TestRedisStore_Get_DownServer_ReturnsError(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	require.NoError(t, ln.Close())

	store := newRedisStore(newRESPClient(deadAddr, "", 0))
	_, err := store.get("k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redisStore: get")
}
