package traefikllmgateway

import (
	"sort"
	"time"
)

type adminRouterTierView struct {
	Name     string   `json:"name"`
	Models   []string `json:"models"`
	MinScore int      `json:"minScore"`
}

type adminRouterRuleView struct {
	Tier     string   `json:"tier"`
	Keywords []string `json:"keywords"`
}

type adminRouterCountView struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type adminRouterEscalationView struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int64  `json:"count"`
}

// adminRouterShadowView is one cell of the live-tier by shadow-tier agreement matrix.
type adminRouterShadowView struct {
	Live   string `json:"live"`
	Shadow string `json:"shadow"`
	Count  int64  `json:"count"`
}

// adminRouterOutcomeView counts finished requests by first attempted tier, reason and outcome.
type adminRouterOutcomeView struct {
	Tier    string `json:"tier"`
	Reason  string `json:"reason"`
	Outcome string `json:"outcome"`
	Count   int64  `json:"count"`
}

// adminRouterDeltaView is one tier's calibration offset: points its threshold sits below the configured minScore.
type adminRouterDeltaView struct {
	Tier  string `json:"tier"`
	Delta int    `json:"delta"`
}

// adminRouterCalibrationView shows calibration mode and the offsets it computed. In
// shadow mode the offsets are computed but not applied.
type adminRouterCalibrationView struct {
	Mode     string                 `json:"mode"`
	Delta    []adminRouterDeltaView `json:"delta"`
	MaxLower int                    `json:"maxLower"`
	MaxRaise int                    `json:"maxRaise"`
	Freeze   bool                   `json:"freeze"`
}

// adminRouterStatsView holds router counters. Scope "shared" sums all gateway
// instances this month (Redis); "instance" is this process since start.
type adminRouterStatsView struct {
	Scope       string                      `json:"scope"`
	Tiers       []adminRouterCountView      `json:"tiers"`
	Reasons     []adminRouterCountView      `json:"reasons"`
	Escalations []adminRouterEscalationView `json:"escalations"`
	Shadow      []adminRouterShadowView     `json:"shadow"`
	Outcomes    []adminRouterOutcomeView    `json:"outcomes"`
	Requests    int64                       `json:"requests"`
}

// adminRouterAdaptiveView shows the settings of adaptive selection.
type adminRouterAdaptiveView struct {
	ExplorePct       int `json:"explorePct"`
	WindowHours      int `json:"windowHours"`
	LatencyWeightPct int `json:"latencyWeightPct"`
}

// adminRouterView is one smart router's read-only row. Keywords are routing
// hints, not secrets.
type adminRouterView struct {
	Adaptive         *adminRouterAdaptiveView   `json:"adaptive,omitempty"`
	Name             string                     `json:"name"`
	DefaultTier      string                     `json:"defaultTier"`
	ToolsMinTier     string                     `json:"toolsMinTier,omitempty"`
	Selection        string                     `json:"selection"`
	Classifier       string                     `json:"classifier"`
	ShadowClassifier string                     `json:"shadowClassifier,omitempty"`
	FirstByteTimeout string                     `json:"firstByteTimeout,omitempty"`
	Tiers            []adminRouterTierView      `json:"tiers"`
	KeywordRules     []adminRouterRuleView      `json:"keywordRules"`
	ContextTokens    *int                       `json:"contextTokens,omitempty"`
	Stats            adminRouterStatsView       `json:"stats"`
	Calibration      adminRouterCalibrationView `json:"calibration"`
	MaxAttempts      int                        `json:"maxAttempts"`
	// Downgraded counts requests a saturated tier sent one tier lower.
	Downgraded          int64 `json:"downgraded"`
	DowngradeBand       int   `json:"downgradeBand"`
	RiskZ               int   `json:"riskZ"`
	ContextEscalation   bool  `json:"contextEscalation"`
	ShadowEnabled       bool  `json:"shadowEnabled"`
	SaturationDowngrade bool  `json:"saturationDowngrade"`
	TimeoutFallback     bool  `json:"timeoutFallback"`
	// FellBack counts requests that took the extra timeout-fallback attempt.
	FellBack int64 `json:"fellBack"`
}

// buildAdminRouterViews lists every router sorted by name; never nil.
func (g *Gateway) buildAdminRouterViews() []adminRouterView {
	counters := g.routerStatsSnapshot(time.Now())
	out := make([]adminRouterView, 0, len(g.registry.routers))
	for _, rt := range g.registry.routers {
		v := adminRouterView{
			Name:                rt.name,
			Selection:           rt.selection,
			MaxAttempts:         rt.maxAttempts,
			ContextEscalation:   rt.contextEscalation,
			ShadowEnabled:       rt.shadowEnabled(),
			Classifier:          rt.classifier,
			RiskZ:               rt.riskZ,
			SaturationDowngrade: rt.satDowngrade,
			TimeoutFallback:     rt.timeoutFallback,
			DowngradeBand:       rt.downgradeBand,
			Calibration:         buildAdminRouterCalibration(rt),
			Tiers:               make([]adminRouterTierView, len(rt.tiers)),
			KeywordRules:        make([]adminRouterRuleView, len(rt.rules)),
		}
		if rt.firstByte > 0 {
			v.FirstByteTimeout = rt.firstByte.String()
		}
		if rt.shadowV2 {
			v.ShadowClassifier = smartClassifierV2
		}
		if rt.bandit != nil {
			v.Adaptive = &adminRouterAdaptiveView{ExplorePct: rt.bandit.cfg.explorePct, WindowHours: rt.bandit.cfg.windowHours, LatencyWeightPct: rt.bandit.cfg.latencyPct}
		}
		if rt.defaultRank >= 0 && rt.defaultRank < len(rt.tiers) {
			v.DefaultTier = rt.tiers[rt.defaultRank].name
		}
		if rt.toolsMinRank > 0 && rt.toolsMinRank < len(rt.tiers) {
			v.ToolsMinTier = rt.tiers[rt.toolsMinRank].name
		}
		for i, t := range rt.tiers {
			v.Tiers[i] = adminRouterTierView{Name: t.name, Models: append([]string{}, t.models...), MinScore: t.minScore}
		}
		for i, r := range rt.rules {
			rule := adminRouterRuleView{Keywords: append([]string{}, r.keywords...)}
			if r.rank >= 0 && r.rank < len(rt.tiers) {
				rule.Tier = rt.tiers[r.rank].name
			}
			v.KeywordRules[i] = rule
		}
		if meta := g.registry.routerMeta(rt.name, rt); meta.ContextKnown {
			tokens := int(meta.ContextTokens)
			v.ContextTokens = &tokens
		}
		v.Stats = buildAdminRouterStats(rt, counters)
		v.Stats.Scope = counters.scope
		for _, reason := range v.Stats.Reasons {
			if reason.Name == smartReasonSaturated {
				v.Downgraded = reason.Count
			}
		}
		v.FellBack = routerFallbackCount(rt.name, counters.outcomes)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func buildAdminRouterCalibration(rt *smartRouter) adminRouterCalibrationView {
	v := adminRouterCalibrationView{Mode: rt.calib.mode, Delta: []adminRouterDeltaView{}, MaxLower: rt.calib.maxLower, MaxRaise: rt.calib.maxRaise, Freeze: rt.calib.freeze}
	if rt.calibState == nil {
		return v
	}
	for i := 1; i < len(rt.tiers); i++ {
		v.Delta = append(v.Delta, adminRouterDeltaView{Tier: rt.tiers[i].name, Delta: rt.calibState.get(i)})
	}
	return v
}

// buildAdminRouterStats zero-fills every configured tier and sorts reasons and
// escalations by name so the response is stable.
func buildAdminRouterStats(rt *smartRouter, c routerCounters) adminRouterStatsView {
	reqs, escs := c.reqs, c.escs
	st := adminRouterStatsView{
		Tiers:       make([]adminRouterCountView, len(rt.tiers)),
		Reasons:     []adminRouterCountView{},
		Escalations: []adminRouterEscalationView{},
		Shadow:      []adminRouterShadowView{},
		Outcomes:    []adminRouterOutcomeView{},
	}
	tierIdx := make(map[string]int, len(rt.tiers))
	for i, t := range rt.tiers {
		st.Tiers[i] = adminRouterCountView{Name: t.name}
		tierIdx[t.name] = i
	}
	reasons := make(map[string]int64)
	for k, n := range reqs {
		if k.router != rt.name {
			continue
		}
		st.Requests += n
		if i, ok := tierIdx[k.tier]; ok {
			st.Tiers[i].Count += n
		}
		reasons[k.reason] += n
	}
	for name, n := range reasons {
		st.Reasons = append(st.Reasons, adminRouterCountView{Name: name, Count: n})
	}
	sort.Slice(st.Reasons, func(i, j int) bool { return st.Reasons[i].Name < st.Reasons[j].Name })
	for k, n := range escs {
		if k.router == rt.name {
			st.Escalations = append(st.Escalations, adminRouterEscalationView{From: k.from, To: k.to, Count: n})
		}
	}
	sort.Slice(st.Escalations, func(i, j int) bool {
		a, b := st.Escalations[i], st.Escalations[j]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	st.Shadow = buildAdminRouterShadow(rt, c.shadow)
	st.Outcomes = buildAdminRouterOutcomes(rt, c.outcomes)
	return st
}

// buildAdminRouterShadow lists the non-zero matrix cells in tier order.
func buildAdminRouterShadow(rt *smartRouter, counts map[routerShadowKey]int64) []adminRouterShadowView {
	out := []adminRouterShadowView{}
	for _, live := range rt.tiers {
		for _, shadow := range rt.tiers {
			if n := counts[routerShadowKey{router: rt.name, live: live.name, shadow: shadow.name}]; n > 0 {
				out = append(out, adminRouterShadowView{Live: live.name, Shadow: shadow.name, Count: n})
			}
		}
	}
	return out
}

// buildAdminRouterOutcomes lists the non-zero outcome counts in tier, reason, outcome order.
func buildAdminRouterOutcomes(rt *smartRouter, counts map[routerOutcomeKey]int64) []adminRouterOutcomeView {
	out := []adminRouterOutcomeView{}
	for _, t := range rt.tiers {
		for _, reason := range routerReasonNames {
			for _, outcome := range routerOutcomeNames {
				if n := counts[routerOutcomeKey{router: rt.name, tier: t.name, reason: reason, outcome: outcome}]; n > 0 {
					out = append(out, adminRouterOutcomeView{Tier: t.name, Reason: reason, Outcome: outcome, Count: n})
				}
			}
		}
	}
	return out
}

// routersByTarget maps each router target string to the sorted router names that list it.
func routersByTarget(views []adminRouterView) map[string][]string {
	by := make(map[string][]string)
	for _, v := range views {
		seen := make(map[string]bool)
		for _, t := range v.Tiers {
			for _, m := range t.Models {
				if !seen[m] {
					seen[m] = true
					by[m] = append(by[m], v.Name)
				}
			}
		}
	}
	return by
}
