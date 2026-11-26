export type MockWindow = 'hour' | 'day' | 'month'

export const CAPTURE_AT = Date.parse('2026-11-25T01:01:07Z')

/** Hours per bucket; a month bucket is treated as 30 days when rescaling. */
export const WINDOW_HOURS: Record<MockWindow, number> = { hour: 1, day: 24, month: 720 }

const HOUR_MS = 3_600_000
const DAY_MS = 24 * HOUR_MS
const ISO_RE = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z$/
const BUCKET_LEN: Record<number, MockWindow> = { 10: 'hour', 8: 'day', 6: 'month' }
const MIN_REAL_TIME = Date.parse('2000-01-01T00:00:00Z')

const pad = (n: number, w: number) => String(n).padStart(w, '0')

function unitIndex(window: MockWindow, ms: number): number {
  if (window === 'hour') return Math.floor(ms / HOUR_MS)
  if (window === 'day') return Math.floor(ms / DAY_MS)
  const d = new Date(ms)
  return d.getUTCFullYear() * 12 + d.getUTCMonth()
}

function unitStart(window: MockWindow, index: number): number {
  if (window === 'hour') return index * HOUR_MS
  if (window === 'day') return index * DAY_MS
  return Date.UTC(Math.floor(index / 12), index % 12, 1)
}

export function formatBucket(window: MockWindow, ms: number): string {
  const d = new Date(ms)
  const date = `${pad(d.getUTCFullYear(), 4)}${pad(d.getUTCMonth() + 1, 2)}`
  if (window === 'month') return date
  const day = `${date}${pad(d.getUTCDate(), 2)}`
  return window === 'day' ? day : `${day}${pad(d.getUTCHours(), 2)}`
}

function parseBucket(s: string): { window: MockWindow; ms: number } | null {
  const window = BUCKET_LEN[s.length]
  if (!window || !/^\d+$/.test(s)) return null
  const y = Number(s.slice(0, 4))
  const mo = Number(s.slice(4, 6)) - 1
  const d = window === 'month' ? 1 : Number(s.slice(6, 8))
  const h = window === 'hour' ? Number(s.slice(8, 10)) : 0
  return { window, ms: Date.UTC(y, mo, d, h) }
}

/** Bucket labels oldest first; the newest is the bucket holding `now` moved back by `offset` buckets. */
export function bucketsFor(window: MockWindow, span: number, offset: number, now: number): string[] {
  const newest = unitIndex(window, now) - offset
  const out: string[] = []
  for (let i = span - 1; i >= 0; i--) out.push(formatBucket(window, unitStart(window, newest - i)))
  return out
}

function shiftBucket(s: string, captureAt: number, now: number): string {
  const b = parseBucket(s)
  if (!b) return s
  const diff = unitIndex(b.window, now) - unitIndex(b.window, captureAt)
  return formatBucket(b.window, unitStart(b.window, unitIndex(b.window, b.ms) + diff))
}

/** Moves every time-bearing value in a captured body forward by `now - captureAt`; the newest bucket lands on now's bucket. */
export function shiftTimestamps(body: unknown, captureAt: number, now: number): unknown {
  const delta = now - captureAt
  const walk = (v: unknown, key: string): unknown => {
    if (typeof v === 'string') {
      if (ISO_RE.test(v)) {
        const t = Date.parse(v)
        return t < MIN_REAL_TIME ? v : new Date(t + delta).toISOString()
      }
      if (key === '__bucket') return shiftBucket(v, captureAt, now)
      return v
    }
    if (typeof v === 'number') return key === 'lastSeen' ? v + Math.round(delta / 1000) : v
    if (Array.isArray(v)) return v.map((x) => walk(x, key === 'buckets' ? '__bucket' : key))
    if (v && typeof v === 'object') {
      const out: Record<string, unknown> = {}
      for (const [k, x] of Object.entries(v)) {
        out[k] = k === 'id' && key === 'points' && typeof x === 'string' ? shiftBucket(x, captureAt, now) : walk(x, k)
      }
      return out
    }
    return v
  }
  return walk(body, '')
}
