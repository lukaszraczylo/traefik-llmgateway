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

`components.json` has no `iconLibrary` or `font` key. **Do not add them
back.** Their omission is the fix, not an oversight — read on before you
"helpfully" restore either one.

Earlier revisions of this file set `"iconLibrary": "lucide"` and
`"font": "inter"`, both wrong for this project (FontAwesome only, per
the `vue-web-development` house skill; no external font at all). Both
`npx shadcn-vue init` and `npx shadcn-vue add <component>` read those two
fields and, when set, rewrite `src/assets/main.css` — hit twice in this
project's own history, `init` then a later `add`, both re-adding a
`@import url('https://fonts.googleapis.com/...')` line and switching the
dark-mode `@custom-variant` back to a `.dark`-class toggle.

The actual shadcn-vue config schema
(`node_modules/shadcn-vue/dist/schema/index.js`'s `rawConfigSchema`)
declares both fields as `z.string().optional()` — not an enum, contrary
to an earlier, incorrect claim in this project's own review notes that
the CLI schema forced one of five fixed values. The CLI's `init --font`/
`--icon-library` flags do offer five fixed choices each, but that is a
flag-parsing constraint, not a `components.json` schema constraint.
Deleting both keys leaves them `undefined` after parse, a valid state:
verified empirically, running `npx shadcn-vue add badge -o -y` against a
`components.json` with both keys removed left `src/assets/main.css`
byte-for-byte unchanged.

That verification also surfaced a narrower, remaining trap: the same run
still added `@lucide/vue` back to `package.json`'s dependencies (Badge
itself does not use it — some components' registry entries list an icon
package regardless of `iconLibrary`). So after any future `shadcn-vue
add`/`init` call:

1. **Check `git diff webui/package.json`** for a re-added
   `lucide-vue-next`/`@lucide/vue` dependency, and remove it
   (`npm uninstall`) if nothing in `src/` imports from it.
2. **Check the new component's own file** for a
   `lucide-vue-next`/`@lucide/vue` import and swap it for the
   FontAwesome equivalent (see `src/components/ui/select/Select*.vue`
   for the pattern already applied there).
3. **Diff `src/assets/main.css` anyway** before committing, in case a
   future shadcn-vue release starts reading a different config key for
   the same font-injection step.

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
  `ChartsView`/`UsageChart`, `UsageTable`, `DataTable` (the shared
  sortable-table wrapper — shadcn-vue's DataTable pattern, `@tanstack/
  vue-table`, generic over row type); `ui/` holds the shadcn-vue
  primitives this panel actually uses (Accordion, Card, Table, Tabs,
  Button, Input, Alert, Select, Badge) — nothing installed and unused.
  Providers (Overview) and Groups (Usage) render as a real Accordion —
  expanding a group shows its member users' own usage rows, a
  client-side join on `AdminUsageEntryView.groupName`.
- `src/lib/` — `api.ts` (the `/admin/api/*` fetch wrapper), `format.ts`,
  `key-storage.ts`, `chart-setup.ts`, `provider-expand.ts` (search-aware
  expand state, adapted to drive Accordion's v-model), `usage-columns.ts`
  (shared `ColumnDef`s for every usage table).
- `src/types/api.ts` — TypeScript mirrors of admin.go's JSON response
  shapes (`adminOverviewResponse`, `adminUsageResponse`,
  `usageHistoryResponse`).
- `generate.mjs` — reads `dist/` after `vite build` and writes
  `../admin_assets_gen.go`; see its own header comment for why every
  asset is base64-encoded rather than embedded as a raw string literal.
