package traefikllmgateway

import (
	"fmt"
	"sort"
	"strings"
)

const (
	smartSelectionOrdered = "ordered"
	smartSelectionHash    = "hash"

	defaultSmartMaxAttempts = 4
	maxSmartKeywords        = 64
	maxSmartMinScore        = 1000

	defaultSmartWeightLength        = 100
	defaultSmartWeightCode          = 300
	defaultSmartWeightReasoning     = 250
	defaultSmartWeightTechnical     = 250
	defaultSmartWeightMultiStep     = 30
	defaultSmartWeightMultiQuestion = 20
	defaultSmartWeightSimple        = 50
	defaultSmartWeightTools         = 100
)

// SmartRouterConfig defines one virtual model that routes each request to a
// real model chosen by the request's estimated complexity.
type SmartRouterConfig struct {
	// ContextEscalation skips targets whose known context window is too small; nil means on.
	ContextEscalation *bool               `json:"contextEscalation,omitempty"`
	Weights           *SmartRouterWeights `json:"weights,omitempty"`
	// DefaultTier serves requests with no user text; empty means the first tier.
	DefaultTier string `json:"defaultTier,omitempty"`
	// Selection is "ordered" (default) or "hash".
	Selection string `json:"selection,omitempty"`
	// Tiers is ordered cheapest first; list position is the rank.
	Tiers        []SmartRouterTier        `json:"tiers"`
	KeywordRules []SmartRouterKeywordRule `json:"keywordRules,omitempty"`
	// MaxAttempts caps upstream attempts per request; 0 means 4.
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

// SmartRouterTier is one rung of a router. Models must be disjoint across tiers.
type SmartRouterTier struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	// MinScore is the lowest complexity score (0..1000, per-mille) this tier serves.
	MinScore int `json:"minScore"`
}

// SmartRouterKeywordRule raises a request to Tier when any keyword appears in its text.
type SmartRouterKeywordRule struct {
	Tier     string   `json:"tier"`
	Keywords []string `json:"keywords"`
}

// SmartRouterWeights overrides the per-mille classifier weights; nil keeps the default.
type SmartRouterWeights struct {
	Length        *int `json:"length,omitempty"`
	Code          *int `json:"code,omitempty"`
	Reasoning     *int `json:"reasoning,omitempty"`
	Technical     *int `json:"technical,omitempty"`
	MultiStep     *int `json:"multiStep,omitempty"`
	MultiQuestion *int `json:"multiQuestion,omitempty"`
	// Simple is a penalty magnitude, subtracted from the score.
	Simple *int `json:"simple,omitempty"`
	Tools  *int `json:"tools,omitempty"`
}

// smartRouter is a validated SmartRouterConfig with defaults applied and
// keywords lowercased; it is immutable after construction.
type smartRouter struct {
	name              string
	selection         string
	tiers             []smartTier
	rules             []smartKeywordRule
	weights           smartWeights
	defaultRank       int
	maxAttempts       int
	contextEscalation bool
}

type smartTier struct {
	name     string
	models   []string
	minScore int
}

type smartKeywordRule struct {
	keywords []string
	rank     int
}

type smartWeights struct {
	length, code, reasoning, technical int
	multiStep, multiQuestion           int
	simple, tools                      int
}

func smartWeightOrDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func buildSmartWeights(w *SmartRouterWeights) smartWeights {
	if w == nil {
		w = &SmartRouterWeights{}
	}
	return smartWeights{
		length:        smartWeightOrDefault(w.Length, defaultSmartWeightLength),
		code:          smartWeightOrDefault(w.Code, defaultSmartWeightCode),
		reasoning:     smartWeightOrDefault(w.Reasoning, defaultSmartWeightReasoning),
		technical:     smartWeightOrDefault(w.Technical, defaultSmartWeightTechnical),
		multiStep:     smartWeightOrDefault(w.MultiStep, defaultSmartWeightMultiStep),
		multiQuestion: smartWeightOrDefault(w.MultiQuestion, defaultSmartWeightMultiQuestion),
		simple:        smartWeightOrDefault(w.Simple, defaultSmartWeightSimple),
		tools:         smartWeightOrDefault(w.Tools, defaultSmartWeightTools),
	}
}

func validateSmartWeights(router string, w *SmartRouterWeights) error {
	if w == nil {
		return nil
	}
	named := []struct {
		v    *int
		name string
	}{
		{w.Length, "length"}, {w.Code, "code"}, {w.Reasoning, "reasoning"},
		{w.Technical, "technical"}, {w.MultiStep, "multiStep"},
		{w.MultiQuestion, "multiQuestion"}, {w.Simple, "simple"}, {w.Tools, "tools"},
	}
	for _, n := range named {
		if n.v != nil && (*n.v < 0 || *n.v > maxSmartMinScore) {
			return fmt.Errorf("llmgateway: smartRouters: router %q: weight %q must be between 0 and %d, got %d", router, n.name, maxSmartMinScore, *n.v)
		}
	}
	return nil
}

func sortedSmartRouterNames(raw map[string]*SmartRouterConfig) []string {
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateSmartRouters validates cfg.SmartRouters and returns the compiled
// routers keyed by name (nil when raw is empty). Iteration is sorted so the
// first reported error is deterministic.
func validateSmartRouters(raw map[string]*SmartRouterConfig, providerNames []string, explicitModels map[string]bool, aliases map[string]string) (map[string]*smartRouter, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	providerSet := make(map[string]bool, len(providerNames))
	for _, p := range providerNames {
		providerSet[p] = true
	}

	out := make(map[string]*smartRouter, len(raw))
	for _, name := range sortedSmartRouterNames(raw) {
		if err := validateSmartRouterName(name, providerSet, explicitModels, aliases); err != nil {
			return nil, err
		}
		cfg := raw[name]
		if cfg == nil {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q must not be nil", name)
		}
		rt, err := compileSmartRouter(name, cfg, raw)
		if err != nil {
			return nil, err
		}
		out[name] = rt
	}
	return out, nil
}

func validateSmartRouterName(name string, providerSet, explicitModels map[string]bool, aliases map[string]string) error {
	if !isValidModelID(name) {
		return fmt.Errorf("llmgateway: smartRouters: router %q is invalid: must be non-empty with no control characters", name)
	}
	if prefix, _, hasSlash := strings.Cut(name, "/"); hasSlash && providerSet[prefix] {
		return fmt.Errorf("llmgateway: smartRouters: router %q is invalid: its prefix %q names a configured provider and would be shadowed by provider-prefix resolution", name, prefix)
	}
	if explicitModels[name] {
		return fmt.Errorf("llmgateway: smartRouters: router %q collides with an explicitly configured model id of the same name", name)
	}
	if _, isAlias := aliases[name]; isAlias {
		return fmt.Errorf("llmgateway: smartRouters: router %q collides with a modelAliases key of the same name", name)
	}
	return nil
}

func compileSmartRouter(name string, cfg *SmartRouterConfig, routers map[string]*SmartRouterConfig) (*smartRouter, error) {
	if len(cfg.Tiers) == 0 {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q needs at least one tier", name)
	}

	tiers, tierRank, err := compileSmartTiers(name, cfg.Tiers, routers)
	if err != nil {
		return nil, err
	}

	defaultRank := 0
	if cfg.DefaultTier != "" {
		r, ok := tierRank[cfg.DefaultTier]
		if !ok {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: defaultTier %q is not a tier of this router", name, cfg.DefaultTier)
		}
		defaultRank = r
	}

	selection := cfg.Selection
	switch selection {
	case "":
		selection = smartSelectionOrdered
	case smartSelectionOrdered, smartSelectionHash:
	default:
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: selection %q must be %q or %q", name, cfg.Selection, smartSelectionOrdered, smartSelectionHash)
	}

	if cfg.MaxAttempts < 0 {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: maxAttempts must not be negative", name)
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultSmartMaxAttempts
	}

	contextEscalation := true
	if cfg.ContextEscalation != nil {
		contextEscalation = *cfg.ContextEscalation
	}

	rules, err := compileSmartKeywordRules(name, cfg.KeywordRules, tierRank)
	if err != nil {
		return nil, err
	}
	if wErr := validateSmartWeights(name, cfg.Weights); wErr != nil {
		return nil, wErr
	}

	return &smartRouter{
		name:              name,
		tiers:             tiers,
		defaultRank:       defaultRank,
		selection:         selection,
		maxAttempts:       maxAttempts,
		contextEscalation: contextEscalation,
		rules:             rules,
		weights:           buildSmartWeights(cfg.Weights),
	}, nil
}

func compileSmartTiers(router string, raw []SmartRouterTier, routers map[string]*SmartRouterConfig) ([]smartTier, map[string]int, error) {
	tiers := make([]smartTier, 0, len(raw))
	tierRank := make(map[string]int, len(raw))
	seenModel := make(map[string]string)
	prevMin := -1
	for i, t := range raw {
		if !isValidModelID(t.Name) {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %d has an invalid name %q: must be non-empty with no control characters", router, i, t.Name)
		}
		if _, dup := tierRank[t.Name]; dup {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: duplicate tier name %q", router, t.Name)
		}
		if t.MinScore < 0 || t.MinScore > maxSmartMinScore {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %q minScore %d must be between 0 and %d", router, t.Name, t.MinScore, maxSmartMinScore)
		}
		if i == 0 && t.MinScore != 0 {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: first tier %q must have minScore 0, got %d", router, t.Name, t.MinScore)
		}
		if t.MinScore <= prevMin {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %q minScore %d must be greater than the previous tier's %d", router, t.Name, t.MinScore, prevMin)
		}
		prevMin = t.MinScore
		if len(t.Models) == 0 {
			return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %q has no models", router, t.Name)
		}
		for _, model := range t.Models {
			if !isValidModelID(model) {
				return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %q has an invalid model id %q", router, t.Name, model)
			}
			if _, isRouter := routers[model]; isRouter {
				return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: tier %q targets %q, which is itself a router", router, t.Name, model)
			}
			if prevTier, dup := seenModel[model]; dup {
				return nil, nil, fmt.Errorf("llmgateway: smartRouters: router %q: model %q appears in tier %q and tier %q; tiers must be disjoint", router, model, prevTier, t.Name)
			}
			seenModel[model] = t.Name
		}
		tierRank[t.Name] = i
		tiers = append(tiers, smartTier{name: t.Name, minScore: t.MinScore, models: append([]string(nil), t.Models...)})
	}
	return tiers, tierRank, nil
}

func compileSmartKeywordRules(router string, raw []SmartRouterKeywordRule, tierRank map[string]int) ([]smartKeywordRule, error) {
	rules := make([]smartKeywordRule, 0, len(raw))
	total := 0
	for i, r := range raw {
		rank, ok := tierRank[r.Tier]
		if !ok {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: keywordRules[%d] tier %q is not a tier of this router", router, i, r.Tier)
		}
		if len(r.Keywords) == 0 {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: keywordRules[%d] has no keywords", router, i)
		}
		kws := make([]string, 0, len(r.Keywords))
		for _, kw := range r.Keywords {
			lowered := strings.ToLower(strings.TrimSpace(kw))
			if lowered == "" {
				return nil, fmt.Errorf("llmgateway: smartRouters: router %q: keywordRules[%d] has an empty keyword", router, i)
			}
			kws = append(kws, lowered)
		}
		total += len(kws)
		if total > maxSmartKeywords {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: more than %d keywords across keywordRules", router, maxSmartKeywords)
		}
		rules = append(rules, smartKeywordRule{rank: rank, keywords: kws})
	}
	return rules, nil
}
