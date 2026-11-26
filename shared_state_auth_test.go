package traefikllmgateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

const testAuthIP = "203.0.113.9"

func failAuth(t *testing.T, gw *Gateway, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = testAuthIP + ":4242"
		r.Header.Set("Authorization", "Bearer not-a-key")
		if _, _, ok := gw.auth.identify(r); ok {
			t.Fatal("identify accepted an unknown key")
		}
	}
}

func TestSharedAuthFailuresThrottleFleetWide(t *testing.T) {
	a, b, _, _, _ := sharedGatewayPair(t, nil)
	failAuth(t, a, authFailureLimit-5)
	failAuth(t, b, 5)
	if a.auth.authThrottled(testAuthIP) || b.auth.authThrottled(testAuthIP) {
		t.Fatal("throttled before any flush, want each pod below the limit on its own")
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	if !b.auth.authThrottled(testAuthIP) {
		t.Fatalf("B not throttled after both pods flushed %d failures", authFailureLimit)
	}
	// A learns the fleet count only from the reply to its own next flush.
	failAuth(t, a, 1)
	a.sharedState.refresh()
	if !a.auth.authThrottled(testAuthIP) {
		t.Fatal("A not throttled after flushing a new failure, want the fleet count from its reply")
	}
	if b.auth.authThrottled("198.51.100.5") {
		t.Fatal("an IP with no failures reads throttled")
	}
}

func TestSharedAuthFailuresOffKeepsLocalOnly(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	if gw.auth.shared != nil {
		t.Fatal("fleet auth arm built with shared state off")
	}
	failAuth(t, gw, authFailureLimit)
	if !gw.auth.authThrottled(testAuthIP) {
		t.Fatal("local tracker no longer throttles with shared state off")
	}
}

func TestSharedAuthFailuresStoreDownKeepsQueued(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	s := newSharedAuthFailures(l)
	for i := 0; i < 3; i++ {
		s.record(testAuthIP)
	}
	store.setFailIncr(errors.New("redis down"))
	l.flush()
	if got := s.count(testAuthIP); got != 3 {
		t.Fatalf("count after failed flush = %d, want the 3 queued", got)
	}
	store.setFailIncr(nil)
	l.flush()
	l.flush()
	if got := s.count(testAuthIP); got != 3 {
		t.Fatalf("count after recovery = %d, want 3 (sent once)", got)
	}
	if s.pending() {
		t.Fatal("failures still queued after a successful flush")
	}
	got, err := store.getMulti([]string{sharedAuthKey(authWindow(clk.now()), testAuthIP)})
	if err != nil || got[0] != 3 {
		t.Fatalf("store count = %v (%v), want 3", got, err)
	}
}

func TestSharedAuthFailuresWindowRoll(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	s := newSharedAuthFailures(l)
	s.record(testAuthIP)
	l.flush()
	s.record(testAuthIP)
	clk.advance(authFailureWindow)
	if got := s.count(testAuthIP); got != 0 {
		t.Fatalf("count in the next window = %d, want 0", got)
	}
	if s.pending() {
		t.Fatal("a failure from the last window is still queued")
	}
}

func TestSharedAuthFailuresQueueCap(t *testing.T) {
	store, clk := sharedTestEnv()
	l := sharedTestLoop(store, clk)
	s := newSharedAuthFailures(l)
	for i := 0; i < sharedAuthMaxQueued; i++ {
		s.record("10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256))
	}
	s.record(testAuthIP)
	s.record("10.0.0.0")
	if got := s.count(testAuthIP); got != 0 {
		t.Fatalf("IP past the cap counted %d, want 0", got)
	}
	if got := s.count("10.0.0.0"); got != 2 {
		t.Fatalf("queued IP count = %d, want 2", got)
	}
	l.flush()
	if got := len(store.writes); got != sharedAuthMaxQueued {
		t.Fatalf("flush wrote %d keys, want %d", got, sharedAuthMaxQueued)
	}
	if got := s.count("10.0.0.0"); got != 2 {
		t.Fatalf("count after chunked flush = %d, want 2", got)
	}
}
