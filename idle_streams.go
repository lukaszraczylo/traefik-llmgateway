package traefikllmgateway

import (
	"context"
	"sync"
)

// idleStreamSet tracks MCP server-to-client event streams. They may stay
// idle indefinitely, so they run without the progress watchdog, and a host
// that stops or swaps this gateway must end them explicitly. Methods are
// safe on a nil set, which tracks nothing.
type idleStreamSet struct {
	cancels  map[int64]context.CancelFunc
	mu       sync.Mutex
	next     int64
	draining bool
}

func newIdleStreamSet() *idleStreamSet {
	return &idleStreamSet{cancels: make(map[int64]context.CancelFunc)}
}

// add registers cancel and returns its id. It returns false when the set is
// draining; the caller then ends the stream at once.
func (s *idleStreamSet) add(cancel context.CancelFunc) (int64, bool) {
	if s == nil {
		return 0, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return 0, false
	}
	s.next++
	s.cancels[s.next] = cancel
	return s.next, true
}

func (s *idleStreamSet) remove(id int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.cancels, id)
	s.mu.Unlock()
}

// drain ends every tracked stream and every stream added afterwards.
func (s *idleStreamSet) drain() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.draining = true
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for id, cancel := range s.cancels {
		cancels = append(cancels, cancel)
		delete(s.cancels, id)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// DrainIdleStreams ends the MCP server-to-client event streams this gateway
// serves, and any opened afterwards, so their clients reconnect elsewhere.
// The standalone binary calls it when its server shuts down or a config
// reload retires this gateway; those streams have no watchdog to end them.
func (g *Gateway) DrainIdleStreams() {
	g.idleStreams.drain()
}
