// Config page (redesign-plan.md section 3.4): a deterministic, dependency-
// free YAML emitter. Two jobs share it:
//   1. ConfigTree.vue renders the FULL redacted config (GET /admin/api/
//      config's arbitrary Record<string, unknown>) as read-only YAML —
//      emitYamlDocument, via toYamlValue's defensive unknown->YamlValue cast.
//   2. lib/snippets.ts builds small, targeted "paste this into middleware.
//      yaml" fragments (emitConfigSnippet) for the ChangeHelper forms —
//      GOLDEN-STRING tested against the real production file (home-cluster
//      namespaces/traefik/base/llmgateway/middleware.yaml), never guessed.
//
// Kept intentionally narrow: this schema's own config never nests an array
// of objects (every ProviderConfig/GroupConfig/LimitsConfig/RedisConfig/
// RetryConfig/CacheConfig/AdminConfig field that is a list is a list of
// strings — providers/models/mcpServers/agents/passthroughPaths), so this
// module does not attempt general-purpose YAML (anchors, multiline scalars,
// comments) — only what this one config shape, and its own redacted mirror,
// ever produce.

/** YamlPrimitive is one leaf value — a plain scalar, matching every non-container field this config's Go structs carry (strings, numbers, bools, or Go's untyped nil for an omitted pointer). */
export type YamlPrimitive = string | number | boolean | null

/** YamlValue is any value this emitter can render: a primitive, a list of primitives, or a nested map. `undefined` on a map's own value is the one non-YAML sentinel this module recognizes — it drops that key entirely (mirrors Go's `omitempty`), never emitted as `null`. */
export type YamlValue = YamlPrimitive | YamlValue[] | { [key: string]: YamlValue | undefined }

const INDENT_UNIT = '  '

/**
 * SAFE_UNQUOTED_PATTERN is the whitelist every plain (unquoted) YAML
 * scalar this module emits — key OR value, block OR flow context — must
 * satisfy: a leading letter or underscore, then any run of letters,
 * digits, `.`, `_`, or `-`. This is deliberately far narrower than what
 * YAML 1.2 actually permits unquoted (this module's own top-of-file doc
 * comment: "only what this one config shape ... ever produces"), so every
 * structurally significant character this schema's real values could
 * ever contain — a space, `:`, `,`, `[`, `]`, `{`, `}`, `#`, a quote, a
 * leading digit, or a leading special character — falls OUTSIDE it and
 * gets quoted, unconditionally, in BOTH the two places this module
 * renders a scalar: a block "key: value" line (renderMapLines) and a
 * flow list/map element (emitYamlFlowList/emitYamlFlowMap) — the same
 * needsQuoting call backs both emitYamlKey and emitYamlScalar below, so
 * neither can drift from the other's rule (P2 item 11's own fix
 * instruction: "apply the same rule to keys").
 *
 * `/` is deliberately NOT in the allowed charset either, even though it
 * is not YAML-structurally significant on its own: this codebase's own
 * pre-existing golden tests (yamlKeyNeedsQuoting('gx10/current') === true,
 * checked against the real production file) already establish that a
 * routable "provider/model" id is quoted by this GENERAL rule, not by a
 * one-off special case — modelMetaSnippet's own "always quoted" note
 * (P2 item 11: "Always quote modelMeta keys") is then true by
 * CONSTRUCTION for every real model id (they all contain "/"), not a
 * separate mechanism fighting this whitelist.
 *
 * P2 item 11 (PyYAML-checked): a flow-list glob like `gpt-[45]*`
 * (path.Match globs are legitimate config, auth.go:492) previously broke
 * parsing because `[`/`]` were only checked as the FIRST character, not
 * anywhere in the string — the old scalar rule quoted a LEADING `[` but
 * not one in the middle. A comma or `}`/`{` anywhere has the identical
 * problem inside this module's own flow-style output. This whitelist
 * quotes the whole string whenever ANY of those characters appear, at
 * any position — always correct, since a flow-context character is
 * unsafe everywhere in a flow scalar, and quoting it in a block context
 * too costs nothing (`"literal"` is valid wherever `literal` was).
 */
const SAFE_UNQUOTED_PATTERN = /^[A-Za-z_][A-Za-z0-9._-]*$/

/**
 * RESERVED_WORDS is every bare word SAFE_UNQUOTED_PATTERN would otherwise
 * let through unquoted (it is made entirely of letters, so the pattern's
 * charset alone cannot exclude it) but that a YAML parser resolves to a
 * non-string type instead of the literal string it looks like. This
 * config is consumed as a Kubernetes CRD spec field (YAML -> JSON via the
 * API machinery's own converter, which follows YAML 1.1-style scalar
 * resolution — the same lineage go.yaml.in/yaml/v3's own decode defaults
 * sit in), so the broader YAML 1.1 boolean/null set is included
 * defensively (on/off, yes/no, not just YAML 1.2's own true/false/null),
 * matched case-insensitively since YAML resolution itself is
 * case-insensitive for these ("True", "ON", "Null" all resolve the same
 * way a parser's plain-scalar resolver would).
 */
const RESERVED_WORDS = new Set(['true', 'false', 'yes', 'no', 'y', 'n', 'on', 'off', 'null', 'nil', 'nan', 'inf'])

/**
 * needsQuoting is the ONE predicate this module uses to decide whether a
 * plain scalar is safe to emit unquoted — SAFE_UNQUOTED_PATTERN first (a
 * fast, purely structural check that also independently catches every
 * number-looking string this schema ever produces: `0x10`, `1e3`,
 * `2026-01-01`, `12:30`, `.inf`, and a bare `~` all contain a character
 * outside the whitelist, or start with a digit/`.`/`~` rather than a
 * letter/underscore, so none of them need their own separate numeric
 * check), then RESERVED_WORDS for the purely-alphabetic words the
 * pattern alone cannot rule out.
 */
function needsQuoting(s: string): boolean {
  if (!SAFE_UNQUOTED_PATTERN.test(s)) return true
  return RESERVED_WORDS.has(s.toLowerCase())
}

/** yamlKeyNeedsQuoting reports whether a mapping key needs JSON-style double-quoting to round-trip as the literal string it is — the same needsQuoting rule emitYamlScalar applies to values below (P2 item 11: keys and values share one rule, not two independently-drifting ones). A modelMeta/pricing "provider/model" key always trips this (contains "/"), matching the production file's own convention of quoting every routable id. */
export function yamlKeyNeedsQuoting(key: string): boolean {
  return needsQuoting(key)
}

/**
 * YAML_UNSAFE_CONTROL_PATTERN matches every character JSON.stringify's own
 * quoting leaves as a literal byte/code unit but that a YAML 1.1 scanner
 * (this config's consumer: Kubernetes' CRD machinery, go.yaml.in/yaml/v3 —
 * needsQuoting's own doc comment) does not accept literally inside a
 * double-quoted scalar (N6, verify-redesign-final.md — checked against
 * PyYAML and go-yaml v3 across 346 round-trip cases):
 *   - U+0085 (NEL) and the rest of the C1 control range U+0080-U+009F —
 *     NEL is itself a YAML 1.1 line-break character, folded into a space
 *     or rejected outright by a scanner mid-key; the surrounding C1 range
 *     is non-printable by the same spec.
 *   - U+2028 (LINE SEPARATOR) / U+2029 (PARAGRAPH SEPARATOR) — YAML's other
 *     two line-break characters.
 *   - U+007F (DEL) — non-printable, rejected outright by PyYAML.
 * JSON already escapes every C0 control (U+0000-U+001F) and the two
 * structural characters ('"', '\\'), so this only needs to add the range
 * JSON's own grammar has no reason to touch.
 */
const YAML_UNSAFE_CONTROL_PATTERN = new RegExp(
  '[\\u007f\\u0080-\\u009f' + String.fromCharCode(0x2028) + String.fromCharCode(0x2029) + ']',
  'g',
)

/**
 * escapeYamlUnsafeControls post-processes a JSON.stringify'd, already-
 * quoted string, replacing every YAML_UNSAFE_CONTROL_PATTERN character
 * with a YAML double-quoted escape a scanner accepts literally: `\xHH`
 * (YAML's 2-hex-digit, 8-bit escape) for a single-byte code point
 * (U+007F, U+0080-U+009F), `\uHHHH` (4-hex-digit, 16-bit escape) for
 * U+2028/U+2029. Safe to run over the whole quoted string, including its
 * surrounding quotes and any escape sequences JSON.stringify already
 * produced: none of those ever contain a raw byte in this pattern's
 * range (JSON's own escapes are all plain ASCII), so this only ever
 * touches a literal occurrence of one of the target characters.
 */
function escapeYamlUnsafeControls(quoted: string): string {
  return quoted.replace(YAML_UNSAFE_CONTROL_PATTERN, (ch) => {
    const code = ch.codePointAt(0) as number
    return code <= 0xff ? `\\x${code.toString(16).padStart(2, '0')}` : `\\u${code.toString(16).padStart(4, '0')}`
  })
}

/** emitYamlKey renders one mapping key — quoted (JSON-style, valid YAML double-quoting) only when yamlKeyNeedsQuoting says so, plain otherwise. */
export function emitYamlKey(key: string): string {
  return yamlKeyNeedsQuoting(key) ? escapeYamlUnsafeControls(JSON.stringify(key)) : key
}

/** emitYamlScalar renders one leaf value in "key: <here>" position OR a flow list/map element — quoted (JSON-style) whenever needsQuoting says the plain form is unsafe, plain otherwise. Booleans/numbers/null render via their own JS literal, never through the string quoting path (a real `true`/`false`/`null`/number is safe unquoted by construction — only a STRING value can accidentally spell one of those). */
export function emitYamlScalar(value: YamlPrimitive): string {
  if (value === null) return 'null'
  if (typeof value === 'boolean' || typeof value === 'number') return String(value)
  return needsQuoting(value) ? escapeYamlUnsafeControls(JSON.stringify(value)) : value
}

/** emitYamlFlowList renders a primitive list as one flow-style line — "[]" empty, "[a, b, c]" otherwise — matching the production file's own short-list convention (middleware.yaml: `providers: []`, `models: ["qwen3.8-max", "qwen3.8-flash"]`). */
export function emitYamlFlowList(values: YamlPrimitive[]): string {
  if (values.length === 0) return '[]'
  return `[${values.map((v) => emitYamlScalar(v)).join(', ')}]`
}

/** emitYamlFlowMap renders a primitive-valued map as one flow-style line — "{}" empty, "{ k: v, k2: v2 }" otherwise (note the padding spaces inside the braces) — matching modelMeta's own production convention (`{ free: true }`, `{ contextTokens: 16384, free: true }`). */
export function emitYamlFlowMap(entries: [string, YamlPrimitive][]): string {
  if (entries.length === 0) return '{}'
  return `{ ${entries.map(([k, v]) => `${emitYamlKey(k)}: ${emitYamlScalar(v)}`).join(', ')} }`
}

function isPlainObject(value: YamlValue): value is { [key: string]: YamlValue | undefined } {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isPrimitive(value: YamlValue): value is YamlPrimitive {
  return value === null || typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean'
}

/**
 * renderMapLines recursively renders a map's own entries as already-
 * indented lines, at INDENT_UNIT * depth spaces. A nested map with at
 * least one (non-undefined) key opens on its own "key:" line and recurses
 * one level deeper; an empty nested map renders inline as "key: {}"; a
 * primitive-only array renders inline as one flow list. A defensive
 * fallback (block "- " items, JSON.stringify per item) covers an array
 * containing anything else — this schema never produces one, but a
 * generic render of the server's redacted config must never throw on a
 * shape this module did not anticipate.
 */
function renderMapLines(map: { [key: string]: YamlValue | undefined }, depth: number): string[] {
  const indent = INDENT_UNIT.repeat(depth)
  const lines: string[] = []
  for (const [key, value] of Object.entries(map)) {
    if (value === undefined) continue
    const keyPrefix = `${indent}${emitYamlKey(key)}:`
    if (isPrimitive(value)) {
      lines.push(`${keyPrefix} ${emitYamlScalar(value)}`)
    } else if (Array.isArray(value)) {
      if (value.every(isPrimitive)) {
        lines.push(`${keyPrefix} ${emitYamlFlowList(value as YamlPrimitive[])}`)
      } else {
        lines.push(keyPrefix)
        for (const item of value) {
          lines.push(`${indent}${INDENT_UNIT}- ${isPrimitive(item) ? emitYamlScalar(item) : JSON.stringify(item)}`)
        }
      }
    } else if (isPlainObject(value)) {
      const nestedKeys = Object.keys(value).filter((k) => value[k] !== undefined)
      if (nestedKeys.length === 0) {
        lines.push(`${keyPrefix} {}`)
      } else {
        lines.push(keyPrefix)
        lines.push(...renderMapLines(value, depth + 1))
      }
    }
  }
  return lines
}

/** emitYamlDocument renders a top-level map as block-style YAML, starting at zero indent — ConfigTree.vue's own top-level call against the whole redacted config. */
export function emitYamlDocument(value: { [key: string]: YamlValue | undefined }, depth = 0): string {
  return renderMapLines(value, depth).join('\n')
}

/**
 * emitConfigSnippet wraps `body` under the fixed "spec.plugin.llmgateway.
 * <...sectionPath>" prefix every ChangeHelper YAML snippet shares
 * (redesign-plan.md section 3.4: "emits YAML nested at spec.plugin.
 * llmgateway.<section>") — e.g. sectionPath ["groups", "eng"] with body
 * {limits: {...}} renders the group's limits block at exactly the depth
 * the production file itself uses (spec:0 plugin:1 llmgateway:2 groups:3
 * eng:4 limits:5 <fields>:6 — 0/2/4/6/8/10/12 spaces).
 */
export function emitConfigSnippet(sectionPath: string[], body: { [key: string]: YamlValue | undefined }): string {
  let wrapped: { [key: string]: YamlValue | undefined } = body
  const fullPath = ['spec', 'plugin', 'llmgateway', ...sectionPath]
  for (let i = fullPath.length - 1; i >= 0; i--) {
    wrapped = { [fullPath[i]]: wrapped }
  }
  return emitYamlDocument(wrapped)
}

/**
 * toYamlValue defensively converts an arbitrary JSON value (GET /admin/api/
 * config's already-redacted `config: Record<string, unknown>`) into a
 * YamlValue this module can render — recursing through plain objects and
 * arrays, passing primitives through unchanged, and falling back to
 * `String(input)` for anything else (a function, a Symbol — never actually
 * present in a JSON-decoded response, but this must not throw even if the
 * server's shape someday surprises it).
 */
export function toYamlValue(input: unknown): YamlValue {
  if (input === null || typeof input === 'string' || typeof input === 'number' || typeof input === 'boolean') return input
  if (Array.isArray(input)) return input.map(toYamlValue)
  if (typeof input === 'object') {
    const out: { [key: string]: YamlValue } = {}
    for (const [k, v] of Object.entries(input as Record<string, unknown>)) out[k] = toYamlValue(v)
    return out
  }
  return String(input)
}
