import { copyText } from '@/lib/clipboard'

/** CSV_MIME_TYPE is the Blob type every export in this panel downloads as — text/csv with an explicit UTF-8 charset, so a spreadsheet importer opening the file directly (not pasting it) reads non-ASCII bytes correctly. */
export const CSV_MIME_TYPE = 'text/csv;charset=utf-8;'

/** The outcome downloadText actually achieved, so a caller (CsvExportButton.vue) can show the right feedback: a real file save, or the clipboard/selection fallback (copyText's own two success modes), or neither. */
export type DownloadOutcome = 'downloaded' | 'copied' | 'selected' | 'failed'

/**
 * REVOKE_DELAY_MS (P5 review fix) defers URL.revokeObjectURL past the
 * click() call below — revoking SYNCHRONOUSLY, right after click(), can
 * cancel the download in some WebKit and older Firefox builds: click()
 * only STARTS the download, it does not wait for the browser to finish
 * reading the blob, so revoking on the very next line raced the read. A
 * full second is generous rather than the minimal `setTimeout(fn, 0)`
 * (which only guarantees one macrotask, not "download started") — the
 * blob itself is only released a little later either way, and this is a
 * one-off per export click, never a hot path.
 */
const REVOKE_DELAY_MS = 1000

/**
 * downloadText saves `content` as a local file named `filename` (Q8,
 * coordinator decision: Blob + `<a download>`, the standard no-server-
 * round-trip browser download idiom — no File System Access API, which
 * is Chromium-only and needs a user-activation prompt this panel's CSP
 * would complicate for no benefit here).
 *
 * Guarded for a non-DOM environment (this module is never unit-tested —
 * see WP-B1's own test plan, lib/download.ts has no .spec.ts — but a
 * defensive `typeof document` check keeps it from throwing if it is ever
 * imported somewhere `document` is not global, matching
 * composables/useHashState.ts's own `hasWindow` convention). The
 * clipboard fallback (copyText, Q8) runs
 * ONLY when the Blob/URL/anchor machinery itself is unavailable or throws
 * (a Blob/URL API genuinely unsupported, or the synthetic click rejected)
 * — never merely because a download was silently blocked by the browser,
 * which raises no exception here to catch. Returns the actual outcome
 * (never discarded — CsvExportButton.vue renders it as brief feedback) so
 * a caller can tell "downloaded" from the fallback's own "copied"/
 * "selected"/"failed".
 */
export async function downloadText(filename: string, content: string, mimeType = CSV_MIME_TYPE): Promise<DownloadOutcome> {
  if (typeof document === 'undefined') return 'failed'
  try {
    const blob = new Blob([content], { type: mimeType })
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = filename
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
    setTimeout(() => URL.revokeObjectURL(url), REVOKE_DELAY_MS)
    return 'downloaded'
  } catch {
    return copyText(content)
  }
}
