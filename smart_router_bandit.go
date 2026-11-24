package traefikllmgateway

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Adaptive selection orders the candidates of one tier with a Thompson bandit.
// It only permutes a tier's own list: it never adds or removes a candidate and
// never moves one across tiers, so the set of models a request can reach is the
// same as with ordered selection.

const (
	defaultAdaptiveExplorePct = 5
	maxAdaptiveExplorePct     = 50
	defaultAdaptiveWindow     = 6
	maxAdaptiveWindow         = 12
	defaultAdaptiveLatencyPct = 30
	maxAdaptiveLatencyPct     = 100

	banditMetricPrefix = "bd"
	banditArmsMax      = 256
	banditMilli        = 1000
	// banditPriorA and banditPriorB give a new arm an optimistic Beta(2,1) prior, so it gets tried.
	banditPriorA = 2.0
	banditPriorB = 1.0
	// banditLoadPenalty is the share of its score an arm loses at full utilisation.
	banditLoadPenalty = 0.5
	banditTTFBAlpha   = 0.1
	banditCounterTTL  = 12 * time.Hour
	// banditSlowPenaltyCap is the ttfb ratio above the tier average at which the latency penalty stops growing.
	banditSlowPenaltyCap = 1.0
)

// SmartRouterAdaptiveConfig tunes adaptive selection. It has no effect unless selection is "adaptive".
type SmartRouterAdaptiveConfig struct {
	// ExplorePct is the share of requests that put a random unsaturated candidate first; nil means 5.
	ExplorePct *int `json:"explorePct,omitempty"`
	// LatencyWeightPct is the share of the reward a slow first byte can take away; nil means 30.
	LatencyWeightPct *int `json:"latencyWeightPct,omitempty"`
	// WindowHours is how many hourly buckets of reward counters count; 0 means 6.
	WindowHours int `json:"windowHours,omitempty"`
}

// smartAdaptiveCfg is the validated adaptive config with defaults applied.
type smartAdaptiveCfg struct {
	explorePct, windowHours, latencyPct int
}

func compileSmartAdaptive(router string, cfg *SmartRouterAdaptiveConfig) (smartAdaptiveCfg, error) {
	c := smartAdaptiveCfg{explorePct: defaultAdaptiveExplorePct, windowHours: defaultAdaptiveWindow, latencyPct: defaultAdaptiveLatencyPct}
	if cfg == nil {
		return c, nil
	}
	if cfg.ExplorePct != nil {
		c.explorePct = *cfg.ExplorePct
	}
	if cfg.LatencyWeightPct != nil {
		c.latencyPct = *cfg.LatencyWeightPct
	}
	c.windowHours = sessionOrDefault(cfg.WindowHours, defaultAdaptiveWindow)
	ranges := []struct {
		name      string
		v, lo, hi int
	}{
		{"explorePct", c.explorePct, 0, maxAdaptiveExplorePct},
		{"windowHours", c.windowHours, 1, maxAdaptiveWindow},
		{"latencyWeightPct", c.latencyPct, 0, maxAdaptiveLatencyPct},
	}
	for _, r := range ranges {
		if r.v < r.lo || r.v > r.hi {
			return smartAdaptiveCfg{}, fmt.Errorf("llmgateway: smartRouters: router %q: adaptive.%s must be between %d and %d, got %d", router, r.name, r.lo, r.hi, r.v)
		}
	}
	return c, nil
}

// smartArm is one candidate's learned state inside a tier. The milli counts are the
// reward sums of the window, refreshed from shared counters; the ewma is local.
type smartArm struct {
	tier, hash     string
	aMilli, nMilli int64
	ttfbEWMA       float64
}

// smartRandom is an injectable random source; its funcs run under the bandit lock.
type smartRandom struct {
	unit func() float64
	norm func() float64
	intn func(n int) int
}

func newSmartRandom(seed int64) *smartRandom {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // G404: load spreading, not a security decision
	return &smartRandom{unit: r.Float64, norm: r.NormFloat64, intn: r.Intn}
}

// smartBandit holds one router's arms. Arms are bounded by banditArmsMax.
type smartBandit struct {
	arms map[string]*smartArm
	rnd  *smartRandom
	cfg  smartAdaptiveCfg
	mu   sync.Mutex
}

func newSmartBandit(cfg smartAdaptiveCfg) *smartBandit {
	return &smartBandit{arms: make(map[string]*smartArm), rnd: newSmartRandom(time.Now().UnixNano()), cfg: cfg}
}

func smartArmKey(tier string, cand resolveCandidate) string {
	return tier + "\x00" + routerCandidateKey(cand)
}

func smartArmHash(cand resolveCandidate) string {
	return strconv.FormatInt(int64(smartTextHash(routerCandidateKey(cand))), 16)
}

// armLocked returns the arm for a candidate, creating it while there is room; nil when full.
func (b *smartBandit) armLocked(tier string, cand resolveCandidate) *smartArm {
	key := smartArmKey(tier, cand)
	if a := b.arms[key]; a != nil {
		return a
	}
	if len(b.arms) >= banditArmsMax {
		return nil
	}
	a := &smartArm{tier: tier, hash: smartArmHash(cand)}
	b.arms[key] = a
	return a
}

// smartSaturated reports whether a provider has no room: a known limit and every slot taken.
func smartSaturated(inFlight, limit int) bool {
	return limit > 0 && inFlight >= limit
}

// smartUtil is the used share of a provider's known limit, 0 when the limit is unknown.
func smartUtil(inFlight, limit int) float64 {
	if limit <= 0 {
		return 0
	}
	u := float64(inFlight) / float64(limit)
	if u > 1 {
		return 1
	}
	return u
}

// smartThompsonScore draws a normal approximation of the Beta(2+a, 1+n-a) posterior
// with the standard normal draw z, then discounts it for load: theta * (1 - 0.5*util^2).
func smartThompsonScore(a, n, z, util float64) float64 {
	if a > n {
		a = n
	}
	if a < 0 {
		a = 0
	}
	alpha := banditPriorA + a
	beta := banditPriorB + n - a
	sum := alpha + beta
	mean := alpha / sum
	variance := alpha * beta / (sum * sum * (sum + 1))
	theta := mean + math.Sqrt(variance)*z
	if theta < 0 {
		theta = 0
	}
	return theta * (1 - banditLoadPenalty*util*util)
}

// smartBanditReward is 0 for a bad or failed attempt, else 1 less the latency penalty:
// weightPct of the reward times how far the first byte ran over the tier average, capped at 1.
func smartBanditReward(failed bool, ttfb, tierTTFB float64, weightPct int) float64 {
	if failed {
		return 0
	}
	if ttfb <= 0 || tierTTFB <= 0 {
		return 1
	}
	over := ttfb/tierTTFB - 1
	if over < 0 {
		over = 0
	}
	if over > banditSlowPenaltyCap {
		over = banditSlowPenaltyCap
	}
	return 1 - float64(weightPct)/100*over
}

// smartBanditOrder returns the permutation of 0..len(scores)-1 to try: the explore pick
// first when set (first >= 0), then unsaturated candidates by score, saturated ones last.
func smartBanditOrder(scores []float64, saturated []bool, first int) []int {
	idx := make([]int, 0, len(scores))
	for i := range scores {
		if i != first {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(x, y int) bool {
		i, j := idx[x], idx[y]
		if saturated[i] != saturated[j] {
			return !saturated[i]
		}
		return scores[i] > scores[j]
	})
	if first >= 0 {
		idx = append([]int{first}, idx...)
	}
	return idx
}

// order scores a tier's candidates and returns them in try order.
func (b *smartBandit) order(tier string, cands []resolveCandidate, load func(provider string) (int, int)) []resolveCandidate {
	n := len(cands)
	util := make([]float64, n)
	saturated := make([]bool, n)
	var open []int
	for i := 0; i < n; i++ {
		in, lim := load(cands[i].providerName)
		util[i] = smartUtil(in, lim)
		saturated[i] = smartSaturated(in, lim)
		if !saturated[i] {
			open = append(open, i)
		}
	}
	scores := make([]float64, n)
	first := -1
	b.mu.Lock()
	for i := 0; i < n; i++ {
		a, cnt := 0.0, 0.0
		if arm := b.armLocked(tier, cands[i]); arm != nil {
			a, cnt = float64(arm.aMilli)/banditMilli, float64(arm.nMilli)/banditMilli
		}
		scores[i] = smartThompsonScore(a, cnt, b.rnd.norm(), util[i])
	}
	if len(open) > 0 && b.cfg.explorePct > 0 && b.rnd.unit()*100 < float64(b.cfg.explorePct) {
		first = open[b.rnd.intn(len(open))]
	}
	b.mu.Unlock()
	perm := smartBanditOrder(scores, saturated, first)
	out := make([]resolveCandidate, n)
	for pos, i := range perm {
		out[pos] = cands[i]
	}
	return out
}

// observe folds one attempt's first-byte time into the arm's average and returns the attempt's reward.
func (b *smartBandit) observe(tier string, cand resolveCandidate, failed bool, ttfb float64) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	arm := b.armLocked(tier, cand)
	if arm == nil {
		return smartBanditReward(failed, 0, 0, b.cfg.latencyPct)
	}
	var sum float64
	var count int
	for _, other := range b.arms {
		if other.tier == tier && other.ttfbEWMA > 0 {
			sum += other.ttfbEWMA
			count++
		}
	}
	tierTTFB := 0.0
	if count > 0 {
		tierTTFB = sum / float64(count)
	}
	reward := smartBanditReward(failed, ttfb, tierTTFB, b.cfg.latencyPct)
	if !failed && ttfb > 0 {
		if arm.ttfbEWMA == 0 {
			arm.ttfbEWMA = ttfb
		} else {
			arm.ttfbEWMA += banditTTFBAlpha * (ttfb - arm.ttfbEWMA)
		}
	}
	return reward
}

func routerBanditKeyFor(router, tier, hash, metric, bucket string) string {
	return windowKeyForBucket(kindRouter, router, banditMetricPrefix+routerKeySep+tier+routerKeySep+hash+routerKeySep+metric, windowHour, bucket)
}

// observeBandit scores one finished attempt for its arm and counts the reward off the request path.
// An attempt the client or the request itself ended says nothing about the model and is skipped.
func (g *Gateway) observeBandit(plan *routerPlan, i int, callErr error, bad bool, lat latencySample, hasLat bool) {
	if callErr != nil && !failoverEligible(callErr) {
		return
	}
	failed := callErr != nil || bad
	ttfb := 0.0
	if !failed && hasLat && lat.hasTTFB {
		ttfb = lat.ttfb.Seconds()
	}
	e := plan.entries[i]
	reward := plan.bandit.observe(e.tier, e.cand, failed, ttfb)
	if g.limiter == nil {
		return
	}
	bucket := bucketFor(g.limiter.now(), windowHour)
	hash := smartArmHash(e.cand)
	g.limiter.countAsync([]counterIncr{
		{key: routerBanditKeyFor(plan.router, e.tier, hash, "a", bucket), delta: int64(math.Round(reward * banditMilli)), ttl: banditCounterTTL, enforceTTL: enforceTTLHourWindow},
		{key: routerBanditKeyFor(plan.router, e.tier, hash, "n", bucket), delta: banditMilli, ttl: banditCounterTTL, enforceTTL: enforceTTLHourWindow},
	})
}

// banditOrder permutes one tier's candidates; the provider load comes from the concurrency learner.
func (g *Gateway) banditOrder(rt *smartRouter, tier string, cands []resolveCandidate) []resolveCandidate {
	if len(cands) < 2 {
		return cands
	}
	return rt.bandit.order(tier, cands, g.providerLoad)
}

// providerLoad returns a provider's in-flight count and known limit; both are 0 without concurrency tracking.
func (g *Gateway) providerLoad(provider string) (int, int) {
	if g.concurrency == nil {
		return 0, 0
	}
	return g.concurrency.load(provider)
}

// refreshBandit reloads every arm's reward window from the shared counters.
func (g *Gateway) refreshBandit(rt *smartRouter, now time.Time) {
	b := rt.bandit
	if b == nil || g.limiter == nil {
		return
	}
	b.mu.Lock()
	arms := make([]*smartArm, 0, len(b.arms))
	for _, a := range b.arms {
		arms = append(arms, a)
	}
	b.mu.Unlock()
	if len(arms) == 0 {
		return
	}
	buckets := recentHourBuckets(now, b.cfg.windowHours)
	names := make([]string, 0, len(arms)*len(buckets)*2)
	for _, a := range arms {
		for _, bucket := range buckets {
			names = append(names, routerBanditKeyFor(rt.name, a.tier, a.hash, "a", bucket), routerBanditKeyFor(rt.name, a.tier, a.hash, "n", bucket))
		}
	}
	vals, ok := g.limiter.chunkedGetMulti(names, usageModelsChunkKeys)
	if !ok {
		return
	}
	sums := make([][2]int64, len(arms))
	pos := 0
	for i := range arms {
		for range buckets {
			sums[i][0] += vals[pos]
			sums[i][1] += vals[pos+1]
			pos += 2
		}
	}
	b.mu.Lock()
	for i, a := range arms {
		a.aMilli, a.nMilli = sums[i][0], sums[i][1]
	}
	b.mu.Unlock()
}
