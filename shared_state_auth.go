package traefikllmgateway

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

const (
	sharedAuthPrefix = "llmgw:authfail:"
	// sharedAuthMaxQueued caps the source IPs queued per window; an IP past it keeps only its local count.
	sharedAuthMaxQueued = 4096
)

// sharedAuthFailures is the fleet arm of authFailureTracker. It queues failures per source IP and adds them to a
// per-minute Redis counter from the shared-state loop, so identify never waits on the store. An instance learns the
// fleet count from the reply to its own flush, so its view lags by at most one tick after its own failure.
type sharedAuthFailures struct {
	loop  *sharedStateLoop
	nowFn func() time.Time
	// acc holds failures not yet sent, for window accWin.
	acc map[string]int64
	// seen holds each IP's fleet count from its last INCRBY reply, for window seenWin.
	seen    map[string]int64
	accWin  int64
	seenWin int64
	mu      sync.Mutex
}

// newSharedAuthFailures reads the loop's clock, so record and flush agree on the window.
func newSharedAuthFailures(loop *sharedStateLoop) *sharedAuthFailures {
	s := &sharedAuthFailures{loop: loop, acc: make(map[string]int64), seen: make(map[string]int64)}
	s.nowFn = func() time.Time { return loop.nowFn() }
	loop.registerFlusher(s)
	return s
}

func authWindow(t time.Time) int64 {
	return t.Unix() / int64(authFailureWindow/time.Second)
}

func sharedAuthKey(win int64, ip string) string {
	return sharedAuthPrefix + strconv.FormatInt(win, 10) + ":" + ip
}

// rollLocked drops state from an earlier window: its counts no longer apply.
func (s *sharedAuthFailures) rollLocked(win int64) {
	if s.accWin != win {
		s.acc = make(map[string]int64)
		s.accWin = win
	}
	if s.seenWin != win {
		s.seen = make(map[string]int64)
		s.seenWin = win
	}
}

func (s *sharedAuthFailures) record(ip string) {
	win := authWindow(s.nowFn())
	s.mu.Lock()
	s.rollLocked(win)
	_, queued := s.acc[ip]
	if queued || len(s.acc) < sharedAuthMaxQueued {
		s.acc[ip]++
	}
	s.mu.Unlock()
	s.loop.touch()
}

// count is the fleet failure count for ip in the current window: the last reply plus failures queued since.
func (s *sharedAuthFailures) count(ip string) int64 {
	win := authWindow(s.nowFn())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(win)
	return s.seen[ip] + s.acc[ip]
}

func (s *sharedAuthFailures) pending() bool {
	win := authWindow(s.nowFn())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(win)
	return len(s.acc) > 0
}

// flush adds the queued failures to the fleet counters in chunks. A failed chunk keeps its failures for the next tick.
func (s *sharedAuthFailures) flush(l *sharedStateLoop, now time.Time) error {
	win := authWindow(now)
	s.mu.Lock()
	s.rollLocked(win)
	ips := make([]string, 0, len(s.acc))
	deltas := make([]int64, 0, len(s.acc))
	for ip, n := range s.acc {
		ips = append(ips, ip)
		deltas = append(deltas, n)
	}
	s.mu.Unlock()
	ttl := 2 * authFailureWindow
	for off := 0; off < len(ips); off += sharedStateChunk {
		end := off + sharedStateChunk
		if end > len(ips) {
			end = len(ips)
		}
		entries := make([]counterIncr, end-off)
		for i := off; i < end; i++ {
			entries[i-off] = counterIncr{key: sharedAuthKey(win, ips[i]), delta: deltas[i], ttl: ttl, enforceTTL: ttl}
		}
		post, _, err := l.store.incrAndGetMulti(entries, nil)
		if err != nil {
			return err
		}
		if len(post) != len(entries) {
			return fmt.Errorf("shared auth failures: %d counts for %d keys", len(post), len(entries))
		}
		s.applyFlushed(win, ips[off:end], deltas[off:end], post)
	}
	return nil
}

func (s *sharedAuthFailures) applyFlushed(win int64, ips []string, deltas, post []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accWin != win {
		return
	}
	for i, ip := range ips {
		if left := s.acc[ip] - deltas[i]; left > 0 {
			s.acc[ip] = left
		} else {
			delete(s.acc, ip)
		}
		s.seen[ip] = post[i]
	}
}
