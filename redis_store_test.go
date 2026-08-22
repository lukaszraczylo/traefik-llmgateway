package traefikllmgateway

import (
	"fmt"
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

// TestRedisStore_IncrMulti_PipelinesAllPairsInOneCall is the perf-review
// (2026-08-21) case: incrMulti sends every entry's INCRBY+EXPIRE pair as
// ONE pipeline — no SELECT (or any other command) interleaved between
// entries, which would only happen if incrMulti dialled a fresh
// connection or made a separate call per entry instead of the single
// s.client.pipeline call it actually makes — and returns each entry's
// INCRBY reply in the same order as entries, not the reply's position in
// the full 2*N command/reply stream.
func TestRedisStore_IncrMulti_PipelinesAllPairsInOneCall(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:user:a:req:hour:2026082014", "1"}, reply: []byte(":11\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:user:a:req:hour:2026082014", "172800"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:user:a:tokin:day:20260820", "40"}, reply: []byte(":140\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:user:a:tokin:day:20260820", "3024000"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:total:all:cost:month:202608", "500"}, reply: []byte(":9500\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:total:all:cost:month:202608", "34560000"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	entries := []counterIncr{
		{key: "llmgw:user:a:req:hour:2026082014", delta: 1, ttl: hourWindowTTL},
		{key: "llmgw:user:a:tokin:day:20260820", delta: 40, ttl: dayWindowTTL},
		{key: "llmgw:total:all:cost:month:202608", delta: 500, ttl: monthWindowTTL},
	}
	got, err := store.incrMulti(entries)
	if err != nil {
		t.Fatalf("incrMulti: %v", err)
	}
	want := []int64{11, 140, 9500}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %d, want %d (entry order, not reply-stream position)", i, got[i], w)
		}
	}
}

// TestRedisStore_IncrAndGetMulti_PipelinesEverythingInOneCall is the
// perf-review round-3 (2026-08-22) case: incrAndGetMulti sends every
// entry's INCRBY+EXPIRE pair AND every read key's GET as ONE pipeline —
// no SELECT (or any other command) interleaved, which would only happen
// if it dialled a fresh connection or split the call into more than one
// round trip — and returns entries' INCRBY replies and reads' GET replies
// each in their own given order, not the reply stream's flat position.
func TestRedisStore_IncrAndGetMulti_PipelinesEverythingInOneCall(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:user:a:req:min:202608220900", "1"}, reply: []byte(":3\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:user:a:req:min:202608220900", "120"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "llmgw:user:a:req:day:20260822", "1"}, reply: []byte(":7\r\n")},
		{wantArgs: []string{"EXPIRE", "llmgw:user:a:req:day:20260822", "3024000"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"GET", "llmgw:user:a:tokin:day:20260822"}, reply: []byte("$3\r\n150\r\n")},
		{wantArgs: []string{"GET", "llmgw:user:a:tokout:day:20260822"}, reply: []byte("$-1\r\n")}, // missing -> 0
		{wantArgs: []string{"GET", "llmgw:user:a:cost:day:20260822"}, reply: []byte("$2\r\n50\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	entries := []counterIncr{
		{key: "llmgw:user:a:req:min:202608220900", delta: 1, ttl: minWindowTTL},
		{key: "llmgw:user:a:req:day:20260822", delta: 1, ttl: dayWindowTTL},
	}
	reads := []string{
		"llmgw:user:a:tokin:day:20260822",
		"llmgw:user:a:tokout:day:20260822",
		"llmgw:user:a:cost:day:20260822",
	}
	incrVals, readVals, err := store.incrAndGetMulti(entries, reads)
	if err != nil {
		t.Fatalf("incrAndGetMulti: %v", err)
	}
	assert.Equal(t, []int64{3, 7}, incrVals)
	assert.Equal(t, []int64{150, 0, 50}, readVals)
}

// TestRedisStore_IncrAndGetMulti_EmptyReads asserts a nil/empty reads
// slice (no scope in the request has a token/cost budget configured — the
// common case) still sends and returns the increment half correctly, with
// no GET commands at all.
func TestRedisStore_IncrAndGetMulti_EmptyReads(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	incrVals, readVals, err := store.incrAndGetMulti([]counterIncr{{key: "k", delta: 1, ttl: time.Minute}}, nil)
	if err != nil {
		t.Fatalf("incrAndGetMulti: %v", err)
	}
	if len(incrVals) != 1 || incrVals[0] != 1 {
		t.Errorf("incrVals = %v, want [1]", incrVals)
	}
	if len(readVals) != 0 {
		t.Errorf("readVals = %v, want empty", readVals)
	}
}

// TestRedisStore_IncrAndGetMulti_NoEntriesOrReads_SkipsThePipelineCall
// asserts the empty-input guard (review fix, 2026-08-22, round 2 —
// symmetry with incrMulti's own len(entries)==0 guard): no commands are
// sent, and the call returns cleanly, when both entries and reads are
// empty.
func TestRedisStore_IncrAndGetMulti_NoEntriesOrReads_SkipsThePipelineCall(t *testing.T) {
	ln := newFakeListener(t)
	// No respStep script at all: any command reaching the fake server
	// (including the connection-setup SELECT) would fail this test by
	// timing out waiting for a script step that does not exist.
	store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
	incrVals, readVals, err := store.incrAndGetMulti(nil, nil)
	if err != nil {
		t.Fatalf("incrAndGetMulti: %v", err)
	}
	if len(incrVals) != 0 || len(readVals) != 0 {
		t.Errorf("incrVals=%v readVals=%v, want both empty", incrVals, readVals)
	}
}

// TestRedisStore_IncrAndGetMulti_GetReplyErrors is the GET-reply-error
// half of the perf-review round-3 pipelining test (review fix, 2026-08-22,
// round 2 — parity with respClient.getBatch's own equivalent table,
// TestRESPClient_GetBatch, resp_test.go): a RESP error reply or a
// non-integer value on any read key fails the WHOLE call, matching
// getBatch's own all-or-nothing contract, since incrAndGetMulti decodes
// GET replies itself rather than delegating to getBatch.
func TestRedisStore_IncrAndGetMulti_GetReplyErrors(t *testing.T) {
	cases := []struct {
		name          string
		wantErrSubstr string
		steps         []respStep
	}{
		{
			name: "a RESP error reply on a read key fails the whole call",
			steps: []respStep{
				{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
				{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
				{wantArgs: []string{"GET", "budget"}, reply: []byte("-ERR busy\r\n")},
			},
			wantErrSubstr: "ERR busy",
		},
		{
			name: "a non-integer value on a read key fails the whole call",
			steps: []respStep{
				{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
				{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
				{wantArgs: []string{"GET", "budget"}, reply: []byte("$3\r\nabc\r\n")},
			},
			wantErrSubstr: "non-integer value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ln := newFakeListener(t)
			steps := append([]respStep{{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")}}, c.steps...)
			runFakeRESPServer(t, ln, steps)

			store := newRedisStore(newRESPClient(ln.Addr().String(), "", 0))
			entries := []counterIncr{{key: "k", delta: 1, ttl: time.Minute}}
			_, _, err := store.incrAndGetMulti(entries, []string{"budget"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErrSubstr)
		})
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

// --- expireOnce: skip redundant EXPIRE for an already-touched key (perf
// finding 2, 2026-08-2x audit) ---
//
// Every test below builds its store over newRESPClientPool(..., 1), not
// newRESPClient's self-tuned default pool: each test issues MORE THAN ONE
// top-level call against the same store, and runFakeRESPServer scripts a
// single, strictly ordered connection — with the default pool (size >1,
// finding 1) a later call could acquire a different, not-yet-dialled
// slot instead of reusing the first connection, sending a second SELECT
// the fixed script never expects. Production callers (llmgateway.go's
// buildRedisClient) are unaffected — they always go through newRESPClient
// itself.

// TestRedisStore_IncrBy_SecondCallSameKey_SkipsExpire proves incrBy sends
// EXPIRE only on the first call for a given key on one redisStore
// instance — the second call for the SAME key sends INCRBY alone, which
// runFakeRESPServer's exact script here proves: an unexpected EXPIRE
// command would fail this test by mismatching (or exhausting) the
// script.
func TestRedisStore_IncrBy_SecondCallSameKey_SkipsExpire(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":2\r\n")}, // no EXPIRE step: none must be sent
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	v1, err := store.incrBy("k", 1, 60*time.Second)
	if err != nil {
		t.Fatalf("incrBy (1st): %v", err)
	}
	if v1 != 1 {
		t.Errorf("incrBy (1st) = %d, want 1", v1)
	}

	v2, err := store.incrBy("k", 1, 60*time.Second)
	if err != nil {
		t.Fatalf("incrBy (2nd): %v", err)
	}
	if v2 != 2 {
		t.Errorf("incrBy (2nd) = %d, want 2", v2)
	}
}

// TestRedisStore_IncrBy_DifferentKeys_EachGetsItsOwnFirstExpire asserts
// expireOnce tracks per KEY, not globally — a second, DIFFERENT key still
// gets its own EXPIRE even though this store already touched a different
// key first.
func TestRedisStore_IncrBy_DifferentKeys_EachGetsItsOwnFirstExpire(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "a", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "a", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "b", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "b", "60"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	if _, err := store.incrBy("a", 1, 60*time.Second); err != nil {
		t.Fatalf("incrBy(a): %v", err)
	}
	if _, err := store.incrBy("b", 1, 60*time.Second); err != nil {
		t.Fatalf("incrBy(b): %v", err)
	}
}

// TestRedisStore_IncrMulti_RepeatedKeyAcrossCalls_SkipsRedundantExpireAndKeepsReplyIndexingCorrect
// is the incrMulti counterpart: a mixed batch where one entry's key was
// already touched (no EXPIRE) and another's is new (gets one) proves the
// variable-stride command layout still returns each entry's own INCRBY
// value at the right index, not shifted by the earlier entries' differing
// command counts.
func TestRedisStore_IncrMulti_RepeatedKeyAcrossCalls_SkipsRedundantExpireAndKeepsReplyIndexingCorrect(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		// First incrMulti: both "hot" and "cold" are new to this store.
		{wantArgs: []string{"INCRBY", "hot", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "hot", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "cold", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "cold", "60"}, reply: []byte(":1\r\n")},
		// Second incrMulti: "hot" was already touched (no EXPIRE, one
		// command shorter), "new" was not (gets one) — reply indexing
		// for "new" must still land correctly despite "hot" contributing
		// only 1 command instead of 2 ahead of it.
		{wantArgs: []string{"INCRBY", "hot", "1"}, reply: []byte(":2\r\n")},
		{wantArgs: []string{"INCRBY", "new", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "new", "60"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	first, err := store.incrMulti([]counterIncr{
		{key: "hot", delta: 1, ttl: time.Minute},
		{key: "cold", delta: 1, ttl: time.Minute},
	})
	if err != nil {
		t.Fatalf("incrMulti (1st): %v", err)
	}
	assert.Equal(t, []int64{1, 1}, first)

	second, err := store.incrMulti([]counterIncr{
		{key: "hot", delta: 1, ttl: time.Minute},
		{key: "new", delta: 1, ttl: time.Minute},
	})
	if err != nil {
		t.Fatalf("incrMulti (2nd): %v", err)
	}
	assert.Equal(t, []int64{2, 1}, second)
}

// TestRedisStore_IncrAndGetMulti_MixedFirstAndRepeatedKeys_ReadsStillLandAfterEntries
// proves incrAndGetMulti's reads (GET commands) still land at the correct
// offset (the dynamically computed "base", not a fixed len(entries)*2)
// when some entries skip their EXPIRE: "hot" was already touched by an
// earlier incrBy call on the SAME store (so this call sends no EXPIRE
// for it), "cold" is new to this store (so it gets one) — the "budget"
// read must still land right after however many commands the entries
// actually produced (3, not the naive len(entries)*2 == 4).
func TestRedisStore_IncrAndGetMulti_MixedFirstAndRepeatedKeys_ReadsStillLandAfterEntries(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "hot", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "hot", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "hot", "1"}, reply: []byte(":2\r\n")},
		{wantArgs: []string{"INCRBY", "cold", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "cold", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"GET", "budget"}, reply: []byte("$2\r\n42\r\n")},
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	if _, err := store.incrBy("hot", 1, time.Minute); err != nil {
		t.Fatalf("incrBy (prime hot's expireOnce state): %v", err)
	}

	incrVals, readVals, err := store.incrAndGetMulti(
		[]counterIncr{
			{key: "hot", delta: 1, ttl: time.Minute},
			{key: "cold", delta: 1, ttl: time.Minute},
		},
		[]string{"budget"},
	)
	if err != nil {
		t.Fatalf("incrAndGetMulti: %v", err)
	}
	assert.Equal(t, []int64{2, 1}, incrVals)
	assert.Equal(t, []int64{42}, readVals)
}

// TestRedisStore_ExpireOnce_BoundedEviction proves commitExpire evicts
// the oldest-inserted key (FIFO) once it holds expireOnceCacheMax keys,
// making that oldest key "unseen" again — its next INCRBY sends a
// redundant-but-harmless EXPIRE, exactly as if this store had never
// touched it before — rather than growing without bound across a
// long-lived process's ever-rolling counter buckets.
func TestRedisStore_ExpireOnce_BoundedEviction(t *testing.T) {
	store := newRedisStore(nil) // needsExpire/commitExpire touch no network at all

	if !store.needsExpire("key-0") {
		t.Fatal("needsExpire(key-0) (before ever committed) = false, want true")
	}
	store.commitExpire("key-0")
	if store.needsExpire("key-0") {
		t.Fatal("needsExpire(key-0) (after commit) = true, want false (already seen)")
	}

	// Fill the tracker past its cap with distinct keys, evicting key-0.
	for i := 1; i <= expireOnceCacheMax; i++ {
		store.commitExpire(fmt.Sprintf("key-%d", i))
	}

	if got := len(store.expireSeen); got != expireOnceCacheMax {
		t.Errorf("len(expireSeen) = %d, want exactly %d (bounded)", got, expireOnceCacheMax)
	}
	if !store.needsExpire("key-0") {
		t.Error("needsExpire(key-0) after the tracker wrapped = false, want true (evicted, so unseen again)")
	}
}

// --- CRITICAL review fix (2026-08-2x): the old expireOnce marked a key
// "seen" at CHECK time, before its EXPIRE was ever confirmed sent —
// leaving a permanently TTL-less, immortal key on either of two paths:
// (1) the pipeline carrying that EXPIRE fails outright (Redis briefly
// unreachable — routine during a rollout); (2) Redis independently loses
// an already-confirmed key (restart, maxmemory eviction, FLUSHDB,
// failover to a replica missing it) and a later INCRBY recreates it with
// no TTL, since this process's stale cache skips sending EXPIRE. Both are
// pinned directly below, over the real wire (fakeRESPServer scripts that
// fail the test outright if an expected EXPIRE is missing, or an
// unexpected one is sent).

// TestRedisStore_IncrBy_FailedPipeline_DoesNotConsumeExpireCredit is path
// 1: a pipeline that fails outright (dead address — Redis unreachable)
// must NOT mark the key as having a confirmed EXPIRE. Proven two ways:
// directly, the store's own expireSeen must still be empty after the
// failure (needsExpire's commit-after-success contract); and
// behaviorally, the very next call for the same key — now against a live
// server — must still send EXPIRE, exactly as if this were the true
// first touch.
func TestRedisStore_IncrBy_FailedPipeline_DoesNotConsumeExpireCredit(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	require.NoError(t, ln.Close()) // nothing listens at deadAddr from here on

	store := newRedisStore(newRESPClientPool(deadAddr, "", 0, 1))
	if _, err := store.incrBy("k", 1, 60*time.Second); err == nil {
		t.Fatal("want an error: nothing listens at deadAddr")
	}
	if len(store.expireSeen) != 0 {
		t.Fatalf("expireSeen = %v after a failed pipeline, want empty (the EXPIRE credit must not be consumed by a failed send)", store.expireSeen)
	}

	// Same store, now pointed at a live server: the next call for the
	// SAME key must still send EXPIRE, proving the failed attempt above
	// left the key genuinely "unseen".
	ln2 := newFakeListener(t)
	runFakeRESPServer(t, ln2, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
	})
	store.client = newRESPClientPool(ln2.Addr().String(), "", 0, 1)
	if _, err := store.incrBy("k", 1, 60*time.Second); err != nil {
		t.Fatalf("incrBy after recovery: %v", err)
	}
}

// TestRedisStore_IncrBy_KeyRecreatedAfterLoss_ForgetsAndResendsExpireNextCall
// is path 2: this process believes "k" already has a confirmed EXPIRE
// (committed by the 1st call below), so the 2nd call sends INCRBY alone
// — but the fake server's reply for it (":1\r\n", equal to this call's
// own delta) simulates Redis having lost the key and INCRBY recreating
// it fresh, with no TTL. forgetExpire must fire so the 3rd call resends
// EXPIRE — pinned by the fake server's exact script, which fails the
// test if the 3rd call's EXPIRE is missing.
func TestRedisStore_IncrBy_KeyRecreatedAfterLoss_ForgetsAndResendsExpireNextCall(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		// 1st call: genuinely new key — sends EXPIRE, commits it.
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
		// 2nd call: cache says "k" already has a confirmed EXPIRE, so
		// only INCRBY is sent — but Redis lost the key, so its reply
		// (":1\r\n") equals this call's own delta, the recreate signal.
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		// 3rd call: forgetExpire must have fired after the 2nd call, so
		// this one sends EXPIRE again.
		{wantArgs: []string{"INCRBY", "k", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "k", "60"}, reply: []byte(":1\r\n")},
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	for i := 0; i < 3; i++ {
		if _, err := store.incrBy("k", 1, 60*time.Second); err != nil {
			t.Fatalf("incrBy (call %d): %v", i+1, err)
		}
	}
}

// TestRedisStore_IncrMulti_KeyRecreatedAfterLoss_ForgetsAndResendsExpireNextCall
// is the incrMulti counterpart, proving the per-entry sendExpire/
// forgetExpire bookkeeping (not just incrBy's single-key path) reacts to
// a recreate signal correctly inside a batch alongside an unrelated,
// genuinely-still-alive entry.
func TestRedisStore_IncrMulti_KeyRecreatedAfterLoss_ForgetsAndResendsExpireNextCall(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		// 1st batch: both keys are new — both get EXPIRE, both commit.
		{wantArgs: []string{"INCRBY", "lost", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "lost", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "alive", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "alive", "60"}, reply: []byte(":1\r\n")},
		// 2nd batch: "lost" was evicted from Redis (reply == its own
		// delta, the recreate signal) but "alive" genuinely still has
		// its counter (reply 2, not equal to delta 1) — only "lost"
		// must be forgotten.
		{wantArgs: []string{"INCRBY", "lost", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "alive", "1"}, reply: []byte(":2\r\n")},
		// 3rd batch: "lost" gets a fresh EXPIRE; "alive" still does not.
		{wantArgs: []string{"INCRBY", "lost", "1"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"EXPIRE", "lost", "60"}, reply: []byte(":1\r\n")},
		{wantArgs: []string{"INCRBY", "alive", "1"}, reply: []byte(":3\r\n")},
	})

	store := newRedisStore(newRESPClientPool(ln.Addr().String(), "", 0, 1))
	entries := []counterIncr{
		{key: "lost", delta: 1, ttl: time.Minute},
		{key: "alive", delta: 1, ttl: time.Minute},
	}
	for i := 0; i < 3; i++ {
		if _, err := store.incrMulti(entries); err != nil {
			t.Fatalf("incrMulti (batch %d): %v", i+1, err)
		}
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
