import { describe, expect, it } from 'vitest'

import { loadState } from './load-state'
import { staleNotice } from './stale-notice'

const at = new Date('2026-11-08T10:11:12Z')

describe('staleNotice', () => {
  it.each([
    { name: 'no data, error', hasData: false, error: 'boom', want: null },
    { name: 'data, no error', hasData: true, error: '', want: null },
    { name: 'no data, no error', hasData: false, error: '', want: null },
  ])('is null: $name', ({ hasData, error, want }) => {
    expect(staleNotice(hasData, error, at)).toBe(want)
  })

  it('names the error and the data age when data is shown and refresh fails', () => {
    const msg = staleNotice(true, 'HTTP 503', at)
    expect(msg).toContain('HTTP 503')
    expect(msg).toContain(at.toLocaleTimeString())
  })

  it('omits the age when lastUpdated is unknown', () => {
    expect(staleNotice(true, 'x', null)).toBe('Refresh failing: x.')
  })

  // Lean gate Gateway.Ui.Stale.no_silent_stale: ready + error always has a notice.
  it.each([true, false])('never silent when ready with error (loading=%s)', (loading) => {
    const state = loadState({ loading, hasData: true, error: 'e' })
    expect(state).toBe('ready')
    expect(staleNotice(true, 'e', at)).not.toBeNull()
  })
})
