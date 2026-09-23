// api.spec.ts covers adminFetch's own error handling (lib/api.ts) — unlike
// history.spec.ts/dashboard.spec.ts, which mock this module entirely to test
// their OWN callers, this file mocks the global `fetch` instead so
// adminFetch's real body runs. Same 'node' vitest environment as those
// files; useAuthStore's sessionStorage access degrades harmlessly (see
// key-storage.ts's own try/catch doc comment) with no DOM present.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { AdminApiError, adminFetch } from './api'
import { useAuthStore } from '@/stores/auth'

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as Response
}

describe('adminFetch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    useAuthStore().submit('test-admin-key')
    vi.stubGlobal('fetch', vi.fn())
  })

  it('resolves the decoded JSON body on a 2xx response', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, { hello: 'world' }))
    await expect(adminFetch('/admin/api/overview')).resolves.toEqual({ hello: 'world' })
  })

  // Review finding: a failed response's own error envelope (errors.go's
  // writeOAIError: {"error":{"message","type","code"}}) was previously
  // discarded entirely — every failure surfaced as a bare
  // "/admin/api/...: HTTP 503", regardless of what the server actually said
  // went wrong.
  it('surfaces the server error envelope\'s message instead of a bare "HTTP <status>"', async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse(503, { error: { message: 'usage history store unavailable', type: 'server_error', code: '503' } }),
    )
    await expect(adminFetch('/admin/api/usage/history')).rejects.toMatchObject({
      message: 'usage history store unavailable',
      status: 503,
    })
  })

  it('falls back to the path/status text when the failed body is not the expected envelope shape', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(500, { unexpected: 'shape' }))
    await expect(adminFetch('/admin/api/overview')).rejects.toMatchObject({
      message: '/admin/api/overview: HTTP 500',
      status: 500,
    })
  })

  it('falls back to the path/status text when the failed body is not valid JSON at all (a proxy error page)', async () => {
    vi.mocked(fetch).mockResolvedValue({
      ok: false,
      status: 502,
      json: () => Promise.reject(new SyntaxError('Unexpected token < in JSON')),
    } as Response)
    await expect(adminFetch('/admin/api/overview')).rejects.toMatchObject({
      message: '/admin/api/overview: HTTP 502',
      status: 502,
    })
  })

  it('still rejects the stored key on a 401, with the server message carried on the thrown error', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(401, { error: { message: 'invalid key' } }))
    const auth = useAuthStore()

    await expect(adminFetch('/admin/api/overview')).rejects.toBeInstanceOf(AdminApiError)

    expect(auth.isAuthenticated).toBe(false)
    expect(auth.error).toBe('invalid key, or not an admin')
  })
})
