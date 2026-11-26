import type { PageId } from '@/lib/pages'

/** Entity kinds a detail panel can open, carried in the hash as `sel=<kind>:<id>`. */
export const PANEL_KINDS = ['user', 'group', 'model', 'provider', 'router', 'target'] as const

export type PanelKind = (typeof PANEL_KINDS)[number]

export interface PanelSelection {
  kind: PanelKind
  id: string
}

function isPanelKind(value: string): value is PanelKind {
  return (PANEL_KINDS as readonly string[]).includes(value)
}

/** Parses a `sel` value; null for absent, unknown kind, empty id or a malformed escape. Splits on the first colon only. */
export function parseSel(v: string | undefined): PanelSelection | null {
  if (!v) return null
  const colon = v.indexOf(':')
  if (colon === -1) return null
  const kind = v.slice(0, colon)
  const rawId = v.slice(colon + 1)
  if (!isPanelKind(kind) || rawId === '') return null
  try {
    return { kind, id: decodeURIComponent(rawId) }
  } catch {
    return null
  }
}

/** The id is percent-encoded so slashes, colons and `&` survive; the hash layer's own query encoding is applied on top and undone symmetrically. */
export function formatSel(kind: PanelKind, id: string): string {
  return `${kind}:${encodeURIComponent(id)}`
}

/** Returns the selection when its entity exists, else null (the caller then clears `sel` with a replace). */
export function resolveSel(
  sel: PanelSelection | null,
  existsFn: (kind: PanelKind, id: string) => boolean,
): PanelSelection | null {
  if (!sel) return null
  return existsFn(sel.kind, sel.id) ? sel : null
}

/** The page whose directory lists each kind; opening a panel from elsewhere navigates here first. */
export const PANEL_PAGE: Record<PanelKind, PageId> = {
  user: 'users',
  group: 'users',
  model: 'models',
  provider: 'models',
  router: 'routers',
  target: 'targets',
}
