// Command pricing-sync regenerates the plugin's repo-root
// pricing_data_gen.go: a built-in, LiteLLM-synced table of per-model
// context window and per-token cost (feature v0.23's builtin layer,
// modelmeta.go's builtinModelMeta/builtinModelMetaTable), which
// resolveModelMeta falls back to when neither an operator's modelMeta
// config override nor a provider's own discovery capture knows a value.
//
// It fetches LiteLLM's community-maintained
// model_prices_and_context_window.json (litellmSourceURL below — 3111
// entries as of this feature's own snapshot, 2026-08) at BUILD TIME
// only, never at plugin runtime: the plugin module stays free of any
// network dependency, and the generated table is a plain, checked-in Go
// map literal an operator's `go build`/Yaegi interpretation never
// fetches anything to produce.
//
// # Pruning rule
//
// A 3111-entry table in Yaegi-interpreted source is a real startup cost
// (parsing and evaluating a large map literal under an AST interpreter
// is far slower than under a compiler) — this tool prunes aggressively,
// on three independent axes, ALL of which an entry must pass to survive:
//
//  1. Provider allowlist (allowedProviders below): only LiteLLM's own
//     "litellm_provider" tag values this gateway's provider TYPES can
//     meaningfully match against an operator's OWN model ids —
//     "openai", "anthropic", "deepseek", "xai" (Grok), "minimax",
//     "cerebras", "dashscope" (Alibaba Cloud's DashScope API, which is
//     where LiteLLM files its Qwen models — verified against a live
//     fetch of the source JSON while building this generator, 2026-08:
//     there is no "qwen" or "alibaba" litellm_provider tag, and no
//     "qwen/"-prefixed key exists in the source either), and "moonshot"
//     (Kimi models — coverage-audit addendum, 2026-08). Every other
//     provider LiteLLM tracks (bedrock, azure, vertex_ai, openrouter,
//     novita, github_copilot, ...) is dropped outright — this gateway
//     has no adapter type that would ever route to most of them under a
//     bare model id anyway. Notably NOT allowlisted despite being
//     considered during the coverage audit: "openrouter" (no entry in
//     the fetched source carries a ":free"-suffixed key — a claimed
//     free-tier naming convention that does not appear in this file as
//     fetched) and "novita" (where LiteLLM does track a few otherwise-
//     uncovered ids, e.g. xiaomi's mimo-v2.5 family, but adding either
//     whole catalog — openrouter alone re-adds ~100 entries — cuts
//     against this rule's own "keep the interpreted table small" goal;
//     left as an operator decision, not made unilaterally here).
//  2. Mode allowlist (allowedModes below): "chat" and "embedding" only —
//     LiteLLM also prices image generation, audio transcription/speech,
//     moderation, and rerank endpoints under the same file, none of
//     which this feature's context-window/cost metadata is meant to
//     describe.
//  3. A well-formed entry: both input_cost_per_token and
//     output_cost_per_token must decode as JSON numbers (many LiteLLM
//     entries price only per-image or per-request and leave these
//     null); an entry whose JSON shape does not even decode into
//     litellmEntry at all (LiteLLM's own "sample_spec" schema-example
//     key mixes description STRINGS into fields this tool expects as
//     numbers) is skipped rather than aborting the whole sync — this is
//     a defensive fallback, not just a hardcoded "sample_spec" skip, so
//     a future upstream schema-example entry with some other key name
//     degrades the same way.
//
// # Bare-id collision resolution
//
// The plugin's builtin tables (both the pre-existing builtinPricing,
// pricing.go, and this feature's builtinModelMetaTable) are keyed by
// BARE model id only, never "provider/model" — an operator's own
// provider config key never matches LiteLLM's canonical provider name,
// so a provider-qualified builtin entry would never be reachable via
// resolveModelMeta's actual lookup (modelmeta.go). LiteLLM itself often
// carries BOTH a bare key ("deepseek-chat") and one or more
// provider-prefixed keys for the identical or a re-hosted model
// ("deepseek/deepseek-chat", sometimes "dashscope/deepseek-v4-flash"
// with DIFFERENT pricing served through a different host) — chooseWinner
// resolves this deterministically: prefer a key that carries no
// "provider/" prefix at all, then the earliest provider in
// providerPriority (declared in the same order as allowedProviders'
// documentation above), then the lexicographically smallest full key as
// a final, always-deterministic tiebreak.
//
// # Naming bridges
//
// LiteLLM's own model naming occasionally disagrees with what a
// provider's own live API reports for the IDENTICAL model — not a
// pricing disagreement (chooseWinner's problem above), but a pure label
// mismatch. namingBridges (below) is a small, explicitly data-driven
// table for exactly this: each entry matches a bare id's suffix under
// one provider and additionally emits an identical-valued duplicate
// under a second suffix, so a future one-off naming split is a one-line
// table addition, not new code. Never invents a value: a bridge only
// ever duplicates an entry LiteLLM already priced, and never overwrites
// an id LiteLLM already prices directly under its own bridged name.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// litellmSourceURL is LiteLLM's community-maintained model price/context
// table, fetched fresh on every `make pricing-sync` run — never at
// plugin runtime (this tool's own doc comment above).
const litellmSourceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// fetchTimeout bounds the network fetch: generous for a ~2MB JSON file
// over a normal connection, while still failing a hung request instead
// of blocking a `make pricing-sync` run indefinitely.
const fetchTimeout = 60 * time.Second

// microUSDPerUSDPerToken converts a LiteLLM per-SINGLE-token USD price
// to micro-USD PER MILLION tokens in one multiplication:
// cost_per_token (USD/token) * 1_000_000 (tokens/Mtok) * 1_000_000
// (micro-USD/USD) = cost_per_token * 1e12.
const microUSDPerUSDPerToken = 1e12

// sampleSpecKey is LiteLLM's own schema-example entry — explicitly named
// here (in addition to the generic decode-failure fallback the package
// doc comment describes) so the pruning rule reads unambiguously for
// this specific, known case.
const sampleSpecKey = "sample_spec"

// allowedProviders is the pruning rule's provider allowlist — see the
// package doc comment for the full rationale and the qwen/alibaba ->
// dashscope mapping.
var allowedProviders = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"deepseek":  true,
	"xai":       true, // Grok
	"minimax":   true,
	"cerebras":  true,
	"dashscope": true, // Qwen / Alibaba Cloud
	// moonshot: Kimi models (coverage-audit addendum, 2026-08) —
	// verified live against a fetch of the source JSON: 23 entries,
	// litellm_provider "moonshot", all mode "chat", bare ids like
	// "kimi-k2.5"/"kimi-k3". Added as a first-class allowlisted provider
	// (not a per-id bridge): builtinModelMetaTable is already keyed by
	// bare id only (bareModelID strips ANY matching allowlisted
	// provider's own "provider/" prefix), so an operator's own
	// Kimi-serving deployment — whether addressed directly or through an
	// aggregator such as Alibaba's DashScope — resolves against these
	// bare ids the same way any other allowlisted provider's entries do.
	"moonshot": true,
}

// providerPriority orders allowedProviders for chooseWinner's collision
// tiebreak — same set, as a stable slice instead of a randomly-ordered
// map.
var providerPriority = []string{"openai", "anthropic", "deepseek", "xai", "minimax", "cerebras", "dashscope", "moonshot"}

// allowedModes is the pruning rule's mode allowlist — see the package
// doc comment.
var allowedModes = map[string]bool{"chat": true, "embedding": true}

// namingBridge is one entry in namingBridges — see the package doc
// comment's "Naming bridges" section.
type namingBridge struct {
	provider   string
	fromSuffix string
	toSuffix   string
}

// namingBridges documents known cases where a LiteLLM key's own naming
// differs from what an operator's live discovery reports for the exact
// same model. Seeded with MiniMax's dual naming (generator addendum,
// 2026-08): LiteLLM's own model list calls the fast tier "-lightning"
// (its international/global naming); MiniMax's own live API
// (api.minimax.io, verified against a running deployment discovering
// bare ids MiniMax-M2, -M2.1, -M2.1-highspeed, -M2.5, -M2.5-highspeed,
// -M2.7, -M2.7-highspeed, -M3) reports the identical models as
// "-highspeed" — same weights, same price, a different marketing label
// per region. -M2.7/-M2.7-highspeed have no LiteLLM entry at all as of
// this generator's own snapshot (verified live) and are deliberately
// left uncovered here — applyNamingBridges only ever duplicates an
// entry LiteLLM already priced, never invents one; -M2.7 is a candidate
// for an operator's own modelMeta config override instead.
var namingBridges = []namingBridge{
	{provider: "minimax", fromSuffix: "-lightning", toSuffix: "-highspeed"},
}

// applyNamingBridges adds one extra table entry per namingBridges match:
// for each winning candidate whose provider and bare-id suffix match a
// bridge, it duplicates that entry (identical ContextTokens/cost — the
// SAME model under a different label, not a different price point)
// under the bridged id. A bridged id that collides with an id already
// present in table (because LiteLLM prices it directly, under its own
// entry) is left untouched — an entry LiteLLM already prices directly
// always wins over a bridged duplicate.
func applyNamingBridges(table map[string]builtinEntry, winners map[string]candidate) {
	for bare, w := range winners {
		for _, br := range namingBridges {
			if w.provider != br.provider || !strings.HasSuffix(bare, br.fromSuffix) {
				continue
			}
			bridged := strings.TrimSuffix(bare, br.fromSuffix) + br.toSuffix
			if _, exists := table[bridged]; exists {
				continue
			}
			table[bridged] = w.entry
		}
	}
}

// litellmEntry is the subset of one LiteLLM model entry this tool reads.
// Cost and max-token fields are *float64, not *int: most LiteLLM entries
// report integer token counts, but a handful (observed live among xai's
// own entries, e.g. "xai/grok-4-fast-reasoning") report
// max_input_tokens/max_tokens as a JSON float (2000000.0) — decoding
// those into *int would fail outright and drop the entry.
type litellmEntry struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
	MaxInputTokens     *float64 `json:"max_input_tokens"`
	MaxTokens          *float64 `json:"max_tokens"`
	LiteLLMProvider    string   `json:"litellm_provider"`
	Mode               string   `json:"mode"`
}

// builtinEntry mirrors the plugin's own builtinModelMeta (modelmeta.go)
// field for field — kept as a separate, tool-local type rather than
// importing the plugin package, so this generator never depends on the
// (interpreted-only-constrained) plugin module at all.
type builtinEntry struct {
	ContextTokens             int
	InputCostPerMTokMicroUSD  int64
	OutputCostPerMTokMicroUSD int64
}

// candidate is one surviving LiteLLM key considered for a given bare
// model id — chooseWinner's input, when more than one key collapses to
// the same bare id.
type candidate struct {
	fullKey  string
	provider string
	entry    builtinEntry
	bare     bool // true when fullKey itself carried no "provider/" prefix
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pricing-sync: FAIL:", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot := "."
	if len(os.Args) > 1 {
		repoRoot = os.Args[1]
	}
	outPath := filepath.Join(repoRoot, "pricing_data_gen.go")

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	data, err := fetchLiteLLMPrices(ctx, litellmSourceURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", litellmSourceURL, err)
	}

	table, totalUpstream, err := parseAndPrune(data)
	if err != nil {
		return fmt.Errorf("parse and prune: %w", err)
	}

	src, err := renderGoSource(table, sourceMeta{
		sourceDigest:  contentDigest(data),
		totalUpstream: totalUpstream,
		kept:          len(table),
	})
	if err != nil {
		return fmt.Errorf("render generated source: %w", err)
	}

	if err := os.WriteFile(outPath, []byte(src), 0o644); err != nil { //nolint:gosec // generated Go source, not a secret
		return fmt.Errorf("write %s: %w", outPath, err)
	}

	fmt.Printf("pricing-sync: wrote %s: %d entries kept (pruned from %d upstream entries)\n", outPath, len(table), totalUpstream)
	return nil
}

// fetchLiteLLMPrices issues the network fetch, returning the raw
// response body.
func fetchLiteLLMPrices(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// parseAndPrune decodes data (LiteLLM's top-level {key: entry} JSON
// object) and applies the pruning rule (package doc comment): the
// provider and mode allowlists, well-formed cost fields, and
// chooseWinner's deterministic bare-id collision resolution. It returns
// the pruned table and the total number of top-level keys data carried
// (including ones this function skips), for the summary line run prints.
func parseAndPrune(data []byte) (map[string]builtinEntry, int, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, fmt.Errorf("decode top-level object: %w", err)
	}

	byBare := make(map[string][]candidate)
	for key, rawEntry := range raw {
		if key == sampleSpecKey {
			continue
		}
		var e litellmEntry
		if err := json.Unmarshal(rawEntry, &e); err != nil {
			// Not this tool's problem to solve generically — an entry
			// whose shape does not match litellmEntry at all (the
			// package doc comment's "sample_spec-shaped" fallback) is
			// simply not a model this table can describe.
			continue
		}
		if !allowedProviders[e.LiteLLMProvider] || !allowedModes[e.Mode] {
			continue
		}
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			continue
		}

		bare, isBareKey := bareModelID(key, e.LiteLLMProvider)
		byBare[bare] = append(byBare[bare], candidate{
			fullKey:  key,
			provider: e.LiteLLMProvider,
			bare:     isBareKey,
			entry: builtinEntry{
				ContextTokens:             contextTokens(e),
				InputCostPerMTokMicroUSD:  microUSDPerMTok(*e.InputCostPerToken),
				OutputCostPerMTokMicroUSD: microUSDPerMTok(*e.OutputCostPerToken),
			},
		})
	}

	winners := make(map[string]candidate, len(byBare))
	for bare, candidates := range byBare {
		winners[bare] = chooseWinner(candidates)
	}

	table := make(map[string]builtinEntry, len(winners))
	for bare, w := range winners {
		table[bare] = w.entry
	}
	applyNamingBridges(table, winners)

	return table, len(raw), nil
}

// bareModelID strips a literal "{provider}/" prefix from key when
// present, returning the bare id and whether key itself already WAS
// bare (carried no such prefix) — chooseWinner's first, strongest
// collision tiebreak.
func bareModelID(key, provider string) (bare string, isBareKey bool) {
	prefix := provider + "/"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix), false
	}
	return key, true
}

// contextTokens picks e's context window: max_input_tokens when
// reported, else max_tokens (LiteLLM's own documented fallback — see
// this repo's litellm_prices.json "sample_spec" field descriptions),
// else 0 (unknown — resolveModelMeta, modelmeta.go, never trusts a
// zero).
func contextTokens(e litellmEntry) int {
	if e.MaxInputTokens != nil && *e.MaxInputTokens > 0 {
		return int(math.Round(*e.MaxInputTokens))
	}
	if e.MaxTokens != nil && *e.MaxTokens > 0 {
		return int(math.Round(*e.MaxTokens))
	}
	return 0
}

// microUSDPerMTok converts a LiteLLM per-token USD price to micro-USD
// per million tokens, rounded to the nearest micro-USD — the same
// round-to-nearest discipline pricing.go's costMicros already documents
// for the plugin's own cost-accounting math, to avoid float64
// representation drift undershooting by one unit.
func microUSDPerMTok(costPerToken float64) int64 {
	return int64(math.Round(costPerToken * microUSDPerUSDPerToken))
}

// chooseWinner picks the deterministic survivor among candidates that
// all collapsed to the same bare model id — see the package doc
// comment's "Bare-id collision resolution" section for the full
// rationale. candidates is never empty (only ever built by appending at
// least one entry, parseAndPrune above).
func chooseWinner(candidates []candidate) candidate {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if candidateLess(c, best) {
			best = c
		}
	}
	return best
}

// candidateLess reports whether a should win over b: a bare key (no
// "provider/" prefix) beats a prefixed one; failing that, an earlier
// providerPriority index wins; failing that, the lexicographically
// smaller full key wins — always deterministic, since fullKey is unique
// per candidate by construction.
func candidateLess(a, b candidate) bool {
	if a.bare != b.bare {
		return a.bare
	}
	ai, bi := providerPriorityIndex(a.provider), providerPriorityIndex(b.provider)
	if ai != bi {
		return ai < bi
	}
	return a.fullKey < b.fullKey
}

// providerPriorityIndex returns provider's index in providerPriority, or
// len(providerPriority) if somehow absent (defensive only — every
// candidate's provider already passed the allowedProviders check, which
// is drawn from the exact same set).
func providerPriorityIndex(provider string) int {
	for i, p := range providerPriority {
		if p == provider {
			return i
		}
	}
	return len(providerPriority)
}

// contentDigestLen is how many leading hex characters of the SHA-256
// digest contentDigest keeps — enough to be a stable, practically
// collision-free content fingerprint for a header comment, without
// printing a full 64-character hash nobody reads in full.
const contentDigestLen = 12

// contentDigest returns a short SHA-256 fingerprint of data, hex
// encoded — the generated file's snapshot label (review fix, folded
// minor), so a `make pricing-sync` re-run against UNCHANGED upstream
// bytes reproduces an identical file, with no diff at all, rather than a
// wall-clock date bumping every run regardless of whether the data
// itself changed.
func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:contentDigestLen]
}

// sourceMeta is renderGoSource's header-comment material. sourceDigest is
// a short content hash of the FETCHED upstream bytes (review fix,
// folded minor), not a wall-clock date: a date-stamped header made every
// `make pricing-sync` run touch the generated file even when the
// upstream data had not changed at all, producing a spurious daily diff
// with nothing real to review. A content hash changes only when the
// underlying data actually changes.
type sourceMeta struct {
	sourceDigest  string
	totalUpstream int
	kept          int
}

// renderGoSource formats table as the generated pricing_data_gen.go
// source: a documented header, then a single, gofmt-canonical map
// literal — plain, no generics (the plugin package is Yaegi-interpreted;
// see this tool's own package doc comment and modelmeta.go's
// builtinModelMeta doc comment) — entries sorted by key for a stable,
// reviewable diff on every regeneration.
func renderGoSource(table map[string]builtinEntry, meta sourceMeta) (string, error) {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("package traefikllmgateway\n\n")
	fmt.Fprintf(&b, "// Code generated by tools/pricing-sync from LiteLLM's\n")
	fmt.Fprintf(&b, "// model_prices_and_context_window.json — DO NOT EDIT.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Source: %s\n", litellmSourceURL)
	fmt.Fprintf(&b, "// Source digest: sha256:%s\n", meta.sourceDigest)
	fmt.Fprintf(&b, "// %d entries kept (pruned from %d upstream entries — see\n", meta.kept, meta.totalUpstream)
	fmt.Fprintf(&b, "// tools/pricing-sync/main.go's own doc comment for the exact pruning\n")
	fmt.Fprintf(&b, "// rule and bare-id collision resolution).\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Regenerate with: make pricing-sync\n")
	b.WriteString("var builtinModelMetaTable = map[string]builtinModelMeta{\n")
	for _, k := range keys {
		e := table[k]
		fmt.Fprintf(&b, "\t%q: {ContextTokens: %d, InputCostPerMTokMicroUSD: %d, OutputCostPerMTokMicroUSD: %d},\n",
			k, e.ContextTokens, e.InputCostPerMTokMicroUSD, e.OutputCostPerMTokMicroUSD)
	}
	b.WriteString("}\n")

	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return "", fmt.Errorf("gofmt generated source: %w", err)
	}
	return string(formatted), nil
}
