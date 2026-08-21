import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
// vitest/config's defineConfig is vite's own, augmented with the `test`
// field's types — importing from here (not 'vite') is what lets the
// `test` block below type-check under vue-tsc -b without a separate
// vitest.config.ts or a triple-slash type reference.
import { defineConfig } from 'vitest/config'

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
  test: {
    // Deliberately narrow: lib/'s pure modules (search-expand.ts,
    // usage-search.ts) plus, since review round 2 (v0.21), stores/
    // dashboard.ts's own refresh() action — a Pinia store is plain
    // reactive state and actions, no DOM or component mount involved, so
    // it fits this same environment. Still no snapshot/DOM/component
    // (.vue mount) testing. 'node' is enough since nothing under test
    // touches a real browser global — dashboard.spec.ts mocks lib/api.ts
    // entirely, so even sessionStorage access (auth.ts) never actually
    // runs; jsdom is not pulled in as a dependency for modules that don't
    // need it.
    environment: 'node',
    include: ['src/**/*.spec.ts'],
  },
})
