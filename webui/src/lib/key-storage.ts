// sessionStorage helpers for the admin API key (auth.go / admin.go's
// browser key-entry flow, ported from the replaced vanilla-JS admin page):
// the key lives only in this tab's sessionStorage — never a cookie, never
// localStorage — so it disappears when the tab closes and is sent only to
// this page's own /admin/api/* fetches. Every call is wrapped: a browser
// with storage disabled (private mode, some embedded webviews) must degrade
// to "key not remembered across reload", never throw.

const KEY_STORAGE = 'llmgwAdminKey'

export function getStoredKey(): string {
  try {
    return sessionStorage.getItem(KEY_STORAGE) ?? ''
  } catch {
    return ''
  }
}

export function setStoredKey(key: string): void {
  try {
    sessionStorage.setItem(KEY_STORAGE, key)
  } catch {
    // storage unavailable — the key still lives in the Pinia store for the
    // rest of this page load, only cross-reload persistence is lost.
  }
}

export function clearStoredKey(): void {
  try {
    sessionStorage.removeItem(KEY_STORAGE)
  } catch {
    // storage unavailable, nothing to clear
  }
}
