package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	traefikllmgateway "github.com/lukaszraczylo/traefik-llmgateway"
)

// genRetired is the retired flag in a generation's state word. The bits below it count the requests running on it.
// The flag and the count share one atomic so that leave sees both in one operation (formal/Gateway/Reload.lean).
const genRetired int64 = 1 << 62

// generation is one constructed gateway and the requests running on it.
type generation struct {
	h       http.Handler
	closeFn func()
	state   atomic.Int64
	once    sync.Once
}

func newGeneration(h http.Handler, closeFn func()) *generation {
	return &generation{h: h, closeFn: closeFn}
}

// enter counts a request in. It returns false when the generation is retired; the caller must not serve on it.
func (g *generation) enter() bool {
	if g.state.Add(1)&genRetired != 0 {
		g.leave()
		return false
	}
	return true
}

func (g *generation) leave() {
	if g.state.Add(-1) == genRetired {
		g.shut()
	}
}

// retire stops new requests. The gateway closes when the last running request leaves, or after maxWait.
func (g *generation) retire(maxWait time.Duration) {
	for {
		old := g.state.Load()
		if old&genRetired != 0 {
			return
		}
		if g.state.CompareAndSwap(old, old|genRetired) {
			if old == 0 {
				g.shut()
				return
			}
			break
		}
	}
	time.AfterFunc(maxWait, g.shut)
}

func (g *generation) shut() { g.once.Do(g.closeFn) }

// swapHandler serves every request on the current generation.
type swapHandler struct {
	cur atomic.Pointer[generation]
}

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for {
		g := s.cur.Load()
		// A false enter means a swap already installed a newer generation, so the next load finds it.
		if g.enter() {
			defer g.leave()
			g.h.ServeHTTP(w, r)
			return
		}
	}
}

// swap installs next and retires the generation it replaces.
func (s *swapHandler) swap(next *generation, drain time.Duration) {
	s.cur.Swap(next).retire(drain)
}

// closeCurrent closes the serving generation. Call it only after the server stopped serving.
func (s *swapHandler) closeCurrent() {
	if g := s.cur.Load(); g != nil {
		g.shut()
	}
}

// gatewayBuilder decodes raw as the config at path and constructs a gateway from it.
func gatewayBuilder(ctx context.Context, path, name string, log *logger) func(raw []byte) (http.Handler, error) {
	return func(raw []byte) (http.Handler, error) {
		cfg, err := decodeConfig(path, raw)
		if err != nil {
			return nil, err
		}
		logConfigWarnings(cfg, log)
		h, err := traefikllmgateway.New(ctx, terminalHandler(), cfg, name)
		if err != nil {
			return nil, fmt.Errorf("construct gateway: %w", err)
		}
		return h, nil
	}
}

// reloader rebuilds the gateway when the config file content changes. Only its run goroutine calls check.
type reloader struct {
	build   func(raw []byte) (http.Handler, error)
	swap    *swapHandler
	log     *logger
	path    string
	lastErr string
	drain   time.Duration
	last    [sha256.Size]byte
}

// newReloader starts from the content the serving gateway was built from.
func newReloader(path string, raw []byte, build func([]byte) (http.Handler, error), swap *swapHandler, drain time.Duration, log *logger) *reloader {
	return &reloader{build: build, swap: swap, log: log, path: path, drain: drain, last: sha256.Sum256(raw)}
}

// check rebuilds when the file content differs from the last attempt, or always when force is set.
// A rejected config keeps the old gateway serving and is not retried until the file changes again.
func (r *reloader) check(force bool) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		r.logOnce(fmt.Sprintf("reload: read %s: %v; still serving the previous config", r.path, err))
		return
	}
	sum := sha256.Sum256(raw)
	if sum == r.last && !force {
		return
	}
	r.last = sum
	h, err := r.build(raw)
	r.lastErr = ""
	if err != nil {
		r.log.error("reload: config %s rejected; still serving the previous config: %v", r.path, err)
		return
	}
	r.swap.swap(newGeneration(h, func() { closeGateway(h, r.log) }), r.drain)
	r.log.info("reload: config %s applied", r.path)
}

// logOnce logs msg unless it repeats the previous error, so a missing file does not log every interval.
func (r *reloader) logOnce(msg string) {
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	r.log.error("%s", msg)
}

// run checks the file every interval (never when interval is 0) and on every value from hup, until ctx ends.
func (r *reloader) run(ctx context.Context, interval time.Duration, hup <-chan os.Signal) {
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			r.check(false)
		case <-hup:
			r.log.info("reload: SIGHUP, rebuilding from %s", r.path)
			r.check(true)
		}
	}
}
