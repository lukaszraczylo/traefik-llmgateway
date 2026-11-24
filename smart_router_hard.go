package traefikllmgateway

import "strings"

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
	// smartTechTokens keep a short greeting from reading as chit-chat when a technical subject follows.
	smartTechTokens = []string{"kafka", "sql", "api", "http", "docker", "kubernetes", "k8s", "redis", "postgres", "golang", "python", "json", "error", "bug"}
)

const (
	smartHardStrongScore = 2
	smartHardWeakScore   = 1
	// smartHardOutTokens is the requested output size at which an implementation ask needs the top tier.
	smartHardOutTokens = 16000
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
}
