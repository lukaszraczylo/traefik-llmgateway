package traefikllmgateway

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	smartSelectionOrdered = "ordered"
	smartSelectionHash    = "hash"
	// smartSelectionAdaptive orders each tier's candidates with a Thompson bandit.
	smartSelectionAdaptive = "adaptive"

	smartClassifierV1 = "v1"
	smartClassifierV2 = "v2"
	// smartIntentForeign is the intentTiers key for text the English markers cannot classify.
	smartIntentForeign = "foreign"

	defaultSmartRiskZ = 128
	maxSmartRiskZ     = 400

	defaultSmartDowngradeBand = 100
	maxSmartDowngradeBand     = 300

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
	// ShadowWeights scores each request a second time for comparison; it never changes routing.
	ShadowWeights *SmartRouterWeights `json:"shadowWeights,omitempty"`
	// Classifier is "v1" (default) or "v2", which scores the prose of the newest ask with an uncertainty margin.
	Classifier string `json:"classifier,omitempty"`
	// ShadowClassifier "v2" scores each request with v2 beside a v1 router and never changes routing.
	ShadowClassifier string `json:"shadowClassifier,omitempty"`
	// FirstByteTimeout (Go duration, at least 1s) escalates when an upstream sends no body byte, or for a
	// stream no data event, in time; empty means off.
	FirstByteTimeout string `json:"firstByteTimeout,omitempty"`
	// RiskZ scales the v2 uncertainty margin in hundredths of a sigma; nil means 128.
	RiskZ *int `json:"riskZ,omitempty"`
	// IntentTiers pins a v2 intent to a tier by name instead of its default ladder position.
	IntentTiers map[string]string `json:"intentTiers,omitempty"`
	// Session keeps an upward-ratcheting tier pin per conversation. It needs classifier v2 to
	// route live; with only shadowClassifier v2 it changes the shadow rank and never live routing.
	Session *SmartRouterSessionConfig `json:"session,omitempty"`
	// Decision asks a decision model to settle near-boundary requests; needs classifier v2, off when absent.
	Decision *SmartRouterDecisionConfig `json:"decision,omitempty"`
	// Adaptive tunes selection "adaptive"; it has no effect with another selection.
	Adaptive *SmartRouterAdaptiveConfig `json:"adaptive,omitempty"`
	// Calibration lowers tier thresholds from outcome counters; it is off by default.
	Calibration *SmartRouterCalibrationConfig `json:"calibration,omitempty"`
	// DowngradeBand is how many points above a tier threshold a score may sit and still be downgraded; nil means 100.
	DowngradeBand *int `json:"downgradeBand,omitempty"`
	// DefaultTier serves requests with no user text; empty means the first tier.
	DefaultTier string `json:"defaultTier,omitempty"`
	// ToolsMinTier floors requests that carry tools or tool history; empty means no floor.
	ToolsMinTier string `json:"toolsMinTier,omitempty"`
	// Selection is "ordered" (default), "hash" or "adaptive".
	Selection string `json:"selection,omitempty"`
	// Tiers is ordered cheapest first; list position is the rank.
	Tiers        []SmartRouterTier        `json:"tiers"`
	KeywordRules []SmartRouterKeywordRule `json:"keywordRules,omitempty"`
	// MaxAttempts caps upstream attempts per request; 0 means 4.
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// SaturationDowngrade starts a score-routed request one tier lower when its tier has no free capacity.
	SaturationDowngrade bool `json:"saturationDowngrade,omitempty"`
	// TimeoutFallback adds one attempt on the highest usable tier below the first attempted tier when every
	// attempt missed firstByteTimeout. It needs firstByteTimeout and two tiers.
	TimeoutFallback bool `json:"timeoutFallback,omitempty"`
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
	shadow            *smartWeights
	calibState        *smartCalibState
	bandit            *smartBandit
	calib             smartCalibCfg
	decision          *smartDecisionCfg
	name              string
	selection         string
	classifier        string
	tiers             []smartTier
	rules             []smartKeywordRule
	session           smartSessionCfg
	weights           smartWeights
	intentBase        [smartIntentCount]int
	foreignBase       int
	riskZ             int
	downgradeBand     int
	defaultRank       int
	toolsMinRank      int
	firstByte         time.Duration
	maxAttempts       int
	shadowV2          bool
	adaptiveSet       bool
	satDowngrade      bool
	timeoutFallback   bool
	contextEscalation bool
	sessionEnabled    bool
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

func validateSmartWeights(router, label string, w *SmartRouterWeights) error {
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
			return fmt.Errorf("llmgateway: smartRouters: router %q: %s %q must be between 0 and %d, got %d", router, label, n.name, maxSmartMinScore, *n.v)
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

// validateRouterModelMeta requires a router-named modelMeta entry to set a
// positive window: the window is the only field a router uses. The upper
// bound is not checked here because targets resolve lazily via discovery.
func validateRouterModelMeta(routers map[string]*smartRouter, meta map[string]*ModelMetaConfig) error {
	var bad []string
	for name := range routers {
		if v, ok := meta[name]; ok && v != nil && v.ContextTokens <= 0 {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("llmgateway: modelMeta: entry %q names a smart router: contextTokens must be greater than zero", bad[0])
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

	toolsMinRank := 0
	if cfg.ToolsMinTier != "" {
		r, ok := tierRank[cfg.ToolsMinTier]
		if !ok {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: toolsMinTier %q is not a tier of this router", name, cfg.ToolsMinTier)
		}
		toolsMinRank = r
	}

	selection := cfg.Selection
	switch selection {
	case "":
		selection = smartSelectionOrdered
	case smartSelectionOrdered, smartSelectionHash, smartSelectionAdaptive:
	default:
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: selection %q must be %q, %q or %q", name, cfg.Selection, smartSelectionOrdered, smartSelectionHash, smartSelectionAdaptive)
	}

	if cfg.MaxAttempts < 0 {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: maxAttempts must not be negative", name)
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultSmartMaxAttempts
	}

	firstByte, err := parseFirstByteTimeout(name, cfg.FirstByteTimeout)
	if err != nil {
		return nil, err
	}

	contextEscalation := true
	if cfg.ContextEscalation != nil {
		contextEscalation = *cfg.ContextEscalation
	}

	rules, err := compileSmartKeywordRules(name, cfg.KeywordRules, tierRank)
	if err != nil {
		return nil, err
	}
	if wErr := validateSmartWeights(name, "weight", cfg.Weights); wErr != nil {
		return nil, wErr
	}
	var shadow *smartWeights
	if cfg.ShadowWeights != nil {
		if wErr := validateSmartWeights(name, "shadowWeights weight", cfg.ShadowWeights); wErr != nil {
			return nil, wErr
		}
		sw := buildSmartWeights(cfg.ShadowWeights)
		shadow = &sw
	}

	classifier, shadowV2, riskZ, err := compileSmartClassifier(name, cfg)
	if err != nil {
		return nil, err
	}
	intentBase, foreignBase, err := compileSmartIntentBases(name, cfg.IntentTiers, tiers, tierRank)
	if err != nil {
		return nil, err
	}
	session, err := compileSmartSession(name, cfg.Session, classifier, shadowV2)
	if err != nil {
		return nil, err
	}

	calib, calibState, err := compileSmartCalibration(name, cfg.Calibration, tiers)
	if err != nil {
		return nil, err
	}
	adaptive, err := compileSmartAdaptive(name, cfg.Adaptive)
	if err != nil {
		return nil, err
	}
	downgradeBand, err := compileSmartDowngrade(name, cfg, len(tiers))
	if err != nil {
		return nil, err
	}
	if err = validateSmartTimeoutFallback(name, cfg, len(tiers), firstByte); err != nil {
		return nil, err
	}
	decision, err := compileSmartDecision(name, cfg.Decision, cfg.Classifier)
	if err != nil {
		return nil, err
	}
	var bandit *smartBandit
	if selection == smartSelectionAdaptive {
		bandit = newSmartBandit(adaptive)
	}

	return &smartRouter{
		bandit:            bandit,
		downgradeBand:     downgradeBand,
		satDowngrade:      cfg.SaturationDowngrade,
		timeoutFallback:   cfg.TimeoutFallback,
		adaptiveSet:       cfg.Adaptive != nil,
		calib:             calib,
		decision:          decision,
		calibState:        calibState,
		classifier:        classifier,
		shadowV2:          shadowV2,
		riskZ:             riskZ,
		session:           session,
		sessionEnabled:    cfg.Session != nil && cfg.Session.Enabled,
		intentBase:        intentBase,
		foreignBase:       foreignBase,
		name:              name,
		tiers:             tiers,
		defaultRank:       defaultRank,
		toolsMinRank:      toolsMinRank,
		selection:         selection,
		maxAttempts:       maxAttempts,
		firstByte:         firstByte,
		contextEscalation: contextEscalation,
		rules:             rules,
		weights:           buildSmartWeights(cfg.Weights),
		shadow:            shadow,
	}, nil
}

func compileSmartDowngrade(router string, cfg *SmartRouterConfig, tiers int) (int, error) {
	band := defaultSmartDowngradeBand
	if cfg.DowngradeBand != nil {
		band = *cfg.DowngradeBand
	}
	if band < 0 || band > maxSmartDowngradeBand {
		return 0, fmt.Errorf("llmgateway: smartRouters: router %q: downgradeBand must be between 0 and %d, got %d", router, maxSmartDowngradeBand, band)
	}
	if cfg.SaturationDowngrade && tiers < 2 {
		return 0, fmt.Errorf("llmgateway: smartRouters: router %q: saturationDowngrade needs at least two tiers", router)
	}
	return band, nil
}

// shadowEnabled reports whether the router fills the shadow matrix, by weights or by classifier.
func (rt *smartRouter) shadowEnabled() bool {
	return rt.shadow != nil || rt.shadowV2
}

func compileSmartClassifier(router string, cfg *SmartRouterConfig) (classifier string, shadowV2 bool, riskZ int, err error) {
	classifier = cfg.Classifier
	switch classifier {
	case "":
		classifier = smartClassifierV1
	case smartClassifierV1, smartClassifierV2:
	default:
		return "", false, 0, fmt.Errorf("llmgateway: smartRouters: router %q: classifier %q must be %q or %q", router, cfg.Classifier, smartClassifierV1, smartClassifierV2)
	}
	switch cfg.ShadowClassifier {
	case "":
	case smartClassifierV2:
		if cfg.ShadowWeights != nil {
			return "", false, 0, fmt.Errorf("llmgateway: smartRouters: router %q: shadowClassifier and shadowWeights cannot both be set", router)
		}
		shadowV2 = true
	default:
		return "", false, 0, fmt.Errorf("llmgateway: smartRouters: router %q: shadowClassifier %q must be empty or %q", router, cfg.ShadowClassifier, smartClassifierV2)
	}
	riskZ = defaultSmartRiskZ
	if cfg.RiskZ != nil {
		riskZ = *cfg.RiskZ
		if riskZ < 0 || riskZ > maxSmartRiskZ {
			return "", false, 0, fmt.Errorf("llmgateway: smartRouters: router %q: riskZ must be between 0 and %d, got %d", router, maxSmartRiskZ, riskZ)
		}
	}
	return classifier, shadowV2, riskZ, nil
}

// compileSmartIntentBases maps each intent to a score on this router's own ladder, so
// the same intent lands on the same relative tier for any minScore layout. A tier
// named in intentTiers pins the intent to that tier's boundary instead.
func compileSmartIntentBases(router string, pins map[string]string, tiers []smartTier, tierRank map[string]int) (bases [smartIntentCount]int, foreign int, err error) {
	for i := 0; i < smartIntentCount; i++ {
		bases[i] = smartLadderScore(tiers, smartIntentPos[i])
	}
	foreign = smartLadderScore(tiers, smartForeignPos)
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rank, ok := tierRank[pins[key]]
		if !ok {
			return bases, 0, fmt.Errorf("llmgateway: smartRouters: router %q: intentTiers[%q] tier %q is not a tier of this router", router, key, pins[key])
		}
		score := tiers[rank].minScore
		if key == smartIntentForeign {
			foreign = score
			continue
		}
		known := false
		for i := smartIntentChitchat; i < smartIntentCount; i++ {
			if smartIntentNames[i] == key {
				bases[i] = score
				known = true
			}
		}
		if !known {
			return bases, 0, fmt.Errorf("llmgateway: smartRouters: router %q: intentTiers key %q is not an intent (chitchat, rewrite, edit, debug, design, math, foreign)", router, key)
		}
	}
	return bases, foreign, nil
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
