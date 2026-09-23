// Config page's ChangeHelper (redesign-plan.md section 3.4): every form
// (LimitsForm, GrantForm, PricingForm, ModelMetaForm, UserForm) funnels
// through this module to build the actual paste-ready text — the ONE place
// that knows the production file's exact shapes (home-cluster namespaces/
// traefik/base/llmgateway/middleware.yaml, users-configmap.yaml), so every
// form's snippet output is provably identical in format, not five
// independent guesses. Group/provider/admin snippets are YAML (lib/
// yaml-emit.ts, nested at spec.plugin.llmgateway.<section>); a user's own
// entry lives in users.json instead, so USER-targeted output is a
// single-line JSON object (new user) or a bare JSON field fragment (adding
// limits/a personal grant to an EXISTING user's line) — never YAML.
//
// Validation lives here too, not in each form component, for the same
// "one place, not five copies" reason: NAME_PATTERN/isValidName (provider
// and group names), isValidNonNegative (every limit/price field), and
// validatePersonalGrant (auth.go's own "models without providers" rejection
// — auth.go ~1006, a user/group with a `models` glob but an EMPTY
// `providers` list can never actually reach any of those models, since
// authorizedProviders is computed first and gates model resolution).
import { emitConfigSnippet, emitYamlFlowMap, emitYamlKey, emitYamlScalar } from '@/lib/yaml-emit'
import type { YamlValue } from '@/lib/yaml-emit'
import type { LimitsConfig } from '@/types/api'

/** NAME_PATTERN is the operator-facing name rule every provider/group id must satisfy — letters, digits, dot, underscore, hyphen, matching every real name in middleware.yaml (anthropic, gx10-embed, macstudio-rerank, friends, ...) and mirroring the plugin's own path.Match glob-safe charset. */
export const NAME_PATTERN = /^[a-zA-Z0-9._-]+$/

/** isValidName reports whether a provider/group name satisfies NAME_PATTERN — GrantForm/LimitsForm's own `targetKind === 'group'` path, and PricingForm/ModelMetaForm's provider-prefixed model ids indirectly (via NAME_PATTERN-shaped provider segments). NOT for a user name — see isValidUserName below. */
export function isValidName(name: string): boolean {
  return NAME_PATTERN.test(name)
}

/**
 * isValidUserName mirrors auth.go's own buildEntry check (~955-975,
 * ground truth): a user name only needs to be non-empty. Go deliberately
 * does NOT apply configNamePattern's provider/mcpServers/agents character
 * restriction (providers.go ~497-502, `^[a-zA-Z0-9._-]+$`) to user names
 * — that pattern exists ONLY because those three kinds get embedded
 * verbatim as a URL path segment (passthroughRoute, targetRoute); a user
 * name is never used as a route path segment anywhere, only as a
 * counter-key component and a JSON field (auth.go's own doc comment,
 * word for word: "Restricting the character set beyond 'non-empty' would
 * risk rejecting a real, currently-working operator-chosen name"). P3
 * item 27: GrantForm.vue/LimitsForm.vue/UserForm.vue used to apply
 * isValidName (NAME_PATTERN) to a user name too, so a real user such as
 * "jan@x" could never get a snippet. userJsonLine/userGrantJsonFragment/
 * userLimitsJsonFragment all go through JSON.stringify for every string
 * field, so relaxing this check to match Go carries no injection risk.
 */
export function isValidUserName(name: string): boolean {
  return name.trim() !== ''
}

/** isValidNonNegative reports whether a limit/price field is a finite, non-negative number — every LimitsConfig/pricing field's own domain (a negative or NaN limit/price is meaningless). Fractions ARE valid here: costPerDayUSD/costPerMonthUSD (LimitsConfig) and inputPerM/outputPerM (ModelPricing) are all Go float64 fields. */
export function isValidNonNegative(n: number): boolean {
  return Number.isFinite(n) && n >= 0
}

/**
 * isValidNonNegativeInt reports whether a count/limit field is a finite,
 * non-negative WHOLE number — the domain LimitsForm.vue's
 * requestsPerMinute/requestsPerDay/tokensPerDay/tokensPerMonth
 * (LimitsConfig, all Go `int64`) and ModelMetaForm.vue's contextTokens
 * (ModelMetaConfig.ContextTokens, Go `int`) actually decode into (P2 item
 * 12): `json.Unmarshal` rejects a fractional value like `1.5` against an
 * int64/int target outright, so a snippet built from an unchecked
 * fraction would fail to apply, not just round unexpectedly.
 */
export function isValidNonNegativeInt(n: number): boolean {
  return Number.isInteger(n) && n >= 0
}

/**
 * validatePersonalGrant mirrors auth.go's own buildEntry rejection
 * (~line 1005-1007, ground truth: redesign-plan.md section 0): a USER's
 * personal `models` glob list with an EMPTY `providers` list is rejected
 * outright — `llmgateway: user %q: personal "models" requires personal
 * "providers"`. This is a USER-ONLY rule (GrantForm.vue must call this
 * only when targetKind === 'user'): a GROUP's own `providers: []` means
 * "every configured provider" (matchesGlob's own empty-means-all
 * contract, auth.go ~487-490) and is applied directly with no such check
 * at group-construction time (auth.go ~763) — a group with `providers: []`
 * and a `models` list is valid config today. For a user's PERSONAL grant
 * specifically, that same empty-means-all reading is the problem, not the
 * fix: it would silently widen the grant to reach every configured
 * provider, including ones no member group of that user covers at all
 * (auth.go's own comment, ~995-1000) — so Go rejects it rather than
 * silently granting more than the operator meant to type. Returns an
 * error string when invalid, null when the grant is acceptable.
 */
export function validatePersonalGrant(providers: string[], models: string[]): string | null {
  if (models.length > 0 && providers.length === 0) {
    return 'A personal grant naming models needs at least one provider too — an empty providers list means EVERY configured provider (not none), so this would silently widen access far beyond what was typed (auth.go rejects it).'
  }
  return null
}

/** LIMITS_FIELD_ORDER is LimitsConfig's own field order (types/api.ts) — the single source of truth every limits snippet (YAML block or JSON fragment) below renders in, so the two never drift relative to each other. */
const LIMITS_FIELD_ORDER: (keyof LimitsConfig)[] = [
  'requestsPerMinute',
  'requestsPerDay',
  'tokensPerDay',
  'tokensPerMonth',
  'costPerDayUSD',
  'costPerMonthUSD',
]

/** limitsEntries reads only the CONFIGURED (truthy) fields off a LimitsConfig, in canonical order — mirrors lib/format.ts's formatLimits/lib/usage-bars.ts's budgetRatios own "0/omitted means unlimited" convention: an unset limit is never emitted as `0`. */
function limitsEntries(limits: LimitsConfig): [string, number][] {
  const out: [string, number][] = []
  for (const key of LIMITS_FIELD_ORDER) {
    const value = limits[key]
    if (value) out.push([key, value])
  }
  return out
}

/** groupLimitsSnippet builds a group's YAML limits block, nested at spec.plugin.llmgateway.groups.<name>.limits — LimitsForm's group-target output. */
export function groupLimitsSnippet(groupName: string, limits: LimitsConfig): string {
  const body: { [key: string]: YamlValue | undefined } = {}
  for (const [key, value] of limitsEntries(limits)) body[key] = value
  return emitConfigSnippet(['groups', groupName], { limits: body })
}

function jsonString(s: string): string {
  return JSON.stringify(s)
}

function jsonStringArray(values: string[]): string {
  return `[${values.map(jsonString).join(', ')}]`
}

/** jsonLimitsObject renders a LimitsConfig as a JSON object literal (no surrounding key) — "{\"requestsPerDay\": 1000}" — the SAME field order/omit-when-unset convention limitsEntries applies for the YAML path, just JSON syntax instead. */
function jsonLimitsObject(limits: LimitsConfig): string {
  const parts = limitsEntries(limits).map(([key, value]) => `${jsonString(key)}: ${value}`)
  return `{${parts.join(', ')}}`
}

/** userLimitsJsonFragment builds the bare `"limits": {...}` field to paste into an EXISTING user's users.json line — LimitsForm's user-target output (a user's own limits live inside their JSON object, not a separate YAML block). */
export function userLimitsJsonFragment(limits: LimitsConfig): string {
  return `"limits": ${jsonLimitsObject(limits)}`
}

/** GroupGrant is the four access lists GroupConfig carries (llmgateway.go) — GrantForm's group-target input. Every field is always emitted, even empty (middleware.yaml's own "every field explicit and empty, not a bare {}" convention, groups.home). */
export interface GroupGrant {
  providers: string[]
  models: string[]
  mcpServers: string[]
  agents: string[]
}

/** groupGrantSnippet builds a group's YAML access-list block, nested at spec.plugin.llmgateway.groups.<name> — GrantForm's group-target output. */
export function groupGrantSnippet(groupName: string, grant: GroupGrant): string {
  return emitConfigSnippet(['groups', groupName], {
    providers: grant.providers,
    models: grant.models,
    mcpServers: grant.mcpServers,
    agents: grant.agents,
  })
}

/** userGrantJsonFragment builds the bare `"providers": [...], "models": [...]` fields to paste into an EXISTING user's users.json line — GrantForm's user-target (personal grant) output. Only non-empty lists are emitted (an omitted personal-grant field means "nothing extra beyond the user's group", the same "0/omitted means unset" convention every other optional field in this file follows) — callers MUST reject the grant first via validatePersonalGrant. */
export function userGrantJsonFragment(grant: { providers: string[]; models: string[] }): string {
  const parts: string[] = []
  if (grant.providers.length > 0) parts.push(`"providers": ${jsonStringArray(grant.providers)}`)
  if (grant.models.length > 0) parts.push(`"models": ${jsonStringArray(grant.models)}`)
  return parts.join(', ')
}

/**
 * ModelMeta is a model's modelMeta entry — every field independently
 * optional (ModelMetaConfig, llmgateway.go: `contextTokens`,
 * `inputCostPerMTokMicroUsd`, `outputCostPerMTokMicroUsd`, `free`), mirroring
 * AdminModelMetaView/AdminCatalogModel's own "undefined means unset"
 * convention. ModelMetaForm sets only contextTokens/free — the two fields
 * this webui exposes an editor for (its own doc comment). PricingForm
 * does NOT go through this type or modelMetaSnippet below: per-token
 * price overrides that drive BILLING live in the top-level Config.Pricing
 * map (`spec.plugin.llmgateway.pricing`, ModelPricing{InputPerM,
 * OutputPerM} — see pricingSnippet), a config section entirely distinct
 * from modelMeta's own cost fields, which only drive metadata EXPOSURE
 * (GET /v1/models, Config's own ModelMeta doc comment: "distinct from
 * Pricing above, which drives request cost ACCOUNTING").
 */
export interface ModelMeta {
  contextTokens?: number
  free?: boolean
}

/**
 * modelMetaSnippet builds a model's modelMeta YAML entry as a single-line
 * flow map, matching middleware.yaml's own convention EXACTLY (golden
 * strings, e.g. `"gx10-embed/text-embedding-qwen3-0.6b": { contextTokens:
 * 16384, free: true }`): field order contextTokens, free — only the
 * fields the caller actually set, in that fixed order, every other field
 * entirely absent from the flow map (never a fabricated 0/false). modelId
 * is ALWAYS quoted (JSON.stringify, not lib/yaml-emit.ts's conditional
 * emitYamlKey — production quotes every modelMeta id, P2 item 11), and
 * the flow map itself via emitYamlFlowMap — placed at modelMeta's own
 * fixed 4-level indent by hand rather than through the generic recursive
 * block renderer, since a flow map is a single line, not a further nested
 * block.
 */
export function modelMetaSnippet(modelId: string, meta: ModelMeta): string {
  const entries: [string, boolean | number][] = []
  if (meta.contextTokens !== undefined) entries.push(['contextTokens', meta.contextTokens])
  if (meta.free !== undefined) entries.push(['free', meta.free])
  const flow = emitYamlFlowMap(entries)
  // Always quoted (JSON.stringify, not the conditional emitYamlKey) — the
  // production file quotes every modelMeta id regardless of whether
  // emitYamlKey's own general rule would require it (P2 item 11),
  // matching pricingSnippet's identical convention below.
  return ['spec:', '  plugin:', '    llmgateway:', '      modelMeta:', `        ${JSON.stringify(modelId)}: ${flow}`].join('\n')
}

/** ModelPricingOverride mirrors ModelPricing exactly (llmgateway.go: `InputPerM float64 json:"inputPerM"`, `OutputPerM float64 json:"outputPerM"`) — the billing-authoritative per-token price override, keyed by canonical model id under the top-level Config.Pricing map. Both fields are required together (PricingForm.vue's own validation): a one-sided override has no billing meaning. */
export interface ModelPricingOverride {
  inputPerM: number
  outputPerM: number
}

/**
 * pricingSnippet builds one model's entry in the top-level `pricing` map
 * (`spec.plugin.llmgateway.pricing.<id>`, Config.Pricing/ModelPricing,
 * llmgateway.go) as a single-line flow map — matching examples/
 * kubernetes.yaml's own real shape exactly: `gpt-5-mini: {inputPerM: 0.25,
 * outputPerM: 2.0}`. Hand-built at `pricing`'s own fixed depth, the same
 * convention modelMetaSnippet above follows for its own sibling top-level
 * map — a flow map is a single line, not a further nested block, so it is
 * not routed through emitConfigSnippet's generic recursive block
 * renderer.
 */
export function pricingSnippet(modelId: string, pricing: ModelPricingOverride): string {
  const flow = emitYamlFlowMap([
    ['inputPerM', pricing.inputPerM],
    ['outputPerM', pricing.outputPerM],
  ])
  // Conditional (yaml-emit.ts's own emitYamlKey), NOT always-quoted —
  // unlike modelMetaSnippet's own convention above: examples/
  // kubernetes.yaml's real "pricing:" entry (line 146, read this session)
  // is `gpt-5-mini: {inputPerM: 0.25, outputPerM: 2.0}`, a bare id with
  // no quotes, so pricing does NOT share modelMeta's "always quote"
  // production convention — it follows the general rule instead.
  return ['spec:', '  plugin:', '    llmgateway:', '      pricing:', `        ${emitYamlKey(modelId)}: ${flow}`].join('\n')
}

/** API_KEY_PLACEHOLDER is UserForm's default apiKey field value before the operator clicks "Generate" — visibly a placeholder, never mistaken for a real key (Q10, redesign-plan.md DECISIONS: "browser key generation offered, placeholder by default"). */
export const API_KEY_PLACEHOLDER = 'sk-llmgw-REPLACE_ME'

/** generateApiKey produces a real random bearer token client-side: 32 bytes from the Web Crypto CSPRNG, base64url-encoded — the same byte length and character set as the production tokens in users-configmap.yaml (openssl rand -hex 24 there is 24 bytes/48 hex chars; 32 raw bytes here is a comparable-or-stronger entropy budget, base64url rather than hex purely for a shorter pasted string). Never sent anywhere — ChangeHelper only ever displays it for the operator to copy into their own users.json edit. */
export function generateApiKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32))
  let binary = ''
  for (const b of bytes) binary += String.fromCharCode(b)
  const base64 = btoa(binary)
  return `sk-llmgw-${base64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')}`
}

/** UserSnippetInput is UserForm's full input shape — mirrors AdminConsumerUser (types/api.ts) plus the two config-only fields (groups plural, limits) that view never carries but users.json itself supports. */
export interface UserSnippetInput {
  name: string
  apiKey: string
  /** Single primary group — production's own convention (users-configmap.yaml: every existing user sets exactly this field). Mutually exclusive with `groups` in practice (userJsonLine emits whichever is set; a caller offering both is a UserForm UI bug, not something this function arbitrates). */
  group?: string
  /** Multi-group membership (newer plugin support, mirrors AdminUsageEntryView.groups' own multi-group doc comment) — used instead of `group` when the operator selects more than one. */
  groups?: string[]
  providers?: string[]
  models?: string[]
  limits?: LimitsConfig
  admin?: boolean
}

/**
 * userJsonLine builds one users.json array entry as a single-line JSON
 * object — GOLDEN-STRING matched against users-configmap.yaml's own real
 * entries, e.g. `{"name": "barteq", "apiKey": "sk-friend-...", "group":
 * "friends", "providers": ["gx10"]}`: key order name, apiKey, group,
 * groups, providers, models, limits, admin (redesign-plan.md section 3.4),
 * `": "` after every key and `", "` between fields (NOT JSON.stringify's
 * own no-space-after-colon default — production's own file has the space),
 * only present/truthy fields emitted, `admin` only ever appears as the
 * literal `true` (an admin:false entry is simply omitted, matching every
 * non-admin user in the production file).
 */
export function userJsonLine(user: UserSnippetInput): string {
  const parts: string[] = [`"name": ${jsonString(user.name)}`, `"apiKey": ${jsonString(user.apiKey)}`]
  if (user.group) parts.push(`"group": ${jsonString(user.group)}`)
  if (user.groups && user.groups.length > 0) parts.push(`"groups": ${jsonStringArray(user.groups)}`)
  if (user.providers && user.providers.length > 0) parts.push(`"providers": ${jsonStringArray(user.providers)}`)
  if (user.models && user.models.length > 0) parts.push(`"models": ${jsonStringArray(user.models)}`)
  if (user.limits && limitsEntries(user.limits).length > 0) parts.push(`"limits": ${jsonLimitsObject(user.limits)}`)
  if (user.admin) parts.push(`"admin": ${emitYamlScalar(true)}`)
  return `{${parts.join(', ')}}`
}
