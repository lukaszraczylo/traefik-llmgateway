import { CategoryScale, Chart, Filler, Legend, LinearScale, LineController, LineElement, PointElement, Tooltip } from 'chart.js'

// Registered once at module load (ES module caching makes this a true
// singleton regardless of how many components import it) — only the
// chart pieces this panel actually uses, not chart.js's full
// auto-registry, to keep the baked-in bundle lean. TimeSeriesChart.vue
// (redesign-plan.md section 3.4) is the only chart component in this
// panel, and it only ever imports `Line` from vue-chartjs (never `Bar`) —
// so this registers exactly Line's own pieces, plus `Filler`:
// TimeSeriesChart.vue sets `fill: props.stacked` on every stacked
// dataset, and without Filler registered chart.js silently ignores that
// option — a stacked area chart drew UNFILLED lines at cumulative
// heights, so a reader saw the top line at the height of the whole total
// rather than its own value (P2). A `BarController`/`BarElement`
// registration used to sit here too; it was dead — grep confirms no
// component imports vue-chartjs's `Bar`.
Chart.register(CategoryScale, LinearScale, Legend, Tooltip, LineController, LineElement, PointElement, Filler)

