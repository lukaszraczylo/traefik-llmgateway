import { describe, expect, it } from 'vitest'

import { formatLastSeen } from './last-seen'

const NOW = new Date(Date.UTC(2026, 3, 16, 12, 0, 0))
const NOW_SECONDS = Math.floor(NOW.getTime() / 1000)

describe('formatLastSeen', () => {
  it.each([
    [undefined, 'never'],
    [0, 'never'],
  ])('%s seconds -> "never" (types/api.ts: 0/omitted means never seen or the feature is off)', (seconds, expected) => {
    expect(formatLastSeen(seconds, NOW)).toBe(expected)
  })

  it('renders "just now" for a timestamp under a minute old', () => {
    expect(formatLastSeen(NOW_SECONDS - 30, NOW)).toBe('just now')
  })

  it('renders "just now" for a timestamp in the FUTURE (clock skew), clamping delta to 0 rather than a negative duration', () => {
    expect(formatLastSeen(NOW_SECONDS + 500, NOW)).toBe('just now')
  })

  it('renders minutes under an hour', () => {
    expect(formatLastSeen(NOW_SECONDS - 5 * 60, NOW)).toBe('5m ago')
  })

  it('renders hours under a day', () => {
    expect(formatLastSeen(NOW_SECONDS - 3 * 3600, NOW)).toBe('3h ago')
  })

  it('renders days under the month threshold', () => {
    expect(formatLastSeen(NOW_SECONDS - 10 * 86400, NOW)).toBe('10d ago')
  })

  it('renders months at/beyond the month threshold', () => {
    expect(formatLastSeen(NOW_SECONDS - 90 * 86400, NOW)).toBe('3mo ago')
  })

  it('boundary: exactly 59 seconds is still "just now"', () => {
    expect(formatLastSeen(NOW_SECONDS - 59, NOW)).toBe('just now')
  })

  it('boundary: exactly 60 seconds is "1m ago"', () => {
    expect(formatLastSeen(NOW_SECONDS - 60, NOW)).toBe('1m ago')
  })

  it('boundary: exactly 23h59m is still hours, not a day', () => {
    expect(formatLastSeen(NOW_SECONDS - (86400 - 60), NOW)).toBe('23h ago')
  })

  it('defaults `now` to the real current time when omitted', () => {
    const nowSeconds = Math.floor(Date.now() / 1000)
    expect(formatLastSeen(nowSeconds)).toBe('just now')
  })
})
