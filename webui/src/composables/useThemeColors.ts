import { onBeforeUnmount, onMounted, ref } from 'vue'

/**
 * useThemeColors exposes the current computed values of the design
 * tokens Chart.js needs to draw axis/grid/legend text (main.css) — Chart.js
 * paints to a <canvas>, so it cannot pick up CSS variables the way a DOM
 * element does; the caller must read them and hand them to Chart.js as
 * plain color strings. Every color literal in main.css is oklch()/hex,
 * both valid canvas fillStyle values in current evergreen browsers (the
 * only ones this admin panel targets — see vite.config.ts's
 * modulePreload note for the same reasoning), so no conversion is needed.
 *
 * Re-reads on a `prefers-color-scheme` change so an open Charts view
 * repaints if the OS theme flips while it's on screen — main.css's own
 * dark-mode block is a plain media query, not a class toggle, so this is
 * the only way a chart already on screen learns about it.
 */
export function useThemeColors() {
  const foreground = ref('')
  const mutedForeground = ref('')
  const border = ref('')

  function sync(): void {
    const style = getComputedStyle(document.documentElement)
    foreground.value = style.getPropertyValue('--foreground').trim()
    mutedForeground.value = style.getPropertyValue('--muted-foreground').trim()
    border.value = style.getPropertyValue('--border').trim()
  }

  let media: MediaQueryList | undefined
  onMounted(() => {
    sync()
    media = window.matchMedia('(prefers-color-scheme: dark)')
    media.addEventListener('change', sync)
  })
  onBeforeUnmount(() => {
    media?.removeEventListener('change', sync)
  })

  return { foreground, mutedForeground, border }
}
