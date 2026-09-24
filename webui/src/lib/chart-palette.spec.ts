import { describe, expect, it } from 'vitest'

import { BREAKDOWN_PALETTE, CHART_SERIES_COLORS, seriesColor } from './chart-palette'

describe('BREAKDOWN_PALETTE', () => {
  it('starts with the same four colors as CHART_SERIES_COLORS, in the same order', () => {
    expect(BREAKDOWN_PALETTE.slice(0, 4)).toEqual(CHART_SERIES_COLORS)
  })

  it('has 8 entries total', () => {
    expect(BREAKDOWN_PALETTE).toHaveLength(8)
  })
})

describe('seriesColor', () => {
  it('cycles through the default palette (CHART_SERIES_COLORS) by index', () => {
    expect(seriesColor(0)).toBe(CHART_SERIES_COLORS[0])
    expect(seriesColor(CHART_SERIES_COLORS.length)).toBe(CHART_SERIES_COLORS[0])
    expect(seriesColor(CHART_SERIES_COLORS.length + 1)).toBe(CHART_SERIES_COLORS[1])
  })

  it('cycles through an explicitly passed palette (e.g. BREAKDOWN_PALETTE) instead', () => {
    expect(seriesColor(0, BREAKDOWN_PALETTE)).toBe(BREAKDOWN_PALETTE[0])
    expect(seriesColor(7, BREAKDOWN_PALETTE)).toBe(BREAKDOWN_PALETTE[7])
    expect(seriesColor(8, BREAKDOWN_PALETTE)).toBe(BREAKDOWN_PALETTE[0])
  })
})
