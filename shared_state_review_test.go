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
	failSuffix    string
	applyThenFail int
	mu            sync.Mutex
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
