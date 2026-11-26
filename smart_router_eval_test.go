package traefikllmgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	evalFixtureGlob   = "testdata/router_fixtures/*.json"
	evalDevGlob       = "testdata/router_fixtures_dev/*.json"
	evalHoldout2Glob  = "testdata/router_fixtures_holdout2/*.json"
	evalHoldout3Glob  = "testdata/router_fixtures_holdout3/*.json"
	evalSessionNote   = "session-required"
	evalV1Golden      = "testdata/router_v1_golden.json"
	evalMinFixtures   = 160
	evalMaxUnderPct   = 2
	evalMaxOverPct    = 25
	evalGroupToolLoop = "toolloop"
	evalGroupNonEng   = "nonenglish"
)

// evalLayouts are the tier minScore layouts every gate must pass on.
var evalLayouts = []struct {
	name string
	mins [3]int
}{
	{"deployed", [3]int{0, 150, 350}},
	{"wide", [3]int{0, 300, 600}},
}

type routerFixture struct {
	Request map[string]any  `json:"request"`
	ID      string          `json:"id"`
	Group   string          `json:"group"`
	Lang    string          `json:"lang"`
	Intent  string          `json:"intent"`
	Wire    string          `json:"wire"`
	Notes   string          `json:"notes"`
	Session json.RawMessage `json:"session"`
	MinTier int             `json:"minTier"`
	MaxTier int             `json:"maxTier"`
}

func (f routerFixture) endpoint() string {
	if f.Wire == "anthropic" {
		return cacheEndpointMessages
	}
	return cacheEndpointChat
}

func loadRouterFixtures(tb testing.TB) []routerFixture {
	tb.Helper()
	return loadRouterFixturesGlob(tb, evalFixtureGlob)
}

func loadRouterFixturesGlob(tb testing.TB, glob string) []routerFixture {
	tb.Helper()
	files, err := filepath.Glob(glob)
	require.NoError(tb, err)
	sort.Strings(files)
	var out []routerFixture
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		require.NoError(tb, readErr)
		var items []routerFixture
		require.NoError(tb, json.Unmarshal(raw, &items), file)
		out = append(out, items...)
	}
	return out
}

func evalRouter(tb testing.TB, mins [3]int, classifier string) *smartRouter {
	tb.Helper()
	cfg := &SmartRouterConfig{
		Classifier: classifier,
		Tiers: []SmartRouterTier{
			{Name: "cheap", MinScore: mins[0], Models: []string{"a/x"}},
			{Name: "mid", MinScore: mins[1], Models: []string{"a/y"}},
			{Name: "top", MinScore: mins[2], Models: []string{"a/z"}},
		},
	}
	rt, err := compileSmartRouter("smart/auto", cfg, nil)
	require.NoError(tb, err)
	return rt
}

type evalGroupStat struct {
	underIDs []string
	overIDs  []string
	n        int
	under    int
	over     int
}

type evalResult struct {
	groups map[string]*evalGroupStat
	total  evalGroupStat
}

func runEval(rt *smartRouter, fixtures []routerFixture) evalResult {
	res := evalResult{groups: map[string]*evalGroupStat{}}
	for _, fx := range fixtures {
		if strings.Contains(fx.Notes, evalSessionNote) {
			continue
		}
		c := classifySmart(rt, fx.Request, fx.endpoint())
		g := res.groups[fx.Group]
		if g == nil {
			g = &evalGroupStat{}
			res.groups[fx.Group] = g
		}
		g.n++
		res.total.n++
		if c.rank < fx.MinTier {
			g.under++
			res.total.under++
			g.underIDs = append(g.underIDs, fmt.Sprintf("%s(%d<%d)", fx.ID, c.rank, fx.MinTier))
			res.total.underIDs = append(res.total.underIDs, fx.ID)
		}
		if c.rank > fx.MaxTier {
			g.over++
			res.total.over++
			g.overIDs = append(g.overIDs, fmt.Sprintf("%s(%d>%d)", fx.ID, c.rank, fx.MaxTier))
			res.total.overIDs = append(res.total.overIDs, fx.ID)
		}
	}
	return res
}

func (r evalResult) table() string {
	names := make([]string, 0, len(r.groups))
	for g := range r.groups {
		names = append(names, g)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, g := range names {
		s := r.groups[g]
		fmt.Fprintf(&b, "  %-11s n=%-3d under=%-2d over=%-2d under:%v over:%v\n", g, s.n, s.under, s.over, s.underIDs, s.overIDs)
	}
	fmt.Fprintf(&b, "  %-11s n=%-3d under=%-2d over=%-2d", "TOTAL", r.total.n, r.total.under, r.total.over)
	return b.String()
}

func TestRouterFixtureComposition(t *testing.T) {
	t.Parallel()
	fixtures := loadRouterFixtures(t)
	require.GreaterOrEqual(t, len(fixtures), evalMinFixtures)
	ids := map[string]bool{}
	wires := map[string]int{}
	loopSizes := map[int]bool{}
	for _, fx := range fixtures {
		assert.False(t, ids[fx.ID], "duplicate id %s", fx.ID)
		ids[fx.ID] = true
		wires[fx.Wire]++
		assert.NotEmpty(t, fx.Notes, fx.ID)
		assert.True(t, fx.MinTier >= 0 && fx.MinTier <= fx.MaxTier && fx.MaxTier <= 2, "%s tier range", fx.ID)
		assert.NotEmpty(t, fx.Request["messages"], fx.ID+" messages", "size-04 has only a system message")
		if fx.Group == evalGroupToolLoop {
			msgs, _ := fx.Request["messages"].([]any)
			loopSizes[len(msgs)] = true
		}
	}
	assert.Positive(t, wires["openai"])
	assert.Positive(t, wires["anthropic"])
	for _, want := range []int{10, 40, 120} {
		assert.True(t, loopSizes[want], "tool loop with %d messages", want)
	}
	groups := map[string]int{}
	for _, fx := range fixtures {
		groups[fx.Group]++
	}
	for g, min := range map[string]int{"chitchat": 12, "rewrite": 14, "edit": 22, "debug": 22, "design": 16, "math": 12, evalGroupToolLoop: 24, "trap": 12, evalGroupNonEng: 14, "size": 8, "output": 4} {
		assert.GreaterOrEqual(t, groups[g], min, g)
	}
}

// TestRouterFixturesV1Unchanged pins v1 output byte for byte against a golden taken before v2 existed.
func TestRouterFixturesV1Unchanged(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(evalV1Golden)
	require.NoError(t, err)
	golden := map[string]map[string][]any{}
	require.NoError(t, json.Unmarshal(raw, &golden))
	fixtures := loadRouterFixtures(t)
	for _, classifier := range []string{"", smartClassifierV1} {
		for _, l := range evalLayouts {
			rt := evalRouter(t, l.mins, classifier)
			for _, fx := range fixtures {
				want := golden[fx.ID][l.name]
				require.NotNil(t, want, fx.ID)
				c := classifySmart(rt, fx.Request, fx.endpoint())
				h := sha256.Sum256([]byte(c.scoredText))
				got := []any{float64(c.rank), c.reason, hex.EncodeToString(h[:8]), c.hasImage}
				assert.Equal(t, want, got, "%s on %s with classifier %q", fx.ID, l.name, classifier)
			}
		}
	}
}

func TestRouterFixturesGate(t *testing.T) {
	t.Parallel()
	gateRouterFixtures(t, loadRouterFixtures(t))
}

// TestRouterDevGate holds the blind dev set to the same gate as the main set.
func TestRouterDevGate(t *testing.T) {
	t.Parallel()
	gateRouterFixtures(t, loadRouterFixturesGlob(t, evalDevGlob))
}

func gateRouterFixtures(t *testing.T, fixtures []routerFixture) {
	t.Helper()
	for _, l := range evalLayouts {
		t.Run(l.name, func(t *testing.T) {
			v1 := runEval(evalRouter(t, l.mins, smartClassifierV1), fixtures)
			v2 := runEval(evalRouter(t, l.mins, smartClassifierV2), fixtures)
			t.Logf("layout %v v1 baseline (not gated):\n%s\nlayout %v v2:\n%s", l.mins, v1.table(), l.mins, v2.table())

			assert.LessOrEqual(t, v2.total.under*100, evalMaxUnderPct*v2.total.n, "overall under-route: %v", v2.total.underIDs)
			assert.LessOrEqual(t, v2.total.over*100, evalMaxOverPct*v2.total.n, "overall over-route: %v", v2.total.overIDs)
			for _, g := range []string{evalGroupToolLoop, evalGroupNonEng} {
				require.NotNil(t, v2.groups[g], g)
				assert.Zero(t, v2.groups[g].under, "%s under-route: %v", g, v2.groups[g].underIDs)
			}
		})
	}
}

func TestScoreV2NeverBelowV1Fixtures(t *testing.T) {
	t.Parallel()
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		for _, fx := range loadRouterFixtures(t) {
			f := extractSmartV2(fx.Request, fx.endpoint())
			if !f.found {
				continue
			}
			v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
			// Routine tool steps and plain transform asks ignore the v1 signals on purpose: tools
			// and pasted length alone do not make them hard.
			if !f.routineToolStep() && (!f.easy || f.hard > 0) {
				sig := smartSignalsFor(f.lower, len(f.prose), countSmartMarkers(f.lower, smartReasoningMarkers), f.hasTools || f.toolHist)
				assert.GreaterOrEqual(t, v.score, sig.score(rt.weights), "%s on %s", fx.ID, l.name)
			}
			assert.GreaterOrEqual(t, v.score, rt.intentBase[f.intent], "%s on %s", fx.ID, l.name)
		}
	}
}

// BenchmarkSmartRouterFixtures decides every fixture once per iteration; ns/op divided by the fixture count is the per-decision cost.
func BenchmarkSmartRouterFixtures(b *testing.B) {
	fixtures := loadRouterFixtures(b)
	for _, classifier := range []string{smartClassifierV1, smartClassifierV2} {
		b.Run(classifier, func(b *testing.B) {
			rt := evalRouter(b, evalLayouts[0].mins, classifier)
			b.ReportAllocs()
			b.ResetTimer()
			sum := 0
			for i := 0; i < b.N; i++ {
				for j := range fixtures {
					sum += classifySmart(rt, fixtures[j].Request, fixtures[j].endpoint()).rank
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(fixtures)), "ns/decision")
			_ = sum
		})
	}
}

// TestRouterHoldout2Gate holds the former blind holdout, now a training set, to the same gate as the main set.
func TestRouterHoldout2Gate(t *testing.T) {
	t.Parallel()
	gateRouterFixtures(t, loadRouterFixturesGlob(t, evalHoldout2Glob))
}

// TestRouterHoldout3Gate holds the second former blind holdout, now a training set, to the same gate.
func TestRouterHoldout3Gate(t *testing.T) {
	t.Parallel()
	gateRouterFixtures(t, loadRouterFixturesGlob(t, evalHoldout3Glob))
}

// fixtureSession is the prior state of a session fixture. Replay derives it by running the
// conversation's earlier asks through the router; explicit pin, loop and lastScore (per-mille)
// set it by hand. The main and dev gates skip these fixtures.
type fixtureSession struct {
	Pin       int  `json:"pin"`
	Loop      int  `json:"loop"`
	LastScore int  `json:"lastScore"`
	Replay    bool `json:"replay"`
}

const evalSessionStep = 30

func evalSessionRouter(tb testing.TB, mins [3]int) *smartRouter {
	tb.Helper()
	cfg := &SmartRouterConfig{
		Classifier: smartClassifierV2,
		Session:    &SmartRouterSessionConfig{Enabled: true},
		Tiers: []SmartRouterTier{
			{Name: "cheap", MinScore: mins[0], Models: []string{"a/x"}},
			{Name: "mid", MinScore: mins[1], Models: []string{"a/y"}},
			{Name: "top", MinScore: mins[2], Models: []string{"a/z"}},
		},
	}
	rt, err := compileSmartRouter("smart/auto", cfg, nil)
	require.NoError(tb, err)
	return rt
}

// classifyInSession classifies req against a session state at a fixed clock, the way the gateway does.
func classifyInSession(rt *smartRouter, req map[string]any, endpoint string, st smartSession, now int64) (smartClassification, *smartSessionRun) {
	f := extractSmartV2(req, endpoint)
	sc := &smartSessionRun{rt: rt, key: "eval", sess: st, now: now}
	return classifySmartWith(rt, req, endpoint, &f, sc, nil), sc
}

// replaySession runs every earlier user ask of the fixture's conversation through the router in order.
func replaySession(rt *smartRouter, fx routerFixture) (smartSession, int64) {
	msgs, _ := fx.Request["messages"].([]any)
	var st smartSession
	now := int64(1_000_000)
	for i := 0; i < len(msgs)-1; i++ {
		m, _ := msgs[i].(map[string]any)
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		prefix := map[string]any{}
		for k, v := range fx.Request {
			prefix[k] = v
		}
		prefix["messages"] = msgs[:i+1]
		_, sc := classifyInSession(rt, prefix, fx.endpoint(), st, now)
		if sc.dirty {
			st = sc.next
		}
		now += evalSessionStep
	}
	return st, now
}

// fixtureSessionState resolves a fixture's session field into a state and a clock.
func fixtureSessionState(tb testing.TB, rt *smartRouter, fx routerFixture) (smartSession, int64) {
	tb.Helper()
	var fs fixtureSession
	require.NoError(tb, json.Unmarshal(fx.Session, &fs), fx.ID)
	if fs.Replay {
		return replaySession(rt, fx)
	}
	now := int64(1_000_000)
	return smartSession{found: true, pin: fs.Pin, loop: fs.Loop, lastScore: fs.LastScore, seenAt: now, askAt: now}, now + evalSessionStep
}

func sessionRequiredFixtures(tb testing.TB) []routerFixture {
	tb.Helper()
	var out []routerFixture
	for _, glob := range []string{evalHoldout2Glob, evalDevGlob} {
		for _, fx := range loadRouterFixturesGlob(tb, glob) {
			if strings.Contains(fx.Notes, evalSessionNote) {
				out = append(out, fx)
			}
		}
	}
	return out
}

func TestRouterSessionFixtureSchema(t *testing.T) {
	t.Parallel()
	fixtures := sessionRequiredFixtures(t)
	require.Len(t, fixtures, 7, "6 holdout2 and 1 dev fixtures need a session")
	for _, fx := range fixtures {
		require.NotEmpty(t, fx.Session, fx.ID)
		assert.NotEqual(t, "null", string(fx.Session), fx.ID)
	}
}

// TestRouterSessionFixturesReport logs the session-required fixtures with and without session
// state. It never gates: a known session removes the short-follow-up doubt margin, so its rank
// can sit below the stateless one. TestSessionMissMatchesStateless pins the cold-state case.
func TestRouterSessionFixturesReport(t *testing.T) {
	t.Parallel()
	fixtures := sessionRequiredFixtures(t)
	for _, l := range evalLayouts {
		plain := evalRouter(t, l.mins, smartClassifierV2)
		withSess := evalSessionRouter(t, l.mins)
		var b strings.Builder
		underPlain, underSess, overPlain, overSess := 0, 0, 0, 0
		for _, fx := range fixtures {
			noSess := classifySmart(plain, fx.Request, fx.endpoint())
			st, now := fixtureSessionState(t, withSess, fx)
			got, sc := classifyInSession(withSess, fx.Request, fx.endpoint(), st, now)
			underPlain, underSess = underPlain+boolInt(noSess.rank < fx.MinTier), underSess+boolInt(got.rank < fx.MinTier)
			overPlain, overSess = overPlain+boolInt(noSess.rank > fx.MaxTier), overSess+boolInt(got.rank > fx.MaxTier)
			fmt.Fprintf(&b, "  %-8s want %d..%d  v2 %d  v2+session %d (%s) prior pin=%d loop=%d lastScore=%d\n",
				fx.ID, fx.MinTier, fx.MaxTier, noSess.rank, got.rank, got.reason, st.pin, st.loop, st.lastScore)
			_ = sc
		}
		require.Equal(t, len(fixtures), len(strings.Split(strings.TrimSpace(b.String()), "\n")))
		t.Logf("session-required fixtures on %s %v (report only, not gated)\n%s  under: v2=%d v2+session=%d  over: v2=%d v2+session=%d  of %d",
			l.name, l.mins, b.String(), underPlain, underSess, overPlain, overSess, len(fixtures))
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
