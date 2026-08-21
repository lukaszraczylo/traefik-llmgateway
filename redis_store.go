package traefikllmgateway

import (
	"fmt"
	"strconv"
	"time"
)

// redisStore is a counterStore backed by a Redis-compatible server over
// respClient, so multiple Traefik instances share the same limit counters
// instead of each enforcing limits against its own in-process state.
type redisStore struct {
	client *respClient
}

// newRedisStore returns a redisStore using client for storage.
func newRedisStore(client *respClient) *redisStore {
	return &redisStore{client: client}
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

// incrBy implements counterStore: INCRBY key n, then EXPIRE key
// ttlToSeconds(ttl), sent as one pipeline so the two commands share a
// single round trip. It returns INCRBY's resulting counter value;
// EXPIRE's reply is not otherwise inspected — a failed EXPIRE right after
// a successful INCRBY on the same key would only leave that key without a
// fresh TTL, not corrupt the count.
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
	replies, err := s.client.pipeline([][]string{
		{"INCRBY", key, strconv.FormatInt(n, 10)},
		{"EXPIRE", key, strconv.FormatInt(ttlToSeconds(ttl), 10)},
	})
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
	return v, nil
}

// incrMulti implements counterStore: every entry's INCRBY+EXPIRE pair sent
// as ONE pipeline (perf review, 2026-08-21) — 2*len(entries) commands, one
// round trip regardless of len(entries), extending incrBy's own
// single-key pipelining to a whole batch of counters at once. Reply i*2
// is entry i's INCRBY result (returned in out[i]); reply i*2+1 is its
// EXPIRE result, not otherwise inspected — same reasoning as incrBy's own
// doc comment: a failed EXPIRE right after a successful INCRBY only
// leaves that one key without a fresh TTL, not a corrupted count. The
// same at-least-once semantics as incrBy (see its own doc comment) apply
// here too, extended to the whole batch: a lost reply after the server
// already applied the pipeline can cause respClient.pipeline's
// reconnect-once retry to re-send it, over-counting every entry in it by
// its own delta.
func (s *redisStore) incrMulti(entries []counterIncr) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	cmds := make([][]string, 0, len(entries)*2)
	for _, e := range entries {
		cmds = append(cmds,
			[]string{"INCRBY", e.key, strconv.FormatInt(e.delta, 10)},
			[]string{"EXPIRE", e.key, strconv.FormatInt(ttlToSeconds(e.ttl), 10)},
		)
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
		reply := replies[i*2]
		if re, ok := reply.(error); ok {
			return nil, fmt.Errorf("redisStore: incrMulti %q: INCRBY failed: %w", e.key, re)
		}
		v, ok := reply.(int64)
		if !ok {
			return nil, fmt.Errorf("redisStore: incrMulti %q: unexpected INCRBY reply type %T", e.key, reply)
		}
		out[i] = v
	}
	return out, nil
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
