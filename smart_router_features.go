package traefikllmgateway

import (
	"strings"
	"unicode/utf8"
)

const (
	smartAskScanMax       = 400
	smartParagraphCap     = 32
	smartPasteDensity     = 60
	smartPasteMinBytes    = 64
	smartPasteShortLines  = 6
	smartPasteShortMean   = 40
	smartNonLatinPerMille = 200
	smartToolResultEdge   = 2048
	smartChitchatMaxBytes = 240
	smartForeignMinBytes  = 24
	smartFenceMarker      = "```"
	smartParagraphBreak   = "\n\n"
)

// Intent ids are ordered by their base score so a higher id wins a tie between matches.
const (
	smartIntentUnknown = iota
	smartIntentChitchat
	smartIntentRewrite
	smartIntentEdit
	smartIntentDebug
	smartIntentDesign
	smartIntentMath
	smartIntentCount
)

var smartIntentNames = [smartIntentCount]string{"unknown", "chitchat", "rewrite", "edit", "debug", "design", "math"}

// smartIntentPos places each intent on the router's ladder, in thousandths of a tier
// step: 0 is score 0, 1000 is the first boundary above tier 0 and 2000 the top boundary.
var smartIntentPos = [smartIntentCount]int{0, 0, 600, 1000, 1500, 1600, 2000}

// Evidence tables are English only and lowercase; non-English text is handled structurally.
var (
	smartRewriteMarkers = []string{"summar", "rewrite", "rephrase", "translate", "proofread", "shorten", "tl;dr"}
	smartEditMarkers    = []string{"implement", "refactor", "rename", "write a", "add ", "create ", "change "}
	smartDebugMarkers   = []string{"fail", "crash", "bug", "broken", "not working", "doesn't work", "why is", "why does"}
	smartDesignMarkers  = []string{"architecture", "design", "trade-off", "tradeoff", "approach", "migrat", "schema", "strategy", "should we"}
	smartMathMarkers    = []string{"prove", "theorem", "lemma", "derive", "invariant", "complexity of"}
	smartMathStructure  = []string{"$$", "\\frac", "\\sum", "∀", "∃", "≤"}
	// smartEnglishWords are padded with spaces so they only match whole words; short words
	// that are common in other Latin-script languages (a, i, to, do, in) are left out.
	smartEnglishWords = []string{" the ", " and ", " of ", " is ", " it ", " you ", " me ", " my ", " how ", " what ", " why ", " can ", " this ", " that ", " with ", " for ", " are "}
	// A strong signature marks a paragraph as a log on its own; a weak one needs the paragraph to look like a paste.
	smartStrongLogSigs = []string{"Traceback (most recent", "panic:", "\tat ", "npm ERR!", "error[E", "exit status"}
	smartWeakLogSigs   = []string{"error:", "FAIL", "Exception", "SyntaxError"}
	smartPasteChars    = []string{"{", "}", ";", "(", ")", "=", "<", ">", "|", "\t", "    "}
	smartListMarkers   = []string{"\n1.", "\n2.", "\n- ", "\n* "}
	smartQuestionMarks = []string{"?", "？", "؟"}
)

// smartFeatures is what the v2 scorer reads: structure of the newest real ask, never the history.
type smartFeatures struct {
	// prose is the ask without fenced code and paste paragraphs; lower is its lowercase form.
	prose, lower string
	system       string
	intent       int
	codeBytes    int
	logBytes     int
	msgCount     int
	questions    int
	lists        int
	promptBytes  int
	outTokens    int64
	jsonMode     int
	nonLatin     bool
	// foreign marks prose the English markers cannot classify.
	foreign      bool
	truncated    bool
	toolLoopTurn bool
	toolErr      bool
	hasTools     bool
	toolHist     bool
	hasImage     bool
	found        bool
	// intentSpread is set when two matched intents sit far apart on the ladder.
	intentSpread bool
}

const (
	smartJSONNone = iota
	smartJSONObject
	smartJSONSchema
)

// extractSmartV2 finds the newest real user ask with a role-only backward scan and
// splits it into prose and pasted material.
func extractSmartV2(req map[string]any, endpoint string) smartFeatures {
	var f smartFeatures
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		f.hasTools = true
	}
	chars, images := smartPromptSize(req, endpoint)
	f.promptBytes = chars
	f.hasImage = images > 0
	f.outTokens = requestedOutputTokens(req, endpoint)
	f.jsonMode = smartJSONModeOf(req)
	if endpoint == cacheEndpointMessages {
		f.system, _, _ = smartContentText(req["system"])
	}

	msgs, _ := req["messages"].([]any)
	f.msgCount = len(msgs)
	if f.system == "" && len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "system" || role == "developer" {
				f.system, _, _ = smartContentText(first["content"])
			}
		}
	}
	f.system = boundSmartText(f.system, smartSystemCap)

	ask := f.findAsk(msgs)
	if !f.found {
		f.truncated = len(msgs) > smartAskScanMax
		return f
	}
	f.splitAsk(ask)
	f.classifyIntent()
	return f
}

// findAsk scans newest first for the user message that carries text and no tool
// parts, and records tool-loop state on the way.
func (f *smartFeatures) findAsk(msgs []any) string {
	stop := len(msgs) - smartAskScanMax
	if stop < 0 {
		stop = 0
	}
	for i := len(msgs) - 1; i >= stop; i-- {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		newest := i == len(msgs)-1
		if calls, isList := msg["tool_calls"].([]any); isList && len(calls) > 0 {
			f.toolHist = true
		}
		role, _ := msg["role"].(string)
		switch role {
		case "tool", "function":
			f.toolHist = true
			if newest {
				f.toolLoopTurn = true
				f.toolErr = smartToolResultFailed(msg["content"])
			}
		case "user":
			text, _, toolParts := smartContentText(msg["content"])
			if toolParts {
				f.toolHist = true
				if newest {
					f.toolLoopTurn = true
					f.toolErr = smartToolResultFailed(msg["content"])
				}
				continue
			}
			if text != "" {
				f.found = true
				return text
			}
		}
	}
	return ""
}

func smartJSONModeOf(req map[string]any) int {
	rf, ok := req["response_format"].(map[string]any)
	if !ok {
		return smartJSONNone
	}
	typ, _ := rf["type"].(string)
	switch typ {
	case "json_schema":
		return smartJSONSchema
	case "json_object":
		return smartJSONObject
	}
	return smartJSONNone
}

// smartToolResultFailed reports an Anthropic is_error result, else a log signature
// in the head or tail of a result text.
func smartToolResultFailed(content any) bool {
	switch c := content.(type) {
	case string:
		return smartEdgeHasLog(c)
	case []any:
		for i := 0; i < len(c); i++ {
			part, ok := c[i].(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := part["type"].(string); typ != "tool_result" {
				continue
			}
			if bad, _ := part["is_error"].(bool); bad {
				return true
			}
			if smartToolResultFailed(part["content"]) {
				return true
			}
		}
	}
	return false
}

func smartEdgeHasLog(s string) bool {
	if len(s) > 2*smartToolResultEdge {
		s = s[:smartToolResultEdge] + "\n" + s[len(s)-smartToolResultEdge:]
	}
	return containsAnySmart(s, smartStrongLogSigs) || containsAnySmart(s, smartWeakLogSigs)
}

// splitAsk separates prose from fenced code and paste paragraphs and records the
// structural sizes. Odd fence segments are code.
func (f *smartFeatures) splitAsk(text string) {
	segs := strings.Split(text, smartFenceMarker)
	var parts []string
	paragraphs := 0
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if i%2 == 1 {
			f.addPaste(len(seg), containsAnySmart(seg, smartStrongLogSigs) || containsAnySmart(seg, smartWeakLogSigs))
			continue
		}
		left := smartParagraphCap - paragraphs
		if left < 1 {
			left = 1
		}
		paras := strings.SplitN(seg, smartParagraphBreak, left)
		for j := 0; j < len(paras); j++ {
			p := paras[j]
			if strings.TrimSpace(p) == "" {
				continue
			}
			paragraphs++
			isPaste, isLog := smartPasteKind(p)
			if !isPaste {
				parts = append(parts, p)
				continue
			}
			f.addPaste(len(p), isLog)
			// The question often trails a paste with no blank line between them.
			if tail := smartTrailingAsk(p); tail != "" {
				parts = append(parts, tail)
			}
		}
	}
	f.prose = boundSmartText(strings.Join(parts, smartParagraphBreak), smartTextCap)
	f.lower = strings.ToLower(f.prose)
}

func (f *smartFeatures) addPaste(n int, isLog bool) {
	if isLog {
		f.logBytes += n
		return
	}
	f.codeBytes += n
}

// smartPasteKind reports whether a paragraph is pasted material and whether it is a log.
func smartPasteKind(p string) (isPaste, isLog bool) {
	if containsAnySmart(p, smartStrongLogSigs) {
		return true, true
	}
	n := len(p)
	lines := strings.Count(p, "\n") + 1
	if n >= smartPasteMinBytes {
		hits := 0
		for i := 0; i < len(smartPasteChars); i++ {
			hits += strings.Count(p, smartPasteChars[i])
		}
		isPaste = hits*smartPerMille >= n*smartPasteDensity
	}
	if !isPaste && lines >= smartPasteShortLines && n/lines < smartPasteShortMean && !containsAnySmart(p, smartListMarkers) {
		isPaste = true
	}
	if !isPaste {
		return false, false
	}
	return true, containsAnySmart(p, smartWeakLogSigs)
}

// smartTrailingAsk returns the last line of a paste paragraph when it reads as a sentence.
func smartTrailingAsk(p string) string {
	cut := strings.LastIndex(p, "\n")
	if cut < 0 {
		return ""
	}
	last := strings.TrimSpace(p[cut+1:])
	if last == "" {
		return ""
	}
	if isPaste, _ := smartPasteKind(last); isPaste || containsAnySmart(last, smartStrongLogSigs) {
		return ""
	}
	if strings.Count(last, " ") < 2 {
		return ""
	}
	return last
}

// classifyIntent picks the highest-ranked intent whose evidence the prose shows and
// derives the language-agnostic signals.
func (f *smartFeatures) classifyIntent() {
	text := f.lower
	pasted := f.codeBytes > 0 || f.logBytes > 0
	best := smartIntentUnknown
	lo, hi, matched := smartIntentPos[smartIntentMath], 0, 0
	note := func(intent int) {
		matched++
		if intent > best {
			best = intent
		}
		if pos := smartIntentPos[intent]; pos < lo {
			lo = pos
		}
		if pos := smartIntentPos[intent]; pos > hi {
			hi = pos
		}
	}
	if len(f.prose) < smartChitchatMaxBytes && !pasted && containsAnySmart(text, smartSimpleMarkers) {
		note(smartIntentChitchat)
	}
	if containsAnySmart(text, smartRewriteMarkers) {
		note(smartIntentRewrite)
	}
	if (f.codeBytes > 0 || f.hasTools || f.toolHist) && containsAnySmart(text, smartEditMarkers) {
		note(smartIntentEdit)
	}
	if f.logBytes > 0 || containsAnySmart(text, smartDebugMarkers) {
		note(smartIntentDebug)
	}
	if containsAnySmart(text, smartDesignMarkers) {
		note(smartIntentDesign)
	}
	if containsAnySmart(text, smartMathMarkers) || containsAnySmart(f.prose, smartMathStructure) {
		note(smartIntentMath)
	}
	f.intent = best
	f.intentSpread = matched >= 2 && hi-lo >= smartSpreadPos

	f.questions = 0
	for i := 0; i < len(smartQuestionMarks); i++ {
		f.questions += strings.Count(f.prose, smartQuestionMarks[i])
	}
	f.lists = 0
	for i := 0; i < len(smartListMarkers); i++ {
		f.lists += strings.Count(f.prose, smartListMarkers[i])
	}
	if n := len(f.prose); n > 0 {
		f.nonLatin = (n-utf8.RuneCountInString(f.prose))*smartPerMille/n > smartNonLatinPerMille
	}
	if best == smartIntentUnknown && len(f.prose) >= smartForeignMinBytes {
		f.foreign = f.nonLatin || !containsAnySmart(" "+text+" ", smartEnglishWords)
	}
}

// smartSpreadPos is the position gap that counts as two clearly different asks.
const smartSpreadPos = 1000
