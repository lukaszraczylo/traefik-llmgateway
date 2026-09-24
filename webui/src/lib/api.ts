import { useAuthStore } from '@/stores/auth'

/** AdminApiError carries the HTTP status alongside the message, so callers
 * can tell an auth failure (already handled by adminFetch itself) apart
 * from any other fetch failure without re-parsing the message string. */
export class AdminApiError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'AdminApiError'
    this.status = status
  }
}

/**
 * serverErrorMessage reads the server's own error envelope off a failed
 * response (errors.go's writeOAIError: {"error":{"message","type","code"}}),
 * so a 503 "usage history store unavailable" or a 404 "unknown user id"
 * reaches the UI instead of a bare "/admin/api/...: HTTP 503". Falls back to
 * that plain path/status text whenever the body is not that shape — an
 * empty body, a proxy error page, or any other non-JSON response — so a
 * malformed error body can never itself throw out of adminFetch.
 */
async function serverErrorMessage(resp: Response, path: string): Promise<string> {
  const fallback = `${path}: HTTP ${resp.status}`
  try {
    const body = (await resp.json()) as { error?: { message?: string } }
    return body.error?.message || fallback
  } catch {
    return fallback
  }
}

/**
 * adminFetch issues a GET against one of the three /admin/api/* routes with
 * the stored admin key attached as `x-api-key`, and decodes the JSON body.
 *
 * A 401/403 response rejects the stored key (authStore.reject) and throws
 * an AdminApiError — callers that only care about real failures (a stopped
 * poll timer should not spam the status line with "unauthorized" on every
 * tick once the gate is already showing) can catch and check `status`.
 */
export async function adminFetch<T>(path: string): Promise<T> {
  const auth = useAuthStore()
  const key = auth.apiKey
  const headers: HeadersInit = key ? { 'x-api-key': key } : {}

  const resp = await fetch(path, { credentials: 'same-origin', headers })

  if (!resp.ok) {
    const message = await serverErrorMessage(resp, path)
    if (resp.status === 401 || resp.status === 403) {
      // Late-401 guard: only reject the key that is STILL the one stored —
      // see authStore.reject's own doc comment for why.
      if (auth.apiKey === key) {
        auth.reject('invalid key, or not an admin')
      }
    }
    throw new AdminApiError(message, resp.status)
  }
  return (await resp.json()) as T
}

/**
 * messageOf (reuse-audit.md F4 step 1) extracts a human-readable message
 * from a caught value of unknown shape: an Error's own `.message`, or the
 * stringified value otherwise (a thrown string, object, etc.). The ONE
 * shared implementation — every Pinia store's fetch action and every
 * component with a local latest-request-wins fetch used to hand-roll this
 * same ternary in its `catch` block.
 */
export function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

/**
 * isAuthRejection (reuse-audit.md F4 step 1) reports whether a caught
 * value is an AdminApiError for a 401/403 — i.e. adminFetch has already
 * rejected the stored key (authStore.reject) for this failure, so the
 * caller's own `catch` block should return early instead of also setting
 * its local `error` state with the same rejection.
 */
export function isAuthRejection(err: unknown): boolean {
  return err instanceof AdminApiError && (err.status === 401 || err.status === 403)
}
