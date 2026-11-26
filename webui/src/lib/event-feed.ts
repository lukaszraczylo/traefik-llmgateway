import type { AdminEventView } from '@/types/api'

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const pad = (n: number): string => String(n).padStart(2, '0')

/** Clock time for today's events; date and time for any other day. Empty for an unparseable time. */
export function formatEventTime(iso: string, now: Date = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const clock = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  const today = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate()
  return today ? clock : `${MONTHS[d.getMonth()]} ${d.getDate()} ${clock}`
}

/** The entity an event is about, falling back to its route and then its message. */
export function eventSubject(e: AdminEventView): string {
  return e.user || e.group || e.provider || e.model || e.route || e.message
}

export interface EventGroup {
  /** The newest event of the run. */
  event: AdminEventView
  count: number
  key: string
}

/** Merges runs of consecutive events with the same kind, subject and message into one row. */
export function collapseEvents(events: readonly AdminEventView[]): EventGroup[] {
  const out: EventGroup[] = []
  const seen = new Map<string, number>()
  for (const e of events) {
    const last = out[out.length - 1]
    if (last && last.event.kind === e.kind && eventSubject(last.event) === eventSubject(e) && last.event.message === e.message) {
      last.count++
      continue
    }
    const base = `${e.time}|${e.replica}|${e.kind}|${e.message}`
    const n = seen.get(base) ?? 0
    seen.set(base, n + 1)
    out.push({ event: e, count: 1, key: `${base}|${n}` })
  }
  return out
}
