# Admin panel (webui)

Source for the traefik-llmgateway read-only admin dashboard: Vue 3 +
TypeScript + Pinia + Tailwind v4 + shadcn-vue + Chart.js, built with Vite.

See the repo root [README.md's Admin section](../README.md#admin) for what
the dashboard shows and how its API/auth works, and the
[Development section](../README.md#development) for `make admin-ui` — the
only way this directory's output (`../admin_assets_gen.go`) is meant to be
regenerated. Nothing here runs at request time: the plugin serves the
built, baked-in output, never `webui/` itself.

## Warning: `shadcn-vue add`/`init` regressions

`components.json`'s `iconLibrary` and `font` fields are locked to
`"lucide"` and `"inter"` — **not this project's real choices** (this
project uses FontAwesome only, per `vue-web-development` house skill, and
no external font at all). The shadcn-vue CLI's schema does not accept
`"none"`/`"fontawesome"`/`"system"` for either field (checked directly:
`node_modules/shadcn-vue/dist/preset-*.js`'s `PRESET_ICON_LIBRARIES` and
`PRESET_FONTS` arrays — five fixed choices each, no escape hatch), and
JSON supports no comment syntax to warn inline (the repo's own
pre-commit JSON validator rejects JSONC comments — see `tsconfig.*.json`
history). This warning is the only place the trap could be recorded.

Both `npx shadcn-vue init` and `npx shadcn-vue add <component>` read
these fields and **silently rewrite `src/assets/main.css`** — hit twice
in this project's own history: `init` first, then a follow-up `add`
call, both re-added a `@import url('https://fonts.googleapis.com/...')`
line and switched the dark-mode `@custom-variant` back to a `.dark`-class
toggle. If you ever run either command again:

1. **Diff `src/assets/main.css` before committing.** Remove any
   re-added `@import url('https://fonts.googleapis...')` line (external
   font, blocked by the admin CSP's `style-src 'self'` anyway) and
   restore the `@custom-variant dark` removal (see the file's own header
   comment for why: `prefers-color-scheme`, not a class toggle).
2. **Check the new component for a `lucide-vue-next`/`@lucide/vue`
   import** and swap it for the FontAwesome equivalent (see
   `src/components/ui/select/Select*.vue` for the pattern already
   applied there).

## Local development

```sh
npm ci
npm run dev      # Vite dev server — /admin/api/* calls need a real
                  # gateway to fetch from; point one at the same origin or
                  # accept the "refresh failed" state while iterating on
                  # layout/styling alone.
npm run build     # what `make admin-ui` runs, plus webui/generate.mjs
```

## Structure

- `src/stores/` — Pinia: `auth` (the sessionStorage key gate), `dashboard`
  (Overview/Usage, 5s poll), `history` (Charts, fetch-on-selection + 30s
  refresh).
- `src/components/` — `AuthGate`, `OverviewView`, `UsageView`,
  `ChartsView`/`UsageChart`, `UsageTable`; `ui/` holds the shadcn-vue
  primitives this panel actually uses (Card, Table, Tabs, Button, Input,
  Alert, Select, Badge) — nothing installed and unused.
- `src/lib/` — `api.ts` (the `/admin/api/*` fetch wrapper), `format.ts`,
  `key-storage.ts`, `chart-setup.ts`.
- `src/types/api.ts` — TypeScript mirrors of admin.go's JSON response
  shapes (`adminOverviewResponse`, `adminUsageResponse`,
  `usageHistoryResponse`).
- `generate.mjs` — reads `dist/` after `vite build` and writes
  `../admin_assets_gen.go`; see its own header comment for why every
  asset is base64-encoded rather than embedded as a raw string literal.
