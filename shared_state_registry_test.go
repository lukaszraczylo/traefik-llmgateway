package traefikllmgateway

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSharedDiscoveryBreaker(t *testing.T) {
	a, b, _, clk, _ := sharedGatewayPair(t, nil)
	boom := errors.New("discovery down")
	now := time.Now()
	for i := 0; i < defaultBreakerFailureThreshold; i++ {
		a.registry.finishRefresh(a.registry.states["alpha"], now, nil, boom)
	}
	if a.registry.discoveryHealthy("alpha") {
		t.Fatal("A healthy after threshold failures, local breaker must open")
	}
	if !b.registry.discoveryHealthy("alpha") {
		t.Fatal("B unhealthy before any refresh, discoveryHealthy must not read the store")
	}
	known := b.registry.states["alpha"].knownIDs()
	a.sharedState.refresh()
	b.sharedState.refresh()
	if b.registry.discoveryHealthy("alpha") {
		t.Fatal("B healthy after A tripped the discovery breaker and one refresh")
	}
	if !b.registry.discoveryHealthy("beta") {
		t.Fatal("B beta unhealthy, a trip on alpha must not leak")
	}
	if got := b.registry.states["alpha"].knownIDs(); !reflect.DeepEqual(got, known) {
		t.Fatalf("B model set changed by a shared trip: %v, want %v", got, known)
	}

	// A discovery success on B closes the fleet breaker.
	b.registry.finishRefresh(b.registry.states["alpha"], now, []string{"a1"}, nil)
	b.sharedState.refresh()
	if !b.registry.discoveryHealthy("alpha") {
		t.Fatal("B unhealthy after its own successful discovery")
	}

	// The open window ends on its own.
	for i := 0; i < defaultBreakerFailureThreshold; i++ {
		a.registry.finishRefresh(a.registry.states["beta"], now, nil, boom)
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	if b.registry.discoveryHealthy("beta") {
		t.Fatal("B beta healthy right after the trip")
	}
	clk.advance(defaultBreakerOpenDuration + time.Second)
	if !b.registry.discoveryHealthy("beta") {
		t.Fatal("B beta unhealthy after the open window elapsed")
	}
}

func TestSharedStateOffIdenticalDiscoveryBreaker(t *testing.T) {
	gw, _ := sharedOffGateway(t)
	rec, _ := sharedTestEnv()
	gw.limiter.store = rec
	st := gw.registry.states["alpha"]
	ref := newHealthTestState(st.interval, defaultBreakerFailureThreshold, defaultBreakerOpenDuration, defaultBreakerMaxOpenDuration)
	boom := errors.New("boom")
	now := time.Now()
	steps := []error{boom, boom, boom, boom, nil, boom}
	for i, err := range steps {
		ids := []string(nil)
		if err == nil {
			ids = []string{"a1"}
		}
		st.finishRefresh(now, ids, err)
		ref.finishRefresh(now, ids, err)
		if st.discoveryHealthy() != ref.discoveryHealthy() || st.health != ref.health || st.consecutiveFailures != ref.consecutiveFailures || st.backoff != ref.backoff {
			t.Fatalf("step %d: diverged from the local-only state machine", i)
		}
	}
	if len(rec.reads) != 0 || len(rec.writes) != 0 {
		t.Fatalf("store touched with sharedState off: %d reads, %d writes", len(rec.reads), len(rec.writes))
	}
}

// One failed round on every instance must not trip the fleet breaker: only an instance whose own consecutive
// failures reach the threshold trips it (formal SharedState local_gate_no_trip_before_t, local_gate_trips_at_t).
func TestSharedDiscoveryBreakerCountsPerInstance(t *testing.T) {
	a, b, _, _, _ := sharedGatewayPair(t, nil)
	boom := errors.New("discovery down")
	now := time.Now()
	for i := 0; i < defaultBreakerFailureThreshold-1; i++ {
		a.registry.finishRefresh(a.registry.states["alpha"], now, nil, boom)
		b.registry.finishRefresh(b.registry.states["alpha"], now, nil, boom)
	}
	a.sharedState.refresh()
	b.sharedState.refresh()
	a.sharedState.refresh()
	b.sharedState.refresh()
	if !a.registry.discoveryHealthy("alpha") || !b.registry.discoveryHealthy("alpha") {
		t.Fatalf("fleet breaker open after %d failures per instance, below the threshold of %d on each", defaultBreakerFailureThreshold-1, defaultBreakerFailureThreshold)
	}
	a.registry.finishRefresh(a.registry.states["alpha"], now, nil, boom)
	a.sharedState.refresh()
	b.sharedState.refresh()
	if b.registry.discoveryHealthy("alpha") {
		t.Fatal("B healthy after A reached the threshold on its own")
	}
}
