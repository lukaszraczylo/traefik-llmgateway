// Pure math behind CostAvoidedCard.vue (redesign-plan.md section 3.4): how
// much a fleet's FREE-tier traffic would have cost against a real, priced
// reference models. Kept out
// of the component the same way lib/forecast.ts/lib/burndown.ts keep their
// own math out of theirs.
import type { AdminCatalogModel, AdminUsageModelEntry } from '@/types/api'

/**
 * FreeUsageRow is the subset of a GET /admin/api/usage/models `detail=1`
 * entry costAvoidedMicros needs — deliberately narrower than
 * AdminUsageModelEntry so the function is testable against plain literals
 * without constructing a full entry.
 */
export interface FreeUsageRow {
  tokensIn: number
  tokensOut: number
}

/**
 * freeRows filters a model ranking down to its FREE rows (detail=1's own
 * `free` flag — a model billed as $0, whether a configured modelMeta.free
 * override or a genuinely unpriced model, types/api.ts's PriceSource doc
 * comment) and reads out just the token counts costAvoidedMicros needs.
 * `tokensIn`/`tokensOut` default to 0 for a detail-less entry (should not
 * happen once the caller has requested detail=1, but this keeps the
 * function total rather than reading `undefined` into arithmetic).
 */
export function freeRows(models: AdminUsageModelEntry[]): FreeUsageRow[] {
  return models.filter((m) => m.free).map((m) => ({ tokensIn: m.tokensIn ?? 0, tokensOut: m.tokensOut ?? 0 }))
}

/**
 * costAvoidedMicros sums, over every free-tier row, what that traffic
 * would have cost at a REAL reference model's own per-token-million rates
 * — the fleet's "cost avoided by routing to free models" figure
 * (redesign-plan.md section 3.4).
 *
 * Unit derivation (why no explicit /1e6 appears despite `refInputPerMTokUsd`
 * being USD PER MILLION tokens, types/api.ts's AdminCatalogModel doc
 * comment): cost_usd = tokensIn * (refInputPerMTokUsd / 1_000_000);
 * cost_micros = cost_usd * 1_000_000 (MICROS_PER_USD, lib/usage-bars.ts) =
 * tokensIn * refInputPerMTokUsd. The two factors of 1_000_000 cancel
 * exactly, so multiplying token counts directly by the $-per-million-token
 * rate already yields micro-USD — the same unit AdminUsageEntryView's own
 * costPerDayMicroUsd/costPerMonthMicroUsd fields use, with no separate
 * scaling step. Rounded once, over the summed total, not per-row, so a sum
 * of many sub-micro rows never underflows to a visibly wrong 0 before
 * accumulating.
 */
export function costAvoidedMicros(rows: FreeUsageRow[], refInputPerMTokUsd: number, refOutputPerMTokUsd: number): number {
  let total = 0
  for (const row of rows) total += row.tokensIn * refInputPerMTokUsd + row.tokensOut * refOutputPerMTokUsd
  return Math.round(total)
}

/** UNPRICED_SOURCES are the catalog priceSource values that carry no usable rate. */
const UNPRICED_SOURCES = new Set(['free', 'unpriced'])

/** GATEWAY_PRICE_LABEL labels a rate read from the live catalog rather than the reference table. */
const GATEWAY_PRICE_LABEL = 'gateway price'

const SONNET_PREFIX = 'anthropic/claude-sonnet-'
const DATE_SEGMENT = /^\d{8}$/

/**
 * COST_REFERENCES holds list prices for models the gateway records as
 * unpriced, used only by the Cost avoided card. `in`/`out` are USD per
 * 1M tokens. A real catalog price for the same id wins over this table.
 */
export const COST_REFERENCES = {
  minimax: { id: 'minimax/MiniMax-M3.1-Flash', label: 'MiniMax M3.1 Flash', in: 0.15, out: 0.6, source: 'MiniMax list price', asOf: '2026-11' },
} as const

export interface CostComparison {
  id: string
  label: string
  inPerMTok: number | null
  outPerMTok: number | null
  priceLabel: string
  avoidedMicros: number | null
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

function asOfLabel(asOf: string): string {
  const [year, month] = asOf.split('-')
  return `${MONTHS[Number(month) - 1] ?? month} ${year}`
}

function isPriced(m: AdminCatalogModel): boolean {
  return !UNPRICED_SOURCES.has(m.priceSource) && m.inputPerMTokUsd !== undefined && m.outputPerMTokUsd !== undefined
}

/** parseSonnet splits "claude-sonnet-4-5-20250929" into version [4, 5] and date 20250929 (0 when undated). */
function parseSonnet(id: string): { version: number[]; date: number } | null {
  const parts = id.slice(SONNET_PREFIX.length).split('-')
  const version: number[] = []
  let date = 0
  for (const p of parts) {
    if (DATE_SEGMENT.test(p)) date = Number(p)
    else if (/^\d+$/.test(p)) version.push(Number(p))
    else return null
  }
  return version.length === 0 ? null : { version, date }
}

function compareSonnet(a: { version: number[]; date: number }, b: { version: number[]; date: number }): number {
  const n = Math.max(a.version.length, b.version.length)
  for (let i = 0; i < n; i++) {
    const d = (a.version[i] ?? 0) - (b.version[i] ?? 0)
    if (d !== 0) return d
  }
  return a.date - b.date
}

/**
 * latestSonnet returns the highest-version priced `anthropic/claude-sonnet-*`
 * in the catalog, or null. A date suffix only breaks ties between equal versions.
 */
export function latestSonnet(catalog: Map<string, AdminCatalogModel>): AdminCatalogModel | null {
  let best: AdminCatalogModel | null = null
  let bestKey: { version: number[]; date: number } | null = null
  for (const m of catalog.values()) {
    if (!m.id.startsWith(SONNET_PREFIX) || !isPriced(m)) continue
    const key = parseSonnet(m.id)
    if (key === null) continue
    if (bestKey === null || compareSonnet(key, bestKey) > 0) {
      best = m
      bestKey = key
    }
  }
  return best
}

/**
 * costComparisons prices the free-tier rows against MiniMax M3.1 Flash, then
 * the latest Sonnet. Entry `avoidedMicros` is null only when no rate exists.
 */
export function costComparisons(rows: FreeUsageRow[], catalog: Map<string, AdminCatalogModel>): CostComparison[] {
  const ref = COST_REFERENCES.minimax
  const mm = catalog.get(ref.id)
  const mmCatalog = mm && isPriced(mm) ? mm : null
  const minimax = mmCatalog
    ? { inP: mmCatalog.inputPerMTokUsd as number, outP: mmCatalog.outputPerMTokUsd as number, priceLabel: GATEWAY_PRICE_LABEL }
    : { inP: ref.in as number, outP: ref.out as number, priceLabel: `${ref.source}, ${asOfLabel(ref.asOf)}` }
  const out: CostComparison[] = [
    {
      id: ref.id,
      label: ref.label,
      inPerMTok: minimax.inP,
      outPerMTok: minimax.outP,
      priceLabel: minimax.priceLabel,
      avoidedMicros: costAvoidedMicros(rows, minimax.inP, minimax.outP),
    },
  ]
  const sonnet = latestSonnet(catalog)
  out.push(
    sonnet
      ? {
          id: sonnet.id,
          label: sonnet.model,
          inPerMTok: sonnet.inputPerMTokUsd as number,
          outPerMTok: sonnet.outputPerMTokUsd as number,
          priceLabel: GATEWAY_PRICE_LABEL,
          avoidedMicros: costAvoidedMicros(rows, sonnet.inputPerMTokUsd as number, sonnet.outputPerMTokUsd as number),
        }
      : { id: `${SONNET_PREFIX}*`, label: 'Latest Claude Sonnet', inPerMTok: null, outPerMTok: null, priceLabel: 'no priced Sonnet in catalog', avoidedMicros: null },
  )
  return out
}
