# Admin panel (webui)

Source for the traefik-llmgateway read-only admin dashboard: Vue 3 +
TypeScript + Pinia + Tailwind v4 + shadcn-vue + Chart.js, built with Vite.

See the repo root [README.md's Admin section](../README.md#admin) for what
the dashboard shows and how its API/auth works, and the
[Development section](../README.md#development) for `make admin-ui` — the
only way this directory's output (`../admin_assets_gen.go`) is meant to be
regenerated. Nothing here runs at request time: the plugin serves the
built, baked-in output, never `webui/` itself.

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
