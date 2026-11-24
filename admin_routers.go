package traefikllmgateway

import "sort"

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

// adminRouterStatsView is this replica's router counters since process
// start; other replicas keep their own.
type adminRouterStatsView struct {
	Tiers       []adminRouterCountView      `json:"tiers"`
	Reasons     []adminRouterCountView      `json:"reasons"`
	Escalations []adminRouterEscalationView `json:"escalations"`
	Requests    int64                       `json:"requests"`
}

// adminRouterView is one smart router's read-only row. Keywords are routing
// hints, not secrets.
type adminRouterView struct {
	Name              string                `json:"name"`
	DefaultTier       string                `json:"defaultTier"`
	Selection         string                `json:"selection"`
	Tiers             []adminRouterTierView `json:"tiers"`
	KeywordRules      []adminRouterRuleView `json:"keywordRules"`
	ContextTokens     *int                  `json:"contextTokens,omitempty"`
	Stats             adminRouterStatsView  `json:"stats"`
	MaxAttempts       int                   `json:"maxAttempts"`
	ContextEscalation bool                  `json:"contextEscalation"`
}

// buildAdminRouterViews lists every router sorted by name; never nil.
func (g *Gateway) buildAdminRouterViews() []adminRouterView {
	reqs, escs := g.routerStats.snapshot()
	out := make([]adminRouterView, 0, len(g.registry.routers))
	for _, rt := range g.registry.routers {
		v := adminRouterView{
			Name:              rt.name,
			Selection:         rt.selection,
			MaxAttempts:       rt.maxAttempts,
			ContextEscalation: rt.contextEscalation,
			Tiers:             make([]adminRouterTierView, len(rt.tiers)),
			KeywordRules:      make([]adminRouterRuleView, len(rt.rules)),
		}
		if rt.defaultRank >= 0 && rt.defaultRank < len(rt.tiers) {
			v.DefaultTier = rt.tiers[rt.defaultRank].name
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
		v.Stats = buildAdminRouterStats(rt, reqs, escs)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// buildAdminRouterStats zero-fills every configured tier and sorts reasons and
// escalations by name so the response is stable.
func buildAdminRouterStats(rt *smartRouter, reqs map[routerRequestKey]int64, escs map[routerEscalationKey]int64) adminRouterStatsView {
	st := adminRouterStatsView{
		Tiers:       make([]adminRouterCountView, len(rt.tiers)),
		Reasons:     []adminRouterCountView{},
		Escalations: []adminRouterEscalationView{},
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
	return st
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
