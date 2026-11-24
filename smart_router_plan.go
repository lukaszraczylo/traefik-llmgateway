package traefikllmgateway

import (
	"fmt"
	"hash/fnv"
	"net/http"
)

const (
	headerRoutedModel = "X-Llmgw-Routed-Model"
	headerRouterTier  = "X-Llmgw-Router-Tier"
	// headerRouterReason is set only on routers whose sessions change live routing.
	headerRouterReason = "X-Llmgw-Router-Reason"
	// headerRouterDowngraded names the tier a saturated router started below.
	headerRouterDowngraded = "X-Llmgw-Router-Downgraded"
	smartReasonSaturated   = "saturated"
	routerOwnedBy          = "smart-router"
)

// routerTargetError is returned when a router cannot serve a request at all.
// Callers detect it by type assertion; errors.As panics under yaegi.
type routerTargetError struct {
	message string
	status  int
}

func (e *routerTargetError) Error() string { return e.message }

type routerPlanEntry struct {
	tier string
	cand resolveCandidate
}

// routerPlan is the ordered attempt list for one routed request.
type routerPlan struct {
	router    string
	reason    string
	startTier string
	// shadowTier is the tier the router's shadowWeights chose; empty when unset.
	shadowTier string
	// downgradedFrom is the classified tier a saturation downgrade started below; empty when not downgraded.
	downgradedFrom string
	// sess is the router's session for this request; nil when the router does not use sessions.
	sess *smartSessionRun
	// bandit is the router's bandit when selection is adaptive; the outcome of each attempt feeds it.
	bandit  *smartBandit
	entries []routerPlanEntry
	// promptBytes is the size estimate that calibration compares with reported usage.
	promptBytes int64
	hasImage    bool
	// calibrate is true when the router counts this request's outcome for threshold calibration.
	calibrate bool
	// maxTokensSet is true when the client capped the output; a length cut is then its own choice.
	maxTokensSet bool
}

func (p *routerPlan) candidates() []resolveCandidate {
	out := make([]resolveCandidate, len(p.entries))
	for i := range p.entries {
		out[i] = p.entries[i].cand
	}
	return out
}

type routerSkips struct {
	// largest is the context-skipped candidate with the biggest known window.
	largest                                       *routerPlanEntry
	denied, unresolved, vision, context, unpriced int
	largestCtx                                    int
}

func (s routerSkips) total() int {
	return s.denied + s.unresolved + s.vision + s.context + s.unpriced
}

func (s routerSkips) detail() string {
	return fmt.Sprintf("skipped: %d denied, %d unresolved, %d without vision, %d too small for the prompt, %d unpriced under a cost budget",
		s.denied, s.unresolved, s.vision, s.context, s.unpriced)
}

type routerRequestFacts struct {
	promptBytes  int64
	imageTokens  int64
	outputTokens int64
	hasImage     bool
	costBudget   bool
}

func smartTextHash(s string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32() & 0x7fffffff)
}

// planSmartRouter validates access and builds the attempt list: tiers from the
// classified rank upward, never below it. A tier never takes the last slot while
// a higher tier has a usable candidate, so escalation stays reachable.
func (g *Gateway) planSmartRouter(rt *smartRouter, req map[string]any, endpoint string, hdr http.Header, grp *group, scopes []limitScope) (*routerPlan, error) {
	if !grp.allowsModelName(rt.name) {
		return nil, errModelDenied
	}
	if endpoint == cacheEndpointEmbeddings {
		return nil, &routerTargetError{status: http.StatusBadRequest, message: fmt.Sprintf("smart router %q does not support embeddings; request an embedding model directly", rt.name)}
	}
	g.registry.noteRouterShadow(rt.name, grp)
	g.touchRouterLearning(rt)

	cls, sess := g.classifyWithSession(rt, req, endpoint, hdr, scopes)
	start, reason, scored, hasImage := cls.rank, cls.reason, cls.scoredText, cls.hasImage
	promptBytes, imageTokens := smartPromptBytes(req, endpoint)
	facts := routerRequestFacts{
		hasImage:     hasImage,
		promptBytes:  promptBytes,
		imageTokens:  imageTokens,
		outputTokens: requestedOutputTokens(req, endpoint),
		costBudget:   !g.cfg.AllowUnpricedWithCostBudget && scopesHaveCostBudget(scopes),
	}
	plan := &routerPlan{router: rt.name, reason: reason, startTier: rt.tiers[start].name, promptBytes: promptBytes, hasImage: hasImage, maxTokensSet: facts.outputTokens > 0, sess: sess, calibrate: rt.calibState != nil, bandit: rt.bandit}
	if cls.shadowRank >= 0 {
		plan.shadowTier = rt.tiers[cls.shadowRank].name
	}
	seen := make(map[string]bool)
	var skips routerSkips

	lists := make([][]resolveCandidate, len(rt.tiers))
	built := make([]bool, len(rt.tiers))
	fresh := func(rank int) []resolveCandidate {
		if !built[rank] {
			built[rank] = true
			lists[rank] = g.routerTierCandidates(rt, rank, scored, facts, grp, &skips)
		}
		out := make([]resolveCandidate, 0, len(lists[rank]))
		for _, cand := range lists[rank] {
			if !seen[routerCandidateKey(cand)] {
				out = append(out, cand)
			}
		}
		return out
	}

	if rt.satDowngrade && reason == smartReasonScore {
		if from, ok := g.saturationDowngrade(rt, cls, start, fresh); ok {
			plan.downgradedFrom = rt.tiers[from].name
			plan.startTier = rt.tiers[from-1].name
			plan.reason = smartReasonSaturated
			start = from - 1
		}
	}

	for rank := start; rank < len(rt.tiers) && len(plan.entries) < rt.maxAttempts; rank++ {
		cands := fresh(rank)
		if len(cands) == 0 {
			continue
		}
		room := rt.maxAttempts - len(plan.entries)
		take := len(cands)
		if take >= room && g.routerHigherTierUsable(rt, rank, fresh) {
			take = room - 1
		}
		if take < 1 {
			take = 1
		}
		for _, cand := range cands[:take] {
			seen[routerCandidateKey(cand)] = true
			plan.entries = append(plan.entries, routerPlanEntry{tier: rt.tiers[rank].name, cand: cand})
		}
	}

	// Context skips alone emptied the plan: the largest known window beats a 404.
	if len(plan.entries) == 0 && skips.largest != nil {
		plan.entries = append(plan.entries, *skips.largest)
	}

	if len(plan.entries) == 0 {
		g.registry.warnRouterOnce("router-empty:"+rt.name, fmt.Sprintf("model registry: smart router %q has no usable target (%s)", rt.name, skips.detail()))
		if skips.denied > 0 && skips.denied == skips.total() {
			return nil, &routerTargetError{status: http.StatusForbidden, message: "model access denied"}
		}
		return nil, &routerTargetError{status: http.StatusNotFound, message: fmt.Sprintf("smart router %q has no usable target", rt.name)}
	}
	g.storeSession(sess)
	return plan, nil
}

// saturationDowngrade decides whether a score-routed request starts one tier below its classified
// tier. It needs a lower tier that clears every floor, a score within the band above the tier's
// threshold, a tier with no free capacity, and a usable candidate one tier down. It returns the
// classified rank to downgrade from.
func (g *Gateway) saturationDowngrade(rt *smartRouter, cls smartClassification, rank int, fresh func(int) []resolveCandidate) (int, bool) {
	if rank < 1 || rank-1 < cls.hardRank || cls.score >= rt.tierMin(rank)+rt.downgradeBand {
		return 0, false
	}
	cands := fresh(rank)
	inFlight := make([]int, len(cands))
	limit := make([]int, len(cands))
	for i := range cands {
		inFlight[i], limit[i] = g.providerLoad(cands[i].providerName)
	}
	if !smartAllSaturated(inFlight, limit) || len(fresh(rank-1)) == 0 {
		return 0, false
	}
	return rank, true
}

// smartAllSaturated reports whether a tier has candidates and every one is at its known limit.
func smartAllSaturated(inFlight, limit []int) bool {
	if len(inFlight) == 0 || len(inFlight) != len(limit) {
		return false
	}
	for i := range inFlight {
		if !smartSaturated(inFlight[i], limit[i]) {
			return false
		}
	}
	return true
}

func routerCandidateKey(cand resolveCandidate) string {
	return cand.providerName + "\x00" + cand.upstreamModel
}

// routerHigherTierUsable reports whether any tier above rank still has a candidate to try.
func (g *Gateway) routerHigherTierUsable(rt *smartRouter, rank int, fresh func(int) []resolveCandidate) bool {
	for r := rank + 1; r < len(rt.tiers); r++ {
		if len(fresh(r)) > 0 {
			return true
		}
	}
	return false
}

// routerTierCandidates returns one tier's usable candidates in attempt order,
// deduplicated within the tier; the plan dedupes across tiers.
func (g *Gateway) routerTierCandidates(rt *smartRouter, rank int, scored string, facts routerRequestFacts, grp *group, skips *routerSkips) []resolveCandidate {
	tier := rt.tiers[rank]
	n := len(tier.models)
	offset := 0
	if rt.selection == smartSelectionHash && n > 1 {
		offset = smartTextHash(scored) % n
	}
	var out []resolveCandidate
	inTier := make(map[string]bool)
	for k := 0; k < n; k++ {
		target := tier.models[(offset+k)%n]
		for _, cand := range g.routerTargetCandidates(rt, tier.name, target, facts, grp, skips) {
			key := routerCandidateKey(cand)
			if inTier[key] {
				continue
			}
			inTier[key] = true
			out = append(out, cand)
		}
	}
	if rt.bandit != nil {
		out = g.banditOrder(rt, tier.name, out)
	}
	return out
}

// routerTargetCandidates resolves one target into its usable candidates.
// Ordering, cost filtering and capping run per model, never across tiers.
func (g *Gateway) routerTargetCandidates(rt *smartRouter, tier, target string, facts routerRequestFacts, grp *group, skips *routerSkips) []resolveCandidate {
	var primary resolveCandidate
	var extra []resolveCandidate
	var err error
	if g.failover.enabled {
		primary, extra, err = g.registry.resolveWithCandidates(target, grp)
	} else {
		primary, err = g.registry.resolvePrimaryOnly(target, grp)
	}
	if err != nil {
		if err == errModelDenied {
			skips.denied++
			return nil
		}
		skips.unresolved++
		g.registry.warnRouterTargetOnce(rt.name, target)
		return nil
	}

	ordered := g.orderedFailoverCandidates(primary, extra)
	usable := make([]resolveCandidate, 0, len(ordered))
	for _, cand := range ordered {
		if g.routerCandidateSkipped(cand, tier, rt, facts, skips) {
			continue
		}
		usable = append(usable, cand)
	}
	usable = g.filterCandidatesByCost(usable, target)
	return capFailoverCandidates(usable, g.failover.maxAttempts)
}

func (g *Gateway) routerCandidateSkipped(cand resolveCandidate, tier string, rt *smartRouter, facts routerRequestFacts, skips *routerSkips) bool {
	if facts.hasImage && !modelVisionCapable(cand.providerName, cand.upstreamModel, g.cfg.ModelMeta) {
		skips.vision++
		return true
	}
	if facts.costBudget && !priceKnown(cand.canonical, cand.upstreamModel, g.cfg.Pricing, g.cfg.ModelMeta) {
		skips.unpriced++
		return true
	}
	// Checked last so the fallback candidate already passed vision and cost.
	if rt.contextEscalation {
		meta := g.registry.resolveMetaFor(cand.providerName, cand.upstreamModel)
		if meta.ContextKnown && !contextFits(facts, g.tokenCalib.promptTokens(cand.providerName, cand.upstreamModel, facts.promptBytes), int64(meta.ContextTokens)) {
			skips.context++
			if skips.largest == nil || meta.ContextTokens > skips.largestCtx {
				skips.largest = &routerPlanEntry{tier: tier, cand: cand}
				skips.largestCtx = meta.ContextTokens
			}
			return true
		}
	}
	return false
}

// setRouterHeaders is the single per-attempt seam for routing: router, reason,
// this attempt's tier and the previous attempt's tier (escalation) are all known here.
func setRouterHeaders(h http.Header, plan *routerPlan, i int) {
	h.Set(headerRoutedModel, plan.entries[i].cand.canonical)
	h.Set(headerRouterTier, plan.entries[i].tier)
	if plan.sess != nil && plan.sess.rt.session.live {
		h.Set(headerRouterReason, routerRequestReason(plan))
	}
	if plan.downgradedFrom != "" {
		if plan.entries[i].tier == plan.startTier {
			h.Set(headerRouterDowngraded, plan.downgradedFrom)
		} else {
			h.Del(headerRouterDowngraded)
		}
	}
}

// smartImageTokens is the flat token cost of one image part: its base64 bytes
// say nothing about what the provider bills or fits in the window.
const smartImageTokens = 1000

// smartPromptBytes returns the prompt's estimated byte size and its flat image token cost.
func smartPromptBytes(req map[string]any, endpoint string) (promptBytes, imageTokens int64) {
	chars, images := smartPromptSize(req, endpoint)
	return int64(chars), int64(images) * smartImageTokens
}

// smartContextHeadroomPct caps prompt plus output at this share of a window:
// the token estimate is a byte heuristic and can undercount.
const smartContextHeadroomPct = 90

// smartOutputTokensCap keeps the fit arithmetic far from int64 overflow.
const smartOutputTokensCap = 1 << 40

// requestedOutputTokens reads the output budget the client asked for, 0 when absent.
func requestedOutputTokens(req map[string]any, endpoint string) int64 {
	keys := []string{"max_completion_tokens", "max_tokens"}
	if endpoint == cacheEndpointMessages {
		keys = keys[1:]
	}
	for _, k := range keys {
		v, present := req[k]
		if !present {
			continue
		}
		n, ok := toInt64(v)
		if !ok || n < 0 {
			return 0
		}
		if n > smartOutputTokensCap {
			return smartOutputTokensCap
		}
		return n
	}
	return 0
}

// contextFits reports whether the text tokens (the candidate's own estimate),
// image tokens and requested output stay within the headroom share of window.
func contextFits(facts routerRequestFacts, textTokens, window int64) bool {
	return (textTokens+facts.imageTokens+facts.outputTokens)*100 <= window*smartContextHeadroomPct
}
