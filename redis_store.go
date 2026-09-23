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
	// forgetExpireLocked's bounded, process-lifetime "does this key already
	// have a confirmed EXPIRE on Redis" tracker (perf finding 2,
	// 2026-08-2x audit) — see needsExpire's own doc comment. Guarded by
	// expireMu since incrMulti/incrAndGetMulti can run
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

// needsExpireBatch is needsExpire (redis_store_helpers_test.go's
// test-only single-key wrapper — every production call site now goes
// through this batched form) extended to a whole incrMulti/
// incrAndGetMulti batch: it takes expireMu ONCE for the whole slice
// instead of once per entry, computed for every entry BEFORE any of the
// batch's commands are even sent (mirroring needsExpire's own
// check-before-any-pipeline-attempt contract) — the per-entry decisions
// this returns are exactly what needsExpire would have returned called
// once per key, just without len(entries) separate lock/unlock pairs.
func (s *redisStore) needsExpireBatch(entries []counterIncr) []bool {
	out := make([]bool, len(entries))
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	for i, e := range entries {
		// An absolute entry's own command (SET key v EX secs, below)
		// already carries its own expiry — it never needs a SEPARATE
		// EXPIRE command the way an INCRBY does, and so is never tracked
		// in expireSeen at all: always false here, regardless of whether
		// this exact key has been written before.
		if e.absolute {
			out[i] = false
			continue
		}
		_, ok := s.expireSeen[e.key]
		out[i] = !ok
	}
	return out
}

// settleExpireBatch applies incrMulti/incrAndGetMulti's post-pipeline
// expire bookkeeping for a whole batch in ONE expireMu acquisition:
// confirmed[i] is whether THIS call sent EXPIRE for entry i AND its reply
// was literally the integer 1 (applyIncrReplies' confirmed — finding F6,
// 2026-09 review: a sent-but-failed EXPIRE is never committed, so
// needsExpire keeps asking); firstWrite[i] is whether entry i's EXPIRE was
// not sent and its INCRBY reply revealed it was recreated (reply ==
// delta), the
// same commit-after-success/forget-on-recreate contract as commitExpire/
// forgetExpireLocked's own doc comments, just decided per-entry and applied
// together here instead of via len(entries) separate commitExpire/
// forgetExpireLocked calls. Only entries[:n] for whatever n the caller actually
// finished processing should ever be passed in — see incrMulti/
// incrAndGetMulti's own `processed` handling for why a partial prefix,
// not always the whole batch, is what gets settled.
func (s *redisStore) settleExpireBatch(entries []counterIncr, confirmed, firstWrite []bool) {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	for i, e := range entries {
		if confirmed[i] {
			s.commitExpireLocked(e.key)
		} else if firstWrite[i] {
			s.forgetExpireLocked(e.key)
		}
	}
}

// commitExpireLocked is commitExpire's body without its own lock/unlock —
// callers already hold expireMu (settleExpireBatch), batching every
// entry's commit into the ONE lock acquisition that method takes for the
// whole call, instead of one lock per key the way calling the exported,
// self-locking commitExpire per entry would. commitExpire itself is now
// just commitExpireLocked wrapped in its own lock, for
// repairRecreatedExpires' per-key commits.
func (s *redisStore) commitExpireLocked(key string) {
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

// forgetExpireLocked removes key from the confirmed-EXPIRE set — called when an
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
// so this self-heals in practice. repairRecreatedExpires (finding F7,
// 2026-09 review) also re-arms the TTL inside the same call; this forget
// is the fallback when that best-effort repair fails.
//
// Callers already hold expireMu (settleExpireBatch), the same
// relationship commitExpireLocked has to commitExpire. There is no
// self-locking wrapper: settleExpireBatch is the only caller, since the
// single-key incrBy path that used one was removed. expireOrder may still
// carry a stale entry for key afterward; left as-is — the only
// consequence is commitExpireLocked's own FIFO eviction occasionally
// evicting a key one step early if it is re-committed at a new position
// later, itself always safe (a spurious extra EXPIRE, the same accepted
// cost as any other eviction).
func (s *redisStore) forgetExpireLocked(key string) {
	delete(s.expireSeen, key)
}

// commitExpire records key as having a CONFIRMED EXPIRE on Redis — call
// only after the pipeline carrying key's EXPIRE command is known to have
// succeeded (client.pipeline returned a nil error). Bounded FIFO
// eviction, same reasoning as expireOnceCacheMax's own doc comment.
func (s *redisStore) commitExpire(key string) {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	s.commitExpireLocked(key)
}

// ttlToSeconds converts ttl to whole Redis EXPIRE seconds, rounded up,
// with a floor of 1s so a sub-second ttl never turns into EXPIRE 0 (an
// immediate delete). Shared by incrMulti and incrAndGetMulti.
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

// repairRecreatedExpires immediately sends one more pipeline of EXPIRE
// commands for every (key, ttl) pair whose INCRBY reply just revealed
// Redis silently recreated it with no TTL, even though this process's own
// cache believed it already had a confirmed EXPIRE (finding F7, 2026-09
// review — v == delta in incrMulti/incrAndGetMulti above; see
// forgetExpireLocked's own doc comment for why that combination means Redis,
// not this process, lost the key). forgetExpireLocked alone only guarantees
// THIS key's own NEXT increment resends EXPIRE — a different Traefik
// instance whose own cache never forgot the key (it never observed the
// loss) can keep skipping EXPIRE for it indefinitely, exactly the
// multi-replica desync scenario the review reports. Repairing here,
// inside the very call that discovered the loss, fixes the key
// regardless of which process or key gets incremented next, without
// adding a persistent cross-call pending-repair queue.
//
// recreated[i] marks entries[i] for repair (applyIncrReplies' firstWrite).
// It is usually all false (the common case — nothing to repair this call,
// and no allocation or round trip happens); a single pipeline batches
// every repair a whole incrMulti/incrAndGetMulti call needs, so a batch
// with several simultaneously-recreated keys still costs one extra round
// trip, not one per key.
//
// Best-effort: this pipeline's own failure, or an EXPIRE reply other than
// 1, is not propagated as an error to the caller — the INCRBY values
// already computed are correct and must still be returned regardless. A
// repair that fails leaves the key exactly as unconfirmed as it already
// was (needsExpire keeps reporting true), so the ordinary per-key
// self-heal on this key's own next increment still applies as a
// fallback.
func (s *redisStore) repairRecreatedExpires(entries []counterIncr, recreated []bool) {
	var keys []string
	var cmds [][]string
	for i, e := range entries {
		if !recreated[i] {
			continue
		}
		keys = append(keys, e.key)
		cmds = append(cmds, []string{"EXPIRE", e.key, strconv.FormatInt(ttlToSeconds(e.ttl), 10)})
	}
	if len(cmds) == 0 {
		return
	}
	replies, err := s.client.pipeline(cmds)
	if err != nil || len(replies) != len(cmds) {
		return
	}
	for i, k := range keys {
		if n, ok := replies[i].(int64); ok && n == 1 {
			s.commitExpire(k)
		}
	}
}

// applyIncrReplies decodes the INCRBY replies for entries — each entry's
// own reply located via incrReplyIdx[i], not a fixed stride (a skipped
// EXPIRE shifts every later entry's commands left by one; see incrMulti/
// incrAndGetMulti's own doc comments) — writing each entry's post-
// increment value into out[i]. sent[i] is needsExpireBatch's verdict for
// entry i (whether THIS call sent its EXPIRE, immediately after its
// INCRBY). confirmed[i] records whether that EXPIRE was sent AND its own
// reply is literally the integer 1 (finding F6, 2026-09 review: an ACL
// denying EXPIRE, or any other error reply, must never be recorded as a
// confirmed TTL). firstWrite[i] records whether EXPIRE was NOT sent and
// the INCRBY reply equalled the entry's own delta (the Redis-lost-the-key
// recreate signal settleExpireBatch forgets and repairRecreatedExpires
// re-arms, finding F7). method names the caller
// ("incrMulti" or "incrAndGetMulti") purely for the error message prefix,
// so both callers' error text stays byte-identical to what they built
// inline before this was factored out.
//
// It reports how many entries it finished examining: len(entries) on full
// success, or the index of the first failing entry (0 if the very first
// one fails) — never entries[i] itself when it fails, since out[i]/
// confirmed[i]/firstWrite[i] were never validly written for it. Both callers settle
// expire bookkeeping for exactly that PROCESSED prefix in ONE
// settleExpireBatch call at their own single call site, on every return
// path, rather than a deferred closure per pipeline (measured ~1.5%
// overhead under yaegi v0.16.1: a closure capturing three slices, paid
// twice per request — one settle call site fed by this helper's reported
// progress keeps the CRITICAL review fix, below, without that cost) or
// losing the bookkeeping for entries already examined before a later
// failure (review fix, CRITICAL: entries[:processed] must still be
// settled even when entries[processed] itself failed — see needsExpire's
// own doc comment for why a lost forget is the dangerous half).
func (s *redisStore) applyIncrReplies(method string, entries []counterIncr, replies []any, incrReplyIdx []int, sent []bool, out []int64, confirmed, firstWrite []bool) (processed int, err error) {
	for i, e := range entries {
		reply := replies[incrReplyIdx[i]]
		if re, ok := reply.(error); ok {
			// P11 fix (admin dashboard redesign verify round): an
			// absolute entry's own command is SET, not INCRBY (below) —
			// this error text used to say "INCRBY failed" even for a
			// failed SET (e.g. a last-seen write against an ACL that
			// denies SET but allows INCRBY), which would have sent an
			// operator investigating the wrong command entirely.
			cmd := "INCRBY"
			if e.absolute {
				cmd = "SET"
			}
			return i, fmt.Errorf("redisStore: %s %q: %s failed: %w", method, e.key, cmd, re)
		}
		// An absolute entry's own command is SET key v EX secs, which
		// replies with the literal simple-string "OK" — resp.go decodes a
		// simple-string reply to a Go string (never int64), and this is
		// Yaegi-safe: reply.(string) is a comma-ok assertion of a COMPILED
		// value (the resp decoder's own output) against a stdlib interface
		// method set, never an interpreted plugin type on either side (the
		// documented-safe direction — matchesSentinel's own doc comment,
		// limits.go). out[i] is set to e.delta itself (the value SET wrote),
		// since a SET reply carries no post-write count the way INCRBY's
		// does; no EXPIRE was ever sent for it (needsExpireBatch's own
		// absolute branch), so confirmed[i]/firstWrite[i] stay at their
		// zero value — nothing here needs expire bookkeeping.
		if e.absolute {
			s, ok := reply.(string)
			if !ok || s != "OK" {
				return i, fmt.Errorf("redisStore: %s %q: unexpected SET reply %v (%T)", method, e.key, reply, reply)
			}
			out[i] = e.delta
			continue
		}
		v, ok := reply.(int64)
		if !ok {
			return i, fmt.Errorf("redisStore: %s %q: unexpected INCRBY reply type %T", method, e.key, reply)
		}
		out[i] = v
		if sent[i] {
			expireReply, isInt := replies[incrReplyIdx[i]+1].(int64)
			confirmed[i] = isInt && expireReply == 1
		} else {
			firstWrite[i] = v == e.delta
		}
	}
	return len(entries), nil
}

// incrMulti implements counterStore: every entry's INCRBY (or, for an
// absolute entry, a SET — counterIncr.absolute's own doc comment) plus an
// EXPIRE only when needsExpireBatch(entries) says so for that entry
// (P11 fix, admin dashboard redesign verify round: this comment named
// needsExpire, the single-key predecessor moved to redis_store_helpers_
// test.go once every production call site went through the batched form
// — needsExpireBatch's own doc comment; an absolute entry never gets one
// at all, needsExpireBatch's own absolute branch), sent as ONE pipeline
// (perf review, 2026-08-21) — one round trip regardless of len(entries)
// or how many of them get an EXPIRE this time. This is the only production path
// that reaches Redis for a counter write (incrBy, its single-key
// predecessor, had no production caller left once checkAndCount/account
// moved onto the batched paths, and was removed — deadcode audit,
// 2026-09 review); a batch of one entry carries the identical behavior.
//
// expireSeen bookkeeping (perf finding 2, review fix, CRITICAL,
// 2026-08-2x) happens strictly AFTER the pipeline succeeds — never
// before: commitExpire(e.key) only when this call actually sent EXPIRE
// for that entry AND its own reply confirms success (finding F6, 2026-09
// review — EXPIRE's reply used to go uninspected: an ACL that allows
// INCRBY but denies EXPIRE, or any other EXPIRE error reply, was
// previously recorded as a CONFIRMED TTL regardless, making the key
// immortal, since needsExpire would never ask again); forgetExpireLocked(e.key)
// when the INCRBY reply reveals that entry's key was recreated (v ==
// e.delta) despite this process's cache believing it already had a
// confirmed EXPIRE, immediately followed by repairRecreatedExpires to
// re-arm its TTL in this SAME call rather than only on that key's own
// next increment (finding F7) — see commitExpire/forgetExpireLocked/
// repairRecreatedExpires' own doc comments.
//
// sendExpire[i] records, per entry, whether THIS call actually sent
// EXPIRE for it — decided once, before the pipeline goes out, and reused
// after the reply arrives (applyIncrReplies) to choose the confirmed-
// EXPIRE commit vs. the v==delta forget-and-repair check above. incrReplyIdx[i] is the index within cmds (and
// so within replies) of entry i's own INCRBY — NOT a fixed i*2 stride,
// since a skipped EXPIRE shifts every later entry's commands left by one;
// entry i's own EXPIRE, when included, immediately follows its INCRBY and
// is not otherwise inspected: a failed EXPIRE right after a successful
// INCRBY only leaves that one key without a fresh TTL, not a corrupted
// count.
//
// Semantics are deliberately at-least-once, not exactly-once, both in the
// conservative direction (never under-counts): respClient.pipeline's
// reconnect-once retry can re-send the whole batch if a reply was lost
// after the server already applied it (e.g. the connection dropped
// between the server processing a command and the client reading its
// reply), which can over-count every entry in it by its own delta; and
// the limiter's failOpen path can additionally count the same request in
// its in-process fallback store when a call to this method errors out
// after a partial success upstream. Both are accepted trade-offs — a
// rate/budget counter that occasionally over-counts by one request's
// worth is fail-safe (more restrictive than reality), never fail-open in
// the unsafe direction.
func (s *redisStore) incrMulti(entries []counterIncr) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	cmds := make([][]string, 0, len(entries)*2)
	incrReplyIdx := make([]int, len(entries))
	sendExpire := s.needsExpireBatch(entries)
	for i, e := range entries {
		incrReplyIdx[i] = len(cmds)
		if e.absolute {
			cmds = append(cmds, []string{"SET", e.key, strconv.FormatInt(e.delta, 10), "EX", strconv.FormatInt(ttlToSeconds(e.ttl), 10)})
			continue
		}
		cmds = append(cmds, []string{"INCRBY", e.key, strconv.FormatInt(e.delta, 10)})
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
	confirmed := make([]bool, len(entries))
	firstWrite := make([]bool, len(entries))
	processed, applyErr := s.applyIncrReplies("incrMulti", entries, replies, incrReplyIdx, sendExpire, out, confirmed, firstWrite)
	s.settleExpireBatch(entries[:processed], confirmed[:processed], firstWrite[:processed])
	if applyErr != nil {
		return nil, applyErr
	}
	// F7: repair every recreated-with-no-TTL key found this call in ONE
	// follow-up pipeline, batched across the whole entries slice — see
	// repairRecreatedExpires' own doc comment.
	s.repairRecreatedExpires(entries, firstWrite)
	return out, nil
}

// incrAndGetMulti implements counterStore: every entry's INCRBY (or SET
// for an absolute entry), plus an EXPIRE only when needsExpireBatch(entries)
// says so for that entry (P11 fix, admin dashboard redesign verify round —
// see incrMulti's own doc comment for why this no longer names needsExpire;
// same commit-after-success/forget-on-recreate bookkeeping as incrMulti's
// own doc comment), AND every read key's GET, sent as ONE pipeline (perf review round 3,
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
	sendExpire := s.needsExpireBatch(entries)
	for i, e := range entries {
		incrReplyIdx[i] = len(cmds)
		if e.absolute {
			cmds = append(cmds, []string{"SET", e.key, strconv.FormatInt(e.delta, 10), "EX", strconv.FormatInt(ttlToSeconds(e.ttl), 10)})
			continue
		}
		cmds = append(cmds, []string{"INCRBY", e.key, strconv.FormatInt(e.delta, 10)})
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
	confirmed := make([]bool, len(entries))
	firstWrite := make([]bool, len(entries))
	processed, applyErr := s.applyIncrReplies("incrAndGetMulti", entries, replies, incrReplyIdx, sendExpire, incrVals, confirmed, firstWrite)
	s.settleExpireBatch(entries[:processed], confirmed[:processed], firstWrite[:processed])
	if applyErr != nil {
		return nil, nil, applyErr
	}
	// F7: repair every recreated-with-no-TTL key found this call in ONE
	// follow-up pipeline, batched across the whole entries slice — see
	// repairRecreatedExpires' own doc comment.
	s.repairRecreatedExpires(entries, firstWrite)

	// The GET half is read-only (incrAndGetMulti's own doc comment) — its
	// own error returns below come strictly after expire bookkeeping is
	// already settled above, so none of them need any settle of their
	// own, and no settle call belongs inside this loop: its index i counts
	// reads, not entries, and slicing entries/sendExpire/firstWrite by it
	// would settle the wrong prefix.
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
