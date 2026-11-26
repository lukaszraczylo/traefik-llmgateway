# Admin panel (webui)

Source for the traefik-llmgateway read-only admin dashboard: Vue 3 +
TypeScript + Pinia + Tailwind v4 + `@perfect-design/ui`, built with Vite.

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

- `src/pages/` and `src/panels/` — one page per nav entry and the detail panels they open.
- `src/widgets/`, `src/components/`, `src/layout/` — shared presentation built on `@perfect-design/ui`.
- `src/stores/` — Pinia stores (auth, dashboard, toasts, ...).
- `src/lib/` — pure helpers with specs; `api.ts` wraps `/admin/api/*`.
- `src/types/api.ts` — TypeScript mirrors of admin.go's JSON response shapes.
- `mock/` — the dev-only mock admin API behind `npm run dev:mock`.
- `scripts/bundle-size.mjs` — the bundle gate: gzip JS+CSS of `dist/assets` must stay at or below its `LIMIT`.
- `generate.mjs` — reads `dist/` after `vite build` and writes `../admin_assets_gen.go`; see its header comment for why every asset is base64-encoded.

`src/assets/main.css` lists the `@perfect-design/ui` component directories Tailwind scans; add a directory there when you import a new component (`src/lib/tailwind-sources.spec.ts` fails otherwise).

## Visual harness

`npm run test:visual` starts the mock dev server on `PW_PORT` (default 5199)
and fails fast if that port is taken, so two runs cannot share one server or
the `.screenshots/` output. Set `PW_REUSE=1` to attach to a server you
already started.
