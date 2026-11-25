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
 * Re-reads when the `class` of <html> changes (perfect-design's theme
 * store toggles `.dark` there, including on an OS flip in "system" mode),
 * so an open chart repaints on a theme switch.
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

  let observer: MutationObserver | undefined
  onMounted(() => {
    sync()
    observer = new MutationObserver(sync)
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
  })
  onBeforeUnmount(() => {
    observer?.disconnect()
  })

  return { foreground, mutedForeground, border }
}
