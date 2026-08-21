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

  if (resp.status === 401 || resp.status === 403) {
    // Late-401 guard: only reject the key that is STILL the one stored —
    // see authStore.reject's own doc comment for why.
    if (auth.apiKey === key) {
      auth.reject('invalid key, or not an admin')
    }
    throw new AdminApiError(`${path}: HTTP ${resp.status}`, resp.status)
  }
  if (!resp.ok) {
    throw new AdminApiError(`${path}: HTTP ${resp.status}`, resp.status)
  }
  return (await resp.json()) as T
}
