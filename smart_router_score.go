package traefikllmgateway

const (
	// smartForeignPos places text the English markers cannot classify on the ladder.
	smartForeignPos = 1000

	smartSigmaBase     = 60
	smartSigmaNonLatin = 80
	// smartSigmaNative is taken back when a native-script greeting or rewrite word names the ask.
	smartSigmaNative  = 40
	smartSigmaUnknown = 60
	// smartSigmaUnknownShort is the unknown-intent doubt for a prose too short to hold much unexplained detail.
	smartSigmaUnknownShort = 30
	smartSigmaTruncated    = 60
	smartSigmaShort        = 100
	smartSigmaSpread       = 50

	smartShortProseBytes   = 80
	smartUnknownProseBytes = 60
	smartUnknownLongBytes  = 120
	smartLongHistory       = 20
	smartBytesPerToken     = 4

	smartBonusPromptTokens = 32000
	smartBonusPrompt       = 60
	smartBonusHistory      = 40
	smartBonusOutputTokens = 8000
	smartBonusOutput       = 60
	smartBonusSchema       = 80
	smartBonusJSONObject   = 30
	smartBonusCodeCap      = 100
	smartBonusCodeBytes    = 200
	smartBonusLog          = 80
	smartCarryPct          = 80
	// smartRefBoundary is the first tier boundary the point constants above were tuned on.
	smartRefBoundary = 150
	// smartRefSpan is the distance from the first to the top boundary on that layout.
	smartRefSpan = 200
)

// smartSessionHint carries session-derived inputs to the scorer; the zero value means no session.
type smartSessionHint struct {
	lastScore int
	found     bool
}

// smartVerdict is the v2 result for one request: the score, its uncertainty and the tier it maps to.
type smartVerdict struct {
	reason string
	score  int
	sigma  int
	rank   int
}

// smartLadderScore turns an intent position into a score on the router's own tiers:
// 0..1000 runs from score 0 to the first boundary above tier 0, 1000..2000 on to the top boundary.
func smartLadderScore(tiers []smartTier, pos int) int {
	top := len(tiers) - 1
	if top < 1 {
		return 0
	}
	first := tiers[1].minScore
	if pos <= smartForeignPos {
		return first * pos / smartForeignPos
	}
	if pos >= 2*smartForeignPos {
		return tiers[top].minScore
	}
	return first + (tiers[top].minScore-first)*(pos-smartForeignPos)/smartForeignPos
}

// smartScaled stretches a point count tuned on the reference layout onto the router's own
// tiers, so a margin or bonus moves a request the same share of a tier on any layout. Below the
// first boundary it follows that boundary; above, the span up to the top boundary.
func smartScaled(rt *smartRouter, points, at int) int {
	top := len(rt.tiers) - 1
	if top < 1 || rt.tiers[1].minScore <= 0 {
		return points
	}
	first := rt.tiers[1].minScore
	if at >= first && rt.tiers[top].minScore > first {
		return points * (rt.tiers[top].minScore - first) / smartRefSpan
	}
	return points * first / smartRefBoundary
}

// scoreSmartV2 scores the extracted features. The v1 signal score is a lower bound:
// the intent base can only raise it. Doubt routes up through the sigma margin.
func scoreSmartV2(rt *smartRouter, f *smartFeatures, w smartWeights, sess smartSessionHint) smartVerdict {
	base := rt.intentBase[f.intent]
	if (f.foreign || (f.truncated && !f.found)) && rt.foreignBase > base {
		base = rt.foreignBase
	}
	score := base
	routine := f.routineToolStep()
	if f.found && !routine && (!f.easy || f.hard > 0) {
		reasoningHits := countSmartMarkers(f.lower, smartReasoningMarkers)
		sig := smartSignalsFor(f.lower, len(f.prose), reasoningHits, f.hasTools || f.toolHist)
		if s := sig.score(w); s > score {
			score = s
		}
	}
	score += smartScaled(rt, smartScoreBonuses(f, routine), score)
	score = smartHardFloor(rt, f, score)
	// A long requested output is a generation-quality risk the wording does not show.
	if f.outTokens >= smartBonusOutputTokens && score < rt.intentBase[smartIntentEdit] {
		score = rt.intentBase[smartIntentEdit]
	}
	if len(f.prose) < smartShortProseBytes && f.history {
		if carried := sess.lastScore * smartCarryPct / 100; carried > score {
			score = carried
		}
	}
	if score > smartPerMille {
		score = smartPerMille
	}

	sigma := smartSigmaBase
	if f.nonLatin {
		sigma += smartSigmaNonLatin
		if f.native {
			sigma -= smartSigmaNative
		}
	}
	if f.intent == smartIntentUnknown && !f.foreign && !routine && len(f.prose) >= smartUnknownProseBytes {
		if len(f.prose) < smartUnknownLongBytes {
			sigma += smartSigmaUnknownShort
		} else {
			sigma += smartSigmaUnknown
		}
	}
	if f.truncated {
		sigma += smartSigmaTruncated
	}
	if len(f.prose) < smartShortProseBytes && f.history && !sess.found && !f.isGreeting() {
		sigma += smartSigmaShort
	}
	if f.intentSpread {
		sigma += smartSigmaSpread
	}
	return smartVerdict{score: score, sigma: sigma, rank: smartTierForScore(rt, score+smartScaled(rt, rt.riskZ*sigma/100, score)), reason: smartReasonScore}
}

// smartScoreBonuses adds the request-shape points that do not depend on the wording.
func smartScoreBonuses(f *smartFeatures, routine bool) int {
	n := 0
	if f.promptBytes/smartBytesPerToken > smartBonusPromptTokens {
		n += smartBonusPrompt
	}
	if f.msgCount > smartLongHistory && !routine {
		n += smartBonusHistory
	}
	if f.outTokens >= smartBonusOutputTokens {
		n += smartBonusOutput
	}
	switch f.jsonMode {
	case smartJSONSchema:
		n += smartBonusSchema
	case smartJSONObject:
		n += smartBonusJSONObject
	}
	code := f.codeBytes / smartBonusCodeBytes
	if code > smartBonusCodeCap {
		code = smartBonusCodeCap
	}
	n += code
	if f.logBytes > 0 && (!f.easy || f.hard > 0) {
		n += smartBonusLog
	}
	return n
}

// smartHardFloor lifts the score to the tier the subject of the ask needs, whatever its wording.
func smartHardFloor(rt *smartRouter, f *smartFeatures, score int) int {
	floor := 0
	switch {
	case f.hard >= smartHardStrongScore:
		floor = rt.intentBase[smartIntentDesign]
	case f.hard >= smartHardWeakScore:
		floor = rt.intentBase[smartIntentDebug]
	}
	if f.outTokens >= smartHardOutTokens && f.intent == smartIntentEdit {
		floor = rt.intentBase[smartIntentDesign]
	}
	// A failing tool result is a debug signal whatever the terse ask says.
	if f.toolFail && f.found && floor < rt.intentBase[smartIntentEdit] {
		floor = rt.intentBase[smartIntentEdit]
	}
	if floor > score {
		return floor
	}
	return score
}
