/**
 * copyText writes text to the system clipboard via the Clipboard API
 * (navigator.clipboard.writeText), available only in secure contexts
 * (https, or localhost) with a user gesture — both satisfied by a click
 * handler on an https-served admin panel. When the API is unavailable or
 * the write itself rejects (permission denied, insecure context), falls
 * back to SELECTING sourceEl's text content so the user can copy it
 * manually (Ctrl/Cmd+C) — not `document.execCommand('copy')`, which is
 * deprecated and needs the exact same secure-context/permission
 * conditions that already ruled out the Clipboard API, so it would add
 * complexity without adding a real fallback path.
 *
 * Returns which outcome happened, so a caller can show the right
 * affordance ("Copied" vs "Selected — press ⌘/Ctrl+C").
 */
export async function copyText(text: string, sourceEl?: HTMLElement): Promise<'copied' | 'selected' | 'failed'> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return 'copied'
    } catch {
      // Fall through to the selection fallback below.
    }
  }
  if (!sourceEl) return 'failed'
  const range = document.createRange()
  range.selectNodeContents(sourceEl)
  const selection = window.getSelection()
  if (!selection) return 'failed'
  selection.removeAllRanges()
  selection.addRange(range)
  return 'selected'
}
