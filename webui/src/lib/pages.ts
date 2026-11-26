/** Page ids of the redesigned shell: the single source for nav, the store and the hash's page segment. */
export const PAGE_IDS = ['overview', 'usage', 'reliability', 'users', 'models', 'routers', 'targets', 'events', 'config'] as const

export type PageId = (typeof PAGE_IDS)[number]

/** The landing page when the hash names nothing recognized, or is absent. */
export const DEFAULT_PAGE: PageId = 'overview'

/** Runtime guard for untrusted input (a hash, a hand-typed URL). */
export function isPageId(value: string): value is PageId {
  return (PAGE_IDS as readonly string[]).includes(value)
}
