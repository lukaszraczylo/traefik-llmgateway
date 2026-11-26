package traefikllmgateway

import (
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
