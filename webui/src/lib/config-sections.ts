export interface ConfigSection {
  key: string
  /** 1-based line of the key in the full document. */
  startLine: number
  lineCount: number
  text: string
}

const TOP_KEY = /^([^\s#-][^:]*|"[^"]*"|'[^']*'):/

function unquote(raw: string): string {
  const q = raw[0]
  return (q === '"' || q === "'") && raw.endsWith(q) ? raw.slice(1, -1) : raw
}

/** Splits a block-style YAML document at its column-0 keys; joining the section texts with newlines gives the document back. */
export function splitConfigSections(yaml: string): ConfigSection[] {
  if (yaml === '') return []
  const out: ConfigSection[] = []
  const lines = yaml.split('\n')
  let current: { key: string; startLine: number; lines: string[] } | null = null
  const flush = (): void => {
    if (current) out.push({ key: current.key, startLine: current.startLine, lineCount: current.lines.length, text: current.lines.join('\n') })
  }
  lines.forEach((line, i) => {
    const m = TOP_KEY.exec(line)
    if (m) {
      flush()
      current = { key: unquote(m[1]), startLine: i + 1, lines: [line] }
    } else current?.lines.push(line)
  })
  flush()
  return out
}

/** Sections whose key contains the query, case-insensitively; a blank query keeps all. */
export function filterSections(sections: ConfigSection[], query: string): ConfigSection[] {
  const q = query.trim().toLowerCase()
  return q === '' ? sections : sections.filter((s) => s.key.toLowerCase().includes(q))
}

const SMALL_SECTION_LINES = 8

/** Keys of the sections that start collapsed: all but small ones, filter matches and the jump target. */
export function initialFold(sections: ConfigSection[], opts: { filter?: string; target?: string }): Set<string> {
  const matched = new Set(filterSections(sections, opts.filter ?? '').map((s) => s.key))
  const filtering = (opts.filter ?? '').trim() !== ''
  return new Set(
    sections.filter((s) => s.lineCount > SMALL_SECTION_LINES && s.key !== opts.target && !(filtering && matched.has(s.key))).map((s) => s.key),
  )
}
