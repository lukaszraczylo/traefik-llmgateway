import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'

import {
  axeNoSeriousOrCritical,
  collectConsoleErrors,
  focusRingVisible,
  headerAlignsWithBody,
  isExpectedResourceError,
  noConsoleErrors,
  noHorizontalOverflow,
  noLiteralNaNUndefined,
  numbersRightAligned,
} from './checks'

// Each check must fail on a bad fixture and pass on a good one, or it cannot gate a page.

const doc = (body: string, head = '') =>
  `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>fixture</title>${head}</head><body><main><h1>fixture</h1>${body}</main></body></html>`

async function load(page: Page, body: string, head = ''): Promise<void> {
  await page.setContent(doc(body, head))
}

const TABLE_CSS = `<style>table{border-collapse:collapse}th,td{padding:0 12px;text-align:left}
th{height:30px}tr{height:32px}td.r,th.r{text-align:right}</style>`

test.describe('numbersRightAligned', () => {
  test('fails on a numeric cell left aligned and unmarked', async ({ page }) => {
    await load(page, `<table><thead><tr><th>n</th></tr></thead><tbody><tr><td>1,234</td></tr></tbody></table>`, TABLE_CSS)
    expect((await numbersRightAligned(page)).ok).toBe(false)
  })
  test('fails on a marked span inside a left aligned cell (the span cannot vouch for its cell)', async ({ page }) => {
    await load(
      page,
      `<table><thead><tr><th>n</th></tr></thead><tbody><tr><td><span data-numeric style="display:block;text-align:right">42</span></td></tr></tbody></table>`,
      TABLE_CSS,
    )
    expect((await numbersRightAligned(page)).ok).toBe(false)
  })
  test('fails on a currency-prefixed cell that is right aligned but unmarked', async ({ page }) => {
    await load(page, `<table><thead><tr><th class="r">c</th></tr></thead><tbody><tr><td class="r">$0.09</td></tr></tbody></table>`, TABLE_CSS)
    expect((await numbersRightAligned(page)).ok).toBe(false)
  })
  test('passes on right aligned marked cells, and honours the ignore hatch', async ({ page }) => {
    await load(
      page,
      `<table><thead><tr><th class="r">n</th><th>id</th></tr></thead><tbody><tr><td class="r"><span data-numeric>1,234</span></td><td data-wb-ignore-numeric>20260101</td></tr></tbody></table>`,
      TABLE_CSS,
    )
    expect((await numbersRightAligned(page)).ok).toBe(true)
  })
})

test.describe('noConsoleErrors scenario filter', () => {
  test('pure filter: status, scenario and API url must all match', () => {
    const line = 'Failed to load resource: the server responded with a status of 503 (Service Unavailable)'
    const api = 'http://x.test/admin/api/overview'
    expect(isExpectedResourceError(line, api, 'redis-down')).toBe(true)
    expect(isExpectedResourceError(line, api, 'live')).toBe(false)
    expect(isExpectedResourceError(line.replace('503', '500'), api, 'redis-down')).toBe(false)
    expect(isExpectedResourceError(line, 'http://x.test/other/thing', 'redis-down')).toBe(false)
    expect(isExpectedResourceError('app exploded status of 503', api, 'redis-down')).toBe(false)
    expect(isExpectedResourceError(line.replace('503', '403'), api, 'auth-401')).toBe(true)
    expect(isExpectedResourceError(line.replace('503', '403'), api, 'auth-403')).toBe(true)
    expect(isExpectedResourceError(line.replace('503', '401'), api, 'auth-403')).toBe(false)
  })

  async function errorsFor(page: Page, scenario: string, img: string, script = ''): Promise<string[]> {
    await page.route('http://x.test/**', (r) => r.fulfill({ status: Number(new URL(r.request().url()).searchParams.get('s') ?? 503), body: '' }))
    const get = collectConsoleErrors(page, scenario)
    await page.setContent(doc(`<img src="${img}"><script>${script}</script>`))
    await page.waitForTimeout(300)
    return get()
  }

  test('real 503 on the API is ignored under redis-down but counted under live', async ({ page }) => {
    expect(await errorsFor(page, 'redis-down', 'http://x.test/admin/api/o?s=503')).toEqual([])
  })
  test('counts the same 503 under live', async ({ page }) => {
    expect((await errorsFor(page, 'live', 'http://x.test/admin/api/o?s=503')).length).toBe(1)
  })
  test('counts a 503 outside /admin/api/ and an unexpected 500 under redis-down', async ({ page }) => {
    expect((await errorsFor(page, 'redis-down', 'http://x.test/static/o?s=503')).length).toBe(1)
  })
  test('app console.error and pageerror always count', async ({ page }) => {
    const e = await errorsFor(page, 'redis-down', 'http://x.test/admin/api/o?s=503', `console.error('boom'); setTimeout(() => { throw new Error('late') })`)
    expect(e.some((m) => m === 'boom')).toBe(true)
    expect(e.some((m) => m.startsWith('pageerror:'))).toBe(true)
    expect(noConsoleErrors(e).ok).toBe(false)
    expect(noConsoleErrors([]).ok).toBe(true)
  })
})

test.describe('focusRingVisible', () => {
  test('fails when focus draws nothing', async ({ page }) => {
    await load(page, `<button>go</button>`, `<style>button{outline:none;box-shadow:none}</style>`)
    expect((await focusRingVisible(page)).ok).toBe(false)
  })
  test('fails when the ring is permanent, so it is not tied to focus', async ({ page }) => {
    await load(page, `<button>go</button>`, `<style>button{outline:2px solid red}</style>`)
    expect((await focusRingVisible(page)).ok).toBe(false)
  })
  test('fails when nothing is focusable', async ({ page }) => {
    await load(page, `<p>text</p>`)
    expect((await focusRingVisible(page)).ok).toBe(false)
  })
  test('passes for an outline that appears on focus-visible', async ({ page }) => {
    await load(page, `<button>go</button>`, `<style>button{outline:none}button:focus-visible{outline:2px solid red}</style>`)
    expect((await focusRingVisible(page)).ok).toBe(true)
  })
  test('passes for a box-shadow ring that appears on focus-visible', async ({ page }) => {
    await load(page, `<button>go</button>`, `<style>button{outline:none}button:focus-visible{box-shadow:0 0 0 3px rgb(200 0 0)}</style>`)
    expect((await focusRingVisible(page)).ok).toBe(true)
  })
})

test.describe('headerAlignsWithBody', () => {
  const head = `<style>table{border-collapse:collapse}th{height:30px;padding:0 12px;text-align:left}td{padding:0 12px}tr{height:32px}</style>`
  test('fails when a header is offset from its body column', async ({ page }) => {
    await load(page, `<table><thead><tr><th>a</th><th style="padding-left:40px">b</th></tr></thead><tbody><tr><td>x</td><td>y</td></tr></tbody></table>`, head)
    expect((await headerAlignsWithBody(page)).ok).toBe(false)
  })
  test('uses a full-width body row, not a leading colspan row', async ({ page }) => {
    await load(page, `<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td colspan="2">group</td></tr><tr><td>x</td><td>y</td></tr></tbody></table>`, head)
    expect((await headerAlignsWithBody(page)).ok).toBe(true)
  })
  test('passes when aligned', async ({ page }) => {
    await load(page, `<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>x</td><td>y</td></tr></tbody></table>`, head)
    const r = await headerAlignsWithBody(page)
    expect(r.ok).toBe(true)
    expect(r.skipped).toBeFalsy()
  })
  test('reports a skip, not a silent pass, when the table renders as cards', async ({ page }) => {
    await load(
      page,
      `<table><thead><tr><th>a</th></tr></thead><tbody><tr><td>x</td></tr></tbody></table>`,
      `<style>table,thead,tbody,tr,td,th{display:block}thead{display:none}</style>`,
    )
    const r = await headerAlignsWithBody(page)
    expect(r.skipped).toBe(true)
    expect(r.detail).toContain('skipped: cards')
  })
  test('fails when a cards-mode table overflows the viewport', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 600 })
    await load(
      page,
      `<table style="width:900px"><thead><tr><th>a</th></tr></thead><tbody><tr><td>x</td></tr></tbody></table>`,
      `<style>table,tbody,tr,td{display:block}thead{display:none}</style>`,
    )
    expect((await headerAlignsWithBody(page)).ok).toBe(false)
  })
  test('wb row metrics fail on a wrong density and pass at 30/32', async ({ page }) => {
    const wb = (h: number) =>
      `<div class="wb-table"><table style="border-collapse:collapse;box-sizing:border-box"><thead><tr><th style="height:30px;box-sizing:border-box">a</th></tr></thead><tbody><tr style="height:${h}px"><td>x</td></tr></tbody></table></div>`
    await load(page, wb(44))
    expect((await headerAlignsWithBody(page)).ok).toBe(false)
    await load(page, wb(32))
    expect((await headerAlignsWithBody(page)).ok).toBe(true)
  })
})

test.describe('noHorizontalOverflow', () => {
  test('fails on a page wider than the viewport', async ({ page }) => {
    await load(page, `<div style="width:3000px">wide</div>`)
    expect((await noHorizontalOverflow(page)).ok).toBe(false)
  })
  test('passes on a normal page', async ({ page }) => {
    await load(page, `<p>narrow</p>`)
    expect((await noHorizontalOverflow(page)).ok).toBe(true)
  })
})

test.describe('no-literal-NaN-undefined', () => {
  test('fails on NaN and on undefined text', async ({ page }) => {
    await load(page, `<p>cost NaN%</p>`)
    expect((await noLiteralNaNUndefined(page)).ok).toBe(false)
    await load(page, `<p>value undefined</p>`)
    expect((await noLiteralNaNUndefined(page)).ok).toBe(false)
  })
  test('passes on clean text', async ({ page }) => {
    await load(page, `<p>banana nanometre</p>`)
    expect((await noLiteralNaNUndefined(page)).ok).toBe(true)
  })
})

test.describe('axeNoSeriousOrCritical', () => {
  test('fails on a critical violation (image without alt)', async ({ page }) => {
    await load(page, `<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=">`)
    const r = await axeNoSeriousOrCritical(page)
    expect(r.ok).toBe(false)
    expect(r.detail).toContain('image-alt')
  })
  test('passes on an accessible page', async ({ page }) => {
    await load(page, `<button>Save</button>`)
    expect((await axeNoSeriousOrCritical(page)).ok).toBe(true)
  })
})
