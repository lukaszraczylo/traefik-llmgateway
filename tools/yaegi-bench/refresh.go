package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

const (
	refreshIDs        = 19
	refreshCoresPerGW = 4.0 // gateways per pod, each refreshing once a second
	refreshTarget     = 400 * time.Millisecond
)

// refreshHook is the interpreted-side half of -mode refresh: it wires the loop as wireSharedState does.
const refreshHook = `package %s

import (
	"strconv"
	"time"
)

var (
	benchLoop *sharedStateLoop
	benchAll  []string
	benchVals []int64
	benchStore counterStore
	benchBig   []string
)

func BenchRefreshSetup(redisAddr string, n int) int {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "provider-" + strconv.Itoa(i)
	}
	var store counterStore
	if redisAddr == "" {
		store = newMemoryStore()
	} else {
		store = newRedisStore(newRESPClient(redisAddr, "", 0))
	}
	l := newSharedStateLoop(store, func(f func()) {}, nil)
	newSharedBreaker(l, sharedKindRequest, ids, 5, time.Second, time.Minute, 2*time.Minute)
	newSharedInflight(l, ids, 15*time.Minute)
	newSharedBreaker(l, sharedKindDiscovery, ids, 5, time.Second, time.Minute, 2*time.Minute)
	newSharedBreaker(l, sharedKindDecision, ids, 5, time.Second, time.Minute, 2*time.Minute)
	benchLoop = l
	benchStore = store
	benchBig = make([]string, usageModelsChunkKeys)
	bigEntries := make([]counterIncr, len(benchBig))
	for i := range benchBig {
		benchBig[i] = "bench-big-" + strconv.Itoa(i)
		bigEntries[i] = counterIncr{key: benchBig[i], delta: int64(i%%5 + 1), ttl: time.Hour, enforceTTL: time.Hour}
	}
	if _, err := store.incrMulti(bigEntries); err != nil {
		panic(err)
	}
	now := time.Now()
	benchAll = nil
	for _, p := range l.snapshot() {
		benchAll = append(benchAll, p.keys(now)...)
	}
	entries := make([]counterIncr, len(benchAll))
	for i, k := range benchAll {
		entries[i] = counterIncr{key: k, delta: int64(i%%5 + 1), ttl: time.Hour, enforceTTL: time.Hour}
	}
	if _, err := store.incrMulti(entries); err != nil {
		panic(err)
	}
	vals, ok := l.chunkedGet(benchAll)
	if !ok {
		panic("chunkedGet failed")
	}
	benchVals = vals
	return len(benchAll)
}

// BenchRefreshCheck returns the refresh failure count and the sum of the stored values.
func BenchRefreshCheck() (int, int) {
	benchLoop.refresh()
	sum := 0
	for _, v := range benchVals {
		sum += int(v)
	}
	return benchLoop.failures, sum
}

func BenchRefreshRun(n int) {
	for i := 0; i < n; i++ {
		benchLoop.refresh()
	}
}

func BenchFlushRun(n int) {
	for i := 0; i < n; i++ {
		benchLoop.flush()
	}
}

func BenchKeysRun(n int) int {
	sum := 0
	for i := 0; i < n; i++ {
		now := benchLoop.nowFn()
		for _, p := range benchLoop.snapshot() {
			sum += len(p.keys(now))
		}
	}
	return sum
}

func BenchGetMultiRun(n int) int {
	sum := 0
	for i := 0; i < n; i++ {
		got, _ := benchLoop.chunkedGet(benchAll)
		sum += len(got)
	}
	return sum
}

func BenchGetMulti1600Run(n int) int {
	sum := 0
	for i := 0; i < n; i++ {
		got, _ := benchStore.getMulti(benchBig)
		sum += len(got)
	}
	return sum
}

func BenchApplyRun(n int) {
	prims := benchLoop.snapshot()
	sizes := make([]int, len(prims))
	now := benchLoop.nowFn()
	for i, p := range prims {
		sizes[i] = len(p.keys(now))
	}
	for i := 0; i < n; i++ {
		pos := 0
		for j, p := range prims {
			p.apply(benchVals[pos : pos+sizes[j]])
			pos += sizes[j]
		}
	}
}
`

const refreshNativeTest = `package %s

import (
	"os"
	"testing"
)

func benchOps(b *testing.B, addr string) {
	ops := []struct {
		name string
		run  func(int)
	}{
		{"refresh", BenchRefreshRun},
		{"keys", func(n int) { BenchKeysRun(n) }},
		{"getmulti", func(n int) { BenchGetMultiRun(n) }},
		{"apply", BenchApplyRun},
		{"flush", BenchFlushRun},
	}
	for _, op := range ops {
		b.Run(op.name, func(b *testing.B) {
			BenchRefreshSetup(addr, 19)
			b.ReportAllocs()
			b.ResetTimer()
			op.run(b.N)
		})
	}
}

func BenchmarkRefreshMem(b *testing.B) { benchOps(b, "") }

func BenchmarkRefreshRedis(b *testing.B) { benchOps(b, os.Getenv("BENCH_REDIS")) }
`

type sample struct{ ns, bytes, allocs float64 }

func median(xs []float64) (med, lo, hi float64) {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2], s[0], s[len(s)-1]
}

func summarize(label string, ss []sample) {
	col := func(f func(sample) float64) []float64 {
		out := make([]float64, len(ss))
		for i, s := range ss {
			out[i] = f(s)
		}
		return out
	}
	ns, nlo, nhi := median(col(func(s sample) float64 { return s.ns }))
	by, _, _ := median(col(func(s sample) float64 { return s.bytes }))
	al, _, _ := median(col(func(s sample) float64 { return s.allocs }))
	fmt.Printf("%-24s n=%d  median %12.0f ns/op  [%.0f..%.0f]  %9.0f B/op  %6.0f allocs/op  = %.4f cores/pod\n",
		label, len(ss), ns, nlo, nhi, by, al, ns*refreshCoresPerGW/1e9)
}

// copyPlugin copies the plugin into dir/src/<module> and writes the hook.
func copyPlugin(root, gopath string) (pkgDir, pkgName, modulePath string, err error) {
	modulePath, err = readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return
	}
	pkgDir = filepath.Join(gopath, "src", modulePath)
	if err = copyTree(root, pkgDir); err != nil {
		return
	}
	if pkgName, err = readPackageName(pkgDir); err != nil {
		return
	}
	err = os.WriteFile(filepath.Join(pkgDir, "zz_refresh_hook.go"), []byte(fmt.Sprintf(refreshHook, pkgName)), 0o600)
	return
}

func runRefresh(root, redisAddr string, samples int) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	gopath, err := os.MkdirTemp("", "llmgw-refresh-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // temp dir
	_, pkgName, modulePath, err := copyPlugin(root, gopath)
	if err != nil {
		return err
	}
	i := interp.New(interp.Options{GoPath: gopath})
	if err = i.Use(stdlib.Symbols); err != nil {
		return err
	}
	if _, err = i.Eval(`import "` + modulePath + `"`); err != nil {
		return fmt.Errorf("import plugin under yaegi: %w", err)
	}
	get := func(name string) (any, error) {
		v, e := i.Eval(pkgName + "." + name)
		if e != nil {
			return nil, e
		}
		return v.Interface(), nil
	}
	setupA, err := get("BenchRefreshSetup")
	if err != nil {
		return err
	}
	setup := setupA.(func(string, int) int)
	checkA, _ := get("BenchRefreshCheck")
	check := checkA.(func() (int, int))
	ops := []struct {
		run  func(int)
		name string
	}{}
	add := func(name string, fn func(int)) {
		ops = append(ops, struct {
			run  func(int)
			name string
		}{fn, name})
	}
	for _, o := range []struct{ name, sym string }{{"refresh", "BenchRefreshRun"}, {"apply", "BenchApplyRun"}, {"flush", "BenchFlushRun"}} {
		f, e := get(o.sym)
		if e != nil {
			return e
		}
		add(o.name, f.(func(int)))
	}
	for _, o := range []struct{ name, sym string }{{"keys", "BenchKeysRun"}, {"getmulti", "BenchGetMultiRun"}, {"getmulti1600", "BenchGetMulti1600Run"}} {
		f, e := get(o.sym)
		if e != nil {
			return e
		}
		fn := f.(func(int) int)
		add(o.name, func(n int) { fn(n) })
	}
	stores := []string{"mem"}
	if redisAddr != "" {
		stores = append(stores, "redis")
	}
	for _, st := range stores {
		addr := ""
		if st == "redis" {
			addr = redisAddr
		}
		nkeys := setup(addr, refreshIDs)
		fails, sum := check()
		fmt.Printf("yaegi %s: %d keys, refresh failures=%d, value sum=%d\n", st, nkeys, fails, sum)
		for _, op := range ops {
			op.run(2)
			n := 1
			for {
				t := time.Now()
				op.run(n)
				d := time.Since(t)
				if d >= refreshTarget || n >= 1<<22 {
					break
				}
				next := int(float64(n) * float64(refreshTarget) / float64(d+1) * 1.2)
				if next <= n {
					next = n * 2
				}
				n = next
			}
			var ss []sample
			for s := 0; s < samples; s++ {
				var m0, m1 runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&m0)
				t := time.Now()
				op.run(n)
				d := time.Since(t)
				runtime.ReadMemStats(&m1)
				ss = append(ss, sample{
					ns:     float64(d.Nanoseconds()) / float64(n),
					bytes:  float64(m1.TotalAlloc-m0.TotalAlloc) / float64(n),
					allocs: float64(m1.Mallocs-m0.Mallocs) / float64(n),
				})
			}
			summarize("yaegi "+st+" "+op.name, ss)
		}
	}
	return nil
}

var benchLine = regexp.MustCompile(`^Benchmark(Refresh(?:Mem|Redis))/(\w+)\S*\s+\d+\s+([\d.]+) ns/op\s+([\d.]+) B/op\s+([\d.]+) allocs/op`)

func runRefreshNative(root, redisAddr string, samples int) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	gopath, err := os.MkdirTemp("", "llmgw-refresh-native-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // temp dir
	pkgDir, pkgName, _, err := copyPlugin(root, gopath)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(pkgDir, "zz_refresh_bench_test.go"), []byte(fmt.Sprintf(refreshNativeTest, pkgName)), 0o600); err != nil {
		return err
	}
	pat := "RefreshMem"
	if redisAddr != "" {
		pat = "Refresh(Mem|Redis)"
	}
	cmd := exec.Command("go", "test", "-run", "^$", "-bench", pat, "-benchmem", "-benchtime", "300ms", "-count", strconv.Itoa(samples), ".") //nolint:gosec // G204: fixed binary, pattern from two constants, count is an int
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "BENCH_REDIS="+redisAddr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("native bench: %w\n%s", err, out)
	}
	groups := map[string][]sample{}
	var order []string
	for _, line := range strings.Split(string(out), "\n") {
		m := benchLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := "native " + strings.ToLower(strings.TrimPrefix(m[1], "Refresh")) + " " + m[2]
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		ns, _ := strconv.ParseFloat(m[3], 64)
		by, _ := strconv.ParseFloat(m[4], 64)
		al, _ := strconv.ParseFloat(m[5], 64)
		groups[key] = append(groups[key], sample{ns, by, al})
	}
	if len(order) == 0 {
		return fmt.Errorf("native bench produced no results:\n%s", out)
	}
	for _, k := range order {
		summarize(k, groups[k])
	}
	return nil
}
