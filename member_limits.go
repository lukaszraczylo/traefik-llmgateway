package traefikllmgateway

// Limit source labels reported for each effective per-user limit field.
const (
	limitSourceUser        = "user"
	limitSourceMemberGroup = "memberLimits:"
)

// limitFieldPacing is the limitSources key for the pacing block; the
// earliest startPct across member groups is the strictest.
const limitFieldPacing = "pacing"

var limitIntFields = []string{"requestsPerMinute", "requestsPerDay", "tokensPerMinute", "tokensPerDay", "tokensPerMonth"}

var limitCostFields = []string{"costPerDayUSD", "costPerMonthUSD"}

func limitIntField(lc *LimitsConfig, name string) int64 {
	switch name {
	case "requestsPerMinute":
		return lc.RequestsPerMinute
	case "requestsPerDay":
		return lc.RequestsPerDay
	case "tokensPerMinute":
		return lc.TokensPerMinute
	case "tokensPerDay":
		return lc.TokensPerDay
	case "tokensPerMonth":
		return lc.TokensPerMonth
	}
	return 0
}

func setLimitIntField(lc *LimitsConfig, name string, v int64) {
	switch name {
	case "requestsPerMinute":
		lc.RequestsPerMinute = v
	case "requestsPerDay":
		lc.RequestsPerDay = v
	case "tokensPerMinute":
		lc.TokensPerMinute = v
	case "tokensPerDay":
		lc.TokensPerDay = v
	case "tokensPerMonth":
		lc.TokensPerMonth = v
	}
}

func limitCostField(lc *LimitsConfig, name string) float64 {
	if name == "costPerDayUSD" {
		return lc.CostPerDayUSD
	}
	return lc.CostPerMonthUSD
}

func setLimitCostField(lc *LimitsConfig, name string, v float64) {
	if name == "costPerDayUSD" {
		lc.CostPerDayUSD = v
		return
	}
	lc.CostPerMonthUSD = v
}

// strictestPacing combines the members' pacing blocks: the earliest startPct
// and the longest maxDelay. It returns the group that set the earliest
// startPct as the source, or nil when no member sets pacing.
func strictestPacing(members []*group) (*PacingConfig, string) {
	var start *PacingConfig
	var longest *PacingConfig
	src := ""
	for _, m := range members {
		if m.memberLimits == nil || m.memberLimits.Pacing == nil {
			continue
		}
		p := m.memberLimits.Pacing
		if start == nil || p.StartPct < start.StartPct {
			start = p
			src = limitSourceMemberGroup + m.name
		}
		if longest == nil || p.maxDelayDuration() > longest.maxDelayDuration() {
			longest = p
		}
	}
	if start == nil {
		return nil, ""
	}
	return &PacingConfig{StartPct: start.StartPct, MaxDelay: longest.MaxDelay, maxDelay: longest.maxDelay}, src
}

// effectiveUserLimits merges a user's own limits with the memberLimits of
// the groups the user belongs to, and reports which source set each
// non-zero field. A non-zero own value wins; otherwise the strictest
// non-zero memberLimits value across members applies. With no memberLimits
// configured it returns own unchanged.
func effectiveUserLimits(own *LimitsConfig, members []*group) (*LimitsConfig, map[string]string) {
	hasMember := false
	for _, m := range members {
		if m.memberLimits != nil {
			hasMember = true
		}
	}
	eff := own
	if hasMember {
		eff = &LimitsConfig{}
	}
	if eff == nil {
		return nil, nil
	}
	sources := map[string]string{}
	for _, f := range limitIntFields {
		if own != nil && limitIntField(own, f) > 0 {
			if hasMember {
				setLimitIntField(eff, f, limitIntField(own, f))
			}
			sources[f] = limitSourceUser
			continue
		}
		var best int64
		src := ""
		for _, m := range members {
			if m.memberLimits == nil {
				continue
			}
			v := limitIntField(m.memberLimits, f)
			if v > 0 && (best == 0 || v < best) {
				best = v
				src = limitSourceMemberGroup + m.name
			}
		}
		if best > 0 {
			setLimitIntField(eff, f, best)
			sources[f] = src
		}
	}
	for _, f := range limitCostFields {
		if own != nil && limitCostField(own, f) > 0 {
			if hasMember {
				setLimitCostField(eff, f, limitCostField(own, f))
			}
			sources[f] = limitSourceUser
			continue
		}
		var best float64
		src := ""
		for _, m := range members {
			if m.memberLimits == nil {
				continue
			}
			v := limitCostField(m.memberLimits, f)
			if v > 0 && (best == 0 || v < best) {
				best = v
				src = limitSourceMemberGroup + m.name
			}
		}
		if best > 0 {
			setLimitCostField(eff, f, best)
			sources[f] = src
		}
	}
	switch {
	case own != nil && own.Pacing != nil:
		if hasMember {
			eff.Pacing = own.Pacing
		}
		sources[limitFieldPacing] = limitSourceUser
	case hasMember:
		if merged, src := strictestPacing(members); merged != nil {
			eff.Pacing = merged
			sources[limitFieldPacing] = src
		}
	}
	if len(sources) == 0 {
		return eff, nil
	}
	return eff, sources
}
