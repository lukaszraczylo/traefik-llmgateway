package traefikllmgateway

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// redisStore is a counterStore backed by a Redis-compatible server over
// respClient, so multiple Traefik instances share the same limit counters
// instead of each enforcing limits against its own in-process state.
type redisStore struct {
	client *respClient
	// expireSeen and expireOrder implement needsExpire/commitExpire/
	// forgetExpire's bounded, process-lifetime "does this key already
	// have a confirmed EXPIRE on Redis" tracker (perf finding 2,
	// 2026-08-2x audit) — see needsExpire's own doc comment. Guarded by
	// expireMu since incrBy/incrMulti/incrAndGetMulti can run
	// concurrently across goroutines sharing one redisStore (the limiter
	// calls them from every request's goroutine).
	expireSeen  map[string]struct{}
	expireOrder []string
	expireMu    sync.Mutex
}

// newRedisStore returns a redisStore using client for storage.
func newRedisStore(client *respClient) *redisStore {
	return &redisStore{client: client, expireSeen: make(map[string]struct{})}
}

// expireOnceCacheMax bounds needsExpire/commitExpire's tracked key set:
// once it holds this many keys, the oldest-inserted is evicted (FIFO) to
// make room for the newest. An evicted key simply becomes "unseen" again
// — its next INCRBY sends a redundant-but-harmless EXPIRE, exactly as if
// this process had never touched it before. This bounds memory for a
// long-lived process against its own ever-rolling counter buckets
// (windowKey embeds a new bucket per window rollover — limits.go), not
// against any single window's real key count, so 4096 is generous
// headroom over any realistic per-process working set of concurrently
// hot counters.
const expireOnceCacheMax = 4096

// needsExpire reports whether key does NOT currently have a CONFIRMED
// EXPIRE recorded for it (perf finding 2, 2026-08-2x audit, deleting
// EXPIRE entirely measured as the upper bound: 74->43 cmds/req, p50
// 867.9->697.9us (-19.6%), conc=16 throughput 1,477->2,336 req/s (+58%)).
// windowKey (limits.go) already embeds each counter's own time bucket in
// its key string, and the TTLs callers pass (dayWindowTTL=35d,
// monthWindowTTL=400d, ...) are deliberately far longer than their
// window's own natural length purely as a stale-key GC margin, not a
// sliding-window mechanism (limits.go's own doc comment on those
// constants) — so once EXPIRE is CONFIRMED sent for an exact key string,
// every further INCRBY against that SAME key happens well within the
// window that key's own bucket belongs to, comfortably inside the TTL's
// margin, and resending EXPIRE for it is redundant, not incorrect.
//
// needsExpire does NOT mark key as seen — unlike this method's
// predecessor (expireOnce), which marked a key seen at CHECK time,
// before its pipeline was ever sent. That was a bug (review finding,
// 2026-08-2x, CRITICAL): a pipeline carrying key's only EXPIRE can still
// fail after the mark — e.g. Redis is briefly unreachable, routine
// during a rollout — leaving this process believing EXPIRE succeeded
// when Redis never received it, and every later INCRBY against that key
// sends no EXPIRE ever again: an immortal, permanently TTL-less key
// (unbounded keyspace growth — precisely what dayWindowTTL/
// monthWindowTTL exist to prevent — and under maxmemory-policy
// volatile-*, a TTL-less key is not evictable, so it can drive Redis to
// OOM-on-write). Marking now happens ONLY in commitExpire, called by the
// caller after confirming the pipeline that carried EXPIRE actually
// succeeded (commit-after-success). See commitExpire and forgetExpire
// for the other half of the fix: Redis can also independently lose an
// already-EXPIRE-confirmed key (restart, maxmemory eviction, FLUSHDB, a
// failover to a replica missing it) without this process's cache ever
// finding out on its own.
func (s *redisStore) needsExpire(key string) bool {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	_, ok := s.expireSeen[key]
	return !ok
}

// commitExpire records key as having a CONFIRMED EXPIRE on Redis — call
// only after the pipeline carrying key's EXPIRE command is known to have
// succeeded (client.pipeline returned a nil error). Bounded FIFO
// eviction, same reasoning as expireOnceCacheMax's own doc comment.
func (s *redisStore) commitExpire(key string) {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	if _, ok := s.expireSeen[key]; ok {
		return
	}
	if len(s.expireOrder) >= expireOnceCacheMax {
		oldest := s.expireOrder[0]
		s.expireOrder = s.expireOrder[1:]
		delete(s.expireSeen, oldest)
	}
	s.expireSeen[key] = struct{}{}
	s.expireOrder = append(s.expireOrder, key)
}

// forgetExpire removes key from the confirmed-EXPIRE set — called when an
// INCRBY reply reveals key did NOT actually exist before this call (its
// returned value equals exactly this call's own delta — Redis treats a
// missing key as 0 for INCRBY), even though this process's cache
// believed EXPIRE was already confirmed for it (review finding,
// 2026-08-2x, CRITICAL, path 2): Redis lost the key — restart, maxmemory
// eviction, FLUSHDB, or a failover to a replica that never had it — and
// this call's own INCRBY just recreated it with NO TTL, because the
// (stale) cache made the caller skip sending EXPIRE this round.
//
// Forgetting key here does not fix THIS round's missing TTL — the
// decision to skip EXPIRE was already made and sent before this reply
// arrived — but it makes the VERY NEXT increment for key resend EXPIRE
// (needsExpire will report true again), bounding the immortal-key window
// to at most one skipped cycle instead of forever. An active counter (the
// only kind this matters for) gets incremented again almost immediately,
// so this self-heals in practice.
func (s *redisStore) forgetExpire(key string) {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	delete(s.expireSeen, key)
	// expireOrder may still carry a stale entry for key; left as-is. The
	// only consequence is commitExpire's FIFO eviction occasionally
	// evicting a key one step early if it is re-committed at a new
	// position later — itself always safe (a spurious extra EXPIRE, the
	// same accepted cost as any other eviction), so a full scan-and-purge
	// here would add complexity for no correctness benefit.
}

// ttlToSeconds converts ttl to whole Redis EXPIRE seconds, rounded up,
// with a floor of 1s so a sub-second ttl never turns into EXPIRE 0 (an
// immediate delete). Shared by incrBy and incrMulti.
func ttlToSeconds(ttl time.Duration) int64 {
	s := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		s++
	}
	if s < 1 {
		s = 1
	}
	return s
}

// incrBy implements counterStore: INCRBY key n, then (only when
// needsExpire(key) is true) EXPIRE key ttlToSeconds(ttl), sent as one
// pipeline so both commands share a single round trip when EXPIRE is
// included. It returns INCRBY's resulting counter value; EXPIRE's reply
// is not otherwise inspected — a failed EXPIRE right after a successful
// INCRBY on the same key would only leave that key without a fresh TTL,
// not corrupt the count.
//
// expireSeen bookkeeping (perf finding 2, review fix, CRITICAL,
// 2026-08-2x) happens strictly AFTER the pipeline succeeds — never
// before: commitExpire(key) only when this call actually sent (and
// confirmed) EXPIRE; forgetExpire(key) when the INCRBY reply reveals key
// was recreated (v == n) despite this process's cache believing it
// already had a confirmed EXPIRE — see both methods' own doc comments.
//
// Semantics are deliberately at-least-once, not exactly-once, both in the
// conservative direction (never under-counts): respClient.pipeline's
// reconnect-once retry can re-send this same INCRBY if the first attempt's
// reply was lost after the server already applied it (e.g. the connection
// dropped between the server processing INCRBY and the client reading its
// reply), which can over-count by n on that key; and the limiter's
// failOpen path can additionally count the same request in its in-process
// fallback store when a call to this method errors out after a partial
// success upstream. Both are accepted trade-offs — a rate/budget counter
// that occasionally over-counts by one request's worth is fail-safe (more
// restrictive than reality), never fail-open in the unsafe direction.
func (s *redisStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
	sendExpire := s.needsExpire(key)
	cmds := [][]string{{"INCRBY", key, strconv.FormatInt(n, 10)}}
	if sendExpire {
		cmds = append(cmds, []string{"EXPIRE", key, strconv.FormatInt(ttlToSeconds(ttl), 10)})
	}
	replies, err := s.client.pipeline(cmds)
	if err != nil {
		return 0, fmt.Errorf("redisStore: incrBy %q: %w", key, err)
	}
	if len(replies) == 0 {
		return 0, fmt.Errorf("redisStore: incrBy %q: empty pipeline reply", key)
	}
	if e, ok := replies[0].(error); ok {
		return 0, fmt.Errorf("redisStore: incrBy %q: INCRBY failed: %w", key, e)
	}
	v, ok := replies[0].(int64)
	if !ok {
		return 0, fmt.Errorf("redisStore: incrBy %q: unexpected INCRBY reply type %T", key, replies[0])
	}
	if sendExpire {
		s.commitExpire(key)
	} else if v == n {
		s.forgetExpire(key)
	}
	return v, nil
}

// incrMulti implements counterStore: every entry's INCRBY, plus an EXPIRE
// only when needsExpire(e.key) is true, sent as ONE pipeline (perf
// review, 2026-08-21) — one round trip regardless of len(entries) or how
// many of them get an EXPIRE this time, extending incrBy's own
// single-key pipelining to a whole batch of counters at once.
// sendExpire[i] records, per entry, whether THIS call actually sent
// EXPIRE for it — decided once, before the pipeline goes out, and reused
// after the reply arrives to choose commitExpire vs. the v==delta
// forgetExpire check (incrBy's own doc comment covers why both matter,
// review fix, CRITICAL, 2026-08-2x). incrReplyIdx[i] is the index within
// cmds (and so within replies) of entry i's own INCRBY — NOT a fixed i*2
// stride any more, since a skipped EXPIRE shifts every later entry's
// commands left by one; entry i's own EXPIRE, when included, immediately
// follows its INCRBY and is not otherwise inspected — same reasoning as
// incrBy's own doc comment: a failed EXPIRE right after a successful
// INCRBY only leaves that one key without a fresh TTL, not a corrupted
// count. The same at-least-once semantics as incrBy (see its own doc
// comment) apply here too, extended to the whole batch: a lost reply
// after the server already applied the pipeline can cause
// respClient.pipeline's reconnect-once retry to re-send it, over-counting
// every entry in it by its own delta.
func (s *redisStore) incrMulti(entries []counterIncr) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	cmds := make([][]string, 0, len(entries)*2)
	incrReplyIdx := make([]int, len(entries))
	sendExpire := make([]bool, len(entries))
	for i, e := range entries {
		incrReplyIdx[i] = len(cmds)
		cmds = append(cmds, []string{"INCRBY", e.key, strconv.FormatInt(e.delta, 10)})
		sendExpire[i] = s.needsExpire(e.key)
		if sendExpire[i] {
			cmds = append(cmds, []string{"EXPIRE", e.key, strconv.FormatInt(ttlToSeconds(e.ttl), 10)})
		}
	}

	replies, err := s.client.pipeline(cmds)
	if err != nil {
		return nil, fmt.Errorf("redisStore: incrMulti: %w", err)
	}
	if len(replies) != len(cmds) {
		return nil, fmt.Errorf("redisStore: incrMulti: expected %d replies, got %d", len(cmds), len(replies))
	}

	out := make([]int64, len(entries))
	for i, e := range entries {
		reply := replies[incrReplyIdx[i]]
		if re, ok := reply.(error); ok {
			return nil, fmt.Errorf("redisStore: incrMulti %q: INCRBY failed: %w", e.key, re)
		}
		v, ok := reply.(int64)
		if !ok {
			return nil, fmt.Errorf("redisStore: incrMulti %q: unexpected INCRBY reply type %T", e.key, reply)
		}
		out[i] = v
		if sendExpire[i] {
			s.commitExpire(e.key)
		} else if v == e.delta {
			s.forgetExpire(e.key)
		}
	}
	return out, nil
}

// incrAndGetMulti implements counterStore: every entry's INCRBY, plus an
// EXPIRE only when needsExpire(e.key) is true (same commit-after-success/
// forget-on-recreate bookkeeping as incrMulti's own doc comment), AND
// every read key's GET, sent as ONE pipeline (perf review round 3,
// 2026-08-22) — one round trip regardless of how many of each there are,
// fusing checkAndCount's own request-counter increments with its
// token/cost budget reads (limits.go's counterStore doc comment explains
// why that fusion is safe). Reply layout mirrors incrMulti's own:
// incrReplyIdx[i] is entry i's own INCRBY reply index (not a fixed 2*i
// stride — a skipped EXPIRE shifts every later command left by one; its
// EXPIRE, when included, immediately follows and is not otherwise
// inspected, same reasoning as incrMulti's own doc comment), followed by
// one GET reply per read key at base+i, where base is however many
// commands entries actually produced. A missing read key (RESP null
// bulk) maps to 0, matching getBatch's own convention. The same
// at-least-once semantics as incrMulti (see its own doc comment) apply to
// the INCRBY half of this call; the GET half is read-only and carries no
// such caveat.
func (s *redisStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	if len(entries) == 0 && len(reads) == 0 {
		return nil, nil, nil
	}

	cmds := make([][]string, 0, len(entries)*2+len(reads))
	incrReplyIdx := make([]int, len(entries))
	sendExpire := make([]bool, len(entries))
	for i, e := range entries {
		incrReplyIdx[i] = len(cmds)
		cmds = append(cmds, []string{"INCRBY", e.key, strconv.FormatInt(e.delta, 10)})
		sendExpire[i] = s.needsExpire(e.key)
		if sendExpire[i] {
			cmds = append(cmds, []string{"EXPIRE", e.key, strconv.FormatInt(ttlToSeconds(e.ttl), 10)})
		}
	}
	base := len(cmds)
	for _, k := range reads {
		cmds = append(cmds, []string{"GET", k})
	}

	replies, err := s.client.pipeline(cmds)
	if err != nil {
		return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti: %w", err)
	}
	if len(replies) != len(cmds) {
		return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti: expected %d replies, got %d", len(cmds), len(replies))
	}

	incrVals := make([]int64, len(entries))
	for i, e := range entries {
		reply := replies[incrReplyIdx[i]]
		if re, ok := reply.(error); ok {
			return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti %q: INCRBY failed: %w", e.key, re)
		}
		v, ok := reply.(int64)
		if !ok {
			return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti %q: unexpected INCRBY reply type %T", e.key, reply)
		}
		incrVals[i] = v
		if sendExpire[i] {
			s.commitExpire(e.key)
		} else if v == e.delta {
			s.forgetExpire(e.key)
		}
	}

	readVals := make([]int64, len(reads))
	for i, k := range reads {
		reply := replies[base+i]
		if reply == nil {
			continue // missing key -> 0, matching getBatch's own convention
		}
		if re, ok := reply.(error); ok {
			return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti %q: GET failed: %w", k, re)
		}
		b, ok := reply.([]byte)
		if !ok {
			return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti %q: unexpected GET reply type %T", k, reply)
		}
		n, perr := strconv.ParseInt(string(b), 10, 64)
		if perr != nil {
			return nil, nil, fmt.Errorf("redisStore: incrAndGetMulti %q: non-integer value %q: %w", k, b, perr)
		}
		readVals[i] = n
	}
	return incrVals, readVals, nil
}

// getMulti implements counterStore: one pipelined GET per key
// (respClient.getBatch), a single round trip regardless of len(keys) —
// used by limiter.currentUsage (admin dashboard, spec §4) to read a
// scope's six current-window counters without serializing six separate
// calls on the one shared connection.
func (s *redisStore) getMulti(keys []string) ([]int64, error) {
	v, err := s.client.getBatch(keys)
	if err != nil {
		return nil, fmt.Errorf("redisStore: getMulti: %w", err)
	}
	return v, nil
}

// get implements counterStore: GET key, treating a missing key (RESP null
// bulk reply) as 0, matching memoryStore's behavior for an absent or
// expired counter.
func (s *redisStore) get(key string) (int64, error) {
	reply, err := s.client.do("GET", key)
	if err != nil {
		return 0, fmt.Errorf("redisStore: get %q: %w", key, err)
	}
	if reply == nil {
		return 0, nil
	}
	if e, ok := reply.(error); ok {
		return 0, fmt.Errorf("redisStore: get %q: %w", key, e)
	}
	b, ok := reply.([]byte)
	if !ok {
		return 0, fmt.Errorf("redisStore: get %q: unexpected reply type %T", key, reply)
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("redisStore: get %q: non-integer value %q: %w", key, b, err)
	}
	return v, nil
}
