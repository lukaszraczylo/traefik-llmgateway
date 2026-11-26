import type { AdminRouterView } from '@/types/api'

const PER_MILLE_PER_PCT = 10

export interface DecisionGrid {
  rows: string[]
  cols: string[]
  values: number[][]
}
export interface DecisionView {
  summary: string
  grid: DecisionGrid | null
  outcomes: Array<{ name: string; count: number }>
}

export const hasDecision = (v: AdminRouterView): boolean => v.decision !== undefined

/** Decision-model section of the router panel; null hides it. Grid rows are heuristic tiers, columns model tiers. */
export function decisionView(v: AdminRouterView): DecisionView | null {
  const d = v.decision
  if (!d) return null
  const cells = v.stats?.decision ?? []
  let grid: DecisionGrid | null = null
  if (cells.length > 0) {
    const names = new Set(v.tiers.map((t) => t.name))
    for (const c of cells) {
      names.add(c.heuristic)
      names.add(c.model)
    }
    const order = [...names]
    const values = order.map(() => order.map(() => 0))
    for (const c of cells) values[order.indexOf(c.heuristic)][order.indexOf(c.model)] += c.count
    grid = { rows: order, cols: order, values }
  }
  return {
    summary: [d.provider, d.mode, d.timeout, `≥${Math.round(d.minProb / PER_MILLE_PER_PCT)}%`].join(' · '),
    grid,
    outcomes: (v.stats?.decisionOutcomes ?? []).map((o) => ({ name: o.name, count: o.count })),
  }
}
