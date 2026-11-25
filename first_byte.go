package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// minFirstByteTimeout is the smallest firstByteTimeout a router accepts.
const minFirstByteTimeout = time.Second

const (
	fbArmed int32 = iota
	fbSatisfied
	fbFired
	fbStopped
)

// errFirstByteTimeout is what a watchdog body returns once the first-byte deadline already fired.
var errFirstByteTimeout = errors.New("llmgateway: upstream missed the first-byte deadline")

// firstByteTimeoutError marks an attempt that sent no body byte (no data event for a stream) in time.
// Callers detect it by type assertion, never errors.As (yaegi).
type firstByteTimeoutError struct {
	provider string
	timeout  time.Duration
}

func (e *firstByteTimeoutError) Error() string {
	return fmt.Sprintf("llmgateway: provider %q sent no response data within %s", e.provider, e.timeout)
}

func isFirstByteTimeout(err error) bool {
	_, ok := err.(*firstByteTimeoutError)
	return ok
}

// firstByteGuard cancels one upstream attempt unless satisfy runs before the deadline.
type firstByteGuard struct {
	timer  *time.Timer
	cancel context.CancelFunc
	state  int32
}

func newFirstByteGuard(cancel context.CancelFunc, d time.Duration) *firstByteGuard {
	g := &firstByteGuard{cancel: cancel}
	g.timer = time.AfterFunc(d, g.fire)
	return g
}

func (g *firstByteGuard) fire() {
	if atomic.CompareAndSwapInt32(&g.state, fbArmed, fbFired) {
		g.cancel()
	}
}

// satisfy reports whether the first byte beat the deadline; a nil guard always does.
func (g *firstByteGuard) satisfy() bool {
	if g == nil {
		return true
	}
	if atomic.CompareAndSwapInt32(&g.state, fbArmed, fbSatisfied) {
		g.timer.Stop()
		return true
	}
	return atomic.LoadInt32(&g.state) == fbSatisfied
}

// stop disarms the guard once the attempt ended.
func (g *firstByteGuard) stop() {
	if g == nil {
		return
	}
	if atomic.CompareAndSwapInt32(&g.state, fbArmed, fbStopped) {
		g.timer.Stop()
	}
}

func (g *firstByteGuard) timedOut() bool {
	return g != nil && atomic.LoadInt32(&g.state) == fbFired
}

// firstByteCtxKey differs in size from the other context keys: yaegi compares distinct zero-size
// struct keys as equal.
type firstByteCtxKey struct{ _ int32 }

func withFirstByteGuard(ctx context.Context, g *firstByteGuard) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, firstByteCtxKey{}, g)
}

func firstByteGuardFromContext(ctx context.Context) *firstByteGuard {
	g, _ := ctx.Value(firstByteCtxKey{}).(*firstByteGuard)
	return g
}

const sseDataField = "data:"

// sseDataDetector finds the first "data:" field line in a text/event-stream byte stream, across read
// boundaries. Comment lines and other fields do not count.
type sseDataDetector struct {
	pos int32
	bad bool
}

func (d *sseDataDetector) feed(p []byte) bool {
	for _, b := range p {
		switch {
		case b == '\n' || b == '\r':
			d.pos = 0
			d.bad = false
		case d.bad:
		case d.pos == 0 && b >= 0x80:
		case b == sseDataField[d.pos]:
			d.pos++
			if int(d.pos) == len(sseDataField) {
				return true
			}
		default:
			d.bad = true
		}
	}
	return false
}

// parseFirstByteTimeout validates a router's firstByteTimeout; empty means off.
func parseFirstByteTimeout(router, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("llmgateway: smartRouters: router %q: firstByteTimeout: %w", router, err)
	}
	if d < minFirstByteTimeout {
		return 0, fmt.Errorf("llmgateway: smartRouters: router %q: firstByteTimeout must be at least %s, got %q", router, minFirstByteTimeout, raw)
	}
	return d, nil
}

// routerTargetProviders names the configured providers a tier target resolves to: its "provider/" prefix, or
// every provider that lists the bare id in models or modelBaseUrls. Discovered models and aliases are unknown here.
func routerTargetProviders(cfg *Config, model string) []string {
	if prefix, _, hasSlash := strings.Cut(model, "/"); hasSlash && cfg.Providers[prefix] != nil {
		return []string{prefix}
	}
	var out []string
	for name, pc := range cfg.Providers {
		if pc == nil {
			continue
		}
		_, inBaseURLs := pc.ModelBaseURLs[model]
		listed := inBaseURLs
		for _, id := range pc.Models {
			if id == model {
				listed = true
			}
		}
		if listed {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// validateRouterFirstByte rejects a deadline longer than the requestTimeout of a provider a tier targets.
// Targets whose provider is not known at construction are skipped.
func validateRouterFirstByte(routers map[string]*smartRouter, cfg *Config) error {
	global, err := resolveRequestTimeout(cfg.RequestTimeout, "requestTimeout")
	if err != nil {
		return err
	}
	for _, name := range sortedSmartRouterNames(cfg.SmartRouters) {
		rt := routers[name]
		if rt == nil || rt.firstByte <= 0 {
			continue
		}
		for _, tier := range rt.tiers {
			for _, model := range tier.models {
				for _, provider := range routerTargetProviders(cfg, model) {
					limit, perr := resolveProviderTimeout(provider, cfg.Providers[provider].RequestTimeout, global)
					if perr != nil {
						return perr
					}
					if rt.firstByte > limit {
						return fmt.Errorf("llmgateway: smartRouters: router %q: firstByteTimeout %s exceeds provider %q requestTimeout %s", name, rt.firstByte, provider, limit)
					}
				}
			}
		}
	}
	return nil
}

// routerSlowMessage is the client-facing 504 text when no candidate started responding in time.
const routerSlowMessage = "the router's models did not start responding in time"

// finishFirstByteAttempt ends the guard for one attempt. When the deadline fired before any byte reached the
// client, it returns a *firstByteTimeoutError so the attempt escalates; otherwise it returns callErr as given.
func (g *Gateway) finishFirstByteAttempt(guard *firstByteGuard, cancel context.CancelFunc, ctx context.Context, r *http.Request, sw *statusTrackingWriter, plan *routerPlan, i int, endpoint string, scopes []limitScope, cand resolveCandidate, callErr error) error {
	guard.stop()
	cancel()
	if !guard.timedOut() || callErr == nil || sw.wroteHeader || r.Context().Err() != nil {
		return callErr
	}
	slow := &firstByteTimeoutError{provider: cand.providerName, timeout: plan.firstByte}
	plan.slowSeen = true
	if rec := attemptRecorderFromContext(ctx); rec != nil {
		rec(nil, fmt.Errorf("%w: %w", errProviderTimeout, slow))
	}
	g.recordRouterEvent(plan, i, endpoint, scopes, fmt.Sprintf("router %q: model %q sent no response data within %s", plan.router, cand.canonical, plan.firstByte))
	return slow
}
