import { describe, expect, it } from 'vitest'

import { deriveAttention } from './attention'
import type { AttentionEvents, AttentionOverview, AttentionRouter, AttentionUsage } from './attention'
import { formatSel } from './panel-selection'

const provider = (over: Partial<AttentionOverview['providers'][number]> = {}) => ({ name: 'uni', healthState: 'closed' as const, ...over })
const overview = (over: Partial<AttentionOverview> = {}): AttentionOverview => ({
  providers: [provider()],
  pricing: { unpriced: 0 },
  redis: { configured: true },
  ...over,
})
const usage: AttentionUsage = { users: [{ id: 'alice' }, { id: 'bob' }] }
const events = (over: Partial<AttentionEvents> = {}): AttentionEvents => ({ source: 'redis', events: [], ...over })
const router = (over: Partial<AttentionRouter> = {}): AttentionRouter => ({ name: 'auto', stats: { escalations: [] }, ...over })

describe('deriveAttention', () => {
  it('returns nothing when nothing is wrong', () => {
    expect(deriveAttention(overview(), usage, events(), [router()])).toEqual([])
  })

  it('returns nothing before data loads', () => {
    expect(deriveAttention(null, null, null, [])).toEqual([])
  })

  it('flags an open breaker as danger linking to the provider', () => {
    const items = deriveAttention(overview({ providers: [provider({ healthState: 'open' })] }), usage, events(), [])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ severity: 'danger', link: { page: 'models', sel: formatSel('provider', 'uni') } })
    expect(items[0]?.text).toContain('uni')
  })

  it('flags a provider lastErr as danger with the provider sel and the reason', () => {
    const items = deriveAttention(overview({ providers: [provider({ lastErr: 'HTTP 401 unauthorized' })] }), usage, events(), [])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ severity: 'danger', link: { page: 'models', sel: formatSel('provider', 'uni') } })
    expect(items[0]?.text).toContain('HTTP 401 unauthorized')
  })

  it('merges breaker and lastErr of one provider into a single item', () => {
    const items = deriveAttention(overview({ providers: [provider({ healthState: 'open', lastErr: 'boom' })] }), usage, events(), [])
    expect(items).toHaveLength(1)
    expect(items[0]?.text).toContain('boom')
  })

  it('groups rate_limit events per user as a warning with the user sel', () => {
    const ev = events({
      events: [
        { kind: 'rate_limit', user: 'alice' },
        { kind: 'rate_limit', user: 'alice' },
        { kind: 'rate_limit', user: 'bob' },
        { kind: 'upstream', user: 'bob' },
      ],
    })
    const items = deriveAttention(overview(), usage, ev, [])
    expect(items.map((i) => i.link.sel)).toEqual([formatSel('user', 'alice'), formatSel('user', 'bob')])
    expect(items[0]).toMatchObject({ severity: 'warning', link: { page: 'users' } })
    expect(items[0]?.text).toContain('2')
  })

  it('links a throttled user that no longer exists to the directory without a sel', () => {
    const items = deriveAttention(overview(), usage, events({ events: [{ kind: 'rate_limit', user: 'ghost' }] }), [])
    expect(items[0]?.link).toEqual({ page: 'users' })
  })

  it('flags unpriced models as info linking to usage', () => {
    const items = deriveAttention(overview({ pricing: { unpriced: 3 } }), usage, events(), [])
    expect(items).toEqual([{ severity: 'info', text: expect.stringContaining('3'), link: { page: 'usage' } }])
  })

  it('flags a configured redis that is not the event source as danger', () => {
    const items = deriveAttention(overview(), usage, events({ source: 'replica' }), [])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ severity: 'danger', link: { page: 'config' } })
    expect(items[0]?.text).toMatch(/redis/i)
  })

  it('does not flag replica events when redis is not configured', () => {
    expect(deriveAttention(overview({ redis: { configured: false } }), usage, events({ source: 'replica' }), [])).toEqual([])
  })

  it('flags router escalations as info linking to the router', () => {
    const r = router({ stats: { escalations: [{ count: 4 }, { count: 1 }] } })
    const items = deriveAttention(overview(), usage, events(), [r])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ severity: 'info', link: { page: 'routers', sel: formatSel('router', 'auto') } })
    expect(items[0]?.text).toContain('5')
  })

  it('orders danger before warning before info', () => {
    const items = deriveAttention(
      overview({ pricing: { unpriced: 1 }, providers: [provider({ healthState: 'open' })] }),
      usage,
      events({ events: [{ kind: 'rate_limit', user: 'alice' }] }),
      [],
    )
    expect(items.map((i) => i.severity)).toEqual(['danger', 'warning', 'info'])
  })
})
