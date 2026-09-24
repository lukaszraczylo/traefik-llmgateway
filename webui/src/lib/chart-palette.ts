// reuse-audit.md F13: the ONE chart colour palette module. Two call sites
// used to each own a palette + its own cycling function — stores/
// reliability.ts's RELIABILITY_SERIES_COLORS (a UI concern that had drifted
// into a Pinia store) and pages/SpendPage.vue's BREAKDOWN_PALETTE (the
// identical first four colours, plus four more) — this module is their one
// shared home.

/**
 * CHART_SERIES_COLORS cycles through main.css's four existing --chart-*
 * tokens (assets/main.css is owned by WP-E — this module does not add a
 * fifth) — used to colour the Reliability page's per-provider error-rate/
 * timeout/failover overlay charts. With more than four configured
 * providers, colors repeat — a known, documented limitation rather than a
 * silent one.
 */
export const CHART_SERIES_COLORS = ['--chart-requests', '--chart-tokens-in', '--chart-tokens-out', '--chart-cost'] as const

/**
 * BREAKDOWN_PALETTE cycles a colorblind-safe (Okabe & Ito, 2008) 8-color
 * qualitative set across the Spend page's breakdown chart's per-model/
 * provider/group series. The first four reuse CHART_SERIES_COLORS (already
 * that same palette's requests/tokens-in/tokens-out/cost members); the
 * remaining four (--chart-series-a..d) extend it, defined alongside them in
 * assets/main.css — main.css's original four tokens were sized for four
 * FIXED semantic metrics, not an open-ended breakdown series count, so this
 * page needed four more, but every chart color token lives in the one
 * design-tokens file (Tailwind-only rule), never a component-local <style>
 * block. Beyond 8 series (e.g. a fleet with more than 8 configured groups)
 * colors repeat — an accepted qualitative-palette limit, not a bug: legend
 * labels remain the disambiguator past that point, the same tradeoff any
 * categorical chart palette makes once it runs out of distinguishable hues.
 */
export const BREAKDOWN_PALETTE = [
  ...CHART_SERIES_COLORS,
  '--chart-series-a',
  '--chart-series-b',
  '--chart-series-c',
  '--chart-series-d',
] as const

/**
 * seriesColor picks one entry from `palette` by index, cycling — defaults
 * to CHART_SERIES_COLORS (the Reliability page's 4-color set); pass
 * BREAKDOWN_PALETTE for the Spend page's 8-color breakdown chart, or any
 * other palette a future chart needs.
 */
export function seriesColor(index: number, palette: readonly string[] = CHART_SERIES_COLORS): string {
  return palette[index % palette.length]!
}
