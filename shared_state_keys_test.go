package traefikllmgateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func freshBreakerKeys(kind string, ids []string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, sharedBreakerKey(kind, id, "open"), sharedBreakerKey(kind, id, "fails"), sharedBreakerKey(kind, id, "opens"))
	}
	return out
}

func freshInflightKeys(s *sharedInflight, now time.Time) []string {
	e := s.epochOf(now)
	var out []string
	for _, id := range s.ids {
		for i := 0; i < s.epochs; i++ {
			out = append(out, s.epochKey(id, e-int64(i), "s"), s.epochKey(id, e-int64(i), "f"))
		}
		out = append(out, sharedInflightKey(id, "limit"))
	}
	return out
}

func TestSharedBreakerKeys_MatchFreshBuild(t *testing.T) {
	ids := []string{"a", "b", "c"}
	b := newSharedBreaker(nil, sharedKindRequest, ids, 3, time.Second, time.Minute, time.Minute)
	want := freshBreakerKeys(sharedKindRequest, ids)
	assert.Equal(t, want, b.keys(time.Unix(0, 0)))
	assert.Equal(t, want, b.keys(time.Unix(1e9, 0)))
	assert.Equal(t, want, b.keys(time.Unix(0, 0)))
}

func TestSharedInflightKeys_MatchFreshBuild(t *testing.T) {
	s := newSharedInflight(nil, []string{"a", "b"}, 15*time.Minute)
	t0 := time.Unix(1_700_000_000, 0)
	sameEpoch := t0.Add(10 * time.Second)
	assert.Equal(t, s.epochOf(t0), s.epochOf(sameEpoch))
	next := t0.Add(sharedInflightEpoch)
	for _, now := range []time.Time{t0, sameEpoch, t0, next, next, t0, next.Add(time.Hour)} {
		assert.Equal(t, freshInflightKeys(s, now), s.keys(now))
	}
}

func TestShareDecisionBreakers_KeysOnlyDecisionProviders(t *testing.T) {
	routers := map[string]*smartRouter{}
	for name, prov := range map[string]string{"r1": "p3", "r2": "p1", "r3": "p3", "r4": ""} {
		rt := &smartRouter{name: name}
		if prov != "" {
			rt.decision = &smartDecisionCfg{provider: prov, dec: newSmartDecider(time.Now)}
		}
		routers[name] = rt
	}
	l := newSharedStateLoop(newMemoryStore(), func(f func()) {}, nil)
	g := &Gateway{sharedState: l, registry: &modelRegistry{routers: routers}}
	g.shareDecisionBreakers()

	prims := l.snapshot()
	if !assert.Len(t, prims, 1) {
		return
	}
	assert.Equal(t, freshBreakerKeys(sharedKindDecision, []string{"p1", "p3"}), prims[0].keys(time.Now()))
	for _, rt := range []*smartRouter{routers["r1"], routers["r2"], routers["r3"]} {
		assert.NotNil(t, rt.decision.dec.shared)
		assert.Equal(t, rt.decision.provider, rt.decision.dec.key)
	}
}
