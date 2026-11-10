export interface ProviderSection {
  /** Sub-section heading; '' for the untitled plain-model section. */
  title: string
  models: string[]
  endpoints: string[]
}

export interface ProviderSectionInput {
  models: string[]
  kinds?: Record<string, string>
  endpoints?: string[]
}

/** Heading for an endpoint with no configured kind. */
export const ENDPOINT_KIND = 'endpoints'

/**
 * providerSections splits one provider's rows into sub-sections by kind:
 * the plain (no-kind) models first, then one section per kind, sorted.
 * A provider with no kinds and no endpoints yields one untitled section,
 * so an unconfigured provider renders exactly as the flat list did.
 */
export function providerSections(p: ProviderSectionInput): ProviderSection[] {
  const kinds = p.kinds ?? {}
  const endpoints = p.endpoints ?? []
  if (endpoints.length === 0 && Object.keys(kinds).length === 0) return [{ title: '', models: p.models, endpoints: [] }]

  const byTitle = new Map<string, ProviderSection>()
  const section = (title: string): ProviderSection => {
    let s = byTitle.get(title)
    if (!s) {
      s = { title, models: [], endpoints: [] }
      byTitle.set(title, s)
    }
    return s
  }
  for (const m of p.models) section(kinds[m] ?? '').models.push(m)
  for (const e of endpoints) section(kinds[e] ?? ENDPOINT_KIND).endpoints.push(e)
  return [...byTitle.values()].sort((a, b) => (a.title === '' ? -1 : b.title === '' ? 1 : a.title.localeCompare(b.title)))
}
