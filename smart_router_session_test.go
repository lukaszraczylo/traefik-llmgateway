package traefikllmgateway

import (
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubStateStore is a routerStateStore that counts calls and can fail on demand.
type stubStateStore struct {
	data    map[string][]byte
	getErr  error
	setErr  error
	setKeys []string
	setVals [][]byte
	gets    int
	sets    int
	mu      sync.Mutex
}

func newStubStore() *stubStateStore { return &stubStateStore{data: map[string][]byte{}} }

func (s *stubStateStore) getBytes(key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	v, ok := s.data[key]
	return v, ok, nil
}

func (s *stubStateStore) setEx(key string, val []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	s.setKeys = append(s.setKeys, key)
	s.setVals = append(s.setVals, append([]byte(nil), val...))
	if s.setErr != nil {
		return s.setErr
	}
	s.data[key] = append([]byte(nil), val...)
	return nil
}

func (s *stubStateStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}

func syncSpawn(f func()) { f() }

// sessionGateway builds the "auto" router (three tiers, 0/300/600) with sessions on and a stub store behind them.
func sessionGateway(t *testing.T, mut func(*SmartRouterConfig)) (*Gateway, *smartRouter, *stubStateStore) {
	t.Helper()
	gw, rt := planTestGateway(t, func(c *Config) {
		rc := c.SmartRouters[routerName]
		rc.Classifier = smartClassifierV2
		rc.Session = &SmartRouterSessionConfig{Enabled: true}
		if mut != nil {
			mut(rc)
		}
	})
	store := newStubStore()
	gw.routerSessions = newRouterSessions(store, syncSpawn, time.Now, nil)
	return gw, rt, store
}

func sessionReq(userID string, asks ...string) map[string]any {
	msgs := []any{}
	for i, a := range asks {
		if i > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": a})
	}
	req := map[string]any{"messages": msgs}
	if userID != "" {
		req["metadata"] = map[string]any{"user_id": userID}
	}
	return req
}

func planOf(t *testing.T, gw *Gateway, rt *smartRouter, req map[string]any) *routerPlan {
	t.Helper()
	plan, err := gw.planSmartRouter(rt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	return plan
}

func TestSessionKeyClientId(t *testing.T) {
	t.Parallel()
	hdr := http.Header{}
	hdr.Set("X-Session-Id", "from-header")
	req := map[string]any{"prompt_cache_key": "from-cache", "metadata": map[string]any{"user_id": "from-meta"}}
	assert.Equal(t, "from-meta", smartSessionClientID(req, hdr, "X-Session-Id"), "metadata.user_id wins")
	delete(req, "metadata")
	assert.Equal(t, "from-cache", smartSessionClientID(req, hdr, "X-Session-Id"), "prompt_cache_key is next")
	delete(req, "prompt_cache_key")
	assert.Equal(t, "from-header", smartSessionClientID(req, hdr, "X-Session-Id"), "the header is last")
	assert.Equal(t, "", smartSessionClientID(req, nil, "X-Session-Id"), "no id at all")

	gw, rt, _ := sessionGateway(t, nil)
	open := func(userID, system string, scopes []limitScope, asks ...string) string {
		r := sessionReq(userID, asks...)
		if system != "" {
			r["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, r["messages"].([]any)...)
		}
		f := extractSmartV2(r, cacheEndpointChat)
		return gw.routerSessions.open(rt, r, &f, nil, scopes).key
	}
	base := open("u1", "sys", nil, "first ask")
	assert.Regexp(t, regexp.MustCompile(`^llmgw:rsess:auto:[0-9a-f]{32}$`), base)
	assert.Equal(t, base, open("u1", "sys", nil, "a different ask", "and another"), "the client id alone names the conversation")
	assert.NotEqual(t, base, open("u2", "sys", nil, "first ask"), "another client id")
	assert.NotEqual(t, base, open("u1", "other system prompt", nil, "first ask"), "another system prompt")
	assert.NotEqual(t, base, open("u1", "sys", []limitScope{{kind: "user", id: "bob"}}, "first ask"), "another user scope")
	assert.Equal(t, base, open("u1", "sys", []limitScope{{kind: "group", id: "ops"}}, "first ask"), "only the user scope counts")
}

func TestSessionKeyDerived(t *testing.T) {
	t.Parallel()
	gw, rt, _ := sessionGateway(t, nil)
	open := func(system string, asks ...string) string {
		r := sessionReq("", asks...)
		r["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, r["messages"].([]any)...)
		f := extractSmartV2(r, cacheEndpointChat)
		return gw.routerSessions.open(rt, r, &f, nil, nil).key
	}
	turn1 := open("you are a helper", "explain raft")
	assert.Equal(t, turn1, open("you are a helper", "explain raft", "and paxos?", "compare them"), "later turns keep the key")
	assert.NotEqual(t, turn1, open("you are a helper", "explain something else"), "another first message")
	assert.NotEqual(t, turn1, open("you are a different helper", "explain raft"), "another system prompt")
}

func sessionCfgForTest() *smartSessionCfg {
	rt, _ := compileSmartRouter("r", &SmartRouterConfig{Classifier: smartClassifierV2, Session: &SmartRouterSessionConfig{Enabled: true}, Tiers: threeTiersCfg()}, nil)
	return &rt.session
}

func threeTiersCfg() []SmartRouterTier {
	return []SmartRouterTier{
		{Name: "a", MinScore: 0, Models: []string{"p/a"}},
		{Name: "b", MinScore: 300, Models: []string{"p/b"}},
		{Name: "c", MinScore: 600, Models: []string{"p/c"}},
	}
}

func newAskFeatures(msgCount int) *smartFeatures {
	return &smartFeatures{found: true, msgCount: msgCount, history: msgCount > 1}
}

func TestPinRatchet(t *testing.T) {
	t.Parallel()
	cfg := sessionCfgForTest()
	var sess smartSession
	steps := []struct {
		wantReason string
		classified int
		hash       int
		wantRank   int
	}{
		{classified: 2, hash: 1, wantRank: 2, wantReason: smartReasonScore},
		{classified: 0, hash: 2, wantRank: 2, wantReason: smartReasonSession},
		{classified: 1, hash: 3, wantRank: 2, wantReason: smartReasonSession},
	}
	for i, s := range steps {
		rank, reason, next := smartSessionRoute(cfg, 2, newAskFeatures(1+2*i), sess, smartVerdict{rank: s.classified, score: 100}, 1000+int64(i), s.hash)
		assert.Equal(t, s.wantRank, rank, "step %d", i)
		assert.Equal(t, s.wantReason, reason, "step %d", i)
		sess = next
		sess.loop = rank
		if rank > sess.pin {
			sess.pin = rank
		}
		assert.GreaterOrEqual(t, sess.pin, 2, "the pin never drops without decay")
	}
}

func TestPinDecay(t *testing.T) {
	t.Parallel()
	cfg := sessionCfgForTest()
	require.Equal(t, 3, cfg.decayAsks)
	sess := smartSession{found: true, pin: 2, loop: 2, seenAt: 1000, askAt: 1000, askHash: 1, msgCount: 1}
	var ranks []int
	for i := 0; i < 6; i++ {
		rank, _, next := smartSessionRoute(cfg, 2, newAskFeatures(3+2*i), sess, smartVerdict{rank: 0}, 1010+int64(i), 100+i)
		next.loop = rank
		sess = next
		ranks = append(ranks, rank)
	}
	assert.Equal(t, []int{2, 2, 1, 1, 1, 0}, ranks, "the pin drops one tier after every decayAsks calm new asks")

	hard := smartSession{found: true, pin: 2, loop: 2, seenAt: 1000, askAt: 1000, askHash: 1, msgCount: 1, calm: 2}
	_, _, next := smartSessionRoute(cfg, 2, newAskFeatures(3), hard, smartVerdict{rank: 2}, 1010, 7)
	assert.Zero(t, next.calm, "an ask at the pin is not calm")

	idle := smartSessionIdle(cfg, hard, 1000+int64(cfg.idle)+1)
	assert.False(t, idle.found, "an idle session resets")
	assert.Zero(t, idle.pin)
	assert.True(t, smartSessionIdle(cfg, hard, 1000+int64(cfg.idle)).found, "the idle limit is inclusive")
}

func TestLoopInherits(t *testing.T) {
	t.Parallel()
	cfg := sessionCfgForTest()
	f := &smartFeatures{found: true, toolLoopTurn: true, msgCount: 12}
	sess := smartSession{found: true, pin: 1, loop: 2, seenAt: 1000, askAt: 1000, askHash: 5, msgCount: 10}
	rank, reason, next := smartSessionRoute(cfg, 2, f, sess, smartVerdict{rank: 0}, 1010, 5)
	assert.Equal(t, 2, rank, "a tool-loop turn keeps the tier of the ask that started the loop")
	assert.Equal(t, smartReasonLoop, reason)
	assert.Zero(t, next.streak)

	rank, reason, _ = smartSessionRoute(cfg, 2, f, smartSession{}, smartVerdict{rank: 1}, 1010, 5)
	assert.Equal(t, 1, rank, "a miss falls back to the classified rank")
	assert.Equal(t, smartReasonScore, reason)

	stale := smartSession{found: true, pin: 0, loop: 1, seenAt: 1000, askAt: 1000, askHash: 5, msgCount: 10}
	rank, reason, _ = smartSessionRoute(cfg, 2, f, stale, smartVerdict{rank: 2}, 1010, 99)
	assert.Equal(t, 2, rank, "a stale session for another ask still takes the higher classified rank")
	assert.Equal(t, smartReasonScore, reason)
}

func TestRegenBump(t *testing.T) {
	t.Parallel()
	cfg := sessionCfgForTest()
	sess := smartSession{found: true, pin: 0, loop: 0, seenAt: 1000, askAt: 1000, askHash: 9, msgCount: 3}
	same := newAskFeatures(3)
	rank, reason, next := smartSessionRoute(cfg, 2, same, sess, smartVerdict{rank: 0}, 1060, 9)
	assert.Equal(t, 1, rank, "regenerating the same ask goes up a tier")
	assert.Equal(t, smartReasonBump, reason)
	assert.Equal(t, 1, next.pin)

	rank, reason, _ = smartSessionRoute(cfg, 2, same, sess, smartVerdict{rank: 0}, 1000+int64(cfg.regen)+1, 9)
	assert.Equal(t, 0, rank, "outside the window it is not a regenerate")
	assert.Equal(t, smartReasonScore, reason)

	rank, _, _ = smartSessionRoute(cfg, 2, newAskFeatures(5), sess, smartVerdict{rank: 0}, 1060, 9)
	assert.Equal(t, 0, rank, "a longer history is a continuation, not a regenerate")

	served := smartSession{found: true, pin: 1, loop: 1, seenAt: 1000, askAt: 1000, askHash: 9, msgCount: 3}
	rank, _, _ = smartSessionRoute(cfg, 2, same, served, smartVerdict{rank: 0}, 1060, 9)
	assert.Equal(t, 2, rank, "a regenerate lands above the tier that served the last answer")
	rank, _, _ = smartSessionRoute(cfg, 2, same, smartSession{found: true, pin: 2, loop: 2, seenAt: 1000, askAt: 1000, askHash: 9, msgCount: 3}, smartVerdict{rank: 0}, 1060, 9)
	assert.Equal(t, 2, rank, "the bump stops at the top tier")
}

func TestToolFailStreakBump(t *testing.T) {
	t.Parallel()
	cfg := sessionCfgForTest()
	require.Equal(t, 3, cfg.toolStreak)
	sess := smartSession{found: true, pin: 0, loop: 0, seenAt: 1000, askAt: 1000, askHash: 5, msgCount: 4}
	failing := &smartFeatures{found: true, toolLoopTurn: true, toolErr: true, msgCount: 6}
	var ranks []int
	var reasons []string
	for i := 0; i < 4; i++ {
		rank, reason, next := smartSessionRoute(cfg, 2, failing, sess, smartVerdict{rank: 0}, 1010+int64(i), 5)
		next.loop = rank
		if rank > next.pin {
			next.pin = rank
		}
		sess = next
		ranks = append(ranks, rank)
		reasons = append(reasons, reason)
	}
	assert.Equal(t, []int{0, 0, 1, 1}, ranks)
	assert.Equal(t, []string{smartReasonLoop, smartReasonLoop, smartReasonBump, smartReasonLoop}, reasons)
	assert.Equal(t, 1, sess.streak, "the streak restarts after a bump")

	ok := &smartFeatures{found: true, toolLoopTurn: true, msgCount: 8}
	_, _, next := smartSessionRoute(cfg, 2, ok, smartSession{found: true, streak: 2, seenAt: 1000, askHash: 5}, smartVerdict{}, 1020, 5)
	assert.Zero(t, next.streak, "a passing tool result clears the streak")
}

func TestSessionOutcomeRaisesPin(t *testing.T) {
	t.Parallel()
	s, changed := smartSessionOutcome(smartSession{found: true, calm: 2}, 0, 1, 2, true, false)
	assert.True(t, changed)
	assert.Equal(t, 1, s.pin, "an escalation pins the tier that served")
	assert.Zero(t, s.calm)

	s, changed = smartSessionOutcome(smartSession{found: true}, 1, 1, 2, true, true)
	assert.True(t, changed)
	assert.Equal(t, 2, s.pin, "a bad answer pins one tier above the tier that gave it")

	s, _ = smartSessionOutcome(smartSession{found: true, pin: 2}, 0, 0, 2, true, true)
	assert.Equal(t, 2, s.pin, "an outcome never lowers the pin")

	s, changed = smartSessionOutcome(smartSession{found: true}, 2, 2, 2, true, true)
	assert.True(t, changed)
	assert.Equal(t, 2, s.pin, "the pin stops at the top tier")

	_, changed = smartSessionOutcome(smartSession{found: true}, 0, 0, 2, true, false)
	assert.False(t, changed, "a clean outcome changes nothing")
}

func TestSessionEncodeDecode(t *testing.T) {
	t.Parallel()
	in := smartSession{pin: 2, loop: 1, streak: 3, calm: 2, lastScore: 999, msgCount: 120, askHash: 2147483647, askAt: 1893456000, seenAt: 1893456999}
	b := smartSessionEncode(in)
	assert.Less(t, len(b), sessionValueMax)
	assert.Regexp(t, regexp.MustCompile(`^v1(\|\d+){9}$`), string(b))
	out, ok := smartSessionDecode(b)
	require.True(t, ok)
	in.found = true
	assert.Equal(t, in, out)

	for _, bad := range []string{"", "v2|1|1|1|1|1|1|1|1|1", "v1|1|2", "v1|a|1|1|1|1|1|1|1|1", "v1|-1|1|1|1|1|1|1|1|1", "v1|1|1|1|1|1|1|1|1|1|1", strings.Repeat("v1|", 40)} {
		_, ok = smartSessionDecode([]byte(bad))
		assert.False(t, ok, "%q is a miss", bad)
	}
}

func TestSessionMergeKeepsNewerAndHigherPin(t *testing.T) {
	t.Parallel()
	older := smartSession{found: true, pin: 2, seenAt: 10, askHash: 1}
	newer := smartSession{found: true, pin: 0, seenAt: 20, askHash: 2}
	m := smartSessionMerge(older, newer)
	assert.Equal(t, 2, m.askHash, "the newer record is the base")
	assert.Equal(t, 2, m.pin, "the higher pin survives")
	assert.Equal(t, m, smartSessionMerge(newer, older), "merge is symmetric")
}

func TestSessionLRUBounded(t *testing.T) {
	t.Parallel()
	l := newSessionLRU()
	for i := 0; i < sessionLRUSize+50; i++ {
		l.put("k"+strings.Repeat("x", i%7)+string(rune('A'+i%26))+time.Duration(i).String(), smartSession{found: true, pin: 1})
	}
	assert.Len(t, l.snapshot(), sessionLRUSize)
	l.put("hot", smartSession{found: true, pin: 2})
	for i := 0; i < sessionLRUSize-1; i++ {
		l.put("fill"+time.Duration(i).String(), smartSession{found: true})
		l.get("hot")
	}
	_, ok := l.get("hot")
	assert.True(t, ok, "a recently read entry survives eviction")
}

func TestSessionConfigValidation(t *testing.T) {
	t.Parallel()
	compile := func(mut func(*SmartRouterSessionConfig)) (*smartRouter, error) {
		s := &SmartRouterSessionConfig{Enabled: true}
		if mut != nil {
			mut(s)
		}
		return compileSmartRouter("r", &SmartRouterConfig{Classifier: smartClassifierV2, Session: s, Tiers: threeTiersCfg()}, nil)
	}
	rt, err := compile(nil)
	require.NoError(t, err)
	assert.Equal(t, smartSessionCfg{header: "X-Session-Id", ttl: 21600, idle: 1800, decayAsks: 3, regen: 120, toolStreak: 3, live: true}, rt.session)

	bad := map[string]func(*SmartRouterSessionConfig){
		"ttlSeconds":         func(s *SmartRouterSessionConfig) { s.TTLSeconds = 59 },
		"ttlSeconds ":        func(s *SmartRouterSessionConfig) { s.TTLSeconds = 604801 },
		"idleResetSeconds":   func(s *SmartRouterSessionConfig) { s.TTLSeconds, s.IdleResetSeconds = 100, 101 },
		"decayAsks":          func(s *SmartRouterSessionConfig) { s.DecayAsks = 51 },
		"toolFailStreak":     func(s *SmartRouterSessionConfig) { s.ToolFailStreak = 1 },
		"regenWindowSeconds": func(s *SmartRouterSessionConfig) { s.RegenWindowSeconds = 3601 },
		"header":             func(s *SmartRouterSessionConfig) { s.Header = "bad header" },
	}
	for name, mut := range bad {
		_, err = compile(mut)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), strings.TrimSpace(name))
	}

	rt, err = compile(func(s *SmartRouterSessionConfig) { s.TTLSeconds = 600; s.Header = "x-my-session" })
	require.NoError(t, err)
	assert.Equal(t, 600, rt.session.idle, "the default idle limit is capped by a short ttl")
	assert.Equal(t, "X-My-Session", rt.session.header)
}

func TestSessionGate(t *testing.T) {
	t.Parallel()
	mk := func(classifier, shadow string, enabled bool) *smartRouter {
		rt, err := compileSmartRouter("r", &SmartRouterConfig{Classifier: classifier, ShadowClassifier: shadow, Session: &SmartRouterSessionConfig{Enabled: enabled}, Tiers: threeTiersCfg()}, nil)
		require.NoError(t, err)
		return rt
	}
	assert.True(t, mk("v2", "", true).session.live)
	assert.False(t, mk("v2", "", false).session.active(), "off by default")
	shadowOnly := mk("v1", "v2", true)
	assert.True(t, shadowOnly.session.shadow)
	assert.False(t, shadowOnly.session.live, "a v1 router never routes on a session")
	inert := mk("v1", "", true)
	assert.False(t, inert.session.active(), "sessions need v2")
	assert.True(t, inert.sessionEnabled)
	rt, err := compileSmartRouter("r", &SmartRouterConfig{Tiers: threeTiersCfg()}, nil)
	require.NoError(t, err)
	assert.False(t, rt.session.active(), "no session block, no sessions")
}

func TestSessionPlanRatchetAndHeader(t *testing.T) {
	gw, rt, _ := sessionGateway(t, nil)
	first := planOf(t, gw, rt, sessionReq("u1", routerKeyword))
	assert.Equal(t, "t2", first.startTier)
	assert.Equal(t, smartReasonKeyword, first.reason)

	second := planOf(t, gw, rt, sessionReq("u1", routerKeyword, "thanks"))
	assert.Equal(t, "t2", second.startTier, "the pin keeps an easy follow-up on the top tier")
	assert.Equal(t, smartReasonSession, second.reason)

	other := planOf(t, gw, rt, sessionReq("u2", "thanks"))
	assert.Equal(t, "t0", other.startTier, "another conversation is unaffected")

	h := http.Header{}
	setRouterHeaders(h, second, 0)
	assert.Equal(t, smartReasonSession, h.Get(headerRouterReason))
}

func TestSessionOffKeepsRoutingAndHeaders(t *testing.T) {
	gw, rt := planTestGateway(t, nil)
	plan := planOf(t, gw, rt, sessionReq("u1", routerKeyword))
	assert.Nil(t, plan.sess)
	plan = planOf(t, gw, rt, sessionReq("u1", routerKeyword, "thanks"))
	assert.Equal(t, "t0", plan.startTier)
	h := http.Header{}
	setRouterHeaders(h, plan, 0)
	assert.Empty(t, h.Get(headerRouterReason), "routers without sessions keep their old headers")

	gwOff, rtOff := planTestGateway(t, func(c *Config) { c.SmartRouters[routerName].Classifier = smartClassifierV2 })
	assert.Nil(t, planOf(t, gwOff, rtOff, sessionReq("u1", "hi")).sess, "v2 alone does not start sessions")
}

func TestSessionShadowOnlyNeverChangesLive(t *testing.T) {
	control, rtControl := planTestGateway(t, func(c *Config) { c.SmartRouters[routerName].ShadowClassifier = smartClassifierV2 })
	gw, rt := planTestGateway(t, func(c *Config) {
		c.SmartRouters[routerName].ShadowClassifier = smartClassifierV2
		c.SmartRouters[routerName].Session = &SmartRouterSessionConfig{Enabled: true}
	})
	gw.routerSessions = newRouterSessions(newStubStore(), syncSpawn, time.Now, nil)
	require.True(t, rt.session.shadow)
	require.False(t, rt.session.live)

	turns := [][]string{{routerKeyword}, {routerKeyword, "thanks"}, {routerKeyword, "thanks", "hello"}}
	for i, asks := range turns {
		withSession := planOf(t, gw, rt, sessionReq("u1", asks...))
		without := planOf(t, control, rtControl, sessionReq("u1", asks...))
		assert.Equal(t, without.startTier, withSession.startTier, "turn %d: live tier must not move", i)
		assert.Equal(t, without.reason, withSession.reason, "turn %d: live reason must not move", i)
		if i > 0 {
			assert.Equal(t, "t0", withSession.startTier)
			assert.Equal(t, "t2", withSession.shadowTier, "turn %d: the shadow rank carries the session pin", i)
			assert.Equal(t, "t0", without.shadowTier)
		}
	}
}

func TestSessionShortFollowUpUsesLastScore(t *testing.T) {
	gw, rt, _ := sessionGateway(t, func(rc *SmartRouterConfig) { rc.KeywordRules = nil })
	long := "please design the architecture for a multi tenant billing system and compare the trade-offs of each approach in detail"
	cold := planOf(t, gw, rt, sessionReq("c1", long, "ok do it"))
	warm1 := planOf(t, gw, rt, sessionReq("w1", long))
	warm := planOf(t, gw, rt, sessionReq("w1", long, "ok do it"))
	assert.GreaterOrEqual(t, rankByName(rt, warm.startTier), rankByName(rt, warm1.startTier), "the follow-up stays at the tier of the ask it continues")
	assert.GreaterOrEqual(t, rankByName(rt, warm.startTier), rankByName(rt, cold.startTier))
}

func rankByName(rt *smartRouter, tier string) int { return rt.rankOf(tier) }

func TestSessionSingleRedisGetAndAsyncWrite(t *testing.T) {
	gw, rt, store := sessionGateway(t, nil)
	var pending []func()
	gw.routerSessions.spawn = func(f func()) { pending = append(pending, f) }

	planOf(t, gw, rt, sessionReq("u1", "hello there"))
	gets, sets := store.counts()
	assert.Equal(t, 1, gets, "exactly one Redis GET on the hot path")
	assert.Zero(t, sets, "the write is not on the request path")
	require.Len(t, pending, 1)
	pending[0]()
	_, sets = store.counts()
	assert.Equal(t, 1, sets)
	require.Len(t, store.setKeys, 1)
	assert.Regexp(t, regexp.MustCompile(`^llmgw:rsess:auto:[0-9a-f]{32}$`), store.setKeys[0])
	assert.Less(t, len(store.setVals[0]), sessionValueMax)
}

func TestSessionRedisDownFailsUp(t *testing.T) {
	gw, rt, store := sessionGateway(t, nil)
	store.getErr = errors.New("redis: connection refused")
	store.setErr = errors.New("redis: connection refused")
	var logged int
	gw.routerSessions.logf = func(string, ...any) { logged++ }
	fixed := time.Now()
	gw.routerSessions.nowFn = func() time.Time { return fixed }

	stateless, rtStateless := planTestGateway(t, func(c *Config) { c.SmartRouters[routerName].Classifier = smartClassifierV2 })
	for i := 0; i < 3; i++ {
		req := sessionReq("u1", routerKeyword, "thanks")
		plan, err := gw.planSmartRouter(rt, req, cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err, "a Redis outage must never fail a request")
		want := planOf(t, stateless, rtStateless, req)
		assert.GreaterOrEqual(t, rt.rankOf(plan.startTier), rt.rankOf(want.startTier), "a miss fails up, never below the stateless rank")
	}
	gets, _ := store.counts()
	assert.Equal(t, 1, gets, "the down-latch stops further Redis calls")
	assert.Equal(t, 1, logged, "one rate-limited log line")

	fixed = fixed.Add(storeDownLatchFor + time.Second)
	planOf(t, gw, rt, sessionReq("u1", "again"))
	gets, _ = store.counts()
	assert.Equal(t, 2, gets, "after the latch window Redis is tried again")
}

func TestSessionRedisValueSharedAcrossInstances(t *testing.T) {
	store := newStubStore()
	a, rtA, _ := sessionGateway(t, nil)
	b, rtB, _ := sessionGateway(t, nil)
	a.routerSessions = newRouterSessions(store, syncSpawn, time.Now, nil)
	b.routerSessions = newRouterSessions(store, syncSpawn, time.Now, nil)

	planOf(t, a, rtA, sessionReq("u1", routerKeyword))
	plan := planOf(t, b, rtB, sessionReq("u1", routerKeyword, "thanks"))
	assert.Equal(t, "t2", plan.startTier, "a second gateway instance reads the pin from Redis")
	assert.Equal(t, smartReasonSession, plan.reason)
}

func TestNoPromptTextStored(t *testing.T) {
	gw, rt, store := sessionGateway(t, nil)
	secrets := []string{"ZEBRA-SECRET-PROMPT", "QUOKKA-SYSTEM-TEXT", "NARWHAL-USER-ID", "OCELOT-HEADER-ID", "TAPIR-CACHE-KEY"}
	hdr := http.Header{}
	hdr.Set("X-Session-Id", secrets[3])
	send := func(req map[string]any, h http.Header) {
		plan, err := gw.planSmartRouter(rt, req, cacheEndpointChat, h, &group{}, []limitScope{{kind: "user", id: "alice"}})
		require.NoError(t, err)
		gw.onRouterOutcome(plan, 0, true, true)
	}
	withSystem := func(r map[string]any) map[string]any {
		r["messages"] = append([]any{map[string]any{"role": "system", "content": secrets[1]}}, r["messages"].([]any)...)
		return r
	}
	send(withSystem(sessionReq(secrets[2], secrets[0]+" design the architecture", "follow up "+secrets[0])), nil)
	send(withSystem(sessionReq("", secrets[0]+" derived key path")), nil)
	send(withSystem(sessionReq("", secrets[0]+" header path")), hdr)
	cacheKeyReq := withSystem(sessionReq("", secrets[0]+" cache key path"))
	cacheKeyReq["prompt_cache_key"] = secrets[4]
	send(cacheKeyReq, nil)

	snap := gw.routerSessions.lru.snapshot()
	require.GreaterOrEqual(t, len(snap), 4)
	keyRe := regexp.MustCompile(`^llmgw:rsess:auto:[0-9a-f]{32}$`)
	valRe := regexp.MustCompile(`^v1(\|\d+){9}$`)
	for key, st := range snap {
		assert.Regexp(t, keyRe, key)
		assert.Regexp(t, valRe, string(smartSessionEncode(st)))
	}
	require.NotEmpty(t, store.setKeys)
	for i, key := range store.setKeys {
		assert.Regexp(t, keyRe, key)
		assert.Regexp(t, valRe, string(store.setVals[i]))
		for _, s := range secrets {
			assert.NotContains(t, key, s)
			assert.NotContains(t, string(store.setVals[i]), s)
		}
	}
	typ := reflect.TypeOf(smartSession{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Type.Kind() {
		case reflect.Int, reflect.Int64, reflect.Bool:
		default:
			t.Errorf("smartSession.%s is a %s: sessions hold small integers only", typ.Field(i).Name, typ.Field(i).Type.Kind())
		}
	}
	assert.Equal(t, store.gets, 4, "one GET per request")
}

func TestSessionMissMatchesStateless(t *testing.T) {
	t.Parallel()
	for _, l := range evalLayouts {
		plain := evalRouter(t, l.mins, smartClassifierV2)
		withSess := evalSessionRouter(t, l.mins)
		for _, fx := range loadRouterFixtures(t) {
			want := classifySmart(plain, fx.Request, fx.endpoint())
			got, _ := classifyInSession(withSess, fx.Request, fx.endpoint(), smartSession{}, 1_000_000)
			assert.Equal(t, want.rank, got.rank, "%s on %s: a session miss must classify like no session", fx.ID, l.name)
			assert.Equal(t, want.reason, got.reason, "%s on %s", fx.ID, l.name)
		}
	}
}

func TestRouterReasonNamesCoverSessionReasons(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, n := range routerReasonNames {
		assert.NotEmpty(t, n)
		assert.False(t, seen[n], "duplicate reason %q", n)
		seen[n] = true
	}
	for _, want := range []string{smartReasonScore, smartReasonKeyword, smartReasonFloor, smartReasonDefault, routerReasonSkipped, smartReasonSession, smartReasonLoop, smartReasonBump} {
		assert.True(t, seen[want], want)
	}
	assert.Equal(t, smartReasonScore, routerReasonNames[0], "existing reasons keep their order")
}

func sessionHTTPGateway(t *testing.T, statuses [3]int, bodies [3]string) (http.Handler, *Gateway) {
	t.Helper()
	a := newRouterBackend(statuses[0], bodies[0])
	b := newRouterBackend(statuses[1], bodies[1])
	c := newRouterBackend(statuses[2], bodies[2])
	t.Cleanup(func() { closeBackends(a, b, c) })
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Metrics = &MetricsConfig{Enabled: true}
	cfg.SmartRouters[routerName].Classifier = smartClassifierV2
	cfg.SmartRouters[routerName].Session = &SmartRouterSessionConfig{Enabled: true}
	h, gw := buildRouterGateway(t, cfg)
	gw.routerSessions = newRouterSessions(nil, syncSpawn, time.Now, nil)
	return h, gw
}

func onlySession(t *testing.T, gw *Gateway) smartSession {
	t.Helper()
	snap := gw.routerSessions.lru.snapshot()
	require.Len(t, snap, 1)
	for _, st := range snap {
		return st
	}
	return smartSession{}
}

func TestSessionEscalationRaisesPinForNextTurn(t *testing.T) {
	h, gw := sessionHTTPGateway(t, [3]int{503, 200, 200}, [3]string{routerFail, successRespBody, successRespBody})
	meta := map[string]any{"metadata": map[string]any{"user_id": "u1"}}
	rec := routerChat(t, h, routerName, "hi", meta)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, "t1", rec.Header().Get(headerRouterTier), "t0 failed, t1 served")
	assert.Equal(t, smartReasonScore, rec.Header().Get(headerRouterReason))
	assert.Equal(t, 1, onlySession(t, gw).pin, "the tier that served pins the session")

	rec = routerChat(t, h, routerName, "hello again", meta)
	assert.Equal(t, "t1", rec.Header().Get(headerRouterTier), "the next turn starts at the pin")
	assert.Equal(t, smartReasonSession, rec.Header().Get(headerRouterReason))
	assert.Contains(t, scrape(gw), `reason="session"`)
}

func TestSessionBadOutcomeRaisesPinForNextTurn(t *testing.T) {
	empty := `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":0}}`
	h, gw := sessionHTTPGateway(t, [3]int{200, 200, 200}, [3]string{empty, successRespBody, successRespBody})
	meta := map[string]any{"metadata": map[string]any{"user_id": "u1"}}
	rec := routerChat(t, h, routerName, "hi", meta)
	require.Equal(t, 200, rec.Code)
	assert.Equal(t, "t0", rec.Header().Get(headerRouterTier))
	assert.Equal(t, 1, onlySession(t, gw).pin, "an empty answer pins one tier above the tier that gave it")

	rec = routerChat(t, h, routerName, "hello again", meta)
	assert.Equal(t, "t1", rec.Header().Get(headerRouterTier))
	assert.Equal(t, smartReasonSession, rec.Header().Get(headerRouterReason))
}
