package traefikllmgateway

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSharedBreakerSuccessResetsBackoff(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	b := testBreaker(l)
	openKey := sharedBreakerKey("req", "p", "open")
	openFor := func() time.Duration {
		got, _ := store.getMulti([]string{openKey})
		return time.UnixMilli(got[0]).Sub(clk.now())
	}
	trip := func() {
		for i := 0; i < 3; i++ {
			b.recordFailure("p")
		}
		l.refresh()
		l.refresh()
	}
	trip()
	if got := openFor(); got != testBrkOpenBase {
		t.Fatalf("first trip open for %v, want %v", got, testBrkOpenBase)
	}
	clk.advance(testBrkOpenBase + time.Second)
	b.recordSuccess("p")
	l.refresh()
	trip()
	if got := openFor(); got != testBrkOpenBase {
		t.Fatalf("trip after a success open for %v, want %v (backoff must reset)", got, testBrkOpenBase)
	}
}

// fussyStore applies a write and still fails it, short-changes the reply, or fails writes to one key suffix.
type fussyStore struct {
	counterStore
	incrErr       error
	failSuffix    string
	applyThenFail int
	mu            sync.Mutex
	incrApply     bool
	shortReply    bool
}

func (f *fussyStore) incrAndGetMulti(e []counterIncr, reads []string) ([]int64, []int64, error) {
	f.mu.Lock()
	apply, short, suffix := f.applyThenFail > 0, f.shortReply, f.failSuffix
	if apply {
		f.applyThenFail--
	}
	f.mu.Unlock()
	if suffix != "" && strings.HasSuffix(e[0].key, suffix) {
		return nil, nil, errors.New("opens write failed")
	}
	post, got, err := f.counterStore.incrAndGetMulti(e, reads)
	if err != nil {
		return nil, nil, err
	}
	if apply {
		return nil, nil, errors.New("reply lost")
	}
	if short {
		return post[:len(post)-1], got, nil
	}
	return post, got, nil
}

func fussyEnv() (*fussyStore, *sharedClock, *sharedStateLoop, *sharedBreaker) {
	clk := &sharedClock{t: time.Unix(1_800_000_000, 0)}
	mem := newMemoryStore()
	mem.nowFn = clk.now
	st := &fussyStore{counterStore: mem}
	l := sharedTestLoop(st, clk)
	return st, clk, l, testBreaker(l)
}

func openUntilOf(t *testing.T, st counterStore, id string) int64 {
	t.Helper()
	got, err := st.getMulti([]string{sharedBreakerKey("req", id, "open")})
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

func TestSharedBreakerTripsAfterAppliedButFailedIncr(t *testing.T) {
	st, _, l, b := fussyEnv()
	b.recordFailure("p")
	b.recordFailure("p")
	l.refresh()
	b.recordFailure("p")
	st.mu.Lock()
	st.applyThenFail = 1
	st.mu.Unlock()
	l.refresh() // INCR applied (count 3), reply lost: delta retained
	if openUntilOf(t, st, "p") != 0 {
		t.Fatal("tripped before any flush saw the reply")
	}
	l.refresh() // resend: count 4, post-delta is already past the threshold
	if openUntilOf(t, st, "p") == 0 {
		t.Fatal("fleet never tripped after an applied-but-failed incr")
	}
}

func TestSharedBreakerRetriesTripAfterOpensIncrFailure(t *testing.T) {
	st, _, l, b := fussyEnv()
	st.mu.Lock()
	st.failSuffix = ":opens"
	st.mu.Unlock()
	for i := 0; i < 3; i++ {
		b.recordFailure("p")
	}
	l.refresh()
	if openUntilOf(t, st, "p") != 0 {
		t.Fatal("tripped although the opens incr failed")
	}
	st.mu.Lock()
	st.failSuffix = ""
	st.mu.Unlock()
	b.recordFailure("p")
	l.refresh()
	if openUntilOf(t, st, "p") == 0 {
		t.Fatal("fleet never tripped after the opens incr failed once")
	}
}

func TestSharedBreakerNoRetripWhileOpen(t *testing.T) {
	st, _, l, b := fussyEnv()
	for i := 0; i < 3; i++ {
		b.recordFailure("p")
	}
	l.refresh()
	l.refresh() // the loop now sees the open mark
	for i := 0; i < 5; i++ {
		b.recordFailure("p")
		l.refresh()
	}
	got, _ := st.getMulti([]string{sharedBreakerKey("req", "p", "opens")})
	if got[0] != 1 {
		t.Errorf("opens = %d after failures during the open window, want 1", got[0])
	}
}

func TestSharedBreakerShortReplyKeepsDeltas(t *testing.T) {
	st, _, l, b := fussyEnv()
	st.mu.Lock()
	st.shortReply = true
	st.mu.Unlock()
	b.recordFailure("p")
	err := b.flush(l, l.nowFn())
	if err == nil {
		t.Fatal("flush with a short reply returned nil, want an error")
	}
	if !b.pending() {
		t.Fatal("deltas dropped after a short reply")
	}
}

func (f *fussyStore) incrMulti(e []counterIncr) ([]int64, error) {
	f.mu.Lock()
	err, apply := f.incrErr, f.incrApply
	f.mu.Unlock()
	if err == nil {
		return f.counterStore.incrMulti(e)
	}
	if apply {
		_, _ = f.counterStore.incrMulti(e)
	}
	return nil, err
}

func (f *fussyStore) failIncr(err error, applied bool) {
	f.mu.Lock()
	f.incrErr, f.incrApply = err, applied
	f.mu.Unlock()
}

func startedTotal(t *testing.T, st counterStore, clk *sharedClock, epochs int) int64 {
	t.Helper()
	in := newSharedInflight(nil, []string{"p"}, time.Minute)
	var keys []string
	for i := 0; i < epochs; i++ {
		keys = append(keys, in.epochKey("p", in.epochOf(clk.now())-int64(i), "s"))
	}
	got, err := st.getMulti(keys)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, v := range got {
		sum += v
	}
	return sum
}

func TestSharedInflightAmbiguousFailureResendsOnce(t *testing.T) {
	st, clk, _, _ := fussyEnv()
	l := sharedTestLoop(st, clk)
	in := newSharedInflight(l, []string{"p"}, time.Minute)
	for i := 0; i < 3; i++ {
		in.begin("p")
	}
	st.failIncr(errors.New("reply lost"), true)
	l.refresh() // applied, reply lost
	l.refresh() // resent once, applied again, reply lost: dropped now
	st.failIncr(nil, false)
	l.refresh()
	l.refresh()
	if in.pending() {
		t.Error("deltas still queued after two ambiguous failures")
	}
	if got := startedTotal(t, st, clk, in.epochs); got > 6 {
		t.Errorf("started total = %d for 3 begins, want at most 2x one batch (6)", got)
	}
}

func TestSharedInflightNotWrittenFailureKeepsExactTotal(t *testing.T) {
	st, clk, _, _ := fussyEnv()
	l := sharedTestLoop(st, clk)
	in := newSharedInflight(l, []string{"p"}, time.Minute)
	st.failIncr(&notAppliedError{err: errors.New("dial refused")}, false)
	for i := 0; i < 4; i++ {
		in.begin("p")
		l.refresh()
	}
	if !in.pending() {
		t.Fatal("deltas dropped after failures that never wrote")
	}
	st.failIncr(nil, false)
	l.refresh()
	if got := startedTotal(t, st, clk, in.epochs); got != 4 {
		t.Errorf("started total = %d, want exactly 4", got)
	}
	if in.pending() {
		t.Error("deltas queued after the successful flush")
	}
}

func TestSharedInflightNewDeltasSurviveAfterAmbiguousDrop(t *testing.T) {
	st, clk, _, _ := fussyEnv()
	l := sharedTestLoop(st, clk)
	in := newSharedInflight(l, []string{"p"}, time.Minute)
	in.begin("p")
	st.failIncr(errors.New("reply lost"), false)
	l.refresh()
	in.begin("p") // new delta lands on the once-sent batch
	l.refresh()   // batch of 1 dropped, new delta kept
	st.failIncr(nil, false)
	l.refresh()
	if got := startedTotal(t, st, clk, in.epochs); got != 1 {
		t.Errorf("started total = %d, want 1 (the dropped batch is gone, the new delta lands once)", got)
	}
}

func waitLoop(t *testing.T, l *sharedStateLoop, running bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		got := l.running
		l.mu.Unlock()
		if got == running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("loop running never became %v", running)
}

func TestSharedInflightEndRestartsIdleStoppedLoop(t *testing.T) {
	store, clk := sharedTestEnv()
	l := newSharedStateLoop(store, nil, nil)
	l.nowFn = clk.now
	l.every = 5 * time.Millisecond
	in := newSharedInflight(l, []string{"p"}, 30*time.Minute)
	end := in.begin("p")
	waitLoop(t, l, true)
	clk.advance(sharedStateIdleStop + time.Second)
	waitLoop(t, l, false)
	end()
	waitLoop(t, l, true)
	deadline := time.Now().Add(2 * time.Second)
	for store.writesTo(":f") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.writesTo(":f") != 1 {
		t.Error("finish delta stranded after the loop idle-stopped")
	}
	l.close()
}

func TestSharedBreakerDropsDeltasPastFailWindow(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	b := testBreaker(l)
	store.setFailIncr(errors.New("redis down"))
	b.recordFailure("p")
	l.refresh()
	if !b.pending() {
		t.Fatal("delta dropped inside the fail window")
	}
	clk.advance(testBrkWindow + time.Second)
	if b.pending() {
		t.Error("failure delta older than the fail window still pending")
	}
}

func TestSharedStateIdleStopsDuringOutageAfterFailWindow(t *testing.T) {
	store, clk := sharedTestEnv()
	l := newSharedStateLoop(store, nil, nil)
	l.nowFn = clk.now
	l.every = 5 * time.Millisecond
	b := testBreaker(l)
	store.setFailIncr(errors.New("redis down"))
	b.recordFailure("p")
	waitLoop(t, l, true)
	clk.advance(sharedStateIdleStop + time.Second)
	waitLoop(t, l, false)
	l.close()
}

func TestSharedStateErrorLogsAreRateLimited(t *testing.T) {
	store, clk := sharedTestEnv()
	var mu sync.Mutex
	var lines []string
	l := newSharedStateLoop(store, nil, func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, f)
		mu.Unlock()
	})
	l.nowFn = clk.now
	l.closed = true
	l.lossy = func(f func()) { f() }
	b := testBreaker(l)
	store.setFail(errors.New("redis down"))
	store.setFailIncr(errors.New("redis down"))
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(lines)
	}
	for i := 0; i < 50; i++ {
		b.recordFailure("p")
		l.refresh()
		l.write([]counterIncr{{key: "k", delta: 1, ttl: time.Minute}})
	}
	if got := count(); got != 1 {
		t.Fatalf("%d error lines inside one interval, want 1", got)
	}
	clk.advance(sharedStateErrorLogEvery + time.Second)
	b.recordFailure("p")
	l.refresh()
	if got := count(); got != 2 {
		t.Errorf("%d error lines after the interval, want 2", got)
	}
}
