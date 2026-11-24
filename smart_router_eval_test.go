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
	fixtures := loadRouterFixtures(t)
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

// TestRouterDevReport scores the dev set on both layouts; it reports and never gates.
func TestRouterDevReport(t *testing.T) {
	t.Parallel()
	fixtures := loadRouterFixturesGlob(t, evalDevGlob)
	require.NotEmpty(t, fixtures)
	for _, l := range evalLayouts {
		t.Run(l.name, func(t *testing.T) {
			v1 := runEval(evalRouter(t, l.mins, smartClassifierV1), fixtures)
			v2 := runEval(evalRouter(t, l.mins, smartClassifierV2), fixtures)
			t.Logf("DEV layout %v v1:\n%s\nDEV layout %v v2:\n%s", l.mins, v1.table(), l.mins, v2.table())
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
			sig := smartSignalsFor(f.lower, len(f.prose), countSmartMarkers(f.lower, smartReasoningMarkers), f.hasTools || f.toolHist)
			assert.GreaterOrEqual(t, v.score, sig.score(rt.weights), "%s on %s", fx.ID, l.name)
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
