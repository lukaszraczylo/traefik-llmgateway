import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import { Badge } from '@/components/ui/badge'
import { targetColumns } from './target-columns'
import type { AdminTargetHealthView, AdminTargetView } from '@/types/api'

/**
 * Target health rendering (feat/target-health): mirrors usage-columns.
 * spec.ts's own approach of exercising a ColumnDef's `cell` renderer
 * directly (no component mount needed — vite.config.ts's `test` block
 * runs vitest in the 'node' environment with no @vue/test-utils/jsdom in
 * this package, so there is no existing "mount a .vue view" pattern to
 * follow here; the health column's rendering logic lives entirely in this
 * plain TanStack ColumnDef, exactly like usage-columns.ts's own cells, so
 * the identical direct-call approach gives full coverage without either).
 */
function testTarget(health: AdminTargetHealthView): AdminTargetView {
  return {
    name: 'my-server',
    url: 'https://mcp.example.com',
    counters: { requestsPerMinute: 0, requestsPerDay: 0, requestsPerMonth: 0 },
    health,
  }
}

function healthCellOf(target: AdminTargetView): unknown {
  const col = targetColumns().find((c) => c.id === 'health')
  const cell = col?.cell as (ctx: { row: { original: AdminTargetView } }) => unknown
  return cell({ row: { original: target } })
}

describe('targetColumns: health column', () => {
  it('renders a destructive Badge for unhealthy, with a relative-time label and lastError in the title', () => {
    const health: AdminTargetHealthView = {
      state: 'unhealthy',
      lastCheck: new Date(Date.now() - 42_000).toISOString(),
      lastError: 'connection refused',
      consecutiveFailures: 3,
      source: 'probe',
    }
    const rendered = healthCellOf(testTarget(health))

    expect(isVNode(rendered)).toBe(true)
    const vnode = rendered as { type: unknown; props?: Record<string, unknown>; children?: unknown }
    expect(vnode.type).toBe(Badge)
    expect(vnode.props?.variant).toBe('destructive')
    expect(vnode.props?.title).toBe('3 consecutive failures via probe: connection refused')

    const slot = (vnode.children as { default: () => string }).default
    expect(slot()).toMatch(/^unhealthy \(\d+s ago\)$/)
  })

  it('falls back to a bare "unhealthy" label when lastCheck is missing (version-skew defensive case)', () => {
    const health: AdminTargetHealthView = { state: 'unhealthy', consecutiveFailures: 1 }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { props?: Record<string, unknown>; children?: { default: () => string } }

    expect(vnode.children?.default()).toBe('unhealthy')
    // Singular "failure", no "via <source>", no lastError.
    expect(vnode.props?.title).toBe('1 consecutive failure')
  })

  it('renders an outline Badge labeled "unknown" with a fixed explanatory title', () => {
    const health: AdminTargetHealthView = { state: 'unknown', consecutiveFailures: 0 }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { type: unknown; props?: Record<string, unknown>; children?: { default: () => string } }

    expect(vnode.type).toBe(Badge)
    expect(vnode.props?.variant).toBe('outline')
    expect(vnode.props?.title).toBe('not observed yet — no traffic and probes disabled or not yet run')
    expect(vnode.children?.default()).toBe('unknown')
  })

  it('renders no Badge for healthy — plain muted text with latency, and relative time + source in the title', () => {
    const health: AdminTargetHealthView = {
      state: 'healthy',
      lastCheck: new Date(Date.now() - 5_000).toISOString(),
      consecutiveFailures: 0,
      latencyMs: 12,
      source: 'traffic',
    }
    const rendered = healthCellOf(testTarget(health))

    expect(isVNode(rendered)).toBe(true)
    const vnode = rendered as { type: unknown; props?: Record<string, unknown>; children?: unknown }
    expect(vnode.type).toBe('span')
    expect(vnode.props?.class).toBe('text-muted-foreground')
    // formatLatencyMs (lib/format.ts), not a raw "${latencyMs} ms" — no
    // space, matching every other latency reading in this panel.
    expect(vnode.children).toBe('healthy · 12ms')
    expect(vnode.props?.title).toMatch(/^\d+s ago via traffic$/)
  })

  it('renders the healthy latency suffix in seconds via formatLatencyMs once above 1000ms', () => {
    const health: AdminTargetHealthView = { state: 'healthy', consecutiveFailures: 0, latencyMs: 1500 }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { children?: unknown }
    expect(vnode.children).toBe('healthy · 1.5s')
  })

  it('omits the latency suffix for healthy when latencyMs is unknown', () => {
    const health: AdminTargetHealthView = { state: 'healthy', consecutiveFailures: 0 }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { children?: unknown; props?: Record<string, unknown> }

    expect(vnode.children).toBe('healthy')
    // Neither lastCheck nor source known — nothing to show in the title.
    expect(vnode.props?.title).toBe('')
  })

  it('the health column is not sortable — three semantic states have no meaningful alphabetical order', () => {
    const col = targetColumns().find((c) => c.id === 'health')
    expect(col?.enableSorting).toBe(false)
  })

  // F4/F7 (feat/target-health review): "unknown" also covers a target
  // that HAS been observed but has never succeeded, still below
  // failureThreshold — that case carries real failure detail even
  // though the label stays "unknown".
  it('surfaces failure detail in the unknown badge title for a target observed but never succeeded', () => {
    const health: AdminTargetHealthView = {
      state: 'unknown',
      consecutiveFailures: 2,
      source: 'probe',
      lastError: 'dial tcp: connection refused',
    }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { type: unknown; props?: Record<string, unknown>; children?: { default: () => string } }

    expect(vnode.type).toBe(Badge)
    expect(vnode.props?.variant).toBe('outline')
    expect(vnode.props?.title).toBe('2 failures so far via probe: dial tcp: connection refused')
    expect(vnode.children?.default()).toBe('unknown')
  })

  it('singular "failure" and no "via <source>"/lastError when only consecutiveFailures is known', () => {
    const health: AdminTargetHealthView = { state: 'unknown', consecutiveFailures: 1 }
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { props?: Record<string, unknown> }
    expect(vnode.props?.title).toBe('1 failure so far')
  })

  it('falls back to the outline "unknown" badge for any state other than healthy/unhealthy', () => {
    // Defensive: healthCell's own state check is an explicit
    // state === 'healthy' branch with "unknown" as the fallback for
    // everything else — not the reverse — so a value this version of
    // the webui does not recognize renders as "unknown", never as a
    // silently-wrong "healthy".
    const health = { state: 'something-future-versions-might-add', consecutiveFailures: 0 } as unknown as AdminTargetHealthView
    const rendered = healthCellOf(testTarget(health))
    const vnode = rendered as { type: unknown; props?: Record<string, unknown>; children?: { default: () => string } }

    expect(vnode.type).toBe(Badge)
    expect(vnode.props?.variant).toBe('outline')
    expect(vnode.children?.default()).toBe('unknown')
  })
})
