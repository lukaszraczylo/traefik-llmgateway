import { defineStore } from 'pinia'

import { clearStoredKey, getStoredKey, setStoredKey } from '@/lib/key-storage'

/**
 * useAuthStore owns the admin API key gate: sessionStorage-backed key
 * entry, sent as the `x-api-key` header on every /admin/api/* fetch
 * (lib/api.ts). GET /admin itself needs no auth (admin.go serves the shell
 * unauthenticated) — this store only gates the JSON routes.
 */
export const useAuthStore = defineStore('auth', {
  state: () => ({
    apiKey: getStoredKey(),
    /** Set after a 401/403 (lib/api.ts) or on a still-empty submit. */
    error: '',
  }),
  getters: {
    isAuthenticated: (state): boolean => state.apiKey !== '',
  },
  actions: {
    /** submit stores a newly entered key and clears any prior error. */
    submit(key: string): void {
      if (!key) return
      this.apiKey = key
      this.error = ''
      setStoredKey(key)
    },
    /**
     * reject clears the key and shows message — called by lib/api.ts on a
     * 401/403. Guards against the late-401 race the vanilla-JS page this
     * replaces also guarded against: adminFetch only calls reject() when
     * the failing request's own key still matches what is currently
     * stored, so a stale in-flight request for a key the user has since
     * replaced can never clobber the newer, possibly-valid key.
     */
    reject(message: string): void {
      this.apiKey = ''
      this.error = message
      clearStoredKey()
    },
  },
})
