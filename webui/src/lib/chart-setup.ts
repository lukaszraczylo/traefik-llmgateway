import {
  BarController,
  BarElement,
  CategoryScale,
  Chart,
  Legend,
  LinearScale,
  Tooltip,
} from 'chart.js'

// Registered once at module load (ES module caching makes this a true
// singleton regardless of how many components import it) — only the bar
// chart pieces this panel actually uses, not chart.js's full auto-registry,
// to keep the baked-in bundle lean.
Chart.register(BarController, BarElement, CategoryScale, LinearScale, Legend, Tooltip)
