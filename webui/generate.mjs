#!/usr/bin/env node
// generate.mjs reads webui/dist/ (the output of `vite build`) and writes
// ../admin_assets_gen.go — the baked-in static assets GET /admin and
// GET /admin/assets/{hashedname} serve (admin.go's serveAdminPage /
// serveAdminAsset). Run via `make admin-ui`, never by hand against a stale
// dist/.
//
// Encoding choice (documented once, here, rather than re-argued at every
// call site): every asset body is emitted as a base64 string literal,
// decoded once via base64.StdEncoding at package-var initialization
// (encoding/base64 — stdlib, Yaegi-safe). This was picked over emitting
// each asset as a Go raw-string (backtick) literal because a Vite
// production build's minified JS routinely CONTAINS backtick characters
// (template literals) and other bytes (control characters in source maps,
// non-ASCII in string/comment content) that would need per-byte escaping
// to embed safely as Go source text. Base64's alphabet is a fixed,
// printable 65-character set — every input byte round-trips through it
// with no escaping logic to get wrong, at the well-known cost of ~33%
// size inflation. At this bundle's size (a few hundred KB), that
// inflation is a non-issue; see the Makefile's admin-ui target for the
// measured before/after byte count this trades against.

import { createHash } from 'node:crypto'
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { dirname, extname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
const DIST_DIR = join(SCRIPT_DIR, 'dist')
const OUT_PATH = join(SCRIPT_DIR, '..', 'admin_assets_gen.go')
const ASSETS_SUBDIR = 'assets'

/** Content-Type per file extension — every type webui/'s build actually emits, plus common source-map/font fallbacks for a future dependency change. */
const CONTENT_TYPES = {
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.map': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
}

function contentTypeFor(filePath) {
  return CONTENT_TYPES[extname(filePath).toLowerCase()] ?? 'application/octet-stream'
}

/** walk lists every regular file under dir, recursively, as paths relative to dir (forward-slash separated). */
function walk(dir, base = dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) {
      out.push(...walk(full, base))
    } else {
      out.push(relative(base, full).split('\\').join('/'))
    }
  }
  return out
}

/**
 * assertNoInlineAssets fails the build if html contains an inline
 * `<script>` (any `<script>` tag with no `src=` attribute) or any
 * `<style>` tag. The admin CSP (admin.go's adminCSP) ships `script-src
 * 'self'` and `style-src 'self'` with no `'unsafe-inline'` — that is only
 * safe because `vite build`'s own output never needs it (an external
 * `<script type="module" src="...">` plus an external `<link
 * rel="stylesheet">`, nothing inline). This check is generate.mjs's own
 * guard against that invariant silently breaking under a future Vite/
 * plugin config change; admin_test.go's TestAdminIndexHTML_NoInlineAssets
 * re-asserts the identical invariant server-side, against the DECODED
 * adminIndexHTML admin.go actually serves — so a corrupted or
 * hand-patched admin_assets_gen.go is still caught even if someone
 * regenerates with a modified copy of this script.
 */
function assertNoInlineAssets(html, sourceLabel) {
  const scriptTagRe = /<script\b([^>]*)>/gi
  let match
  while ((match = scriptTagRe.exec(html)) !== null) {
    if (!/\bsrc\s*=/i.test(match[1])) {
      console.error(
        `generate.mjs: ${sourceLabel} contains an inline <script> (no src= attribute): ${match[0]}\n` +
          `The admin CSP ships script-src 'self' with no 'unsafe-inline' — every script must be external.`,
      )
      return false
    }
  }
  if (/<style\b/i.test(html)) {
    console.error(
      `generate.mjs: ${sourceLabel} contains a <style> tag.\n` +
        `The admin CSP ships style-src 'self' with no 'unsafe-inline' — every style must be an external <link rel="stylesheet">.`,
    )
    return false
  }
  return true
}

/** goStringLiteral renders a Go double-quoted string literal for s, escaping the handful of characters that require it. base64 output never needs this (its alphabet is a strict subset of what a Go string literal permits unescaped), but this stays generic rather than assuming that forever. */
function goStringLiteral(s) {
  return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`
}

/**
 * goBase64Expr renders base64Body (a base64 string) as a Go expression
 * calling decodeAdminAsset, split into LINE_WIDTH-character chunks joined
 * with `+` — a single 400KB-wide line is valid Go but painful for humans
 * and diff tools alike; chunking keeps every line reviewable.
 */
const LINE_WIDTH = 120
function goBase64Expr(base64Body) {
  if (base64Body.length === 0) return 'decodeAdminAsset("")'
  const chunks = []
  for (let i = 0; i < base64Body.length; i += LINE_WIDTH) {
    chunks.push(goStringLiteral(base64Body.slice(i, i + LINE_WIDTH)))
  }
  return `decodeAdminAsset(\n\t\t${chunks.join(' +\n\t\t')},\n\t)`
}

function main() {
  let distFiles
  try {
    distFiles = walk(DIST_DIR)
  } catch (err) {
    console.error(`generate.mjs: cannot read ${DIST_DIR} — run \`vite build\` first: ${err.message}`)
    process.exitCode = 1
    return
  }
  if (distFiles.length === 0) {
    console.error(`generate.mjs: ${DIST_DIR} is empty — run \`vite build\` first`)
    process.exitCode = 1
    return
  }

  const indexRel = 'index.html'
  if (!distFiles.includes(indexRel)) {
    console.error(`generate.mjs: ${DIST_DIR} has no top-level index.html`)
    process.exitCode = 1
    return
  }

  const assetEntries = []
  for (const rel of distFiles) {
    if (rel === indexRel) continue
    if (!rel.startsWith(`${ASSETS_SUBDIR}/`)) {
      // admin.go's serveAdminAsset only ever looks a filename up under
      // /admin/assets/ — a build output file living anywhere else (e.g. a
      // future public/ static file copied to dist/'s top level) would
      // silently 404 forever if this generator didn't fail on it first.
      console.error(`generate.mjs: unexpected dist/ file outside ${ASSETS_SUBDIR}/: ${rel}`)
      process.exitCode = 1
      return
    }
    const name = rel.slice(ASSETS_SUBDIR.length + 1)
    const body = readFileSync(join(DIST_DIR, rel))
    assetEntries.push({ name, contentType: contentTypeFor(name), base64: body.toString('base64') })
  }
  assetEntries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))

  const indexBody = readFileSync(join(DIST_DIR, indexRel))
  if (!assertNoInlineAssets(indexBody.toString('utf-8'), `${DIST_DIR}/${indexRel}`)) {
    process.exitCode = 1
    return
  }
  const indexBase64 = indexBody.toString('base64')

  const sourceDigest = createHash('sha256')
  for (const f of distFiles.slice().sort()) {
    sourceDigest.update(f)
    sourceDigest.update(readFileSync(join(DIST_DIR, f)))
  }

  const lines = []
  lines.push('// Code generated by webui/generate.mjs; DO NOT EDIT.')
  lines.push('//')
  lines.push('// Source: webui/dist/ (vite build output), digest ' + sourceDigest.digest('hex').slice(0, 16) + '.')
  lines.push('// Regenerate via `make admin-ui` after any webui/ source change.')
  lines.push('//')
  lines.push('// Every asset body below is base64-encoded and decoded once at package-var')
  lines.push('// initialization (encoding/base64, stdlib — Yaegi-safe). See generate.mjs\'s')
  lines.push('// own header comment for why base64 was chosen over a raw-string literal.')
  lines.push('')
  lines.push('package traefikllmgateway')
  lines.push('')
  lines.push('import "encoding/base64"')
  lines.push('')
  lines.push('// adminAsset is one baked static file: its HTTP Content-Type and decoded body.')
  lines.push('type adminAsset struct {')
  lines.push('\tcontentType string')
  lines.push('\tbody        []byte')
  lines.push('}')
  lines.push('')
  lines.push('// decodeAdminAsset base64-decodes one embedded asset body. Every literal')
  lines.push('// passed to it below was produced by this exact encoding (base64.StdEncoding)')
  lines.push('// in webui/generate.mjs, so a decode error here can only mean this generated')
  lines.push('// file is corrupt — a build-time invariant violation, not a reachable runtime')
  lines.push('// condition — so it panics rather than threading an error return through')
  lines.push('// every var initializer below.')
  lines.push('func decodeAdminAsset(encoded string) []byte {')
  lines.push('\tdata, err := base64.StdEncoding.DecodeString(encoded)')
  lines.push('\tif err != nil {')
  lines.push('\t\tpanic("admin_assets_gen.go: corrupt embedded asset: " + err.Error())')
  lines.push('\t}')
  lines.push('\treturn data')
  lines.push('}')
  lines.push('')
  lines.push('// adminIndexHTML is GET /admin\'s response body (admin.go\'s serveAdminPage) —')
  lines.push('// the built Vue app\'s HTML shell.')
  lines.push(`var adminIndexHTML = ${goBase64Expr(indexBase64)}`)
  lines.push('')
  lines.push('// adminAssets maps each hashed filename under /admin/assets/ (admin.go\'s')
  lines.push('// serveAdminAsset) to its content.')
  lines.push('var adminAssets = map[string]adminAsset{')
  for (const { name, contentType, base64 } of assetEntries) {
    lines.push(`\t${goStringLiteral(name)}: {`)
    lines.push(`\t\tcontentType: ${goStringLiteral(contentType)},`)
    lines.push(`\t\tbody:        ${goBase64Expr(base64)},`)
    lines.push('\t},')
  }
  lines.push('}')
  lines.push('')

  const goSource = lines.join('\n')
  writeFileSync(OUT_PATH, goSource)

  const totalBytes = distFiles.reduce((sum, f) => sum + statSync(join(DIST_DIR, f)).size, 0)
  console.log(`generate.mjs: wrote ${OUT_PATH}`)
  console.log(`generate.mjs: ${distFiles.length} dist/ files, ${totalBytes} bytes raw -> ${goSource.length} bytes generated Go source`)
}

main()
