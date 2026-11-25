package traefikllmgateway

import "strings"

const (
	smartReasonScore   = "score"
	smartReasonModel   = "model"
	smartReasonKeyword = "keyword"
	smartReasonFloor   = "floor"
	smartReasonDefault = "default"

	smartHistoryWindow = 32
	smartTextCap       = 8192
	smartSystemCap     = 1024

	smartLengthMinTokens = 15
	smartLengthMaxTokens = 400
	smartSimpleMaxTokens = 60
	smartMultiQuestionAt = 2
	smartReasoningFloor  = 2
	smartPerMille        = 1000
)

// Marker tables are English only and lowercase; a request matches a table by
// containing any entry, so entries stay specific enough to avoid common words.
var (
	smartCodeMarkers = []string{
		"```", "def ", "func ", "function ", "class ", "import ", "#include", "select ", "=> ",
		"stack trace", "traceback", "exception", "segfault", "compile", "refactor", "debug",
		"unit test", "regex", "implement", "algorithm", "pull request", "null pointer",
	}
	smartReasoningMarkers = []string{
		"step by step", "think through", "analyze", "analyse", "prove", "derive", "trade-off",
		"tradeoff", "pros and cons", "compare and contrast", "evaluate", "reason about",
		"explain why", "why does", "what if", "implications", "justify", "critique", "optimize",
	}
	smartTechnicalMarkers = []string{
		"kubernetes", "docker", "terraform", "database", "distributed", "concurrency", "mutex",
		"deadlock", "latency", "throughput", "encryption", "cryptograph", "oauth", "kafka",
		"postgres", "microservice", "architecture", "compiler", "neural network", "machine learning",
		"gradient", "eigen", "differential", "probability", "statistical", "scalab", "infrastructure",
	}
	smartMultiStepMarkers = []string{
		"step 1", "step 2", "first,", "then ", "next,", "finally", "after that", "followed by",
		"and then", "1. ", "2. ",
	}
	smartSimpleMarkers = []string{
		"what is", "who is", "hello", "thanks", "thank you", "translate", "define ", "capital of",
		"how are you", "tell me a joke", "what time",
	}
)

// smartExtract is what classification reads from a request body.
type smartExtract struct {
	userText   string
	systemText string
	hasImage   bool
	hasTools   bool
	toolHist   bool
}

// smartClassification is the full result of scoring one request.
type smartClassification struct {
	reason     string
	scoredText string
	// decisionOutcome is the decision-model outcome; empty when no call was made.
	decisionOutcome string
	rank            int
	// shadowRank is the rank the router's shadowWeights would have chosen, or -1 when unset.
	shadowRank int
	// score is the router's score before any risk margin, and hardRank the highest floor or keyword
	// rank; the saturation downgrade reads both. hardRank is 0 when nothing floors the request and includes the session pin.
	score    int
	hardRank int
	// decisionTier is the rank the decision model answered, or -1 when it gave none.
	decisionTier int
	// heuristicRank is the rank before the decision model; meaningful only when decisionOutcome is set.
	heuristicRank int
	hasImage      bool
}

// smartAssist returns the model's view for a near-boundary request; nil means no assist.
type smartAssist func(f *smartFeatures) decisionAnswer

// smartNearBoundary reports whether score sits within the decision band of a tier boundary.
func smartNearBoundary(rt *smartRouter, score int) bool {
	for i := 1; i < len(rt.tiers); i++ {
		d := score - rt.tierMin(i)
		if d < 0 {
			d = -d
		}
		if d <= rt.decision.band {
			return true
		}
	}
	return false
}

// applyDecision folds a model answer into the heuristic rank, moving at most one tier toward the model.
func applyDecision(rt *smartRouter, rank int, ans decisionAnswer) (int, string) {
	switch ans.outcome {
	case decisionTimeout, decisionError, decisionBreaker:
		return rank, ans.outcome
	}
	outcome := ans.outcome
	if outcome == "" {
		outcome = decisionOverride
	}
	newRank := rank
	switch {
	case ans.prob < rt.decision.minProb:
		if ans.outcome == "" {
			outcome = decisionLowConf
		}
	case ans.rank == rank:
		if ans.outcome == "" {
			outcome = decisionAgree
		}
	case ans.rank > rank:
		newRank = rank + 1
	default:
		newRank = rank - 1
	}
	return newRank, outcome
}

// smartTransportOutcome reports whether the outcome says the model gave no answer.
func smartTransportOutcome(outcome string) bool {
	return outcome == decisionTimeout || outcome == decisionError || outcome == decisionBreaker
}

// classifySmartRequest scores req and returns the starting tier rank, why that
// rank won, the bounded text it scored (stable input for hash selection) and
// whether the request carries image input. It never lowers a rank: floors and
// keyword rules only raise the score-derived one.
func classifySmartRequest(rt *smartRouter, req map[string]any, endpoint string) (rank int, reason string, scoredText string, hasImage bool) {
	c := classifySmart(rt, req, endpoint)
	return c.rank, c.reason, c.scoredText, c.hasImage
}

// classifySmart is classifySmartRequest plus the shadow rank. The shadow pass
// reuses the extracted features and only re-weights them.
func classifySmart(rt *smartRouter, req map[string]any, endpoint string) smartClassification {
	return classifySmartWith(rt, req, endpoint, nil, nil, nil)
}

// classifySmartWith classifies with optional pre-extracted v2 features, session state and decision assist.
// A v1 router passes the session to the shadow pass only, so it never routes on it or asks the assist.
func classifySmartWith(rt *smartRouter, req map[string]any, endpoint string, f *smartFeatures, sc *smartSessionRun, assist smartAssist) smartClassification {
	if rt.classifier == smartClassifierV2 {
		out := classifySmartV2With(rt, req, endpoint, f, sc, assist)
		if rt.shadowV2 {
			out.shadowRank = out.rank
		}
		return out
	}
	out := classifySmartV1(rt, req, endpoint)
	if rt.shadowV2 {
		out.shadowRank = classifySmartV2With(rt, req, endpoint, f, sc, nil).rank
	}
	return out
}

func classifySmartV1(rt *smartRouter, req map[string]any, endpoint string) smartClassification {
	ex := extractSmartRequest(req, endpoint)
	top := len(rt.tiers) - 1
	out := smartClassification{shadowRank: -1, hasImage: ex.hasImage, decisionTier: -1, heuristicRank: -1}

	floorRank := 0
	if ex.hasTools || ex.toolHist {
		floorRank = rt.toolsMinRank
	}
	if smartWantsReasoning(req) {
		floorRank = top
	}

	if ex.userText == "" {
		out.rank = rt.defaultRank
		out.reason = smartReasonDefault
		if floorRank > out.rank {
			out.rank = floorRank
			out.reason = smartReasonFloor
		}
		out.scoredText = boundSmartText(ex.systemText, smartTextCap)
		if rt.shadow != nil {
			out.shadowRank = out.rank
		}
		return out
	}

	userLen := len(ex.userText)
	text := strings.ToLower(boundSmartText(ex.userText, smartTextCap))
	out.scoredText = text
	if ex.systemText != "" {
		out.scoredText = text + "\n" + boundSmartText(ex.systemText, smartSystemCap)
	}

	reasoningHits := countSmartMarkers(text, smartReasoningMarkers)
	if reasoningHits >= smartReasoningFloor {
		floorRank = top
	}

	sig := smartSignalsFor(text, userLen, reasoningHits, ex.hasTools || ex.toolHist)
	out.score = sig.score(rt.weights)
	scoreRank := smartTierForScore(rt, out.score)
	kwRank := -1
	for i := 0; i < len(rt.rules); i++ {
		if rt.rules[i].rank > kwRank && containsAnySmart(text, rt.rules[i].keywords) {
			kwRank = rt.rules[i].rank
		}
	}

	out.hardRank = smartMaxInt(floorRank, kwRank)
	out.rank = scoreRank
	out.reason = smartReasonScore
	if kwRank > out.rank {
		out.rank = kwRank
		out.reason = smartReasonKeyword
	}
	if floorRank > out.rank {
		out.rank = floorRank
		out.reason = smartReasonFloor
	}
	if rt.shadow != nil {
		shadow := smartTierForScore(rt, sig.score(*rt.shadow))
		if kwRank > shadow {
			shadow = kwRank
		}
		if floorRank > shadow {
			shadow = floorRank
		}
		out.shadowRank = shadow
	}
	return out
}

// smartSignals are the weight-independent features of one request's text.
type smartSignals struct {
	userLen, reasoningHits, codeHits, technicalHits, multiStepHits int
	multiQuestion, simple, tools                                   bool
}

func smartSignalsFor(text string, userLen, reasoningHits int, tools bool) smartSignals {
	return smartSignals{
		userLen:       userLen,
		reasoningHits: reasoningHits,
		codeHits:      countSmartMarkers(text, smartCodeMarkers),
		technicalHits: countSmartMarkers(text, smartTechnicalMarkers),
		multiStepHits: countSmartMarkers(text, smartMultiStepMarkers),
		multiQuestion: strings.Count(text, "?") >= smartMultiQuestionAt,
		simple:        userLen/4 < smartSimpleMaxTokens && containsAnySmart(text, smartSimpleMarkers),
		tools:         tools,
	}
}

func (sg smartSignals) score(w smartWeights) int {
	score := smartLengthScore(w.length, sg.userLen)
	score += w.code * smartHitFraction(sg.codeHits) / smartPerMille
	score += w.reasoning * smartHitFraction(sg.reasoningHits) / smartPerMille
	score += w.technical * smartHitFraction(sg.technicalHits) / smartPerMille
	score += w.multiStep * smartHitFraction(sg.multiStepHits) / smartPerMille
	if sg.multiQuestion {
		score += w.multiQuestion
	}
	if sg.simple {
		score -= w.simple
	}
	if sg.tools {
		score += w.tools
	}
	if score < 0 {
		return 0
	}
	if score > smartPerMille {
		return smartPerMille
	}
	return score
}

func smartLengthScore(weight, userLen int) int {
	tokens := userLen / 4
	if tokens < smartLengthMinTokens {
		return 0
	}
	if tokens >= smartLengthMaxTokens {
		return weight
	}
	return weight * (tokens - smartLengthMinTokens) / (smartLengthMaxTokens - smartLengthMinTokens)
}

// smartHitFraction maps a distinct-marker hit count to a per-mille share of a
// dimension's weight, saturating at three hits.
func smartHitFraction(hits int) int {
	switch {
	case hits <= 0:
		return 0
	case hits == 1:
		return 500
	case hits == 2:
		return 750
	}
	return smartPerMille
}

func smartTierForScore(rt *smartRouter, score int) int {
	rank := 0
	for i := 0; i < len(rt.tiers); i++ {
		if rt.tierMin(i) <= score {
			rank = i
		}
	}
	return rank
}

func countSmartMarkers(text string, markers []string) int {
	hits := 0
	for i := 0; i < len(markers); i++ {
		if strings.Contains(text, markers[i]) {
			hits++
		}
	}
	return hits
}

func containsAnySmart(text string, needles []string) bool {
	for i := 0; i < len(needles); i++ {
		if strings.Contains(text, needles[i]) {
			return true
		}
	}
	return false
}

// boundSmartText keeps the head and tail of an oversized text: the ask often
// follows pasted material, so dropping only the tail would lose it.
func boundSmartText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := limit / 2
	return s[:half] + "\n" + s[len(s)-half:]
}

func smartWantsReasoning(req map[string]any) bool {
	if effort, ok := req["reasoning_effort"].(string); ok {
		e := strings.ToLower(effort)
		if e == "medium" || e == "high" {
			return true
		}
	}
	if th, ok := req["thinking"].(map[string]any); ok {
		if typ, isStr := th["type"].(string); isStr && typ == "enabled" {
			return true
		}
	}
	return false
}

// extractSmartRequest walks the newest smartHistoryWindow messages once,
// newest first. A user turn without text (tool results only, or images only)
// is skipped so an agent loop keeps the tier of the original ask.
func extractSmartRequest(req map[string]any, endpoint string) smartExtract {
	var ex smartExtract
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		ex.hasTools = true
	}
	if endpoint == cacheEndpointMessages {
		ex.systemText, _, _ = smartContentText(req["system"])
	}
	_, images := smartMessagesSize(req, endpoint)
	ex.hasImage = images > 0

	msgs, _ := req["messages"].([]any)
	stop := len(msgs) - smartHistoryWindow
	if stop < 0 {
		stop = 0
	}
	for i := len(msgs) - 1; i >= stop; i-- {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text, _, toolParts := smartContentText(msg["content"])
		if toolParts || role == "tool" || role == "function" {
			ex.toolHist = true
		}
		if calls, isList := msg["tool_calls"].([]any); isList && len(calls) > 0 {
			ex.toolHist = true
		}
		switch role {
		case "user":
			if ex.userText == "" {
				ex.userText = text
			}
		case "system", "developer":
			if ex.systemText == "" {
				ex.systemText = text
			}
		}
	}
	return ex
}

// smartContentText flattens a message content value (a string, or a list of
// typed parts) into its text, counting image parts and reporting whether any
// part is a tool call or tool result.
func smartContentText(content any) (text string, images int, toolParts bool) {
	// yaegi v0.16.1 keeps named results from the previous call, so reset them.
	images, toolParts = 0, false
	switch c := content.(type) {
	case string:
		return c, 0, false
	case []any:
		var parts []string
		for i := 0; i < len(c); i++ {
			part, ok := c[i].(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text", "input_text":
				if s, isStr := part["text"].(string); isStr && s != "" {
					parts = append(parts, s)
				}
			case "image_url", "image", "input_image":
				images++
			case "tool_use", "tool_result":
				toolParts = true
			}
		}
		return strings.Join(parts, "\n"), images, toolParts
	}
	return "", 0, false
}

// smartPromptSize returns the prompt's text length and image-part count:
// messages plus the tools schema, whose bytes count toward the context window.
func smartPromptSize(req map[string]any, endpoint string) (chars, images int) {
	chars, images = smartMessagesSize(req, endpoint)
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		chars += smartValueSize(tools)
	}
	return chars, images
}

// smartMessagesSize walks every message, plus the Anthropic system field, and
// returns the text and tool-argument length and the image-part count. Images
// inside tool_result content are included.
func smartMessagesSize(req map[string]any, endpoint string) (chars, images int) {
	chars, images = 0, 0
	if endpoint == cacheEndpointMessages {
		c, i := smartContentSize(req["system"], 0)
		chars, images = chars+c, images+i
	}
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		c, i := smartContentSize(msg["content"], 0)
		chars, images = chars+c+smartToolCallsSize(msg["tool_calls"]), images+i
	}
	return chars, images
}

// smartContentSize measures a content value; nested tool_result content is
// followed one level deep.
func smartContentSize(content any, depth int) (chars, images int) {
	chars, images = 0, 0
	switch c := content.(type) {
	case string:
		return len(c), 0
	case []any:
		for i := 0; i < len(c); i++ {
			part, ok := c[i].(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text", "input_text":
				if s, isStr := part["text"].(string); isStr {
					chars += len(s)
				}
			case "image_url", "image", "input_image":
				images++
			case "tool_use":
				chars += smartValueSize(part["input"])
			case "tool_result":
				if depth < 1 {
					ic, ii := smartContentSize(part["content"], depth+1)
					chars, images = chars+ic, images+ii
				}
			}
		}
	}
	return chars, images
}

// classifySmartV2With is the v2 counterpart of classifySmartV1: it scores the prose of
// the newest ask with an uncertainty margin, then raises the tier for floors and
// keyword rules. It never lowers a rank.
// It takes optional pre-extracted features, session state and decision assist.
func classifySmartV2With(rt *smartRouter, req map[string]any, endpoint string, f *smartFeatures, sc *smartSessionRun, assist smartAssist) smartClassification {
	if f == nil {
		ext := extractSmartV2(req, endpoint)
		f = &ext
	}
	top := len(rt.tiers) - 1
	out := smartClassification{shadowRank: -1, hasImage: f.hasImage, decisionTier: -1, heuristicRank: -1}

	floorRank := 0
	if f.hasTools || f.toolHist {
		floorRank = rt.toolsMinRank
	}
	if smartWantsReasoning(req) {
		floorRank = top
	}

	if !f.found && !f.truncated {
		out.rank = rt.defaultRank
		out.reason = smartReasonDefault
		if floorRank > out.rank {
			out.rank = floorRank
			out.reason = smartReasonFloor
		}
		out.scoredText = f.system
		if rt.shadow != nil {
			out.shadowRank = out.rank
		}
		return out
	}

	out.scoredText = f.lower
	if f.system != "" {
		out.scoredText = f.lower + "\n" + f.system
	}
	if countSmartMarkers(f.lower, smartReasoningMarkers) >= smartReasoningFloor {
		floorRank = top
	}
	kwRank := -1
	for i := 0; i < len(rt.rules); i++ {
		if rt.rules[i].rank > kwRank && containsAnySmart(f.lower, rt.rules[i].keywords) {
			kwRank = rt.rules[i].rank
		}
	}

	var hint smartSessionHint
	askHash := 0
	if sc != nil {
		hint = sc.hint()
		askHash = smartAskHash(f)
	}
	verdict := scoreSmartV2(rt, f, rt.weights, hint)
	out.score = verdict.score
	out.hardRank = smartMaxInt(floorRank, kwRank)
	rank, reason := verdict.rank, smartReasonScore
	var next smartSession
	if sc != nil {
		rank, reason, next = smartSessionRoute(&rt.session, top, f, sc.sess, verdict, sc.now, askHash)
		out.hardRank = smartMaxInt(out.hardRank, next.pin)
	}
	if assist != nil && reason == smartReasonScore && f.found && smartNearBoundary(rt, verdict.score) {
		ans := assist(f)
		newRank, outcome := applyDecision(rt, rank, ans)
		out.heuristicRank, out.decisionOutcome = rank, outcome
		if !smartTransportOutcome(ans.outcome) {
			out.decisionTier = ans.rank
		}
		if rt.decision.live && newRank != rank {
			rank, reason = newRank, smartReasonModel
		}
	}
	out.rank, out.reason = smartRaiseRank(rank, reason, kwRank, floorRank)
	if sc != nil {
		sc.finish(next, out.rank)
	}
	if rt.shadow != nil {
		shadow := scoreSmartV2(rt, f, *rt.shadow, hint)
		shadowRank := shadow.rank
		if sc != nil {
			shadowRank, _, _ = smartSessionRoute(&rt.session, top, f, sc.sess, shadow, sc.now, askHash)
		}
		out.shadowRank, _ = smartRaiseRank(shadowRank, smartReasonScore, kwRank, floorRank)
	}
	return out
}

// smartRaiseRank applies the keyword and floor ranks to a score rank, keeping the reason of the winner.
func smartRaiseRank(rank int, reason string, kwRank, floorRank int) (int, string) {
	if kwRank > rank {
		rank = kwRank
		reason = smartReasonKeyword
	}
	if floorRank > rank {
		rank = floorRank
		reason = smartReasonFloor
	}
	return rank, reason
}
