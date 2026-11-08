// api.spec.ts covers adminFetch's own error handling (lib/api.ts) — unlike
// history.spec.ts/dashboard.spec.ts, which mock this module entirely to test
// their OWN callers, this file mocks the global `fetch` instead so
// adminFetch's real body runs. Same 'node' vitest environment as those
// files; useAuthStore's sessionStorage access degrades harmlessly (see
// key-storage.ts's own try/catch doc comment) with no DOM present.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { AdminApiError, adminFetch, isAuthRejection, messageOf } from './api'
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

describe('messageOf', () => {
  it('returns an Error instance\'s own message', () => {
    expect(messageOf(new Error('boom'))).toBe('boom')
  })

  it('stringifies a non-Error thrown value', () => {
    expect(messageOf('plain string')).toBe('plain string')
    expect(messageOf(404)).toBe('404')
    expect(messageOf(null)).toBe('null')
  })
})

describe('isAuthRejection', () => {
  it('is true for an AdminApiError with status 401 or 403', () => {
    expect(isAuthRejection(new AdminApiError('nope', 401))).toBe(true)
    expect(isAuthRejection(new AdminApiError('nope', 403))).toBe(true)
  })

  it('is false for any other AdminApiError status or a non-AdminApiError value', () => {
    expect(isAuthRejection(new AdminApiError('server error', 500))).toBe(false)
    expect(isAuthRejection(new Error('plain'))).toBe(false)
    expect(isAuthRejection('nope')).toBe(false)
  })
})

describe('adminFetch resilience', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    useAuthStore().submit('test-admin-key')
    vi.stubGlobal('fetch', vi.fn())
  })

  it('passes an abort signal so a hung request cannot block polling forever', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, {}))
    await adminFetch('/admin/api/overview')
    const init = vi.mocked(fetch).mock.calls[0][1]
    expect(init?.signal).toBeInstanceOf(AbortSignal)
  })

  it('turns a timeout into an AdminApiError naming the path', async () => {
    vi.mocked(fetch).mockRejectedValue(new DOMException('timed out', 'TimeoutError'))
    await expect(adminFetch('/admin/api/overview')).rejects.toMatchObject({
      name: 'AdminApiError',
      message: expect.stringContaining('/admin/api/overview'),
    })
  })

  it('turns a 200 with a non-JSON body into an AdminApiError, not a raw SyntaxError', async () => {
    vi.mocked(fetch).mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.reject(new SyntaxError('Unexpected token <')),
    } as Response)
    await expect(adminFetch('/admin/api/overview')).rejects.toBeInstanceOf(AdminApiError)
  })

  it('rethrows network errors unchanged', async () => {
    vi.mocked(fetch).mockRejectedValue(new TypeError('Failed to fetch'))
    await expect(adminFetch('/admin/api/overview')).rejects.toThrow('Failed to fetch')
  })
})

describe('adminFetch in-flight dedupe', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    useAuthStore().submit('test-admin-key')
    vi.stubGlobal('fetch', vi.fn())
  })

  it('shares one request between concurrent calls for the same path', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, { n: 1 }))
    const [a, b] = await Promise.all([adminFetch('/admin/api/x'), adminFetch('/admin/api/x')])
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(a).toEqual({ n: 1 })
    expect(b).toEqual({ n: 1 })
  })

  it('does not share between different paths', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, {}))
    await Promise.all([adminFetch('/admin/api/x'), adminFetch('/admin/api/y')])
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('issues a fresh request once the first settles, including after failure', async () => {
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('down')).mockResolvedValue(jsonResponse(200, { ok: true }))
    await expect(adminFetch('/admin/api/x')).rejects.toThrow('down')
    await expect(adminFetch('/admin/api/x')).resolves.toEqual({ ok: true })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('does not share across different admin keys', async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, {}))
    const first = adminFetch('/admin/api/x')
    useAuthStore().submit('other-key')
    const second = adminFetch('/admin/api/x')
    await Promise.all([first, second])
    expect(fetch).toHaveBeenCalledTimes(2)
  })
})
