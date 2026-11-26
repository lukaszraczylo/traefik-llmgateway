/** The ChangeHelper forms, in picker order; the single source for the Config page's `sel=helper:<id>` hash and the form switch. */
export const HELPER_IDS = ['limits', 'grant', 'pricing', 'model', 'router', 'user'] as const

export type HelperId = (typeof HELPER_IDS)[number]

export const HELPER_LABEL: Record<HelperId, string> = {
  limits: 'Limits',
  grant: 'Access grant',
  pricing: 'Pricing',
  model: 'Model metadata',
  router: 'Smart router',
  user: 'New user',
}

const PREFIX = 'helper'

export function formatHelperSel(id: HelperId): string {
  return `${PREFIX}:${id}`
}

/** A bare `helper` opens the first form; an unknown id or foreign kind is null. */
export function parseHelperSel(v: string | undefined): HelperId | null {
  if (v === PREFIX) return HELPER_IDS[0]
  if (!v?.startsWith(`${PREFIX}:`)) return null
  const id = v.slice(PREFIX.length + 1)
  return (HELPER_IDS as readonly string[]).includes(id) ? (id as HelperId) : null
}

/** True when `sel` is set but opens no helper form; the Config page has no other sheet, so it clears such a value. */
export function isStaleHelperSel(v: string | undefined): boolean {
  return v !== undefined && parseHelperSel(v) === null
}
