import AxeBuilder from '@axe-core/playwright'
import type { Page } from '@playwright/test'

export interface CheckResult {
  name: CheckName
  ok: boolean
  detail: string
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
const FOCUS_TRANSITION_MS = 300
const LITERAL_RE = /\bNaN\b|undefined/

/** Console errors and uncaught page errors seen since the collector was attached. */
export function collectConsoleErrors(page: Page, ignore: RegExp[] = []): () => string[] {
  const errors: string[] = []
  page.on('console', (m) => {
    if (m.type() === 'error' && !ignore.some((re) => re.test(m.text()))) errors.push(m.text())
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

export async function headerAlignsWithBody(page: Page): Promise<CheckResult> {
  const bad = await page.evaluate((tol) => {
    const out: string[] = []
    document.querySelectorAll('table').forEach((table, ti) => {
      const ths = Array.from(table.querySelectorAll('thead th'))
      const row = table.querySelector('tbody tr')
      if (!ths.length || !row) return
      const cells = Array.from(row.children)
      ths.forEach((th, i) => {
        const td = cells[i]
        const a = th.getBoundingClientRect()
        const b = td?.getBoundingClientRect()
        if (!b || a.width === 0 || b.width === 0) return
        if (Math.abs(a.left - b.left) > tol) out.push(`table ${ti} col ${i} header ${a.left.toFixed(1)} body ${b.left.toFixed(1)}`)
      })
    })
    return out
  }, ALIGN_TOLERANCE_PX)
  return { name: 'headerAlignsWithBody', ok: bad.length === 0, detail: bad.slice(0, 3).join(' | ') }
}

export async function numbersRightAligned(page: Page): Promise<CheckResult> {
  const { total, bad } = await page.evaluate(() => {
    const cells = Array.from(document.querySelectorAll('[data-numeric]'))
    const wrong = cells.filter((el) => !['right', 'end'].includes(getComputedStyle(el).textAlign))
    return { total: cells.length, bad: wrong.length }
  })
  return { name: 'numbersRightAligned', ok: bad === 0, detail: `${bad} of ${total} [data-numeric] cells not right aligned` }
}

// perfect-design draws its focus ring as a box-shadow spread with `outline-style: none`; a ring is
// a layer with zero offset and blur, a spread above 0 and a non-transparent colour.
const RING_LAYER_RE = /(rgba?\([^)]*\)|oklab\([^)]*\)|oklch\([^)]*\)|color\([^)]*\)|#[0-9a-f]+)\s+0px\s+0px\s+0px\s+([\d.]+)px/g

export async function focusRingVisible(page: Page): Promise<CheckResult> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  await page.keyboard.press('Tab')
  await page.waitForTimeout(FOCUS_TRANSITION_MS)
  const info = await page.evaluate(() => {
    const el = document.activeElement
    if (!el || el === document.body) return null
    const cs = getComputedStyle(el)
    return { tag: el.tagName.toLowerCase(), width: parseFloat(cs.outlineWidth) || 0, style: cs.outlineStyle, shadow: cs.boxShadow }
  })
  if (!info) return { name: 'focusRingVisible', ok: false, detail: 'Tab focused nothing' }
  const outline = info.width > 0 && info.style !== 'none'
  const ring = Array.from(info.shadow.matchAll(RING_LAYER_RE)).some(
    (m) => parseFloat(m[2]) > 0 && !/rgba\(0, 0, 0, 0\)|\/ 0\)/.test(m[1]),
  )
  return { name: 'focusRingVisible', ok: outline || ring, detail: `${info.tag} outline ${info.width}px ${info.style}, ring ${ring}` }
}

export async function axeNoSeriousOrCritical(page: Page): Promise<CheckResult> {
  const { violations } = await new AxeBuilder({ page }).analyze()
  const bad = violations.filter((v) => v.impact === 'serious' || v.impact === 'critical')
  return { name: 'axeNoSeriousOrCritical', ok: bad.length === 0, detail: bad.map((v) => `${v.id}(${v.nodes.length})`).join(', ') }
}

export async function noLiteralNaNUndefined(page: Page): Promise<CheckResult> {
  const text = await page.evaluate(() => document.body.innerText)
  const m = LITERAL_RE.exec(text)
  const ctx = m ? text.slice(Math.max(0, m.index - 30), m.index + 30).replace(/\s+/g, ' ') : ''
  return { name: 'no-literal-NaN-undefined', ok: !m, detail: ctx }
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
