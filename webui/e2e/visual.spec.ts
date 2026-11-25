import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'

import { PAGE_IDS } from '../src/lib/pages'
import { collectConsoleErrors, runChecks } from './checks'

const SCENARIOS = ['live', 'empty', 'limits-hot', 'breaker-open', 'redis-down', 'auth-401', 'huge'] as const
const VIEWPORTS = {
  desktop: { width: 1440, height: 900 },
  phone: { width: 390, height: 844 },
} as const
const THEMES = ['light', 'dark'] as const
const KEY_STORAGE = 'llmgwAdminKey'
const OUT = join(import.meta.dirname, '..', '.screenshots')
const SETTLE_MS = 600

async function sampleIds(page: Page): Promise<Record<string, string>> {
  const key = { 'x-api-key': 'e2e-dummy-key' }
  const get = async (path: string) => (await page.request.get(`/admin/api/${path}`, { headers: key })).json()
  const [usage, catalog, targets] = await Promise.all([get('usage'), get('catalog'), get('targets')])
  return {
    user: `user:${encodeURIComponent(usage.users[0].id)}`,
    model: `model:${encodeURIComponent(catalog.providers[0].models[0].id)}`,
    router: `router:${encodeURIComponent(catalog.routers[0].name)}`,
    target: `target:${encodeURIComponent((targets.mcpServers[0] ?? targets.agents[0]).name)}`,
  }
}

const PANEL_PAGES: Array<{ page: (typeof PAGE_IDS)[number]; kind: string }> = [
  { page: 'users', kind: 'user' },
  { page: 'models', kind: 'model' },
  { page: 'routers', kind: 'router' },
  { page: 'targets', kind: 'target' },
]

async function visit(page: Page, hash: string, scenario: string, theme: string): Promise<void> {
  await page.addInitScript(
    ([k, t]) => {
      sessionStorage.setItem(k, 'e2e-dummy-key')
      localStorage.setItem('pd-theme', JSON.stringify({ mode: t }))
    },
    [KEY_STORAGE, theme],
  )
  await page.emulateMedia({ colorScheme: theme as 'light' | 'dark' })
  await page.goto(`/admin/?mock=${scenario}${hash}`)
  await page.waitForLoadState('networkidle').catch(() => undefined)
  await page.waitForTimeout(SETTLE_MS)
}

async function exercise(page: Page, label: string, hash: string, scenario: string, viewport: keyof typeof VIEWPORTS, theme: string): Promise<void> {
  const errors = collectConsoleErrors(page, scenario)
  const diag = { navigationsAfterSettle: 0, crashed: false }
  page.on('crash', () => (diag.crashed = true))
  await visit(page, hash, scenario, theme)
  page.on('framenavigated', () => diag.navigationsAfterSettle++)
  mkdirSync(OUT, { recursive: true })
  const name = `${label}-${scenario}-${viewport}-${theme}`
  await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true })
  const results = await runChecks(page, errors())
  mkdirSync(join(OUT, 'results'), { recursive: true })
  writeFileSync(join(OUT, 'results', `${name}.json`), JSON.stringify({ label, scenario, viewport, theme, results, diag }))
  for (const r of results) expect.soft(r.ok, `${r.name}: ${r.detail}`).toBe(true)
}

for (const id of PAGE_IDS) {
  for (const scenario of SCENARIOS) {
    for (const [vp, size] of Object.entries(VIEWPORTS)) {
      for (const theme of THEMES) {
        test(`${id} ${scenario} ${vp} ${theme}`, async ({ page }) => {
          await page.setViewportSize(size)
          await exercise(page, id, `#${id}`, scenario, vp as keyof typeof VIEWPORTS, theme)
        })
      }
    }
  }
}

for (const { page: pageId, kind } of PANEL_PAGES) {
  for (const [vp, size] of Object.entries(VIEWPORTS)) {
    for (const theme of THEMES) {
      test(`${pageId} panel ${kind} live ${vp} ${theme}`, async ({ page }) => {
        await page.setViewportSize(size)
        const sel = (await sampleIds(page))[kind]
        await exercise(page, `${pageId}-panel-${kind}`, `#${pageId}?sel=${encodeURIComponent(sel)}`, 'live', vp as keyof typeof VIEWPORTS, theme)
      })
    }
  }
}

// Compare mode: tiles show a delta and the chart adds a dashed previous-period series.
for (const theme of THEMES) {
  test(`overview-cmp live desktop ${theme}`, async ({ page }) => {
    await page.setViewportSize(VIEWPORTS.desktop)
    await exercise(page, 'overview-cmp', '#overview?cmp=prev', 'live', 'desktop', theme)
    await expect(page.locator('text=Previous period >> visible=true').first()).toBeVisible()
  })
}

// Usage ranges whose model detail the API refuses (400 range too large): the page falls back to the plain ranking.
for (const scenario of ['live', 'empty'] as const) {
  for (const [vp, size] of Object.entries(VIEWPORTS)) {
    for (const theme of THEMES) {
      test(`usage-48h ${scenario} ${vp} ${theme}`, async ({ page }) => {
        await page.setViewportSize(size)
        await exercise(page, 'usage-48h', '#usage?range=48h', scenario, vp as keyof typeof VIEWPORTS, theme)
      })
    }
  }
}

for (const theme of THEMES) {
  test(`usage-cmp live desktop ${theme}`, async ({ page }) => {
    await page.setViewportSize(VIEWPORTS.desktop)
    await exercise(page, 'usage-cmp', '#usage?cmp=prev', 'live', 'desktop', theme)
    await expect(page.locator('text=Previous period >> visible=true').first()).toBeVisible()
  })
}
