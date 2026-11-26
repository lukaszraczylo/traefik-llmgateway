import type { AdminRouterView } from '@/types/api'

const UNSET = '—'
const WHOLE = 100
const OUTCOME_ORDER = ['ok', 'slow', 'bad', 'escalated', 'error'] as const

export interface RouterTierModel {
  name: string
  minScore: number
  models: string[]
  isDefault: boolean
  count: number
}
export interface RouterSplit {
  name: string
  count: number
  pct: number
}
export interface RouterOutcome {
  outcome: string
  count: number
  pct: number
}
export interface RouterSetting {
  key: string
  label: string
  value: string
}
export interface RouterShadow {
  rows: string[]
  cols: string[]
  values: number[][]
}
export interface RouterPanelModel {
  tiers: RouterTierModel[]
  split: RouterSplit[]
  reasons: Array<{ name: string; count: number }>
  outcomes: RouterOutcome[]
  shadow: RouterShadow | null
  settings: RouterSetting[]
}

/** Whole-number percentages that sum to exactly 100 (largest remainder); all zero without traffic. */
function percentages(counts: number[]): number[] {
  const total = counts.reduce((a, b) => a + b, 0)
  if (total <= 0) return counts.map(() => 0)
  const exact = counts.map((c) => (c * WHOLE) / total)
  const pct = exact.map(Math.floor)
  let left = WHOLE - pct.reduce((a, b) => a + b, 0)
  const byRemainder = exact.map((e, i) => ({ i, r: e - pct[i] })).sort((a, b) => b.r - a.r || a.i - b.i)
  for (const { i } of byRemainder) {
    if (left-- <= 0) break
    pct[i]++
  }
  return pct
}

const onOff = (v: boolean): string => (v ? 'on' : 'off')

function settings(v: AdminRouterView): RouterSetting[] {
  const s = (key: string, label: string, value: string | number | undefined): RouterSetting => ({ key, label, value: value === undefined || value === '' ? UNSET : String(value) })
  return [
    s('classifier', 'Classifier', v.classifier),
    s('shadowClassifier', 'Shadow classifier', v.shadowClassifier),
    s('riskZ', 'Risk margin (σ/100)', v.riskZ),
    s('selection', 'Selection', v.selection),
    s('defaultTier', 'Default tier', v.defaultTier),
    s('toolsMinTier', 'Tools floor tier', v.toolsMinTier),
    s('maxAttempts', 'Max attempts', v.maxAttempts),
    s('firstByteTimeout', 'First-byte timeout', v.firstByteTimeout),
    s('timeoutFallback', 'Timeout fallback', onOff(v.timeoutFallback)),
    s('fellBack', 'Fell back', v.fellBack),
    s('saturationDowngrade', 'Saturation downgrade', onOff(v.saturationDowngrade)),
    s('downgraded', 'Downgraded', v.downgraded),
    s('contextEscalation', 'Context escalation', onOff(v.contextEscalation)),
    s('contextTokens', 'Advertised context (tokens)', v.contextTokens),
    s('calibration', 'Calibration', v.calibration.mode),
  ]
}

function shadow(v: AdminRouterView, order: string[]): RouterShadow | null {
  const cells = v.stats?.shadow ?? []
  if (cells.length === 0) return null
  const seen = new Set(order)
  for (const c of cells) for (const n of [c.live, c.shadow]) if (!seen.has(n)) seen.add(n)
  const names = [...seen]
  const values = names.map(() => names.map(() => 0))
  for (const c of cells) values[names.indexOf(c.live)][names.indexOf(c.shadow)] += c.count
  return { rows: names, cols: names, values }
}

/** Derived view of one smart router for the panel: tiers, tier split, reasons, outcomes, shadow matrix and settings rows. */
export function routerPanelModel(view: AdminRouterView): RouterPanelModel {
  const stats = view.stats
  const counts = new Map((stats?.tiers ?? []).map((t) => [t.name, t.count]))
  const order = view.tiers.map((t) => t.name)
  for (const name of counts.keys()) if (!order.includes(name)) order.push(name)

  const splitCounts = order.map((n) => counts.get(n) ?? 0)
  const pct = percentages(splitCounts)
  const split = order.map((name, i) => ({ name, count: splitCounts[i], pct: pct[i] }))

  const totals = new Map<string, number>()
  for (const o of stats?.outcomes ?? []) totals.set(o.outcome, (totals.get(o.outcome) ?? 0) + o.count)
  const kinds = [...OUTCOME_ORDER, ...[...totals.keys()].filter((k) => !(OUTCOME_ORDER as readonly string[]).includes(k))].filter((k) => (totals.get(k) ?? 0) > 0)
  const outcomePct = percentages(kinds.map((k) => totals.get(k) ?? 0))
  const outcomes = kinds.map((outcome, i) => ({ outcome, count: totals.get(outcome) ?? 0, pct: outcomePct[i] }))

  return {
    tiers: view.tiers.map((t) => ({ name: t.name, minScore: t.minScore, models: t.models, isDefault: t.name === view.defaultTier, count: counts.get(t.name) ?? 0 })),
    split,
    reasons: [...(stats?.reasons ?? [])].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name)),
    outcomes,
    shadow: shadow(view, order),
    settings: settings(view),
  }
}
