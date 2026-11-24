package traefikllmgateway

import "strings"

const (
	smartReasonScore   = "score"
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

// classifySmartRequest scores req and returns the starting tier rank, why that
// rank won, the bounded text it scored (stable input for hash selection) and
// whether the request carries image input. It never lowers a rank: floors and
// keyword rules only raise the score-derived one.
func classifySmartRequest(rt *smartRouter, req map[string]any, endpoint string) (rank int, reason string, scoredText string, hasImage bool) {
	ex := extractSmartRequest(req, endpoint)
	top := len(rt.tiers) - 1

	floorRank := 0
	if ex.hasTools || ex.toolHist {
		floorRank = rt.toolsMinRank
	}
	if smartWantsReasoning(req) {
		floorRank = top
	}

	if ex.userText == "" {
		rank = rt.defaultRank
		reason = smartReasonDefault
		if floorRank > rank {
			rank = floorRank
			reason = smartReasonFloor
		}
		return rank, reason, boundSmartText(ex.systemText, smartTextCap), ex.hasImage
	}

	userLen := len(ex.userText)
	text := strings.ToLower(boundSmartText(ex.userText, smartTextCap))
	scoredText = text
	if ex.systemText != "" {
		scoredText = text + "\n" + boundSmartText(ex.systemText, smartSystemCap)
	}

	reasoningHits := countSmartMarkers(text, smartReasoningMarkers)
	if reasoningHits >= smartReasoningFloor {
		floorRank = top
	}

	scoreRank := smartTierForScore(rt, scoreSmartText(rt, text, userLen, reasoningHits, ex.hasTools || ex.toolHist))
	kwRank := -1
	for i := 0; i < len(rt.rules); i++ {
		if rt.rules[i].rank > kwRank && containsAnySmart(text, rt.rules[i].keywords) {
			kwRank = rt.rules[i].rank
		}
	}

	rank = scoreRank
	reason = smartReasonScore
	if kwRank > rank {
		rank = kwRank
		reason = smartReasonKeyword
	}
	if floorRank > rank {
		rank = floorRank
		reason = smartReasonFloor
	}
	return rank, reason, scoredText, ex.hasImage
}

func scoreSmartText(rt *smartRouter, text string, userLen, reasoningHits int, tools bool) int {
	w := rt.weights
	score := smartLengthScore(w.length, userLen)
	score += w.code * smartHitFraction(countSmartMarkers(text, smartCodeMarkers)) / smartPerMille
	score += w.reasoning * smartHitFraction(reasoningHits) / smartPerMille
	score += w.technical * smartHitFraction(countSmartMarkers(text, smartTechnicalMarkers)) / smartPerMille
	score += w.multiStep * smartHitFraction(countSmartMarkers(text, smartMultiStepMarkers)) / smartPerMille
	if strings.Count(text, "?") >= smartMultiQuestionAt {
		score += w.multiQuestion
	}
	if userLen/4 < smartSimpleMaxTokens && containsAnySmart(text, smartSimpleMarkers) {
		score -= w.simple
	}
	if tools {
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
		if rt.tiers[i].minScore <= score {
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
