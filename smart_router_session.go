package traefikllmgateway

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sessions give a conversation an upward-ratcheting tier pin. Only a hash of the
// conversation identity and a few small integers are stored, never prompt text.
//
// A router uses sessions only when session.enabled is set and the v2 classifier runs.
// With classifier v2 the pin changes live routing. With only shadowClassifier v2 the
// pin changes the shadow rank alone, so a v1 router never routes on session state.

const (
	smartReasonSession = "session"
	smartReasonLoop    = "loop"
	smartReasonBump    = "bump"

	sessionKeyPrefix   = "llmgw:rsess:"
	sessionValueTag    = "v1"
	sessionFieldCount  = 10
	sessionValueMax    = 96
	sessionLRUSize     = 4096
	sessionHashBytes   = 16
	sessionIDCap       = 512
	sessionSystemKey   = 256
	sessionDerivedSys  = 1024
	sessionDerivedUser = 2048
	sessionSep         = "\x00"

	defaultSessionTTL        = 21600
	defaultSessionIdle       = 1800
	defaultSessionDecayAsks  = 3
	defaultSessionRegen      = 120
	defaultSessionToolStreak = 3
	defaultSessionHeader     = "X-Session-Id"

	minSessionTTL    = 60
	maxSessionTTL    = 604800
	maxSessionDecay  = 50
	minSessionStreak = 2
	maxSessionStreak = 20
	minSessionRegen  = 5
	maxSessionRegen  = 3600
)

// SmartRouterSessionConfig tunes the per-conversation tier pin. Zero values mean the default.
type SmartRouterSessionConfig struct {
	// Header names the request header that carries a client session id.
	Header string `json:"header,omitempty"`
	// TTLSeconds is how long an idle session record lives; each turn renews it.
	TTLSeconds int `json:"ttlSeconds,omitempty"`
	// IdleResetSeconds drops the pin after this much silence.
	IdleResetSeconds int `json:"idleResetSeconds,omitempty"`
	// DecayAsks is how many calm new asks lower the pin by one tier.
	DecayAsks int `json:"decayAsks,omitempty"`
	// RegenWindowSeconds is how soon a repeat of the same ask counts as a regenerate.
	RegenWindowSeconds int `json:"regenWindowSeconds,omitempty"`
	// ToolFailStreak is how many failing tool results in a row raise a tool loop one tier.
	ToolFailStreak int  `json:"toolFailStreak,omitempty"`
	Enabled        bool `json:"enabled,omitempty"`
}

// smartSessionCfg is the validated session config with defaults applied.
type smartSessionCfg struct {
	header                                  string
	ttl, idle, decayAsks, regen, toolStreak int
	// live: the pin changes routing. shadow: it changes only the shadow rank.
	live, shadow bool
}

func (c *smartSessionCfg) active() bool { return c.live || c.shadow }

func compileSmartSession(router string, cfg *SmartRouterSessionConfig, classifier string, shadowV2 bool) (smartSessionCfg, error) {
	if cfg == nil {
		return smartSessionCfg{}, nil
	}
	ranges := []struct {
		name      string
		v, lo, hi int
	}{
		{"ttlSeconds", cfg.TTLSeconds, minSessionTTL, maxSessionTTL},
		{"idleResetSeconds", cfg.IdleResetSeconds, 1, maxSessionTTL},
		{"decayAsks", cfg.DecayAsks, 1, maxSessionDecay},
		{"toolFailStreak", cfg.ToolFailStreak, minSessionStreak, maxSessionStreak},
		{"regenWindowSeconds", cfg.RegenWindowSeconds, minSessionRegen, maxSessionRegen},
	}
	for _, r := range ranges {
		if r.v != 0 && (r.v < r.lo || r.v > r.hi) {
			return smartSessionCfg{}, fmt.Errorf("llmgateway: smartRouters: router %q: session.%s must be between %d and %d, got %d", router, r.name, r.lo, r.hi, r.v)
		}
	}
	if strings.ContainsAny(cfg.Header, " \t\r\n:") {
		return smartSessionCfg{}, fmt.Errorf("llmgateway: smartRouters: router %q: session.header %q is not a valid header name", router, cfg.Header)
	}
	c := smartSessionCfg{
		header:     cfg.Header,
		ttl:        sessionOrDefault(cfg.TTLSeconds, defaultSessionTTL),
		idle:       sessionOrDefault(cfg.IdleResetSeconds, defaultSessionIdle),
		decayAsks:  sessionOrDefault(cfg.DecayAsks, defaultSessionDecayAsks),
		regen:      sessionOrDefault(cfg.RegenWindowSeconds, defaultSessionRegen),
		toolStreak: sessionOrDefault(cfg.ToolFailStreak, defaultSessionToolStreak),
	}
	if cfg.IdleResetSeconds == 0 && c.idle > c.ttl {
		c.idle = c.ttl
	}
	if c.idle > c.ttl {
		return smartSessionCfg{}, fmt.Errorf("llmgateway: smartRouters: router %q: session.idleResetSeconds %d must not exceed session.ttlSeconds %d", router, c.idle, c.ttl)
	}
	if c.header == "" {
		c.header = defaultSessionHeader
	}
	c.header = http.CanonicalHeaderKey(c.header)
	if cfg.Enabled {
		c.live = classifier == smartClassifierV2
		c.shadow = !c.live && shadowV2
	}
	return c, nil
}

func sessionOrDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// smartSession is the whole stored state of one conversation: small integers only.
type smartSession struct {
	askAt, seenAt                                         int64
	pin, loop, streak, calm, lastScore, msgCount, askHash int
	// found is false for a miss and is never stored.
	found bool
}

// smartSessionEncode writes the value stored under a session key.
func smartSessionEncode(s smartSession) []byte {
	b := make([]byte, 0, sessionValueMax)
	b = append(b, sessionValueTag...)
	for _, n := range []int64{int64(s.pin), int64(s.loop), int64(s.streak), int64(s.calm), int64(s.lastScore), int64(s.msgCount), int64(s.askHash), s.askAt, s.seenAt} {
		b = append(b, '|')
		b = strconv.AppendInt(b, n, 10)
	}
	return b
}

// smartSessionDecode parses a stored value; a malformed one is a miss.
func smartSessionDecode(b []byte) (smartSession, bool) {
	if len(b) == 0 || len(b) >= sessionValueMax {
		return smartSession{}, false
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != sessionFieldCount || parts[0] != sessionValueTag {
		return smartSession{}, false
	}
	var n [sessionFieldCount - 1]int64
	for i := 1; i < sessionFieldCount; i++ {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil || v < 0 {
			return smartSession{}, false
		}
		n[i-1] = v
	}
	return smartSession{
		pin: int(n[0]), loop: int(n[1]), streak: int(n[2]), calm: int(n[3]), lastScore: int(n[4]),
		msgCount: int(n[5]), askHash: int(n[6]), askAt: n[7], seenAt: n[8], found: true,
	}, true
}

// smartSessionMerge returns the newer of two views of one session, so a decay recorded
// elsewhere is not undone. Equal times keep the higher pin.
func smartSessionMerge(a, b smartSession) smartSession {
	switch {
	case b.seenAt > a.seenAt:
		return b
	case b.seenAt == a.seenAt && b.pin > a.pin:
		return b
	}
	return a
}

// smartSessionRaisePin merges two views of one session and keeps the higher pin of the two.
func smartSessionRaisePin(a, b smartSession) smartSession {
	out := smartSessionMerge(a, b)
	out.pin = smartMaxInt(a.pin, b.pin)
	return out
}

func smartCut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func smartMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func smartMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// smartSessionHash is the conversation identity: only this digest is ever stored.
func smartSessionHash(router, userScope, clientID, system string) string {
	h := sha256.New()
	for _, part := range []string{router, userScope, clientID, smartCut(system, sessionSystemKey)} {
		_, _ = io.WriteString(h, part)
		_, _ = io.WriteString(h, sessionSep)
	}
	return hex.EncodeToString(h.Sum(nil)[:sessionHashBytes])
}

func smartStringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return smartCut(s, sessionIDCap)
}

// smartSessionClientID returns the client's own session id: metadata.user_id, prompt_cache_key, then the header.
func smartSessionClientID(req map[string]any, hdr http.Header, header string) string {
	if md, ok := req["metadata"].(map[string]any); ok {
		if id := smartStringField(md, "user_id"); id != "" {
			return id
		}
	}
	if id := smartStringField(req, "prompt_cache_key"); id != "" {
		return id
	}
	if hdr != nil {
		return smartCut(hdr.Get(header), sessionIDCap)
	}
	return ""
}

// smartFirstUserText returns the text of the first user message: stable across the turns of one conversation.
func smartFirstUserText(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		if text, _, _ := smartContentText(msg["content"]); text != "" {
			return text
		}
	}
	return ""
}

func smartUserScopeID(scopes []limitScope) string {
	for i := 0; i < len(scopes); i++ {
		if scopes[i].kind == "user" {
			return scopes[i].id
		}
	}
	return ""
}

// smartCallerID names the caller for a session key: the user scope, else a digest of the
// presented API key, so callers without a user scope never share a pin. Never the raw key.
func smartCallerID(scopes []limitScope, hdr http.Header) string {
	if id := smartUserScopeID(scopes); id != "" {
		return id
	}
	if hdr == nil {
		return ""
	}
	key, ok := presentedKeyFromHeader(hdr)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "key" + sessionSep + hex.EncodeToString(sum[:sessionHashBytes])
}

// smartAskHash fingerprints the newest ask by its prose and paste sizes; 0 when no ask was found.
func smartAskHash(f *smartFeatures) int {
	if !f.found {
		return 0
	}
	return (smartTextHash(f.lower) + f.codeBytes*31 + f.logBytes*17) & 0x7fffffff
}

// smartSessionIdle drops a session that sat unused past the idle limit.
func smartSessionIdle(cfg *smartSessionCfg, s smartSession, now int64) smartSession {
	if s.found && now-s.seenAt > int64(cfg.idle) {
		return smartSession{}
	}
	return s
}

// smartSessionRoute applies a session to a scored ask. It returns the rank, the reason and
// the next state; the caller sets the final pin and loop after floors and keyword rules.
func smartSessionRoute(cfg *smartSessionCfg, top int, f *smartFeatures, sess smartSession, v smartVerdict, now int64, askHash int) (int, string, smartSession) {
	next := sess
	next.found = true
	next.seenAt = now
	next.msgCount = f.msgCount
	sameAsk := f.found && sess.found && askHash == sess.askHash

	if f.toolLoopTurn {
		streak := 0
		if f.toolErr {
			streak = sess.streak + 1
		}
		rank := smartMaxInt(sess.loop, sess.pin)
		reason := smartReasonLoop
		if streak >= cfg.toolStreak {
			rank = smartMinInt(rank+1, top)
			streak = 0
			next.calm = 0
			reason = smartReasonBump
		}
		if !sess.found {
			rank, reason = v.rank, smartReasonScore
		} else if f.found && !sameAsk && v.rank > rank {
			rank, reason = v.rank, smartReasonScore
		}
		next.streak = streak
		if f.found && !sameAsk {
			next.askHash = askHash
			next.askAt = now
		}
		return rank, reason, next
	}

	rank, reason := v.rank, smartReasonScore
	pin, calm := sess.pin, sess.calm
	newAsk := f.found && !sameAsk
	if newAsk && pin > 0 {
		if rank < pin {
			calm++
			if calm >= cfg.decayAsks {
				pin--
				calm = 0
			}
		} else {
			calm = 0
		}
	}
	bumped := false
	if sameAsk && f.msgCount <= sess.msgCount && now-sess.askAt <= int64(cfg.regen) {
		pin = smartMinInt(smartMaxInt(pin, sess.loop)+1, top)
		calm = 0
		bumped = true
	}
	if pin > rank {
		rank, reason = pin, smartReasonSession
		if bumped {
			reason = smartReasonBump
		}
	}
	next.pin, next.calm, next.streak, next.lastScore = pin, calm, 0, v.score
	if f.found {
		next.askHash = askHash
		if newAsk || !sess.found {
			next.askAt = now
		}
	}
	return rank, reason, next
}

// smartSessionOutcome raises the pin after a request finished: an escalation pins the tier
// that served it, a bad answer pins one tier above the tier that gave it.
func smartSessionOutcome(s smartSession, firstRank, servedRank, top int, served, bad bool) (smartSession, bool) {
	changed := false
	if served && servedRank > firstRank && servedRank > s.pin {
		s.pin = servedRank
		changed = true
	}
	if bad {
		if p := smartMinInt(firstRank+1, top); p > s.pin {
			s.pin = p
			changed = true
		}
	}
	if changed {
		s.calm = 0
	}
	return s, changed
}

// routerStateStore is the slice of the Redis client the session store needs.
type routerStateStore interface {
	setEx(key string, val []byte, ttl time.Duration) error
	getBytes(key string) ([]byte, bool, error)
}

type sessionEntry struct {
	key string
	st  smartSession
}

// sessionLRU is the bounded in-process session map. It is the whole store without Redis.
type sessionLRU struct {
	items map[string]*list.Element
	order *list.List
	mu    sync.Mutex
}

func newSessionLRU() *sessionLRU {
	return &sessionLRU{items: make(map[string]*list.Element), order: list.New()}
}

func (l *sessionLRU) get(key string) (smartSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.items[key]
	if !ok {
		return smartSession{}, false
	}
	l.order.MoveToFront(el)
	entry, _ := el.Value.(*sessionEntry)
	return entry.st, true
}

func (l *sessionLRU) put(key string, st smartSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.items[key]; ok {
		entry, _ := el.Value.(*sessionEntry)
		entry.st = st
		l.order.MoveToFront(el)
		return
	}
	l.items[key] = l.order.PushFront(&sessionEntry{key: key, st: st})
	for l.order.Len() > sessionLRUSize {
		oldest := l.order.Back()
		entry, _ := oldest.Value.(*sessionEntry)
		delete(l.items, entry.key)
		l.order.Remove(oldest)
	}
}

// snapshot returns every stored state, for tests that inspect what is retained.
func (l *sessionLRU) snapshot() map[string]smartSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]smartSession, len(l.items))
	for k, el := range l.items {
		entry, _ := el.Value.(*sessionEntry)
		out[k] = entry.st
	}
	return out
}

// routerSessions reads and writes session state: the LRU first, then one Redis GET.
// A Redis error is a miss and opens a short down-latch; it never fails a request.
type routerSessions struct {
	store routerStateStore
	lru   *sessionLRU
	spawn func(func())
	nowFn func() time.Time
	logf  func(format string, args ...any)

	lastLogAt   time.Time
	lastFailure time.Time
	mu          sync.Mutex
}

func newRouterSessions(store routerStateStore, spawn func(func()), nowFn func() time.Time, logf func(format string, args ...any)) *routerSessions {
	return &routerSessions{store: store, lru: newSessionLRU(), spawn: spawn, nowFn: nowFn, logf: logf}
}

func sessionRedisKey(router, hash string) string {
	return sessionKeyPrefix + router + ":" + hash
}

func (rs *routerSessions) latched(now time.Time) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return !rs.lastFailure.IsZero() && now.Sub(rs.lastFailure) < storeDownLatchFor
}

func (rs *routerSessions) recordFailure(err error, now time.Time) {
	rs.mu.Lock()
	rs.lastFailure = now
	shouldLog := now.Sub(rs.lastLogAt) >= storeErrorLogEvery
	if shouldLog {
		rs.lastLogAt = now
	}
	rs.mu.Unlock()
	if shouldLog && rs.logf != nil {
		rs.logf("router sessions: redis error, treating as a session miss: %v", err)
	}
}

// get returns the freshest known state; found is false on a miss.
func (rs *routerSessions) get(key string) smartSession {
	mem, memOK := rs.lru.get(key)
	if rs.store == nil {
		return mem
	}
	now := rs.nowFn()
	if rs.latched(now) {
		return mem
	}
	raw, ok, err := rs.store.getBytes(key)
	if err != nil {
		rs.recordFailure(err, now)
		return mem
	}
	if !ok {
		return mem
	}
	remote, decoded := smartSessionDecode(raw)
	switch {
	case !decoded:
		return mem
	case !memOK:
		return remote
	}
	return smartSessionMerge(mem, remote)
}

// put stores the state in the LRU at once and in Redis off the request path.
func (rs *routerSessions) put(key string, st smartSession, ttl time.Duration) {
	rs.lru.put(key, st)
	if rs.store == nil || rs.spawn == nil {
		return
	}
	val := smartSessionEncode(st)
	rs.spawn(func() {
		if rs.latched(rs.nowFn()) {
			return
		}
		if err := rs.store.setEx(key, val, ttl); err != nil {
			rs.recordFailure(err, rs.nowFn())
		}
	})
}

// smartSessionRun carries one request's session through classification, planning and its outcome.
type smartSessionRun struct {
	rt    *smartRouter
	key   string
	sess  smartSession
	next  smartSession
	now   int64
	dirty bool
}

func (r *smartSessionRun) hint() smartSessionHint {
	return smartSessionHint{lastScore: r.sess.lastScore, found: r.sess.found}
}

// finish records the state the request leaves behind: the pin only rises to the final rank here.
func (r *smartSessionRun) finish(next smartSession, rank int) {
	next.loop = rank
	if rank > next.pin {
		next.pin = rank
	}
	r.next = next
	r.dirty = true
}

// open derives the session key and loads its state with at most one Redis GET.
func (rs *routerSessions) open(rt *smartRouter, req map[string]any, f *smartFeatures, hdr http.Header, scopes []limitScope) *smartSessionRun {
	now := rs.nowFn().Unix()
	client := smartSessionClientID(req, hdr, rt.session.header)
	if client == "" {
		client = smartCut(f.system, sessionDerivedSys) + sessionSep + smartCut(smartFirstUserText(req), sessionDerivedUser)
	}
	key := sessionRedisKey(rt.name, smartSessionHash(rt.name, smartCallerID(scopes, hdr), client, f.system))
	sess := smartSessionIdle(&rt.session, rs.get(key), now)
	if top := len(rt.tiers) - 1; sess.pin > top || sess.loop > top {
		sess.pin, sess.loop = smartMinInt(sess.pin, top), smartMinInt(sess.loop, top)
	}
	return &smartSessionRun{rt: rt, key: key, sess: sess, now: now}
}

// classifyWithSession classifies the request, with session state when the router uses it.
func (g *Gateway) classifyWithSession(ctx context.Context, rt *smartRouter, req map[string]any, endpoint string, hdr http.Header, scopes []limitScope) (smartClassification, *smartSessionRun) {
	var assist smartAssist
	if rt.decision != nil {
		assist = func(f *smartFeatures) decisionAnswer {
			return g.askDecisionModel(ctx, rt, decisionState(f.sysHead, f.ask))
		}
	}
	if !rt.session.active() || g.routerSessions == nil {
		if assist == nil {
			return classifySmart(rt, req, endpoint), nil
		}
		f := extractSmartV2(req, endpoint)
		return classifySmartWith(rt, req, endpoint, &f, nil, assist), nil
	}
	f := extractSmartV2(req, endpoint)
	sc := g.routerSessions.open(rt, req, &f, hdr, scopes)
	cls := classifySmartWith(rt, req, endpoint, &f, sc, assist)
	return cls, sc
}

// storeSession saves the state a classification left behind.
func (g *Gateway) storeSession(sc *smartSessionRun) {
	if sc != nil && sc.dirty {
		g.routerSessions.put(sc.key, sc.next, time.Duration(sc.rt.session.ttl)*time.Second)
	}
}

func (rt *smartRouter) rankOf(tier string) int {
	for i := 0; i < len(rt.tiers); i++ {
		if rt.tiers[i].name == tier {
			return i
		}
	}
	return -1
}

// onRouterOutcome feeds a finished request back into its session.
func (g *Gateway) onRouterOutcome(plan *routerPlan, attempt int, served, bad bool) {
	sc := plan.sess
	if sc == nil || !sc.dirty || g.routerSessions == nil || !sc.rt.session.live || plan.fallbackAt > 0 {
		return
	}
	rt := sc.rt
	first, servedRank := rt.rankOf(plan.entries[0].tier), rt.rankOf(plan.entries[attempt].tier)
	if first < 0 || servedRank < 0 {
		return
	}
	st := sc.next
	if mem, ok := g.routerSessions.lru.get(sc.key); ok {
		st = smartSessionRaisePin(mem, st)
	}
	st, changed := smartSessionOutcome(st, first, servedRank, len(rt.tiers)-1, served, bad)
	if changed {
		g.routerSessions.put(sc.key, st, time.Duration(rt.session.ttl)*time.Second)
	}
}

// warnRouterSessions notes session settings that do nothing or stay per instance.
func (g *Gateway) warnRouterSessions() {
	names := make([]string, 0, len(g.registry.routers))
	for name := range g.registry.routers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := g.registry.routers[name]
		switch {
		case rt.sessionEnabled && !rt.session.active():
			g.warnf("config: smartRouters %q: session.enabled has no effect without classifier v2 or shadowClassifier v2", name)
		case rt.session.active() && g.redisClient == nil:
			g.warnf("config: smartRouters %q: sessions are kept per gateway instance because redis is not configured", name)
		}
	}
}
