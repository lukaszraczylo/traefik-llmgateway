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

// adminRouterView is one smart router's read-only row. Keywords are routing
// hints, not secrets.
type adminRouterView struct {
	Name              string                `json:"name"`
	DefaultTier       string                `json:"defaultTier"`
	Selection         string                `json:"selection"`
	Tiers             []adminRouterTierView `json:"tiers"`
	KeywordRules      []adminRouterRuleView `json:"keywordRules"`
	MaxAttempts       int                   `json:"maxAttempts"`
	ContextEscalation bool                  `json:"contextEscalation"`
}

// buildAdminRouterViews lists every router sorted by name; never nil.
func (g *Gateway) buildAdminRouterViews() []adminRouterView {
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
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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
