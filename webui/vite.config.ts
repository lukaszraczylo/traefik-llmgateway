import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  // admin.go serves this build's index.html at GET /admin and every hashed
  // asset at GET /admin/assets/{hashedname} — the base must match so the
  // emitted index.html's own <script>/<link> references resolve correctly
  // regardless of what path the browser first loaded the page from.
  base: '/admin/',
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // No inline bootstrap script in the emitted index.html (admin.go's CSP
    // ships script-src 'self' with no 'unsafe-inline' — see the Go-side
    // asset-serving comment for the full rationale): Vite's module-preload
    // polyfill is otherwise inlined as a <script> tag, which that CSP would
    // block. Disabling it leaves only <link rel="modulepreload"> hints,
    // which need no script-src allowance at all. The admin panel only ever
    // targets current evergreen browsers (native ES module support), so the
    // polyfill has nothing to do here anyway.
    modulePreload: { polyfill: false },
  },
})
