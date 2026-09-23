import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import CompactNumber from '@/components/CompactNumber.vue'
import { Badge } from '@/components/ui/badge'
import { composeTargetId, parseTargetCallerId, resolveTargetRef, targetCallerColumns, targetColumns } from './target-columns'
import type { AdminTargetHealthView, AdminTargetsResponse, AdminTargetView, AdminTotalsResponse } from '@/types/api'

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

function accessCellOf(access: AdminTargetView['access']): unknown {
  const col = targetColumns().find((c) => c.id === 'access')
  const cell = col?.cell as (ctx: { row: { original: AdminTargetView } }) => unknown
  return cell({ row: { original: { ...testTarget({ state: 'unknown', consecutiveFailures: 0 }), access } } })
}

describe('targetColumns: access column', () => {
  it('renders "All groups" for null and absent access', () => {
    for (const access of [null, undefined]) {
      const node = accessCellOf(access)
      expect(isVNode(node) && node.children).toBe('All groups')
    }
  })

  it('renders "No groups" for an empty access list, not "All groups"', () => {
    const node = accessCellOf([])
    expect(isVNode(node) && node.children).toBe('No groups')
  })

  it('renders one Badge per group for a non-empty access list', () => {
    const node = accessCellOf(['a', 'b'])
    expect(isVNode(node)).toBe(true)
    const children = (node as { children: unknown[] }).children
    expect(children).toHaveLength(2)
    expect(children.every((c) => isVNode(c) && c.type === Badge)).toBe(true)
  })
})

describe('targetColumns: name column selection', () => {
  it('renders plain text with no onSelect (default, backward-compatible)', () => {
    const col = targetColumns().find((c) => c.id === 'name')
    const cell = col?.cell as (ctx: { row: { original: AdminTargetView } }) => unknown
    const rendered = cell({ row: { original: testTarget({ state: 'unknown', consecutiveFailures: 0 }) } })
    expect(isVNode(rendered) && rendered.type).toBe('span')
  })

  it('renders a clickable button that invokes onSelect with the row when given', () => {
    const selected: AdminTargetView[] = []
    const col = targetColumns((t) => selected.push(t)).find((c) => c.id === 'name')
    const cell = col?.cell as (ctx: { row: { original: AdminTargetView } }) => unknown
    const target = testTarget({ state: 'unknown', consecutiveFailures: 0 })
    const rendered = cell({ row: { original: target } })
    expect(isVNode(rendered) && rendered.type).toBe('button')

    const onClick = (rendered as { props?: Record<string, unknown> }).props?.onClick as (e: MouseEvent) => void
    onClick({ stopPropagation: () => {} } as MouseEvent)
    expect(selected).toEqual([target])
  })
})

describe('composeTargetId', () => {
  it('joins kind and name with a slash', () => {
    expect(composeTargetId('mcp', 'fetch')).toBe('mcp/fetch')
    expect(composeTargetId('agent', 'researcher')).toBe('agent/researcher')
  })
})

describe('parseTargetCallerId', () => {
  it('splits a well-formed "kind/target/caller" id into its three parts', () => {
    expect(parseTargetCallerId('mcp/fetch/alice')).toEqual({ kind: 'mcp', target: 'fetch', caller: 'alice' })
  })

  it('handles a caller name that itself contains a slash by keeping it in `caller` verbatim', () => {
    expect(parseTargetCallerId('agent/researcher/team/bob')).toEqual({ kind: 'agent', target: 'researcher', caller: 'team/bob' })
  })

  it('falls back to an empty target/caller for an id with no slash at all, without throwing', () => {
    expect(parseTargetCallerId('malformed')).toEqual({ kind: '', target: '', caller: 'malformed' })
  })

  it('falls back to an empty caller for an id with only one slash', () => {
    expect(parseTargetCallerId('mcp/fetch')).toEqual({ kind: 'mcp', target: 'fetch', caller: '' })
  })
})

// dayOrMonthWindow moved to lib/range.ts (P2 item 2: a SHARED clamp used
// by stores/consumers.ts, stores/spend.ts, AND this module's own
// TargetCallers.vue — see range.spec.ts's own describe('dayOrMonthWindow').

describe('targetCallerColumns', () => {
  function row(id: string, req: number): AdminTotalsResponse['rows'][number] {
    return { id, values: { req } }
  }

  it('omits the Target column when showTarget is false', () => {
    const columns = targetCallerColumns(false)
    expect(columns.find((c) => c.id === 'target')).toBeUndefined()
    expect(columns.map((c) => c.id)).toEqual(['caller', 'requests'])
  })

  it('includes the Target column, rendering "kind/target", when showTarget is true', () => {
    const columns = targetCallerColumns(true)
    const col = columns.find((c) => c.id === 'target')
    expect(col).toBeDefined()
    const cell = col?.cell as (ctx: { row: { original: AdminTotalsResponse['rows'][number] } }) => unknown
    const rendered = cell({ row: { original: row('mcp/fetch/alice', 3) } })
    expect(isVNode(rendered) && rendered.children).toBe('mcp/fetch')
  })

  it('caller column reads the parsed caller name, falling back to the raw id', () => {
    const columns = targetCallerColumns(false)
    const col = columns.find((c) => c.id === 'caller') as unknown as {
      accessorFn: (r: AdminTotalsResponse['rows'][number], i: number) => unknown
    }
    const accessor = col.accessorFn
    expect(accessor(row('mcp/fetch/alice', 3), 0)).toBe('alice')
    expect(accessor(row('malformed', 3), 0)).toBe('malformed')
  })

  it('requests column renders a CompactNumber reading values.req, defaulting to 0', () => {
    const columns = targetCallerColumns(false)
    const col = columns.find((c) => c.id === 'requests')
    const cell = col?.cell as (ctx: { row: { original: AdminTotalsResponse['rows'][number] } }) => unknown
    const rendered = cell({ row: { original: { id: 'mcp/fetch/alice', values: {} } } })
    expect(isVNode(rendered) && rendered.type).toBe(CompactNumber)
    expect(isVNode(rendered) && rendered.props?.value).toBe(0)
  })
})

// --- resolveTargetRef (P2 item: a hand-typed #targets?target=demo-mcp
// 400ed — parseTargetRef, stats_read.go, only accepts "mcp/{name}"/
// "agent/{name}") ---

function namedTarget(name: string): AdminTargetView {
  return {
    name,
    url: 'https://example.com/mcp',
    counters: { requestsPerMinute: 0, requestsPerDay: 0, requestsPerMonth: 0 },
    health: { state: 'unknown', consecutiveFailures: 0 },
  }
}

function targets(mcpNames: string[], agentNames: string[]): Pick<AdminTargetsResponse, 'mcpServers' | 'agents'> {
  return { mcpServers: mcpNames.map(namedTarget), agents: agentNames.map(namedTarget) }
}

describe('resolveTargetRef', () => {
  it('passes an already-prefixed "mcp/{name}" ref through unchanged, without needing targets loaded', () => {
    expect(resolveTargetRef('mcp/fetch', null)).toEqual({ kind: 'ref', ref: 'mcp/fetch', name: 'fetch' })
  })

  it('passes an already-prefixed "agent/{name}" ref through unchanged', () => {
    expect(resolveTargetRef('agent/agentkit', targets([], ['agentkit']))).toEqual({
      kind: 'ref',
      ref: 'agent/agentkit',
      name: 'agentkit',
    })
  })

  it('round-trips a name that itself contains a slash, matching stats_read.go\'s Cut-on-first-slash', () => {
    expect(resolveTargetRef('mcp/team/fetch', null)).toEqual({ kind: 'ref', ref: 'mcp/team/fetch', name: 'team/fetch' })
  })

  it('is "pending" for a bare name while dashboard.targets has not loaded yet (null)', () => {
    expect(resolveTargetRef('demo-mcp', null)).toEqual({ kind: 'pending' })
  })

  it('resolves a bare name found only in mcpServers', () => {
    expect(resolveTargetRef('demo-mcp', targets(['demo-mcp'], []))).toEqual({
      kind: 'ref',
      ref: 'mcp/demo-mcp',
      name: 'demo-mcp',
    })
  })

  it('resolves a bare name found only in agents', () => {
    expect(resolveTargetRef('agentkit', targets([], ['agentkit']))).toEqual({
      kind: 'ref',
      ref: 'agent/agentkit',
      name: 'agentkit',
    })
  })

  it('shows a notice for a bare name found in neither list (unknown)', () => {
    const result = resolveTargetRef('nope', targets(['demo-mcp'], ['agentkit']))
    expect(result.kind).toBe('notice')
    expect(result.kind === 'notice' && result.message).toContain('No MCP server or agent named "nope"')
  })

  it('shows a notice for a bare name found in BOTH lists (ambiguous) rather than guessing', () => {
    const result = resolveTargetRef('shared', targets(['shared'], ['shared']))
    expect(result.kind).toBe('notice')
    expect(result.kind === 'notice' && result.message).toContain('matches both an MCP server and an agent')
  })

  it('rejects an unknown prefix as a bare name, not a ref (e.g. "http/foo" is looked up, not passed through)', () => {
    expect(resolveTargetRef('http/foo', targets([], []))).toEqual({
      kind: 'notice',
      message: 'No MCP server or agent named "http/foo" is configured.',
    })
  })
})
