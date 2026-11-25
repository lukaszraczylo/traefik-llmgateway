import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'

import { PAGE_IDS } from '../src/lib/pages'
import { collectConsoleErrors, expandPanelViewport, runChecks } from './checks'

const SCENARIOS = ['live', 'empty', 'limits-hot', 'breaker-open', 'redis-down', 'auth-401', 'huge'] as const
const VIEWPORTS = {
  desktop: { width: 1440, height: 900 },
  phone: { width: 390, height: 844 },
} as const
const THEMES = ['light', 'dark'] as const
const KEY_STORAGE = 'llmgwAdminKey'
const OUT = join(import.meta.dirname, '..', '.screenshots')
const SETTLE_MS = 600

interface UsageSample {
  id: string
  requestsPerMinute: number
  limits?: { requestsPerMinute?: number }
  memberLimits?: unknown
}

/** The user closest to its per-minute request limit: under limits-hot this is a paced user. */
function hottestUser(users: UsageSample[]): string {
  const fill = (u: UsageSample) => (u.limits?.requestsPerMinute ? u.requestsPerMinute / u.limits.requestsPerMinute : 0)
  return [...users].sort((a, b) => fill(b) - fill(a))[0].id
}

async function sampleIds(page: Page, scenario: string): Promise<Record<string, string>> {
  const headers = { 'x-api-key': 'e2e-dummy-key', 'x-mock-scenario': scenario }
  const get = async (path: string) => (await page.request.get(`/admin/api/${path}`, { headers })).json()
  const [usage, catalog, targets] = await Promise.all([get('usage'), get('catalog'), get('targets')])
  const memberGroup = usage.groups.find((g: UsageSample) => g.memberLimits) ?? usage.groups[0]
  return {
    user: `user:${encodeURIComponent(scenario === 'limits-hot' ? hottestUser(usage.users) : usage.users[0].id)}`,
    group: `group:${encodeURIComponent(memberGroup.id)}`,
    model: `model:${encodeURIComponent(catalog.providers[0].models[0].id)}`,
    'routed-model': `model:${encodeURIComponent(catalog.routers[0].tiers[0].models[0])}`,
    'provider-degraded': `provider:${encodeURIComponent('amd')}`,
    'provider-open': `provider:${encodeURIComponent('alibaba')}`,
    router: `router:${encodeURIComponent(catalog.routers[0].name)}`,
    'router-uni': `router:${encodeURIComponent(catalog.routers[1].name)}`,
    target: `target:${encodeURIComponent(targets.mcpServers[0] ? `mcp/${targets.mcpServers[0].name}` : `agent/${targets.agents[0].name}`)}`,
  }
}

const PANEL_PAGES: Array<{ page: (typeof PAGE_IDS)[number]; kind: string; scenario?: string }> = [
  { page: 'users', kind: 'user' },
  { page: 'users', kind: 'user', scenario: 'limits-hot' },
  { page: 'users', kind: 'group', scenario: 'limits-hot' },
  { page: 'models', kind: 'model' },
  { page: 'models', kind: 'routed-model' },
  { page: 'models', kind: 'provider-degraded' },
  { page: 'models', kind: 'provider-open', scenario: 'breaker-open' },
  { page: 'routers', kind: 'router' },
  { page: 'routers', kind: 'router', scenario: 'router-escalations' },
  { page: 'routers', kind: 'router-uni' },
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
  // Panels are measured and captured whole: a Sheet scrolls inside the viewport, which fullPage cannot see.
  const hiddenAfterExpand = await expandPanelViewport(page)
  if (hiddenAfterExpand !== null) {
    await page.locator('[data-panel-scroll]').screenshot({ path: join(OUT, `${name}-full.png`) })
    expect.soft(hiddenAfterExpand, `${name}: panel still scrolls after expanding the viewport`).toBe(0)
  }
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

for (const { page: pageId, kind, scenario = 'live' } of PANEL_PAGES) {
  for (const [vp, size] of Object.entries(VIEWPORTS)) {
    for (const theme of THEMES) {
      test(`${pageId} panel ${kind} ${scenario} ${vp} ${theme}`, async ({ page }) => {
        await page.setViewportSize(size)
        const sel = (await sampleIds(page, scenario))[kind]
        await exercise(page, `${pageId}-panel-${kind}`, `#${pageId}?sel=${encodeURIComponent(sel)}`, scenario, vp as keyof typeof VIEWPORTS, theme)
      })
    }
  }
}

// The limits snippet form inside the user panel, opened.
for (const [vp, size] of Object.entries(VIEWPORTS)) {
  for (const theme of THEMES) {
    test(`users panel snippet limits-hot ${vp} ${theme}`, async ({ page }) => {
      await page.setViewportSize(size)
      const sel = (await sampleIds(page, 'limits-hot')).user
      await visit(page, `#users?sel=${encodeURIComponent(sel)}`, 'limits-hot', theme)
      await page.getByRole('button', { name: 'Generate limits snippet' }).click()
      await page.getByLabel('Requests/minute').fill('45')
      const snippet = page.locator('pre', { hasText: 'requestsPerMinute' })
      await snippet.scrollIntoViewIfNeeded()
      await expect(snippet).toBeVisible()
      const name = `users-panel-snippet-limits-hot-${vp}-${theme}`
      mkdirSync(OUT, { recursive: true })
      await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true })
      const results = await runChecks(page, [])
      for (const r of results) expect.soft(r.ok, `${r.name}: ${r.detail}`).toBe(true)
    })
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

// Only alibaba at 14d and 30d carries TTFB points in the fixtures: the chart must draw, not fall back to the empty state.
for (const theme of THEMES) {
  test(`reliability-ttfb live desktop ${theme}`, async ({ page }) => {
    await page.setViewportSize(VIEWPORTS.desktop)
    await exercise(page, 'reliability-ttfb', '#reliability?range=30d&provider=alibaba', 'live', 'desktop', theme)
    const chart = page.getByTestId('ttfb-chart')
    await expect(chart).toBeVisible()
    // The fixture has two isolated points, drawn as markers; a line needs adjacent points.
    expect(await chart.locator('svg path, svg circle').count()).toBeGreaterThan(0)
    await expect(page.getByText('No latency samples in this range')).toHaveCount(0)
  })
}

// Month ranges clamp to 30 days; the page says so.
for (const theme of THEMES) {
  test(`reliability-3mo live desktop ${theme}`, async ({ page }) => {
    await page.setViewportSize(VIEWPORTS.desktop)
    await exercise(page, 'reliability-3mo', '#reliability?range=3mo', 'live', 'desktop', theme)
    await expect(page.getByTestId('clamped-range')).toBeVisible()
  })
}

// Events with a filter set in the hash: the chips show and the feed narrows. Also the Reliability handoff param.
const EVENT_FILTER_HASHES = { 'events-filter': '#events?kind=upstream&route=chat', 'events-provider': '#events?provider=alibaba' } as const
for (const [label, hash] of Object.entries(EVENT_FILTER_HASHES)) {
  for (const [vp, size] of Object.entries(VIEWPORTS)) {
    for (const theme of THEMES) {
      test(`${label} live ${vp} ${theme}`, async ({ page }) => {
        await page.setViewportSize(size)
        await exercise(page, label, hash, 'live', vp as keyof typeof VIEWPORTS, theme)
        await expect(page.getByRole('group', { name: /filter/i }).first()).toBeVisible()
        await expect(page.getByText(label === 'events-filter' ? /Kind.*Upstream error/i : /Provider.*alibaba/i).first()).toBeVisible()
      })
    }
  }
}

test('events pause and resume the live tail', async ({ page }) => {
  await page.setViewportSize(VIEWPORTS.desktop)
  let polls = 0
  page.on('request', (r) => r.url().includes('/admin/api/events') && polls++)
  await visit(page, '#events', 'live', 'light')
  const status = page.getByTestId('events-live-status')
  await expect(status).toContainText('Live')
  await page.getByRole('button', { name: 'Pause' }).click()
  await expect(status).toContainText('Paused')
  const paused = polls
  await page.waitForTimeout(6500)
  expect(polls, 'no events request while paused').toBe(paused)
  await page.getByRole('button', { name: 'Resume' }).click()
  await expect(status).toContainText('Live')
  await expect.poll(() => polls).toBeGreaterThan(paused)
})

test('events rows are capped and show more', async ({ page }) => {
  await page.setViewportSize(VIEWPORTS.desktop)
  await visit(page, '#events', 'huge', 'light')
  const rows = page.getByRole('list', { name: 'Events' }).locator('li')
  expect(await rows.count()).toBeLessThanOrEqual(50)
  await page.getByRole('button', { name: /Show \d+ more/ }).click()
  expect(await rows.count()).toBeGreaterThan(50)
})
