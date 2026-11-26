package traefikllmgateway

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	sharedStateRefreshInterval = time.Second
	// sharedStateIdleStop ends the ticker of a gateway that uses no shared state, such as one a config reload replaced.
	sharedStateIdleStop = 10 * time.Minute
	// sharedStateMaxFailures is the count of consecutive failed refreshes after which shared values count as absent.
	sharedStateMaxFailures = 3
	// sharedStateChunk bounds the keys of one getMulti.
	sharedStateChunk = 500

	// Breaker kinds, the {kind} part of a breaker key.
	sharedKindRequest   = "req"
	sharedKindDiscovery = "disc"
	sharedKindDecision  = "dec"

	sharedBreakerPrefix  = "llmgw:brk:"
	sharedInflightPrefix = "llmgw:inf:"

	sharedBreakerOpensTTLFactor = 4
	sharedBreakerMaxDoublings   = 30
	// sharedBreakerKeysPerID counts the keys keys() lists per id: open, fails, opens.
	sharedBreakerKeysPerID = 3

	sharedInflightEpochSecs = 300
	sharedInflightEpoch     = 5 * time.Minute
	sharedInflightLimitTTL  = 10 * time.Minute
)

// sharedPrimitive is a state that reads a fixed key set from the store on every refresh.
type sharedPrimitive interface {
	keys(now time.Time) []string
	// apply hands over the values of keys in order; nil means shared values are absent.
	apply(vals []int64)
}

// sharedFlusher is a primitive that accumulates writes locally and sends them from the loop.
type sharedFlusher interface {
	// flush writes the accumulated deltas; on error it keeps them for the next tick.
	flush(l *sharedStateLoop, now time.Time) error
	pending() bool
}

// sharedStateLoop refreshes every registered primitive from the store in the background, so request paths read local copies only.
type sharedStateLoop struct {
	store counterStore
	nowFn func() time.Time
	logf  func(string, ...any)
	// lossy runs best-effort writes and may drop them under pressure.
	lossy    func(func())
	lastUse  time.Time
	stop     chan struct{}
	done     chan struct{}
	prims    []sharedPrimitive
	flushers []sharedFlusher
	every    time.Duration
	failures int
	mu       sync.Mutex
	flushMu  sync.Mutex
	running  bool
	closed   bool
}

// newSharedStateLoop returns a loop over store; lossy is the droppable write spawn (the limiter's).
func newSharedStateLoop(store counterStore, lossy func(func()), logf func(string, ...any)) *sharedStateLoop {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	l := &sharedStateLoop{store: store, nowFn: time.Now, logf: logf, lossy: lossy, every: sharedStateRefreshInterval}
	if l.lossy == nil {
		l.lossy = func(f func()) {
			go func() { //nolint:gosec // G118: best-effort write; bounded by the caller's request rate
				defer func() {
					if rec := recover(); rec != nil {
						logf("shared state write panicked: %v", rec)
					}
				}()
				f()
			}()
		}
	}
	return l
}

// log forwards to logf; calling the func field directly with two or more variadic args panics under yaegi v0.16.1.
func (l *sharedStateLoop) log(format string, args ...any) {
	l.logf(format, args...)
}

func (l *sharedStateLoop) registerFlusher(f sharedFlusher) {
	l.mu.Lock()
	l.flushers = append(l.flushers, f)
	l.mu.Unlock()
}

// flush sends every flusher's accumulated deltas. A failed flusher keeps its deltas for the next call.
func (l *sharedStateLoop) flush() {
	// One flush at a time: concurrent ones would send the same deltas twice.
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	fl := make([]sharedFlusher, len(l.flushers))
	copy(fl, l.flushers)
	l.mu.Unlock()
	now := l.nowFn()
	for _, f := range fl {
		if err := f.flush(l, now); err != nil {
			l.log("shared state flush failed: %v", err)
		}
	}
}

// hasPending reports whether any flusher holds unsent deltas. The caller holds l.mu.
func (l *sharedStateLoop) hasPending() bool {
	for _, f := range l.flushers {
		if f.pending() {
			return true
		}
	}
	return false
}

func (l *sharedStateLoop) register(p sharedPrimitive) {
	l.mu.Lock()
	l.prims = append(l.prims, p)
	l.mu.Unlock()
}

// stale reports whether the last refresh failed and shared values are last-known or absent.
func (l *sharedStateLoop) stale() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures > 0
}

// touch notes a use and starts the refresh goroutine if it is not running. A nil loop is a no-op.
func (l *sharedStateLoop) touch() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.lastUse = l.nowFn()
	start := !l.running && !l.closed
	if start {
		l.running = true
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
	}
	stop, done := l.stop, l.done
	l.mu.Unlock()
	if start {
		go l.run(stop, done) //nolint:gosec // G118: bounded by close and sharedStateIdleStop, one goroutine per gateway
	}
}

func (l *sharedStateLoop) run(stop, done chan struct{}) {
	// No request goroutine can catch a panic here, and it would end the whole Traefik process.
	defer func() {
		if rec := recover(); rec != nil {
			l.log("shared state refresh panicked: %v", rec)
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
		}
		close(done)
	}()
	l.refresh()
	t := time.NewTicker(l.every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			l.mu.Lock()
			idle := l.nowFn().Sub(l.lastUse) > sharedStateIdleStop && !l.hasPending()
			if idle {
				l.running = false
			}
			l.mu.Unlock()
			if idle {
				l.clearAll()
				return
			}
			l.refresh()
		}
	}
}

// close stops the refresh goroutine for good. A nil loop is a no-op.
func (l *sharedStateLoop) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	if l.running {
		l.running = false
		close(l.stop)
	}
	l.mu.Unlock()
	l.flush()
}

func (l *sharedStateLoop) snapshot() []sharedPrimitive {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]sharedPrimitive, len(l.prims))
	copy(out, l.prims)
	return out
}

func (l *sharedStateLoop) clearAll() {
	var none []int64
	for _, p := range l.snapshot() {
		p.apply(none)
	}
}

// refresh flushes pending deltas, then reads every primitive's keys in chunked getMulti calls and hands each its values.
func (l *sharedStateLoop) refresh() {
	l.flush()
	prims := l.snapshot()
	now := l.nowFn()
	var all []string
	sizes := make([]int, len(prims))
	for i, p := range prims {
		k := p.keys(now)
		sizes[i] = len(k)
		all = append(all, k...)
	}
	vals := make([]int64, 0, len(all))
	for off := 0; off < len(all); off += sharedStateChunk {
		end := off + sharedStateChunk
		if end > len(all) {
			end = len(all)
		}
		got, err := l.store.getMulti(all[off:end])
		if err != nil || len(got) != end-off {
			l.refreshFailed(prims, err)
			return
		}
		vals = append(vals, got...)
	}
	l.mu.Lock()
	l.failures = 0
	l.mu.Unlock()
	pos := 0
	for i, p := range prims {
		p.apply(vals[pos : pos+sizes[i]])
		pos += sizes[i]
	}
}

func (l *sharedStateLoop) refreshFailed(prims []sharedPrimitive, err error) {
	l.mu.Lock()
	l.failures++
	absent := l.failures >= sharedStateMaxFailures
	n := l.failures
	l.mu.Unlock()
	l.log("shared state refresh failed (%d consecutive): %v", n, err)
	if absent {
		var none []int64
		for _, p := range prims {
			p.apply(none)
		}
	}
}

func (l *sharedStateLoop) incrAndGet(e counterIncr) (int64, bool) {
	got, _, err := l.store.incrAndGetMulti([]counterIncr{e}, nil)
	if err != nil || len(got) != 1 {
		l.log("shared state write %q failed: %v", e.key, err)
		return 0, false
	}
	return got[0], true
}

func (l *sharedStateLoop) write(entries []counterIncr) {
	if _, err := l.store.incrMulti(entries); err != nil {
		l.log("shared state write of %d keys failed: %v", len(entries), err)
	}
}

func sharedBreakerKey(kind, id, field string) string {
	return sharedBreakerPrefix + kind + ":" + id + ":" + field
}

func sharedInflightKey(provider, suffix string) string {
	return sharedInflightPrefix + provider + ":" + suffix
}

// sharedStateIDs returns the sorted provider ids shared primitives track: configured providers plus each router's decision provider.
func sharedStateIDs(cfg *Config) []string {
	seen := make(map[string]bool)
	for name := range cfg.Providers {
		seen[name] = true
	}
	for _, r := range cfg.SmartRouters {
		if r != nil && r.Decision != nil && r.Decision.Provider != "" {
			seen[r.Decision.Provider] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// breakerLocal is one id's local breaker state, mirroring requestHealthState.
type breakerLocal struct {
	openUntil           time.Time
	backoff             time.Duration
	consecutiveFailures int
	open                bool
}

// sharedBreaker is a per-id circuit breaker whose trips are shared fleet-wide through the store. With a nil loop it is purely local.
type sharedBreaker struct {
	loop       *sharedStateLoop
	nowFn      func() time.Time
	local      map[string]*breakerLocal
	openUntil  map[string]int64
	opensSeen  map[string]int64
	failsSeen  map[string]int64
	failAcc    map[string]int64
	kind       string
	ids        []string
	openBase   time.Duration
	openMax    time.Duration
	failWindow time.Duration
	threshold  int
	mu         sync.Mutex
}

// newSharedBreaker returns a breaker over ids; loop may be nil for a local-only breaker.
func newSharedBreaker(loop *sharedStateLoop, kind string, ids []string, threshold int, openBase, openMax, failWindow time.Duration) *sharedBreaker {
	b := &sharedBreaker{
		loop: loop, nowFn: time.Now, kind: kind, ids: ids,
		threshold: threshold, openBase: openBase, openMax: openMax, failWindow: failWindow,
		local: make(map[string]*breakerLocal), openUntil: make(map[string]int64), opensSeen: make(map[string]int64), failsSeen: make(map[string]int64), failAcc: make(map[string]int64),
	}
	if loop != nil {
		b.nowFn = loop.nowFn
		loop.register(b)
		loop.registerFlusher(b)
	}
	return b
}

// keys lists, per id, the open-until, fail-count and open-count keys.
func (b *sharedBreaker) keys(_ time.Time) []string {
	out := make([]string, 0, sharedBreakerKeysPerID*len(b.ids))
	for _, id := range b.ids {
		out = append(out, sharedBreakerKey(b.kind, id, "open"), sharedBreakerKey(b.kind, id, "fails"), sharedBreakerKey(b.kind, id, "opens"))
	}
	return out
}

func (b *sharedBreaker) apply(vals []int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, id := range b.ids {
		if vals == nil {
			delete(b.openUntil, id)
			delete(b.failsSeen, id)
			delete(b.opensSeen, id)
		} else {
			b.openUntil[id] = vals[sharedBreakerKeysPerID*i]
			b.failsSeen[id] = vals[sharedBreakerKeysPerID*i+1]
			b.opensSeen[id] = vals[sharedBreakerKeysPerID*i+2]
		}
	}
}

func (b *sharedBreaker) localFor(id string) *breakerLocal {
	st, ok := b.local[id]
	if !ok {
		st = &breakerLocal{}
		b.local[id] = st
	}
	return st
}

// isOpen reports whether the local or the last-refreshed shared breaker is open. It never calls the store.
func (b *sharedBreaker) isOpen(id string) bool {
	b.loop.touch()
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.nowFn()
	if st, ok := b.local[id]; ok && st.open && now.Before(st.openUntil) {
		return true
	}
	return b.openUntil[id] > now.UnixMilli()
}

// sharedOpen reports whether the last-refreshed fleet state says id is open, ignoring this breaker's local arm.
// Callers with their own local breaker use it to layer the fleet view on top. It never calls the store.
func (b *sharedBreaker) sharedOpen(id string) bool {
	b.loop.touch()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil[id] > b.nowFn().UnixMilli()
}

// recordFailure updates local state and queues the failure for the loop to flush.
func (b *sharedBreaker) recordFailure(id string) {
	b.mu.Lock()
	b.recordFailureLocal(b.localFor(id), b.nowFn())
	if b.loop != nil {
		b.failAcc[id]++
	}
	b.mu.Unlock()
	b.loop.touch()
}

// recordFailureLocal is requestHealthTracker.record's failure arm with this breaker's tunables.
func (b *sharedBreaker) recordFailureLocal(st *breakerLocal, now time.Time) {
	if st.open && now.Before(st.openUntil) {
		return
	}
	if st.open {
		if st.backoff <= 0 {
			st.backoff = b.openBase
		} else {
			st.backoff *= 2
		}
		if st.backoff > b.openMax {
			st.backoff = b.openMax
		}
		st.openUntil = now.Add(st.backoff)
		return
	}
	st.consecutiveFailures++
	if st.consecutiveFailures >= b.threshold {
		st.open = true
		st.backoff = b.openBase
		st.openUntil = now.Add(st.backoff)
	}
}

func (b *sharedBreaker) pending() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.failAcc) > 0
}

// flush adds the queued failures to the fleet counts. The instance whose flush takes a count from below the
// threshold to at or above it is the single opener.
func (b *sharedBreaker) flush(l *sharedStateLoop, _ time.Time) error {
	b.mu.Lock()
	ids := make([]string, 0, len(b.failAcc))
	deltas := make([]int64, 0, len(b.failAcc))
	for id, n := range b.failAcc {
		ids = append(ids, id)
		deltas = append(deltas, n)
	}
	b.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	entries := make([]counterIncr, len(ids))
	for i, id := range ids {
		entries[i] = counterIncr{key: sharedBreakerKey(b.kind, id, "fails"), delta: deltas[i], ttl: b.failWindow, enforceTTL: b.failWindow}
	}
	post, _, err := l.store.incrAndGetMulti(entries, nil)
	if err != nil {
		return err
	}
	if len(post) != len(ids) {
		return nil
	}
	b.mu.Lock()
	for i, id := range ids {
		if left := b.failAcc[id] - deltas[i]; left > 0 {
			b.failAcc[id] = left
		} else {
			delete(b.failAcc, id)
		}
	}
	b.mu.Unlock()
	for i, id := range ids {
		if post[i]-deltas[i] < int64(b.threshold) && post[i] >= int64(b.threshold) {
			b.trip(l, id)
		}
	}
	return nil
}

func (b *sharedBreaker) trip(l *sharedStateLoop, id string) {
	opensTTL := b.openMax * sharedBreakerOpensTTLFactor
	opens, ok := l.incrAndGet(counterIncr{key: sharedBreakerKey(b.kind, id, "opens"), delta: 1, ttl: opensTTL, enforceTTL: opensTTL})
	if !ok {
		return
	}
	dur := b.openDuration(opens)
	until := l.nowFn().Add(dur).UnixMilli()
	// Resetting fails lets the next threshold failures trip again after the open window.
	l.write([]counterIncr{
		{key: sharedBreakerKey(b.kind, id, "open"), delta: until, ttl: dur, enforceTTL: dur, absolute: true},
		{key: sharedBreakerKey(b.kind, id, "fails"), delta: 0, ttl: b.failWindow, enforceTTL: b.failWindow, absolute: true},
	})
}

// openDuration is min(openBase << (opens-1), openMax).
func (b *sharedBreaker) openDuration(opens int64) time.Duration {
	d := b.openBase
	for i := int64(1); i < opens && i <= sharedBreakerMaxDoublings; i++ {
		d *= 2
		if d >= b.openMax {
			break
		}
	}
	if d > b.openMax {
		d = b.openMax
	}
	return d
}

// recordSuccess resets local state and clears the fleet failure count, open mark and open count when any may exist.
func (b *sharedBreaker) recordSuccess(id string) {
	b.mu.Lock()
	st := b.localFor(id)
	dirty := st.consecutiveFailures > 0 || b.failsSeen[id] > 0 || b.opensSeen[id] > 0
	// A success closes the fleet breaker too, as it closes the local one.
	wasOpen := b.openUntil[id] > b.nowFn().UnixMilli()
	opensSeen := b.opensSeen[id] > 0
	delete(b.failAcc, id)
	st.open = false
	st.consecutiveFailures = 0
	st.backoff = 0
	st.openUntil = time.Time{}
	b.mu.Unlock()
	if b.loop == nil || (!dirty && !wasOpen) {
		return
	}
	b.loop.touch()
	b.loop.lossy(func() {
		entries := []counterIncr{{key: sharedBreakerKey(b.kind, id, "fails"), delta: 0, ttl: b.failWindow, enforceTTL: b.failWindow, absolute: true}}
		if wasOpen {
			entries = append(entries, counterIncr{key: sharedBreakerKey(b.kind, id, "open"), delta: 0, ttl: b.openBase, enforceTTL: b.openBase, absolute: true})
		}
		if opensSeen {
			opensTTL := b.openMax * sharedBreakerOpensTTLFactor
			entries = append(entries, counterIncr{key: sharedBreakerKey(b.kind, id, "opens"), delta: 0, ttl: opensTTL, enforceTTL: opensTTL, absolute: true})
		}
		b.loop.write(entries)
	})
	b.mu.Lock()
	b.failsSeen[id] = 0
	b.opensSeen[id] = 0
	if wasOpen {
		delete(b.openUntil, id)
	}
	b.mu.Unlock()
}

// infDelta is one counter's unflushed increment.
type infDelta struct {
	key   string
	epoch int64
	n     int64
}

// sharedInflight counts in-flight requests per provider fleet-wide with epoch-bucketed started/finished counters, so a crashed pod's leak ages out.
type sharedInflight struct {
	loop   *sharedStateLoop
	nowFn  func() time.Time
	counts map[string]int
	limits map[string]int
	acc    map[string]*infDelta
	ids    []string
	epochs int
	mu     sync.Mutex
}

// newSharedInflight returns the tracker over ids; loop may be nil for a no-op tracker.
func newSharedInflight(loop *sharedStateLoop, ids []string, maxRequestTimeout time.Duration) *sharedInflight {
	m := int((maxRequestTimeout+sharedInflightEpoch-1)/sharedInflightEpoch) + 1
	if m < 1 {
		m = 1
	}
	s := &sharedInflight{loop: loop, nowFn: time.Now, ids: ids, epochs: m, counts: make(map[string]int), limits: make(map[string]int), acc: make(map[string]*infDelta)}
	if loop != nil {
		s.nowFn = loop.nowFn
		loop.register(s)
		loop.registerFlusher(s)
	}
	return s
}

func (s *sharedInflight) epochOf(t time.Time) int64 {
	return t.Unix() / sharedInflightEpochSecs
}

func (s *sharedInflight) ttl() time.Duration {
	return time.Duration(s.epochs+1) * sharedInflightEpoch
}

func (s *sharedInflight) epochKey(provider string, epoch int64, kind string) string {
	return sharedInflightKey(provider, strconv.FormatInt(epoch, 10)+":"+kind)
}

// keys lists, per provider, the started and finished keys of the last M epochs, then the learned limit.
func (s *sharedInflight) keys(now time.Time) []string {
	e := s.epochOf(now)
	out := make([]string, 0, len(s.ids)*(2*s.epochs+1))
	for _, id := range s.ids {
		for i := 0; i < s.epochs; i++ {
			out = append(out, s.epochKey(id, e-int64(i), "s"), s.epochKey(id, e-int64(i), "f"))
		}
		out = append(out, sharedInflightKey(id, "limit"))
	}
	return out
}

func (s *sharedInflight) apply(vals []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	per := 2*s.epochs + 1
	for i, id := range s.ids {
		if vals == nil {
			delete(s.counts, id)
			delete(s.limits, id)
		} else {
			base := i * per
			total := 0
			for j := 0; j < s.epochs; j++ {
				if d := vals[base+2*j] - vals[base+2*j+1]; d > 0 {
					total += int(d)
				}
			}
			s.counts[id] = total
			s.limits[id] = int(vals[base+2*s.epochs])
		}
	}
}

// begin records a started request and returns the func that records its end, once.
// It only adds to a local accumulator; the loop flushes it.
func (s *sharedInflight) begin(provider string) func() {
	if s.loop == nil {
		return func() {}
	}
	s.loop.touch()
	epoch := s.epochOf(s.nowFn())
	s.add(provider, epoch, "s")
	var once sync.Once
	return func() {
		once.Do(func() { s.add(provider, epoch, "f") })
	}
}

func (s *sharedInflight) add(provider string, epoch int64, kind string) {
	key := s.epochKey(provider, epoch, kind)
	s.mu.Lock()
	d, ok := s.acc[key]
	if !ok {
		d = &infDelta{key: key, epoch: epoch}
		s.acc[key] = d
	}
	d.n++
	s.mu.Unlock()
}

func (s *sharedInflight) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acc) > 0
}

// flush writes every non-zero delta in one incrMulti. Epochs outside the read window are discarded.
// On error the deltas stay and later begin/end calls add on top of them.
func (s *sharedInflight) flush(l *sharedStateLoop, now time.Time) error {
	oldest := s.epochOf(now) - int64(s.epochs)
	s.mu.Lock()
	entries := make([]counterIncr, 0, len(s.acc))
	for k, d := range s.acc {
		if d.epoch <= oldest {
			delete(s.acc, k)
		} else if d.n > 0 {
			entries = append(entries, counterIncr{key: d.key, delta: d.n, ttl: s.ttl(), enforceTTL: s.ttl()})
		}
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	if _, err := l.store.incrMulti(entries); err != nil {
		return err
	}
	s.mu.Lock()
	for _, e := range entries {
		if d, ok := s.acc[e.key]; ok {
			d.n -= e.delta
			if d.n <= 0 {
				delete(s.acc, e.key)
			}
		}
	}
	s.mu.Unlock()
	return nil
}

// shared returns the fleet-wide in-flight count from the last refresh.
func (s *sharedInflight) shared(provider string) int {
	s.loop.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[provider]
}

// merged returns max(local, shared) in-flight for provider.
func (s *sharedInflight) merged(provider string, local int) int {
	if sh := s.shared(provider); sh > local {
		return sh
	}
	return local
}

// setLearnedLimit shares a 429-learned limit; the last writer wins.
func (s *sharedInflight) setLearnedLimit(provider string, limit int) {
	if s.loop == nil || limit <= 0 {
		return
	}
	s.loop.touch()
	s.loop.lossy(func() {
		s.loop.write([]counterIncr{{key: sharedInflightKey(provider, "limit"), delta: int64(limit), ttl: sharedInflightLimitTTL, enforceTTL: sharedInflightLimitTTL, absolute: true}})
	})
}

// learnedLimit returns the shared learned limit when present, else local.
func (s *sharedInflight) learnedLimit(provider string, local int) int {
	s.loop.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.limits[provider]; v > 0 {
		return v
	}
	return local
}
