// Command yaegi-bench measures smart router classification throughput under
// the Yaegi interpreter, the way Traefik loads the plugin. It copies the
// plugin source into a temporary GOPATH, adds a small exported hook file that
// loads the router fixtures and runs classifySmart in a loop, then times that
// loop from outside the interpreter.
//
// The module is separate from the plugin (like tools/yaegi-check) so Yaegi
// never becomes a plugin dependency. Run it with: make yaegi-bench
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// hookSource is written into the interpreted copy of the plugin package. It
// returns a rank checksum so a loop that skips the work cannot look fast.
const hookSource = `package %s

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type benchFixture struct {
	ID      string         ` + "`json:\"id\"`" + `
	Request map[string]any ` + "`json:\"request\"`" + `
	Wire    string         ` + "`json:\"wire\"`" + `
}

var (
	benchFixtures []benchFixture
	benchRouter   *smartRouter
	benchSessions *routerSessions
)

func BenchSetup(dir string, classifier string, mid int, top int) int {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	benchFixtures = nil
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			panic(err)
		}
		var items []benchFixture
		if err := json.Unmarshal(raw, &items); err != nil {
			panic(err)
		}
		benchFixtures = append(benchFixtures, items...)
	}
	cfg := &SmartRouterConfig{Classifier: classifier, Tiers: []SmartRouterTier{
		{Name: "a", MinScore: 0, Models: []string{"p/a"}},
		{Name: "b", MinScore: mid, Models: []string{"p/b"}},
		{Name: "c", MinScore: top, Models: []string{"p/c"}},
	}}
	benchSessions = nil
	if classifier == "v2s" {
		cfg.Classifier = "v2"
		cfg.Session = &SmartRouterSessionConfig{Enabled: true}
		benchSessions = newRouterSessions(nil, nil, time.Now, nil)
	}
	rt, err := compileSmartRouter("bench", cfg, nil)
	if err != nil {
		panic(err)
	}
	benchRouter = rt
	return len(benchFixtures)
}

func BenchRanks() []string {
	var out []string
	for _, fx := range benchFixtures {
		endpoint := cacheEndpointChat
		if fx.Wire == "anthropic" {
			endpoint = cacheEndpointMessages
		}
		out = append(out, fx.ID+"="+string(rune('0'+classifySmart(benchRouter, fx.Request, endpoint).rank)))
	}
	return out
}

// BenchHelperCheck replays the call sequences that exposed yaegi v0.16.1 leaking
// named results between calls; it returns a description of the first mismatch.
func BenchHelperCheck() string {
	small := []any{map[string]any{"type": "text", "text": "ab"}}
	big := []any{map[string]any{"type": "text", "text": "0123456789"}, map[string]any{"type": "image"}}
	for round := 0; round < 3; round++ {
		if c, im := smartContentSize(small, 0); c != 2 || im != 0 {
			return "smartContentSize(small) changed between calls"
		}
		if c, im := smartContentSize(big, 0); c != 10 || im != 1 {
			return "smartContentSize(big) changed between calls"
		}
		if c, im := smartMessagesSize(map[string]any{"messages": []any{map[string]any{"role": "user", "content": big}}}, cacheEndpointChat); c != 10 || im != 1 {
			return "smartMessagesSize changed between calls"
		}
		if _, _, tp := smartContentText([]any{map[string]any{"type": "tool_use"}}); !tp {
			return "smartContentText missed a tool part"
		}
		if _, _, tp := smartContentText(small); tp {
			return "smartContentText kept toolParts from the previous call"
		}
		if p1, _ := smartPasteKind("if (a) { b = c; } else { d = e; }\nx = y;\nfoo(bar);\nreturn z;\nint q = 1;\n"); !p1 {
			return "smartPasteKind missed a code paragraph"
		}
		if p2, _ := smartPasteKind("a plain sentence with no code in it at all, just words for a while"); p2 {
			return "smartPasteKind kept isPaste from the previous call"
		}
		if chunkFinishReason([]byte("{\"choices\":[{\"finish_reason\":\"length\"}]}")) != "length" || chunkFinishReason([]byte("{\"choices\":[{\"finish_reason\":null}]}")) != "" {
			return "chunkFinishReason changed between calls"
		}
		if !chunkHasOutput([]byte("{\"delta\":{\"content\":\"x\"}}")) || chunkHasOutput([]byte("{\"delta\":{\"content\":\"\"}}")) {
			return "chunkHasOutput changed between calls"
		}
		if !smartJSONEmpty([]byte("null")) || smartJSONEmpty([]byte("\"x\"")) {
			return "smartJSONEmpty changed between calls"
		}
		if u := withAnswerFlags(usage{}, "length", false); !u.finishLength || !u.emptyOutput {
			return "withAnswerFlags missed a flag"
		}
		if u := withAnswerFlags(usage{}, "stop", true); u.finishLength || u.emptyOutput {
			return "withAnswerFlags kept a flag from the previous call"
		}
		if msg := sessionHelperCheck(); msg != "" {
			return msg
		}
		if msg := learnHelperCheck(); msg != "" {
			return msg
		}
		if msg := banditHelperCheck(); msg != "" {
			return msg
		}
	}
	return ""
}

// sessionHelperCheck replays the session helpers with known answers; yaegi v0.16.1 leaks
// named results between calls, so each pure helper runs twice with different inputs.
func sessionHelperCheck() string {
	cfg := &smartSessionCfg{idle: 1800, decayAsks: 3, regen: 120, toolStreak: 3}
	enc := smartSessionEncode(smartSession{pin: 2, loop: 1, streak: 1, calm: 2, lastScore: 300, msgCount: 7, askHash: 99, askAt: 5, seenAt: 6})
	if string(enc) != "v1|2|1|1|2|300|7|99|5|6" {
		return "smartSessionEncode changed between calls"
	}
	if got, ok := smartSessionDecode(enc); !ok || got.pin != 2 || got.seenAt != 6 || !got.found {
		return "smartSessionDecode missed a value"
	}
	if _, ok := smartSessionDecode([]byte("v1|1")); ok {
		return "smartSessionDecode kept ok from the previous call"
	}
	loopFeat := &smartFeatures{found: true, toolLoopTurn: true, msgCount: 9}
	loopSess := smartSession{found: true, pin: 1, loop: 2, askHash: 5, msgCount: 8, seenAt: 1}
	if rank, reason, _ := smartSessionRoute(cfg, 2, loopFeat, loopSess, smartVerdict{rank: 0}, 2, 5); rank != 2 || reason != "loop" {
		return "smartSessionRoute lost loop inheritance"
	}
	regenSess := smartSession{found: true, askHash: 9, askAt: 100, msgCount: 3, seenAt: 100}
	if rank, reason, _ := smartSessionRoute(cfg, 2, &smartFeatures{found: true, msgCount: 3}, regenSess, smartVerdict{}, 110, 9); rank != 1 || reason != "bump" {
		return "smartSessionRoute lost the regenerate bump"
	}
	if rank, reason, _ := smartSessionRoute(cfg, 2, &smartFeatures{found: true, msgCount: 5}, regenSess, smartVerdict{}, 110, 7); rank != 0 || reason != "score" {
		return "smartSessionRoute kept a bump from the previous call"
	}
	if s, changed := smartSessionOutcome(smartSession{found: true}, 0, 1, 2, true, false); !changed || s.pin != 1 {
		return "smartSessionOutcome missed an escalation"
	}
	if _, changed := smartSessionOutcome(smartSession{found: true}, 0, 0, 2, true, false); changed {
		return "smartSessionOutcome kept changed from the previous call"
	}
	if h := smartSessionHash("r", "u", "c", "s"); h != "f1968510ce9e8987eb3987daa6ba2c0d" || smartSessionHash("r", "u", "c", "t") == h {
		return "smartSessionHash changed between calls"
	}
	l := newSessionLRU()
	l.put("a", smartSession{found: true, pin: 1})
	if got, ok := l.get("a"); !ok || got.pin != 1 {
		return "sessionLRU lost an entry"
	}
	if _, ok := l.get("b"); ok {
		return "sessionLRU kept ok from the previous call"
	}
	return ""
}

// learnHelperCheck replays the calibration helpers with known answers, twice with different inputs.
func learnHelperCheck() string {
	if lo, hi := smartWilsonBounds(5, 10, 1.96); lo < 0.236 || lo > 0.237 || hi < 0.763 || hi > 0.764 {
		return "smartWilsonBounds changed between calls"
	}
	if lo, hi := smartWilsonBounds(0, 0, 1.96); lo != 0 || hi != 1 {
		return "smartWilsonBounds kept bounds from the previous call"
	}
	c := &smartCalibCfg{maxLower: 150, step: 25, targetBadPct: 3, minSamples: 200}
	if got := smartCalibStep(0, 30, 200, c); got != 25 {
		return "smartCalibStep missed a step up"
	}
	if got := smartCalibStep(40, 0, 5000, c); got != 30 {
		return "smartCalibStep missed a relax step"
	}
	if got := smartCalibStep(10, 0, 100, c); got != 10 {
		return "smartCalibStep kept a step from the previous call"
	}
	return ""
}

// banditHelperCheck replays the bandit and saturation helpers with known answers, twice with different inputs.
func banditHelperCheck() string {
	if got := smartThompsonScore(0, 0, 0, 0); got < 0.6666 || got > 0.6667 {
		return "smartThompsonScore changed between calls"
	}
	if got := smartThompsonScore(90, 100, 0, 1); got < 0.4466 || got > 0.4467 {
		return "smartThompsonScore kept a value from the previous call"
	}
	if smartBanditReward(true, 1, 1, 30) != 0 || smartBanditReward(false, 5, 1, 30) != 0.7 {
		return "smartBanditReward changed between calls"
	}
	if smartBanditReward(false, 0, 1, 30) != 1 {
		return "smartBanditReward kept a penalty from the previous call"
	}
	if !smartSaturated(4, 4) || smartSaturated(3, 4) || smartSaturated(9, 0) {
		return "smartSaturated changed between calls"
	}
	if smartUtil(2, 4) != 0.5 || smartUtil(9, 4) != 1 || smartUtil(1, 0) != 0 {
		return "smartUtil changed between calls"
	}
	perm := smartBanditOrder([]float64{0.2, 0.9, 0.5}, []bool{false, true, false}, -1)
	if len(perm) != 3 || perm[0] != 2 || perm[1] != 0 || perm[2] != 1 {
		return "smartBanditOrder missed the saturated-last order"
	}
	perm = smartBanditOrder([]float64{0.2, 0.9}, []bool{false, false}, 0)
	if len(perm) != 2 || perm[0] != 0 || perm[1] != 1 {
		return "smartBanditOrder kept an order from the previous call"
	}
	return ""
}

func benchRank(fx benchFixture) int {
	endpoint := cacheEndpointChat
	if fx.Wire == "anthropic" {
		endpoint = cacheEndpointMessages
	}
	return classifySmart(benchRouter, fx.Request, endpoint).rank
}

// BenchUnstable counts fixtures whose rank depends on call order, which is how
// yaegi v0.16.1 leaking named results between calls shows up.
func BenchUnstable() int {
	first := make([]int, len(benchFixtures))
	for i := 0; i < len(benchFixtures); i++ {
		first[i] = benchRank(benchFixtures[i])
	}
	bad := 0
	for i := len(benchFixtures) - 1; i >= 0; i-- {
		if benchRank(benchFixtures[i]) != first[i] {
			bad++
		}
	}
	return bad
}

func BenchRun(n int) int {
	sum := 0
	for i := 0; i < n; i++ {
		fx := benchFixtures[i%%len(benchFixtures)]
		endpoint := cacheEndpointChat
		if fx.Wire == "anthropic" {
			endpoint = cacheEndpointMessages
		}
		if benchSessions != nil {
			f := extractSmartV2(fx.Request, endpoint)
			sc := benchSessions.open(benchRouter, fx.Request, &f, nil, nil)
			sum += classifySmartWith(benchRouter, fx.Request, endpoint, &f, sc).rank + 1
			if sc.dirty {
				benchSessions.put(sc.key, sc.next, time.Hour)
			}
			continue
		}
		sum += classifySmart(benchRouter, fx.Request, endpoint).rank + 1
	}
	return sum
}
`

var excludedDirs = map[string]bool{"tools": true, "integration": true, ".superpowers": true, "docs": true, ".git": true, "webui": true, "testdata": true}

type benchLayout struct {
	name     string
	mid, top int
}

func main() {
	minDPS := flag.Float64("min-dps", 2000, "fail when a v2 run decides fewer requests per second than this")
	dump := flag.Bool("ranks", false, "print each fixture's rank under yaegi, to diff against a native run")
	passes := flag.Int("passes", 3, "timed passes over the fixture set; the fastest counts")
	fixtures := flag.String("fixtures", "testdata/router_fixtures", "fixture directory, relative to the repo root")
	flag.Parse()
	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}
	if err := run(root, *fixtures, *minDPS, *passes, *dump); err != nil {
		fmt.Fprintln(os.Stderr, "yaegi-bench: FAIL:", err)
		os.Exit(1)
	}
}

func run(root, fixtureDir string, minDPS float64, passes int, dump bool) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	modulePath, err := readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	gopath, err := os.MkdirTemp("", "llmgw-yaegi-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gopath) //nolint:errcheck // best-effort cleanup of a temp dir
	pkgDir := filepath.Join(gopath, "src", modulePath)
	if err = copyTree(root, pkgDir); err != nil {
		return err
	}
	if err = os.Setenv("DO_NOT_TRACK", "1"); err != nil {
		return err
	}
	pkgName, err := readPackageName(pkgDir)
	if err != nil {
		return err
	}
	hook := fmt.Sprintf(hookSource, pkgName)
	if err = os.WriteFile(filepath.Join(pkgDir, "zz_bench_hook.go"), []byte(hook), 0o600); err != nil {
		return err
	}

	i := interp.New(interp.Options{GoPath: gopath})
	if err = i.Use(stdlib.Symbols); err != nil {
		return err
	}
	if _, err = i.Eval(`import "` + modulePath + `"`); err != nil {
		return fmt.Errorf("import plugin under yaegi: %w", err)
	}
	setupV, err := i.Eval(pkgName + ".BenchSetup")
	if err != nil {
		return err
	}
	runV, err := i.Eval(pkgName + ".BenchRun")
	if err != nil {
		return err
	}
	setup := setupV.Interface().(func(string, string, int, int) int)
	loop := runV.Interface().(func(int) int)
	checkV, err := i.Eval(pkgName + ".BenchHelperCheck")
	if err != nil {
		return err
	}
	if msg := checkV.Interface().(func() string)(); msg != "" {
		return fmt.Errorf("interpreter state leak: %s", msg)
	}
	unstableV, err := i.Eval(pkgName + ".BenchUnstable")
	if err != nil {
		return err
	}
	unstable := unstableV.Interface().(func() int)
	ranksV, err := i.Eval(pkgName + ".BenchRanks")
	if err != nil {
		return err
	}
	ranks := ranksV.Interface().(func() []string)

	layouts := []benchLayout{{"deployed", 150, 350}, {"wide", 300, 600}}
	var failures []string
	for _, classifier := range []string{"v1", "v2", "v2s"} {
		for _, l := range layouts {
			n := setup(filepath.Join(root, fixtureDir), classifier, l.mid, l.top)
			if n == 0 {
				return fmt.Errorf("no fixtures under %s", fixtureDir)
			}
			if bad := unstable(); bad > 0 && classifier != "v2s" {
				return fmt.Errorf("%s %s: %d fixtures changed rank between a forward and a reverse pass", classifier, l.name, bad)
			}
			if dump {
				fmt.Printf("ranks %s %s %s\n", classifier, l.name, strings.Join(ranks(), " "))
			}
			loop(n)
			best := time.Duration(1<<63 - 1)
			sum := 0
			for p := 0; p < passes; p++ {
				start := time.Now()
				sum = loop(n)
				if d := time.Since(start); d < best {
					best = d
				}
			}
			dps := float64(n) / best.Seconds()
			fmt.Printf("yaegi %s %-8s fixtures=%d pass=%v decisions/s=%.0f rank-sum=%d\n", classifier, l.name, n, best.Round(time.Millisecond), dps, sum)
			if classifier != "v1" && dps < minDPS {
				failures = append(failures, fmt.Sprintf("%s %.0f < %.0f", l.name, dps, minDPS))
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("below the throughput gate: %s", strings.Join(failures, "; "))
	}
	fmt.Println("yaegi-bench: OK")
	return nil
}

func readModulePath(goModPath string) (string, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-side close
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if after, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", fmt.Errorf("no module directive in %s", goModPath)
}

func readPackageName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			return "", readErr
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), "package "); ok {
				return strings.TrimSpace(after), nil
			}
		}
	}
	return "", fmt.Errorf("no package declaration in %s", dir)
}

// copyTree copies src to dst without the directories the interpreter must not see.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if excludedDirs[rel] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-side close
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // Close is checked below
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
