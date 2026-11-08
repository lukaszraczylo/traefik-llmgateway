import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useConfigStore } from '@/stores/config'
import type { AdminConfigResponse } from '@/types/api'

const mocked = vi.mocked(adminFetch)
const cfg = (tag: string) => ({ tag }) as unknown as AdminConfigResponse

function deferred<T>() {
  let resolve!: (v: T) => void
  const promise = new Promise<T>((r) => (resolve = r))
  return { promise, resolve }
}

describe('useConfigStore.fetchConfig', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocked.mockReset()
    useAuthStore().submit('k')
  })

  it('keeps the newest response when an older request settles last', async () => {
    const older = deferred<AdminConfigResponse>()
    const newer = deferred<AdminConfigResponse>()
    mocked.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise)
    const store = useConfigStore()
    const first = store.fetchConfig()
    const second = store.fetchConfig()
    newer.resolve(cfg('new'))
    await second
    older.resolve(cfg('old'))
    await first
    expect(store.data).toEqual(cfg('new'))
    expect(store.loading).toBe(false)
  })

  it('records the error message on failure', async () => {
    mocked.mockRejectedValue(new Error('boom'))
    const store = useConfigStore()
    await store.fetchConfig()
    expect(store.error).toBe('boom')
    expect(store.loading).toBe(false)
  })
})
