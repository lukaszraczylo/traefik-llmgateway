package traefikllmgateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type sharedClock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *sharedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sharedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sharedRecStore wraps a counterStore, logging every key touched and optionally failing reads.
type sharedRecStore struct {
	counterStore
	failGet error
	reads   []string
	writes  []counterIncr
	mu      sync.Mutex
}

func (r *sharedRecStore) getMulti(keys []string) ([]int64, error) {
	r.mu.Lock()
	r.reads = append(r.reads, keys...)
	fail := r.failGet
	r.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	return r.counterStore.getMulti(keys)
}

func (r *sharedRecStore) incrMulti(e []counterIncr) ([]int64, error) {
	r.mu.Lock()
	r.writes = append(r.writes, e...)
	r.mu.Unlock()
	return r.counterStore.incrMulti(e)
}

func (r *sharedRecStore) incrAndGetMulti(e []counterIncr, reads []string) ([]int64, []int64, error) {
	r.mu.Lock()
	r.writes = append(r.writes, e...)
	r.mu.Unlock()
	return r.counterStore.incrAndGetMulti(e, reads)
}

func (r *sharedRecStore) writesTo(suffix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.writes {
		if strings.HasSuffix(w.key, suffix) {
			n++
		}
	}
	return n
}

func (r *sharedRecStore) setFail(err error) {
	r.mu.Lock()
	r.failGet = err
	r.mu.Unlock()
}

func sharedTestEnv() (*sharedRecStore, *sharedClock) {
	clk := &sharedClock{t: time.Unix(1_800_000_000, 0)}
	mem := newMemoryStore()
	mem.nowFn = clk.now
	return &sharedRecStore{counterStore: mem}, clk
}

// sharedTestLoop is one "pod": its own loop, writes run synchronously, no goroutine until touch.
func sharedTestLoop(store counterStore, clk *sharedClock) *sharedStateLoop {
	l := newSharedStateLoop(store, nil, nil)
	l.nowFn = clk.now
	sync := func(f func()) { f() }
	l.lossy, l.sure = sync, sync
	// Tests drive refresh by hand; closed keeps touch from starting the goroutine.
	l.closed = true
	return l
}

const (
	testBrkOpenBase = 30 * time.Second
	testBrkOpenMax  = 5 * time.Minute
	testBrkWindow   = time.Minute
)

func testBreaker(l *sharedStateLoop) *sharedBreaker {
	return newSharedBreaker(l, "req", []string{"p"}, 3, testBrkOpenBase, testBrkOpenMax, testBrkWindow)
}

func TestSharedBreakerTripsFleetWide(t *testing.T) {
	store, clk := sharedTestEnv()
	la, lb, lc := sharedTestLoop(store, clk), sharedTestLoop(store, clk), sharedTestLoop(store, clk)
	a, b, c := testBreaker(la), testBreaker(lb), testBreaker(lc)

	a.recordFailure("p")
	a.recordFailure("p")
	lc.refresh()
	if c.isOpen("p") {
		t.Fatal("C open after 2 fleet failures, want closed")
	}
	b.recordFailure("p")
	if c.isOpen("p") {
		t.Fatal("C open before refresh, isOpen must not read the store")
	}
	lc.refresh()
	if !c.isOpen("p") {
		t.Fatal("C closed after threshold failures and a refresh, want open")
	}
	clk.advance(testBrkOpenBase + time.Second)
	if c.isOpen("p") {
		t.Fatal("C still open after the open window")
	}
}

func TestSharedBreakerOnlyOneOpener(t *testing.T) {
	store, clk := sharedTestEnv()
	loops := []*sharedStateLoop{sharedTestLoop(store, clk), sharedTestLoop(store, clk)}
	brks := []*sharedBreaker{testBreaker(loops[0]), testBreaker(loops[1])}
	for i := 0; i < 5; i++ {
		brks[i%2].recordFailure("p")
	}
	if got := store.writesTo(":open"); got != 1 {
		t.Errorf("writes to open = %d, want 1", got)
	}
	if got := store.writesTo(":opens"); got != 1 {
		t.Errorf("increments of opens = %d, want 1", got)
	}
}

func TestSharedBreakerSuccessResets(t *testing.T) {
	store, clk := sharedTestEnv()
	la, lb, lc := sharedTestLoop(store, clk), sharedTestLoop(store, clk), sharedTestLoop(store, clk)
	a, b, c := testBreaker(la), testBreaker(lb), testBreaker(lc)

	a.recordFailure("p")
	a.recordFailure("p")
	lb.refresh()
	b.recordSuccess("p")
	// A's own local count stays; the fleet count is what C reads.
	lc.refresh()
	a2 := testBreaker(sharedTestLoop(store, clk))
	a2.recordFailure("p")
	a2.recordFailure("p")
	lc.refresh()
	if c.isOpen("p") {
		t.Fatal("C open after success reset then 2 failures, want closed")
	}
	a2.recordFailure("p")
	lc.refresh()
	if !c.isOpen("p") {
		t.Fatal("C closed after 3 failures since the reset, want open")
	}
}

func TestSharedBreakerSuccessWithoutFailuresWritesNothing(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	b := testBreaker(l)
	l.refresh()
	for i := 0; i < 5; i++ {
		b.recordSuccess("p")
	}
	if len(store.writes) != 0 {
		t.Errorf("healthy successes wrote %d entries, want 0", len(store.writes))
	}
}

func TestSharedBreakerBackoffDoubles(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	b := testBreaker(l)
	openKey := sharedBreakerKey("req", "p", "open")
	read := func() time.Duration {
		got, _ := store.getMulti([]string{openKey})
		return time.UnixMilli(got[0]).Sub(clk.now())
	}
	trip := func() {
		for i := 0; i < 3; i++ {
			b.recordFailure("p")
		}
	}
	want := []time.Duration{testBrkOpenBase, 2 * testBrkOpenBase, 4 * testBrkOpenBase, 8 * testBrkOpenBase, testBrkOpenMax, testBrkOpenMax}
	for i, w := range want {
		trip()
		if got := read(); got != w {
			t.Fatalf("trip %d open for %v, want %v", i+1, got, w)
		}
		clk.advance(w + time.Second)
	}
}

func TestSharedBreakerOpenDurationTable(t *testing.T) {
	b := testBreaker(nil)
	for _, tc := range []struct {
		opens int64
		want  time.Duration
	}{
		{1, 30 * time.Second}, {2, time.Minute}, {3, 2 * time.Minute}, {4, 4 * time.Minute},
		{5, 5 * time.Minute}, {100, 5 * time.Minute},
	} {
		if got := b.openDuration(tc.opens); got != tc.want {
			t.Errorf("openDuration(%d) = %v, want %v", tc.opens, got, tc.want)
		}
	}
}

func TestSharedBreakerLocalOnlyMatchesRequestHealth(t *testing.T) {
	clk := &sharedClock{t: time.Unix(1_800_000_000, 0)}
	b := newSharedBreaker(nil, "req", []string{"p"}, requestBreakerFailureThreshold, requestBreakerOpenDuration, requestBreakerMaxOpenDuration, time.Minute)
	b.nowFn = clk.now
	ref := newRequestHealthTracker()
	ref.nowFn = clk.now
	steps := []struct {
		success bool
		advance time.Duration
	}{
		{false, 0}, {false, 0}, {false, 0}, {false, time.Second}, {false, 31 * time.Second},
		{false, 0}, {false, 61 * time.Second}, {true, 0}, {false, 0}, {false, 0}, {false, 0},
	}
	for i, s := range steps {
		clk.advance(s.advance)
		if s.success {
			b.recordSuccess("p")
		} else {
			b.recordFailure("p")
		}
		ref.record("p", s.success)
		if got, want := !b.isOpen("p"), ref.healthy("p"); got != want {
			t.Fatalf("step %d: healthy = %v, want %v", i, got, want)
		}
	}
}

func inflightEnv(maxReq time.Duration) (*sharedRecStore, *sharedClock, func() (*sharedStateLoop, *sharedInflight)) {
	store, clk := sharedTestEnv()
	return store, clk, func() (*sharedStateLoop, *sharedInflight) {
		l := sharedTestLoop(store, clk)
		return l, newSharedInflight(l, []string{"p"}, maxReq)
	}
}

func TestSharedInflightSums(t *testing.T) {
	_, _, mk := inflightEnv(10 * time.Minute)
	_, a := mk()
	_, b := mk()
	lc, c := mk()
	end1 := a.begin("p")
	a.begin("p")
	b.begin("p")
	lc.refresh()
	if got := c.shared("p"); got != 3 {
		t.Fatalf("shared = %d, want 3", got)
	}
	end1()
	end1() // a second end must not double-count
	lc.refresh()
	if got := c.shared("p"); got != 2 {
		t.Fatalf("shared after one end = %d, want 2", got)
	}
	if got := c.merged("p", 5); got != 5 {
		t.Errorf("merged(local 5) = %d, want 5", got)
	}
	if got := c.merged("p", 1); got != 2 {
		t.Errorf("merged(local 1) = %d, want 2", got)
	}
}

func TestSharedInflightCrashLeakExpires(t *testing.T) {
	_, clk, mk := inflightEnv(10 * time.Minute)
	_, a := mk()
	lc, c := mk()
	a.begin("p") // never ended: the pod crashed
	lc.refresh()
	if got := c.shared("p"); got != 1 {
		t.Fatalf("shared = %d, want 1", got)
	}
	clk.advance(time.Duration(c.epochs) * sharedInflightEpoch)
	lc.refresh()
	if got := c.shared("p"); got != 0 {
		t.Fatalf("shared after %d epochs = %d, want 0", c.epochs, got)
	}
}

func TestSharedInflightEndInLaterEpochCreditsStartEpoch(t *testing.T) {
	_, clk, mk := inflightEnv(10 * time.Minute)
	_, a := mk()
	lc, c := mk()
	end := a.begin("p")
	clk.advance(sharedInflightEpoch + time.Second)
	end()
	lc.refresh()
	if got := c.shared("p"); got != 0 {
		t.Fatalf("shared = %d, want 0 (end credited to the start epoch)", got)
	}
}

func TestSharedInflightNegativeEpochClamped(t *testing.T) {
	store, clk, mk := inflightEnv(10 * time.Minute)
	lc, c := mk()
	e := c.epochOf(clk.now())
	// Epoch e-1 saw more finishes than starts; epoch e has 2 open.
	if _, err := store.incrMulti([]counterIncr{
		{key: c.epochKey("p", e-1, "s"), delta: 1, ttl: time.Hour},
		{key: c.epochKey("p", e-1, "f"), delta: 3, ttl: time.Hour},
		{key: c.epochKey("p", e, "s"), delta: 2, ttl: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	lc.refresh()
	if got := c.shared("p"); got != 2 {
		t.Fatalf("shared = %d, want 2", got)
	}
}

func TestSharedInflightLearnedLimit(t *testing.T) {
	_, clk, mk := inflightEnv(time.Minute)
	_, a := mk()
	lc, c := mk()
	if got := c.learnedLimit("p", 7); got != 7 {
		t.Fatalf("before any shared limit = %d, want local 7", got)
	}
	a.setLearnedLimit("p", 4)
	a.setLearnedLimit("p", 6) // last writer wins
	lc.refresh()
	if got := c.learnedLimit("p", 7); got != 6 {
		t.Fatalf("learnedLimit = %d, want shared 6", got)
	}
	clk.advance(sharedInflightLimitTTL + time.Second)
	lc.refresh()
	if got := c.learnedLimit("p", 7); got != 7 {
		t.Fatalf("after TTL = %d, want local 7", got)
	}
}

func TestSharedStateStoreErrorFallsBackLocal(t *testing.T) {
	store, clk := sharedTestEnv()
	la, lc := sharedTestLoop(store, clk), sharedTestLoop(store, clk)
	a, c := testBreaker(la), testBreaker(lc)
	for i := 0; i < 3; i++ {
		a.recordFailure("p")
	}
	lc.refresh()
	if !c.isOpen("p") {
		t.Fatal("setup: C should see the shared open")
	}

	store.setFail(errors.New("redis down"))
	lc.refresh()
	lc.refresh()
	if !lc.stale() {
		t.Error("loop not marked stale after failed refreshes")
	}
	if !c.isOpen("p") {
		t.Error("last-known shared open dropped before 3 failed refreshes")
	}
	lc.refresh()
	if c.isOpen("p") {
		t.Error("shared open still used after 3 failed refreshes, want absent")
	}

	// Local breaker keeps working while the store is down.
	for i := 0; i < 3; i++ {
		c.recordFailure("p")
	}
	if !c.isOpen("p") {
		t.Error("local breaker did not open while store is down")
	}

	store.setFail(nil)
	lc.refresh()
	if lc.stale() {
		t.Error("loop still stale after a good refresh")
	}
}

func TestSharedStateChunkedRefresh(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	ids := make([]string, 0, 700)
	for i := 0; i < 700; i++ {
		ids = append(ids, "prov"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
	}
	b := newSharedBreaker(l, "req", ids, 3, testBrkOpenBase, testBrkOpenMax, testBrkWindow)
	b.recordFailure(ids[699])
	l.refresh()
	if len(store.reads) != 1400 {
		t.Fatalf("keys read = %d, want 1400", len(store.reads))
	}
	if l.stale() {
		t.Error("stale after a chunked successful refresh")
	}
}

func TestSharedStateOffNoKeys(t *testing.T) {
	store, _ := sharedTestEnv()
	b := newSharedBreaker(nil, "req", []string{"p"}, 3, testBrkOpenBase, testBrkOpenMax, testBrkWindow)
	in := newSharedInflight(nil, []string{"p"}, time.Minute)
	for i := 0; i < 4; i++ {
		b.recordFailure("p")
		b.recordSuccess("p")
		b.isOpen("p")
	}
	in.begin("p")()
	in.shared("p")
	in.merged("p", 1)
	in.setLearnedLimit("p", 3)
	in.learnedLimit("p", 1)
	if len(store.reads) != 0 || len(store.writes) != 0 {
		t.Fatalf("nil loop touched the store: %d reads, %d writes", len(store.reads), len(store.writes))
	}
}

func TestSharedStateConfigValidation(t *testing.T) {
	on, off := true, false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct {
		redis   *RedisConfig
		name    string
		wantErr bool
		wantLp  bool
	}{
		{&RedisConfig{SharedState: &on}, "on without address", true, false},
		{&RedisConfig{SharedState: &off}, "off without address", true, false}, // pre-existing address error
		{&RedisConfig{Address: "127.0.0.1:0", SharedState: &on}, "on with address", false, true},
		{&RedisConfig{Address: "127.0.0.1:0", SharedState: &off}, "off with address", false, false},
		{&RedisConfig{Address: "127.0.0.1:0"}, "nil with address", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
			cfg.Redis = tc.redis
			h, err := New(context.Background(), next, cfg, "llmgw")
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				if tc.redis.SharedState != nil && *tc.redis.SharedState && !strings.Contains(err.Error(), "sharedState") {
					t.Errorf("error %q does not name sharedState", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw := h.(*Gateway)
			defer gw.Close()
			if got := gw.sharedState != nil; got != tc.wantLp {
				t.Errorf("sharedState loop present = %v, want %v", got, tc.wantLp)
			}
		})
	}
}

func TestSharedStateNoRedisNoLoop(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatal(err)
	}
	if h.(*Gateway).sharedState != nil {
		t.Error("loop built with no Redis")
	}
}

func TestSharedStateLoopLifecycle(t *testing.T) {
	store, clk := sharedTestEnv()
	l := newSharedStateLoop(store, nil, nil)
	l.nowFn = clk.now
	l.every = 5 * time.Millisecond
	b := testBreaker(l)
	b.recordFailure("p") // touch starts the goroutine
	deadline := time.Now().Add(2 * time.Second)
	for {
		l.mu.Lock()
		running := l.running
		l.mu.Unlock()
		if running || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Idle past the stop interval: the next tick ends the goroutine.
	clk.advance(sharedStateIdleStop + time.Second)
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on idle")
	}
	l.mu.Lock()
	if l.running {
		t.Error("running still set after idle stop")
	}
	l.mu.Unlock()

	// A new use restarts it; close stops it for good and blocks restarts.
	b.isOpen("p")
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.mu.Unlock()
	l.close()
	select {
	case <-stop:
	default:
		t.Fatal("close did not signal stop")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on close")
	}
	b.isOpen("p")
	l.mu.Lock()
	if l.running {
		t.Error("closed loop restarted")
	}
	l.mu.Unlock()
}

func TestSharedStateLoopRecoversFromPanic(t *testing.T) {
	store, clk := sharedTestEnv()
	l := newSharedStateLoop(store, nil, nil)
	l.nowFn = clk.now
	l.every = 5 * time.Millisecond
	l.register(panicPrimitive{})
	l.touch()
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking refresh did not end the goroutine cleanly")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		t.Error("running still set after panic")
	}
}

type panicPrimitive struct{}

func (panicPrimitive) keys(time.Time) []string { return []string{"k"} }
func (panicPrimitive) apply([]int64)           { panic("boom") }

func TestSharedStateIDs(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"b": {}, "a": {}},
		SmartRouters: map[string]*SmartRouterConfig{
			"r": {Decision: &SmartRouterDecisionConfig{Provider: "c"}},
			"s": {},
			"t": nil,
		},
	}
	got := strings.Join(sharedStateIDs(cfg), ",")
	if got != "a,b,c" {
		t.Errorf("ids = %q, want a,b,c", got)
	}
}
