// This file probes the shared breaker and shared in-flight counter under Yaegi. Two loops stand for two pods over
// one in-memory store, so the interpreter runs the loop, the flush, the refresh and the merge, not only the import.
package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
)

const (
	ssProbeOK = "ok"
	// ssOpenBase, ssOpenMax and ssFailWindow are untyped nanosecond counts, so the interpreted call converts them to time.Duration.
	ssOpenBase   = int64(30 * time.Second)
	ssOpenMax    = int64(5 * time.Minute)
	ssFailWindow = int64(time.Minute)
	// ssMaxRequest gives the in-flight tracker two epochs, so a flush that straddles an epoch edge is still read.
	ssMaxRequest = int64(5 * time.Minute)
)

// ssProbeSource returns "ok" or the first mismatch. PKG stands for the interpreted package's name.
// The loops write synchronously so each refresh reads what the previous step wrote.
const ssProbeSource = `func sharedStateProbe() string {
	store := PKG.newMemoryStore()
	sync := func(f func()) { f() }
	la := PKG.newSharedStateLoop(store, sync, nil)
	lb := PKG.newSharedStateLoop(store, sync, nil)
	defer la.close()
	defer lb.close()

	a := PKG.newSharedBreaker(la, "probe", []string{"p"}, 3, %d, %d, %d)
	b := PKG.newSharedBreaker(lb, "probe", []string{"p"}, 3, %d, %d, %d)
	for i := 0; i < 3; i++ {
		a.recordFailure("p")
	}
	la.refresh()
	lb.refresh()
	if !b.isOpen("p") {
		return "breaker: second instance closed after the first tripped it and one refresh"
	}
	if b.isOpen("other") {
		return "breaker: a trip on one id opened another"
	}
	b.recordSuccess("p")
	lb.refresh()
	if b.isOpen("p") {
		return "breaker: second instance still open after its own success"
	}

	lc := PKG.newSharedStateLoop(store, sync, nil)
	defer lc.close()
	c := PKG.newSharedBreaker(lc, "probe2", []string{"q"}, 3, %d, %d, %d)
	trip := func() {
		for i := 0; i < 3; i++ {
			c.recordFailure("q")
		}
		lc.refresh()
		lc.refresh()
	}
	openFor := func() time.Duration {
		got, _ := store.getMulti([]string{PKG.sharedBreakerKey("probe2", "q", "open")})
		return time.UnixMilli(got[0]).Sub(time.Now())
	}
	trip()
	c.recordSuccess("q")
	lc.refresh()
	trip()
	if d := openFor(); d > time.Duration(%d)+5*time.Second {
		return fmt.Sprintf("breaker: trip after a success opens for %%v, want the base duration", d)
	}

	inner := fmt.Errorf("inner")
	if !PKG.respNotApplied(&PKG.notAppliedError{err: inner}) {
		return "resp: a not-applied error was not recognised"
	}
	if PKG.respNotApplied(inner) {
		return "resp: a plain error was reported as not applied"
	}
	dead := PKG.newRedisStore(PKG.newRESPClient("127.0.0.1:1", "", 0))
	_, deadErr := dead.incrMulti([]PKG.counterIncr{{key: "k", delta: 1, ttl: time.Minute, enforceTTL: time.Minute}})
	if deadErr == nil || !PKG.respNotApplied(deadErr) {
		return fmt.Sprintf("resp: a dial failure through redisStore was not marked not applied: %%v", deadErr)
	}

	logged := 0
	ll := PKG.newSharedStateLoop(store, sync, func(string, ...any) { logged++ })
	for i := 0; i < 3; i++ {
		ll.logErr("store error %%d: %%v", i, inner)
	}
	if logged != 1 {
		return fmt.Sprintf("log: %%d error lines in one interval, want 1", logged)
	}

	ia := PKG.newSharedInflight(la, []string{"p"}, %d)
	ib := PKG.newSharedInflight(lb, []string{"p"}, %d)
	e1 := ia.begin("p")
	e2 := ia.begin("p")
	e3 := ib.begin("p")
	// Each refresh flushes its own deltas then reads, so the first instance needs a second pass to see the other's.
	la.refresh()
	lb.refresh()
	la.refresh()
	if got := ia.shared("p"); got != 3 {
		return fmt.Sprintf("inflight: first instance sees %%d in flight, want 3", got)
	}
	if got := ib.merged("p", 1); got != 3 {
		return fmt.Sprintf("inflight: second instance merged %%d, want 3", got)
	}
	e1()
	e1()
	e2()
	e3()
	la.refresh()
	lb.refresh()
	la.refresh()
	if got := ia.shared("p"); got != 0 {
		return fmt.Sprintf("inflight: %%d still in flight after every end, want 0", got)
	}
	return "ok"
}`

// exerciseSharedState trips a shared breaker through one interpreted instance, reads it from another, and sums in-flight across two.
func exerciseSharedState(i *interp.Interpreter, pkgName string) error {
	src := fmt.Sprintf(strings.ReplaceAll(ssProbeSource, "PKG", pkgName),
		ssOpenBase, ssOpenMax, ssFailWindow, ssOpenBase, ssOpenMax, ssFailWindow,
		ssOpenBase, ssOpenMax, ssFailWindow, ssOpenBase, ssMaxRequest, ssMaxRequest)
	if _, err := i.Eval(`import "fmt"`); err != nil {
		return fmt.Errorf("shared state probe: import fmt under yaegi: %w", err)
	}
	if _, err := i.Eval(`import "time"`); err != nil {
		return fmt.Errorf("shared state probe: import time under yaegi: %w", err)
	}
	if _, err := i.Eval(src); err != nil {
		return fmt.Errorf("shared state probe: declare the probe under yaegi: %w", err)
	}
	v, err := i.Eval("sharedStateProbe()")
	if err != nil {
		return fmt.Errorf("shared state probe: run under yaegi: %w", err)
	}
	if got := v.String(); got != ssProbeOK {
		return fmt.Errorf("shared state probe: %s", got)
	}
	fmt.Println("yaegi-check: shared breaker and shared in-flight matched native Go under the interpreter")
	return nil
}
