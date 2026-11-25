import AxeBuilder from '@axe-core/playwright'
import type { Page } from '@playwright/test'

export interface CheckResult {
  name: CheckName
  ok: boolean
  detail: string
  /** True when the check had nothing to measure on this page (reported, never silent). */
  skipped?: boolean
}

export const CHECK_NAMES = [
  'noConsoleErrors',
  'noHorizontalOverflow',
  'headerAlignsWithBody',
  'numbersRightAligned',
  'focusRingVisible',
  'axeNoSeriousOrCritical',
  'no-literal-NaN-undefined',
] as const
export type CheckName = (typeof CHECK_NAMES)[number]

const ALIGN_TOLERANCE_PX = 1
const HEADER_ROW_PX = 30
const BODY_ROW_PX = 32
const METRIC_ROWS_CHECKED = 5
const FOCUS_TRANSITION_MS = 300
const LITERAL_RE = /\bNaN\b|undefined/
const NUMERIC_TEXT_RE = /^[-+]?[$€£]?\s?\d[\d,.]*\s?(%|ms|s|k|K|M|B)?$/
const RESOURCE_ERROR_RE = /Failed to load resource.*status of (\d{3})/
const API_PATH = '/admin/api/'

/** HTTP statuses a scenario deliberately provokes; their "Failed to load resource" lines are not defects. */
export const EXPECTED_STATUS: Record<string, number[]> = {
  'redis-down': [503],
  'auth-401': [401, 403],
  'auth-403': [403],
}

/** Ignored only when it is a resource-load line, with a status the scenario expects, for an admin API URL. */
export function isExpectedResourceError(text: string, url: string, scenario: string): boolean {
  const m = RESOURCE_ERROR_RE.exec(text)
  if (!m) return false
  return (EXPECTED_STATUS[scenario] ?? []).includes(Number(m[1])) && url.includes(API_PATH)
}

/** Console errors and uncaught page errors since attachment; app-level console.error always counts. */
export function collectConsoleErrors(page: Page, scenario: string): () => string[] {
  const errors: string[] = []
  page.on('console', (m) => {
    if (m.type() !== 'error') return
    if (isExpectedResourceError(m.text(), m.location().url, scenario)) return
    errors.push(m.text())
  })
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
  return () => errors
}

export function noConsoleErrors(errors: string[]): CheckResult {
  return { name: 'noConsoleErrors', ok: errors.length === 0, detail: errors.slice(0, 3).join(' | ') }
}

export async function noHorizontalOverflow(page: Page): Promise<CheckResult> {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }))
  return { name: 'noHorizontalOverflow', ok: sw <= iw, detail: `scrollWidth ${sw} > innerWidth ${iw}` }
}

interface AlignReport {
  problems: string[]
  aligned: number
  cards: number
}

/** Header/body left edges, Workbench row metrics for .wb-table, and overflow for tables rendered as cards. */
export async function headerAlignsWithBody(page: Page): Promise<CheckResult> {
  const r: AlignReport = await page.evaluate(
    ([tol, headerPx, bodyPx, rowsChecked]) => {
      const rep = { problems: [] as string[], aligned: 0, cards: 0 }
      document.querySelectorAll('table').forEach((table, ti) => {
        const ths = Array.from(table.querySelectorAll('thead th'))
        const rows = Array.from(table.querySelectorAll('tbody tr'))
        if (!ths.length || !rows.length) return
        const asCards = getComputedStyle(table).display !== 'table' || getComputedStyle(rows[0]).display !== 'table-row' || ths.every((th) => th.getBoundingClientRect().width === 0)
        if (asCards) {
          rep.cards++
          const box = table.parentElement
          const right = table.getBoundingClientRect().right
          if (right > window.innerWidth + tol || (box && box.scrollWidth > box.clientWidth + tol)) rep.problems.push(`table ${ti} cards overflow (right ${right.toFixed(0)}, vw ${window.innerWidth})`)
          return
        }
        const full = rows.find((row) => row.children.length === ths.length && !row.querySelector('[colspan]'))
        if (!full) {
          rep.problems.push(`table ${ti} has no body row with ${ths.length} cells`)
          return
        }
        rep.aligned++
        ths.forEach((th, i) => {
          const td = full.children[i]
          const cell = (el: Element) => el.getBoundingClientRect().left
          // Cell edges share a column by construction; the content edge also catches an inset mismatch.
          const content = (el: Element) => {
            const cs = getComputedStyle(el)
            return cell(el) + (parseFloat(cs.paddingLeft) || 0) + (parseFloat(cs.borderLeftWidth) || 0)
          }
          if (Math.abs(cell(th) - cell(td)) > tol) rep.problems.push(`table ${ti} col ${i} cell edge header ${cell(th).toFixed(1)} body ${cell(td).toFixed(1)}`)
          else if (Math.abs(content(th) - content(td)) > tol) rep.problems.push(`table ${ti} col ${i} content inset header ${content(th).toFixed(1)} body ${content(td).toFixed(1)}`)
        })
        if (table.closest('.wb-table')) {
          const hh = ths[0].getBoundingClientRect().height
          if (Math.abs(hh - headerPx) > tol) rep.problems.push(`table ${ti} wb header ${hh.toFixed(1)}px, want ${headerPx}`)
          rows.slice(0, rowsChecked).forEach((row, ri) => {
            const h = row.getBoundingClientRect().height
            if (Math.abs(h - bodyPx) > tol) rep.problems.push(`table ${ti} wb row ${ri} ${h.toFixed(1)}px, want ${bodyPx}`)
          })
        }
      })
      return rep
    },
    [ALIGN_TOLERANCE_PX, HEADER_ROW_PX, BODY_ROW_PX, METRIC_ROWS_CHECKED] as const,
  )
  const skipped = r.aligned === 0 && r.cards > 0 && r.problems.length === 0
  const detail = r.problems.slice(0, 3).join(' | ') || (skipped ? `skipped: cards (${r.cards} tables)` : '')
  return { name: 'headerAlignsWithBody', ok: r.problems.length === 0, detail, skipped }
}

/** Measures the containing cell (not the span), and every numeric-looking body cell must be right aligned and marked. */
export async function numbersRightAligned(page: Page): Promise<CheckResult> {
  const { bad, marked, looksNumeric } = await page.evaluate((reSrc) => {
    const re = new RegExp(reSrc)
    const out: string[] = []
    const right = (el: Element) => ['right', 'end'].includes(getComputedStyle(el).textAlign)
    const ignored = (el: Element) => !!el.closest('[data-wb-ignore-numeric]')
    const shown = (el: Element) => el.getBoundingClientRect().width > 0
    let marked = 0
    document.querySelectorAll('[data-numeric]').forEach((el) => {
      if (ignored(el)) return
      marked++
      const cell = el.closest('td,th') ?? el
      if (!right(cell)) out.push(`marked cell "${(el.textContent ?? '').trim().slice(0, 20)}" is ${getComputedStyle(cell).textAlign}`)
    })
    let looksNumeric = 0
    document.querySelectorAll('tbody td, tbody th').forEach((cell) => {
      if (ignored(cell) || !shown(cell)) return
      const text = (cell.textContent ?? '').trim()
      if (!re.test(text)) return
      looksNumeric++
      const mark = cell.hasAttribute('data-numeric') || cell.querySelector('[data-numeric]')
      if (!right(cell) || !mark) out.push(`numeric "${text.slice(0, 20)}" ${right(cell) ? '' : 'not right aligned'}${mark ? '' : ' unmarked'}`.trim())
    })
    return { bad: out, marked, looksNumeric }
  }, NUMERIC_TEXT_RE.source)
  return { name: 'numbersRightAligned', ok: bad.length === 0, detail: bad.slice(0, 3).join(' | ') || `${marked} marked, ${looksNumeric} numeric-looking cells` }
}

const RING_LAYER_RE = /(rgba?\([^)]*\)|oklab\([^)]*\)|oklch\([^)]*\)|color\([^)]*\)|#[0-9a-f]+)\s+0px\s+0px\s+0px\s+([\d.]+)px/g

interface FocusStyle {
  width: number
  style: string
  color: string
  shadow: string
}

function hasRing(s: FocusStyle): boolean {
  const outline = s.width > 0 && s.style !== 'none'
  const shadow = Array.from(s.shadow.matchAll(RING_LAYER_RE)).some((m) => parseFloat(m[2]) > 0 && !/rgba\(0, 0, 0, 0\)|\/ 0\)/.test(m[1]))
  return outline || shadow
}

/** Tab to the first control; it must match :focus-visible and its ring must appear on focus and vanish on blur. */
export async function focusRingVisible(page: Page): Promise<CheckResult> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  await page.keyboard.press('Tab')
  await page.waitForTimeout(FOCUS_TRANSITION_MS)
  const handle = await page.evaluateHandle(() => (document.activeElement === document.body ? null : document.activeElement))
  const el = handle.asElement()
  if (!el) return { name: 'focusRingVisible', ok: false, detail: 'Tab focused nothing' }
  const read = () =>
    el.evaluate((e) => {
      const cs = getComputedStyle(e)
      return { tag: e.tagName.toLowerCase(), fv: e.matches(':focus-visible'), width: parseFloat(cs.outlineWidth) || 0, style: cs.outlineStyle, color: cs.outlineColor, shadow: cs.boxShadow }
    })
  const focused = await read()
  await el.evaluate((e) => (e as HTMLElement).blur())
  await page.waitForTimeout(FOCUS_TRANSITION_MS)
  const blurred = await read()
  const sig = (s: FocusStyle) => `${s.width}|${s.style}|${s.color}|${s.shadow}`
  const ring = hasRing(focused)
  const differs = sig(focused) !== sig(blurred)
  const ok = focused.fv && ring && differs
  return { name: 'focusRingVisible', ok, detail: `${focused.tag} :focus-visible ${focused.fv}, ring ${ring}, differs from blurred ${differs}` }
}

type AxeViolation = Awaited<ReturnType<AxeBuilder['analyze']>>['violations'][number]

// One full run on a very large DOM (~100k nodes) exceeds ~14s and Playwright reports a spurious
// "execution context destroyed" with no navigation; the same rule sets pass when run separately.
const AXE_BATCHES: string[][] = [['wcag2a'], ['wcag2aa'], ['wcag21a', 'wcag21aa', 'wcag22aa'], ['best-practice']]

async function axeViolations(page: Page): Promise<{ list: AxeViolation[]; batched: boolean }> {
  try {
    return { list: (await new AxeBuilder({ page }).analyze()).violations, batched: false }
  } catch {
    const seen = new Map<string, AxeViolation>()
    for (const tags of AXE_BATCHES) {
      for (const v of (await new AxeBuilder({ page }).withTags(tags).analyze()).violations) seen.set(v.id, v)
    }
    return { list: Array.from(seen.values()), batched: true }
  }
}

export async function axeNoSeriousOrCritical(page: Page): Promise<CheckResult> {
  const { list, batched } = await axeViolations(page)
  const bad = list.filter((v) => v.impact === 'serious' || v.impact === 'critical')
  const detail = bad.map((v) => `${v.id}(${v.nodes.length})`).join(', ')
  return { name: 'axeNoSeriousOrCritical', ok: bad.length === 0, detail: batched ? `${detail} [batched run]`.trim() : detail }
}

export async function noLiteralNaNUndefined(page: Page): Promise<CheckResult> {
  const text = await page.evaluate(() => document.body.innerText)
  const m = LITERAL_RE.exec(text)
  const ctx = m ? text.slice(Math.max(0, m.index - 30), m.index + 30).replace(/\s+/g, ' ') : ''
  return { name: 'no-literal-NaN-undefined', ok: !m, detail: ctx }
}

const PANEL_SCROLL = '[data-panel-scroll]'
const MAX_EXPANDED_VIEWPORT_PX = 8000

/**
 * Grows the viewport by the panel's hidden scroll height so the whole panel is on screen.
 * A fullPage screenshot misses the Sheet's inner scroll, and axe and the geometry checks skip
 * content clipped by a scroller, so every panel check runs after this. Returns the remaining
 * hidden height (0 when the whole panel fits), or null when no panel is open.
 */
export async function expandPanelViewport(page: Page): Promise<number | null> {
  const box = page.locator(PANEL_SCROLL)
  if ((await box.count()) === 0) return null
  const { hidden } = await box.evaluate((el) => ({ hidden: el.scrollHeight - el.clientHeight }))
  const size = page.viewportSize()
  if (hidden > 0 && size) {
    await page.setViewportSize({ width: size.width, height: Math.min(size.height + hidden, MAX_EXPANDED_VIEWPORT_PX) })
    await page.waitForTimeout(300)
  }
  return box.evaluate((el) => Math.max(0, el.scrollHeight - el.clientHeight))
}

async function guarded(name: CheckName, run: () => Promise<CheckResult>): Promise<CheckResult> {
  try {
    return await run()
  } catch (e) {
    return { name, ok: false, detail: `check threw: ${(e as Error).message.split('\n')[0]}` }
  }
}

export async function runChecks(page: Page, consoleErrors: string[]): Promise<CheckResult[]> {
  return [
    noConsoleErrors(consoleErrors),
    await guarded('noHorizontalOverflow', () => noHorizontalOverflow(page)),
    await guarded('headerAlignsWithBody', () => headerAlignsWithBody(page)),
    await guarded('numbersRightAligned', () => numbersRightAligned(page)),
    await guarded('axeNoSeriousOrCritical', () => axeNoSeriousOrCritical(page)),
    await guarded('no-literal-NaN-undefined', () => noLiteralNaNUndefined(page)),
    await guarded('focusRingVisible', () => focusRingVisible(page)),
  ]
}
