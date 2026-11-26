package traefikllmgateway

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Calibration moves a tier threshold from outcome counters. A boundary lowers
// its threshold (more requests go up) when the tier below it keeps giving bad
// answers. The controller runs off the request path on a background ticker and
// reads shared hourly counters; the request path reads one small snapshot.

const (
	smartCalibOff    = "off"
	smartCalibShadow = "shadow"
	smartCalibLive   = "live"

	defaultCalibMaxLower     = 150
	maxCalibMaxLower         = 300
	maxCalibMaxRaise         = 100
	defaultCalibStep         = 25
	maxCalibStep             = 100
	defaultCalibTargetBadPct = 3
	maxCalibTargetBadPct     = 50
	defaultCalibMinSamples   = 200
	minCalibMinSamples       = 50
	calibRelaxMinSamples     = 500
	calibRelaxBadPct         = 1
	calibRelaxStep           = 10
	calibWindowBuckets       = 24
	calibWilsonZ             = 1.96
	calibMetricPrefix        = "cal"

	routerLearnInterval = 60 * time.Second
	// routerLearnIdleStop ends the ticker of a gateway that routes nothing, such as one a config reload replaced.
	routerLearnIdleStop = 10 * time.Minute
)

// SmartRouterCalibrationConfig tunes threshold calibration. Zero values mean the default.
type SmartRouterCalibrationConfig struct {
	// MaxLower caps how far a threshold may drop; nil means 150, shrunk to fit the smallest tier gap.
	MaxLower *int `json:"maxLower,omitempty"`
	// Mode is "off" (default), "shadow" (computes and exports offsets only) or "live" (applies them).
	Mode string `json:"mode,omitempty"`
	// MaxRaise caps how far a threshold may rise above its configured value; 0 means never.
	MaxRaise int `json:"maxRaise,omitempty"`
	// Step is the offset added per recompute while a tier is too often bad.
	Step int `json:"step,omitempty"`
	// TargetBadPct is the bad-answer share, in percent, that the tier must stay under.
	TargetBadPct int `json:"targetBadPct,omitempty"`
	// MinSamples is the request count a boundary needs before it moves.
	MinSamples int `json:"minSamples,omitempty"`
	// Freeze is a kill switch: every offset is zero and nothing moves.
	Freeze bool `json:"freeze,omitempty"`
}

// smartCalibCfg is the validated calibration config with defaults applied.
type smartCalibCfg struct {
	mode                                               string
	maxLower, maxRaise, step, targetBadPct, minSamples int
	freeze                                             bool
}

func (c *smartCalibCfg) enabled() bool { return c.mode == smartCalibShadow || c.mode == smartCalibLive }

// smartCalibState holds one router's offsets, indexed by tier rank. The offset of
// tier i lowers its threshold; tier 0 has none.
type smartCalibState struct {
	delta []int
	mu    sync.RWMutex
}

func (s *smartCalibState) get(rank int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rank < 0 || rank >= len(s.delta) {
		return 0
	}
	return s.delta[rank]
}

func (s *smartCalibState) snapshot() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]int(nil), s.delta...)
}

func (s *smartCalibState) set(delta []int) {
	s.mu.Lock()
	s.delta = delta
	s.mu.Unlock()
}

// smartDefaultMaxLower shrinks the default cap so it fits tightly spaced tiers; an explicit maxLower is still validated as given.
func smartDefaultMaxLower(tiers []smartTier, maxRaise int) int {
	limit := defaultCalibMaxLower
	for i := 1; i < len(tiers); i++ {
		if room := tiers[i].minScore - tiers[i-1].minScore - maxRaise - 1; room < limit {
			limit = room
		}
	}
	if limit < 0 {
		limit = 0
	}
	return limit
}

func compileSmartCalibration(router string, cfg *SmartRouterCalibrationConfig, tiers []smartTier) (smartCalibCfg, *smartCalibState, error) {
	if cfg == nil {
		return smartCalibCfg{mode: smartCalibOff}, nil, nil
	}
	c := smartCalibCfg{
		mode:         cfg.Mode,
		maxLower:     defaultCalibMaxLower,
		maxRaise:     cfg.MaxRaise,
		step:         sessionOrDefault(cfg.Step, defaultCalibStep),
		targetBadPct: sessionOrDefault(cfg.TargetBadPct, defaultCalibTargetBadPct),
		minSamples:   sessionOrDefault(cfg.MinSamples, defaultCalibMinSamples),
		freeze:       cfg.Freeze,
	}
	switch c.mode {
	case "":
		c.mode = smartCalibOff
	case smartCalibOff, smartCalibShadow, smartCalibLive:
	default:
		return smartCalibCfg{}, nil, fmt.Errorf("llmgateway: smartRouters: router %q: calibration.mode %q must be %q, %q or %q", router, cfg.Mode, smartCalibOff, smartCalibShadow, smartCalibLive)
	}
	if cfg.MaxLower != nil {
		c.maxLower = *cfg.MaxLower
	} else {
		c.maxLower = smartDefaultMaxLower(tiers, c.maxRaise)
	}
	ranges := []struct {
		name      string
		v, lo, hi int
	}{
		{"maxLower", c.maxLower, 0, maxCalibMaxLower},
		{"maxRaise", c.maxRaise, 0, maxCalibMaxRaise},
		{"step", c.step, 1, maxCalibStep},
		{"targetBadPct", c.targetBadPct, 1, maxCalibTargetBadPct},
		{"minSamples", c.minSamples, minCalibMinSamples, math.MaxInt32},
	}
	for _, r := range ranges {
		if r.v < r.lo || r.v > r.hi {
			return smartCalibCfg{}, nil, fmt.Errorf("llmgateway: smartRouters: router %q: calibration.%s must be between %d and %d, got %d", router, r.name, r.lo, r.hi, r.v)
		}
	}
	// Offsets of two neighbouring tiers may move toward each other by maxLower + maxRaise.
	for i := 1; i < len(tiers); i++ {
		if gap := tiers[i].minScore - tiers[i-1].minScore; c.maxLower+c.maxRaise >= gap {
			return smartCalibCfg{}, nil, fmt.Errorf("llmgateway: smartRouters: router %q: calibration.maxLower %d plus maxRaise %d must stay below the %d gap between tiers %q and %q", router, c.maxLower, c.maxRaise, gap, tiers[i-1].name, tiers[i].name)
		}
	}
	if !c.enabled() {
		return c, nil, nil
	}
	return c, &smartCalibState{delta: make([]int, len(tiers))}, nil
}

// tierMin is the threshold the router applies to tier i: its configured minScore
// less the live calibration offset.
func (rt *smartRouter) tierMin(i int) int {
	m := rt.tiers[i].minScore
	if rt.calibState != nil && rt.calib.mode == smartCalibLive && !rt.calib.freeze {
		m -= rt.calibState.get(i)
	}
	return m
}

func smartClampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// smartWilsonBounds returns the Wilson score interval of bad out of n at z.
func smartWilsonBounds(bad, n int64, z float64) (float64, float64) {
	if n <= 0 {
		return 0, 1
	}
	nf := float64(n)
	p := float64(bad) / nf
	z2 := z * z
	denom := 1 + z2/nf
	centre := p + z2/(2*nf)
	half := z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	lo := (centre - half) / denom
	hi := (centre + half) / denom
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

// smartCalibStep moves one boundary's offset by at most one step from the
// bad count of the tier below it. Evidence must clear the Wilson bound, so a
// small sample never moves a threshold.
func smartCalibStep(cur int, bad, n int64, c *smartCalibCfg) int {
	if c.freeze {
		return 0
	}
	lo, hi := smartWilsonBounds(bad, n, calibWilsonZ)
	next := cur
	switch {
	case n >= int64(c.minSamples) && lo*100 > float64(c.targetBadPct):
		next = cur + c.step
	case n >= calibRelaxMinSamples && hi*100 < calibRelaxBadPct:
		next = cur - calibRelaxStep
	}
	return smartClampInt(next, -c.maxRaise, c.maxLower)
}

func routerCalibKeyFor(router, tier, metric, bucket string) string {
	return windowKeyForBucket(kindRouter, router, calibMetricPrefix+routerKeySep+tier+routerKeySep+metric, windowHour, bucket)
}

// countCalibration counts a finished score-routed request against the tier it first tried.
// Failures that are capacity, not answer quality, are left out.
func (g *Gateway) countCalibration(plan *routerPlan, attempt int, served, bad bool) {
	if !plan.calibrate || g.limiter == nil || routerRequestReason(plan) != smartReasonScore {
		return
	}
	tier := plan.entries[0].tier
	if plan.entries[attempt].tier != tier || (!served && !bad) {
		return
	}
	bucket := bucketFor(g.limiter.now(), windowHour)
	entries := []counterIncr{{key: routerCalibKeyFor(plan.router, tier, "n", bucket), delta: 1, ttl: hourWindowTTL, enforceTTL: enforceTTLHourWindow}}
	if bad {
		entries = append(entries, counterIncr{key: routerCalibKeyFor(plan.router, tier, "bad", bucket), delta: 1, ttl: hourWindowTTL, enforceTTL: enforceTTLHourWindow})
	}
	g.limiter.countAsync(entries)
}

// recentHourBuckets lists the hourly bucket names ending at now, newest first.
func recentHourBuckets(now time.Time, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = bucketFor(now.Add(-time.Duration(i)*time.Hour), windowHour)
	}
	return out
}

// refreshCalibration recomputes one router's offsets from the last 24 hourly buckets.
func (g *Gateway) refreshCalibration(rt *smartRouter, now time.Time) {
	st := rt.calibState
	if st == nil || g.limiter == nil {
		return
	}
	cur := st.snapshot()
	next := make([]int, len(cur))
	if rt.calib.freeze {
		st.set(next)
		return
	}
	buckets := recentHourBuckets(now, calibWindowBuckets)
	var names []string
	for b := 1; b < len(rt.tiers); b++ {
		for _, bucket := range buckets {
			names = append(names, routerCalibKeyFor(rt.name, rt.tiers[b-1].name, "n", bucket), routerCalibKeyFor(rt.name, rt.tiers[b-1].name, "bad", bucket))
		}
	}
	if len(names) == 0 {
		return
	}
	vals, ok := g.limiter.chunkedGetMulti(names, usageModelsChunkKeys)
	if !ok {
		return
	}
	pos := 0
	for b := 1; b < len(rt.tiers); b++ {
		var n, bad int64
		for range buckets {
			n += vals[pos]
			bad += vals[pos+1]
			pos += 2
		}
		next[b] = smartCalibStep(cur[b], bad, n, &rt.calib)
	}
	st.set(next)
}

// routerLearner runs the background refresh of calibration offsets and bandit
// snapshots. It starts on the first routed request, stops on Close, and stops
// by itself after routerLearnIdleStop without a request.
type routerLearner struct {
	lastUse time.Time
	stop    chan struct{}
	// done closes when the refresh goroutine returns.
	done    chan struct{}
	every   time.Duration
	mu      sync.Mutex
	running bool
	closed  bool
}

func newRouterLearner() *routerLearner {
	return &routerLearner{every: routerLearnInterval}
}

// learns reports whether the router has anything for the background refresh to do.
func (rt *smartRouter) learns() bool {
	return rt.calibState != nil || rt.bandit != nil
}

// touchRouterLearning notes a routed request and starts the refresh goroutine if it is not running.
func (g *Gateway) touchRouterLearning(rt *smartRouter) {
	l := g.routerLearn
	if l == nil || (!rt.learns() && !g.sharesCalibration(rt)) {
		return
	}
	l.mu.Lock()
	l.lastUse = time.Now()
	start := !l.running && !l.closed
	if start {
		l.running = true
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
	}
	stop, done := l.stop, l.done
	l.mu.Unlock()
	if start {
		go g.runRouterLearning(l, stop, done) //nolint:gosec // G118: bounded by Close and routerLearnIdleStop, one goroutine per gateway
	}
}

func (g *Gateway) runRouterLearning(l *routerLearner, stop, done chan struct{}) {
	// No request goroutine can catch a panic here, and it would end the whole Traefik process.
	defer func() {
		if rec := recover(); rec != nil {
			g.errorf("router learning refresh panicked: %v", rec)
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
		}
		close(done)
	}()
	t := time.NewTicker(l.every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			l.mu.Lock()
			idle := time.Since(l.lastUse) > routerLearnIdleStop
			if idle {
				l.running = false
			}
			l.mu.Unlock()
			if idle {
				return
			}
			g.refreshRouterLearning(g.limiter.now())
		}
	}
}

// closeRouterLearning stops the refresh goroutine for good.
func (g *Gateway) closeRouterLearning() {
	l := g.routerLearn
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.running {
		l.running = false
		close(l.stop)
	}
}

// refreshRouterLearning recomputes every router's learned state once.
func (g *Gateway) refreshRouterLearning(now time.Time) {
	names := make([]string, 0, len(g.registry.routers))
	for name := range g.registry.routers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := g.registry.routers[name]
		if rt.calibState != nil {
			g.refreshCalibration(rt, now)
		}
		if rt.bandit != nil {
			g.refreshBandit(rt, now)
		}
	}
	g.refreshSharedCalibration(now)
}

// sharesCalibration reports whether rt's token estimate reads the fleet counters.
func (g *Gateway) sharesCalibration(rt *smartRouter) bool {
	return rt.contextEscalation && g.tokenCalib.sharing()
}

// calibrationTargets lists the upstream (provider, model) pairs the calibrating routers can send to.
func (g *Gateway) calibrationTargets() []smartCalibKey {
	seen := make(map[smartCalibKey]bool)
	var out []smartCalibKey
	grp := &group{}
	for _, rt := range g.registry.routers {
		if !g.sharesCalibration(rt) {
			continue
		}
		for _, tier := range rt.tiers {
			for _, target := range tier.models {
				var cands []resolveCandidate
				if g.failover.enabled {
					primary, extra, err := g.registry.resolveWithCandidates(target, grp)
					if err != nil {
						continue
					}
					cands = append([]resolveCandidate{primary}, extra...)
				} else {
					primary, err := g.registry.resolvePrimaryOnly(target, grp)
					if err != nil {
						continue
					}
					cands = []resolveCandidate{primary}
				}
				for _, cand := range cands {
					k := smartCalibKey{provider: cand.providerName, model: cand.upstreamModel}
					if !seen[k] {
						seen[k] = true
						out = append(out, k)
					}
				}
			}
		}
	}
	return out
}

// refreshSharedCalibration reads the fleet counters of every calibrating router's targets and keeps one bytes-per-token ratio per model.
// A failed read keeps the previous ratios.
func (g *Gateway) refreshSharedCalibration(now time.Time) {
	if !g.tokenCalib.sharing() || g.limiter == nil {
		return
	}
	targets := g.calibrationTargets()
	if len(targets) == 0 {
		return
	}
	buckets := make([]string, calibFleetDays)
	for i := range buckets {
		buckets[i] = bucketFor(now.Add(-time.Duration(i)*24*time.Hour), windowDay)
	}
	names := make([]string, 0, len(targets)*len(buckets)*3)
	for _, k := range targets {
		for _, bucket := range buckets {
			names = append(names, calibFleetKey(k, calibMetricBytes, bucket), calibFleetKey(k, calibMetricToken, bucket), calibFleetKey(k, calibMetricCount, bucket))
		}
	}
	vals, ok := g.limiter.chunkedGetMulti(names, usageModelsChunkKeys)
	if !ok {
		return
	}
	fleet := make(map[smartCalibKey]smartCalibStat, len(targets))
	pos := 0
	for _, k := range targets {
		var bytes, tokens, n int64
		for range buckets {
			bytes += vals[pos]
			tokens += vals[pos+1]
			n += vals[pos+2]
			pos += 3
		}
		if bytes > 0 && tokens > 0 {
			fleet[k] = smartCalibStat{ratio: float64(bytes) / float64(tokens), samples: int(n)}
		}
	}
	g.tokenCalib.setFleet(fleet)
}

// warnRouterLearning notes learning settings that stay per instance.
func (g *Gateway) warnRouterLearning() {
	names := make([]string, 0, len(g.registry.routers))
	for name := range g.registry.routers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := g.registry.routers[name]
		if rt.adaptiveSet && rt.bandit == nil {
			g.warnf("config: smartRouters %q: adaptive has no effect unless selection is %q", name, smartSelectionAdaptive)
		}
		if rt.learns() && g.redisClient == nil {
			g.warnf("config: smartRouters %q: calibration and adaptive selection learn per gateway instance because redis is not configured", name)
		}
	}
}

// writeRouterCalibrationMetrics exports each calibrating router's offsets as a gauge, in shadow and live mode.
func (g *Gateway) writeRouterCalibrationMetrics(m *metricWriter) {
	names := make([]string, 0, len(g.registry.routers))
	for name, rt := range g.registry.routers {
		if rt.calibState != nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	m.family("llmgateway_router_calibration_delta", "gauge",
		"Points a smart router tier threshold sits below its configured minScore, by router, tier and calibration mode (shadow offsets are not applied)."+aggregationNotePerProcess)
	for _, name := range names {
		rt := g.registry.routers[name]
		for i := 1; i < len(rt.tiers); i++ {
			m.sampleInt("llmgateway_router_calibration_delta",
				[]metricLabel{{"router", name}, {"tier", rt.tiers[i].name}, {"mode", rt.calib.mode}}, int64(rt.calibState.get(i)))
		}
	}
}
