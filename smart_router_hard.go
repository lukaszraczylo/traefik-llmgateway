package traefikllmgateway

import (
	"strings"
	"unicode/utf8"
)

// Hardness vocabulary for the v2 classifier. It names the subject of an ask, not its phrasing:
// a short question about consensus is hard however politely or tersely it is worded.
// Entries are lowercase. Each entry matches at a word start, so "draft" does not hit "raft".
// A trailing "|" also requires a word end. Latin technical tokens inside non-Latin text match too,
// because raft, jwt, sql and O(n) are written in Latin script in every language.
var (
	// smartHardStrong terms mark an ask that needs the top tier on their own.
	smartHardStrong = []string{
		"raft|", "paxos", "quorum", "linearizab", "serializab", "exactly-once", "exactly once", "idempoten",
		"two-phase", "2pc|", "saga|", "tcc|", "split-brain", "split brain", "consensus", "byzantine", "crdt|",
		"distributed transaction", "snapshot isolation",
		"authz bypass", "authn bypass", "auth bypass", "authorization bypass", "authentication bypass",
		"jwt|", "ssrf|", "csrf|", "xss|", "privilege escalation", "exploitable", "vulnerab",
		"lock-free", "lockfree", "wait-free", "zero-downtime", "zero downtime", "online migration",
		// Native-script terms: zh, ru, ar, ja, hi.
		"幂等", "冪等", "一致性", "分布式", "分散", "死锁", "证明", "証明", "并发", "並行", "漏洞",
		"доказ", "распредел", "конкурент", "взаимоблок", "уязвим",
		"توزيع", "متزامن", "إثبات", "ثغر", "موزع", "مرة واحدة",
		"वितरित", "सिद्ध",
	}
	// smartHardWeak terms raise an ask to the middle tier; two of them count as one strong term.
	smartHardWeak = []string{
		"deadlock", "race condition", "data race", "goroutine", "mutex", "lock-free", "atomic", "concurren", "thread-safe", "cancellab", "context.context",
		"injection", "crypto", "privilege", "authz|", "authn|", "jwt|",
		"o(n", "big-o", "big o|", "complexity", "optimal",
		"billion", "million rows", "window function", "partition", "p99", "latency", "pagination", "sessioniz", "oom|", "ooms|",
		"replica", "cache invalidation", "load balancer", "migration", "backfill",
		"consisten", "transacci", "transaç", "transaktion",
	}
	// smartEasyMarkers name a trivial extraction or a one-line answer; a scary paste next to them does not make the ask hard.
	smartEasyMarkers = []string{
		"which line", "what line", "which file", "what file", "which package", "which function", "just tell me", "just say",
		"in one sentence", "in plain words", "in plain english", "short q", "quick q", "need to read", "no need for", "no lecture",
		"difference between", "docstring", "add a comment", "which db", "what files",
	}
	// smartNativeEasyMarkers are greeting, thanks and rewrite words in common languages other than English.
	smartNativeEasyMarkers = []string{
		"good morning", "just testing", "testing if", "hola", "oye", "hallo", "guten tag", "bonjour", "salut", "cześć", "czesc", "ciao", "olá",
		"danke", "gracias", "merci", "dziękuj", "obrigad", "grazie", "привет", "здравствуй", "спасибо", "مرحبا", "شكرا", "السلام",
		"नमस्ते", "धन्यवाद", "你好", "谢谢", "こんにちは", "ありがとう",
		"korrigier", "übersetz", "résumer", "reformul", "traduire", "traduc", "resumir", "cómo se dice", "wie sagt man", "tłumacz", "przepisz", "riassum", "correggi",
		"直して", "修正", "翻译", "翻訳", "润色", "改写", "переведи", "перепиши", "исправ", "сократ", "صحح", "ترجم", "لخص", "अनुवाद", "सारांश",
	}
	// smartRewriteHeads open an ask that transforms text the user supplies; the material after them can say anything.
	smartRewriteHeads = []string{
		"summar", "rewrite", "rephrase", "translate", "proofread", "shorten", "tl;dr", "tldr", "paraphras", "condense",
		"fix grammar", "fix the grammar", "fix spelling", "make this sound", "make it sound", "make the email", "make this email",
		"more friendly", "more formal", "more concise", "reword", "turn these", "polish",
	}
	// smartRewriteVetoes turn a transform ask into a judgement ask.
	smartRewriteVetoes = []string{"recommend", "should we", "which is better", "good idea", "root cause"}
	// smartFidelityMarkers name text whose meaning must survive a rewrite exactly.
	smartFidelityMarkers = []string{"legal", "clause", "contract", "obligation", "liability", "medical", "dosage", "compliance", "statute"}
	// smartNativeDebugTokens keep a short non-English question about a failure from reading as chit-chat.
	smartNativeDebugTokens = []string{"error", "bug", "exception", "crash", "traceback", "fail", "segfault", "panic", "崩溃", "错误", "报错", "エラー", "ошибк", "خطأ", "त्रुटि", "erro", "fehler", "stürz", "absturz", "falla", "falha", "zawies", "błąd", "panne"}
	// smartGreetingMarkers name a greeting or thanks, which a short follow-up cannot turn into a hard ask.
	smartGreetingMarkers = []string{"hello", "thanks", "thank you", "how are you", "good morning", "good night"}
	// smartTechTokens keep a short greeting from reading as chit-chat when a technical subject follows.
	smartTechTokens = []string{"kafka", "sql", "api", "http", "docker", "kubernetes", "k8s", "redis", "postgres", "golang", "python", "json", "error", "bug"}
)

const (
	smartHardStrongScore = 2
	smartHardWeakScore   = 1
	// smartHardOutTokens is the requested output size at which an implementation ask needs the top tier.
	smartHardOutTokens = 16000
	// smartRewriteHeadBytes is how far into the ask a transform verb still names the whole request.
	smartRewriteHeadBytes = 48
	// smartShortQuestionRunes bounds a non-English question that counts as chit-chat.
	smartShortQuestionRunes = 80
)

func isSmartWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// hasSmartTerm reports whether term occurs in text at a word start.
func hasSmartTerm(text, term string) bool {
	whole := strings.HasSuffix(term, "|")
	if whole {
		term = term[:len(term)-1]
	}
	from := 0
	for from < len(text) {
		i := strings.Index(text[from:], term)
		if i < 0 {
			return false
		}
		at := from + i
		end := at + len(term)
		startOK := at == 0 || !isSmartWordByte(text[at-1])
		endOK := !whole || end >= len(text) || !isSmartWordByte(text[end])
		if startOK && endOK {
			return true
		}
		from = at + 1
	}
	return false
}

func countSmartTerms(text string, terms []string) int {
	n := 0
	for i := 0; i < len(terms); i++ {
		if hasSmartTerm(text, terms[i]) {
			n++
		}
	}
	return n
}

// scanHard records the hardness, easy-ask and native-chit-chat signals of the prose.
func (f *smartFeatures) scanHard() {
	strong := countSmartTerms(f.lower, smartHardStrong)
	weak := countSmartTerms(f.lower, smartHardWeak)
	f.easy = containsAnySmart(f.lower, smartEasyMarkers)
	f.hard = strong*smartHardStrongScore + weak*smartHardWeakScore
	if f.easy && strong == 0 {
		f.hard = 0
	}
	f.native = f.hard == 0 && len(f.prose) < smartChitchatMaxBytes*2 && containsAnySmart(f.lower, smartNativeEasyMarkers) && !containsAnySmart(f.lower, smartTechTokens)
	if !f.native && f.hard == 0 && f.codeBytes == 0 && f.logBytes == 0 {
		f.native = f.shortForeignQuestion()
	}
}

// shortForeignQuestion reports a short question in a language the English words cannot read.
func (f *smartFeatures) shortForeignQuestion() bool {
	if len(f.prose) == 0 || utf8.RuneCountInString(f.prose) >= smartShortQuestionRunes || !containsAnySmart(f.prose, smartQuestionMarks) {
		return false
	}
	return !containsAnySmart(" "+f.lower+" ", smartEnglishWords) && !containsAnySmart(f.lower, smartNativeDebugTokens)
}

// isGreeting reports a greeting or thanks in any language the markers cover.
func (f *smartFeatures) isGreeting() bool {
	return f.native || containsAnySmart(f.lower, smartGreetingMarkers)
}

// scanRewrite flags an ask that opens with a transform verb and carries no hard subject or judgement cue.
func (f *smartFeatures) scanRewrite() {
	f.rewrite = false
	if f.hard > 0 {
		return
	}
	head := smartHead(f.lower, smartRewriteHeadBytes)
	if !containsAnySmart(head, smartRewriteHeads) || containsAnySmart(smartHead(f.lower, 2*smartRewriteHeadBytes), smartRewriteVetoes) {
		return
	}
	f.rewrite = countSmartMarkers(f.lower, smartReasoningMarkers) == 0
}

func smartHead(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
