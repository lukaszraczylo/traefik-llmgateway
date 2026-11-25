// Gate scope: gzip (level 9) of every .js and .css in dist/assets, lazy chunks included.
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'

export const LIMIT = 324300
export const DIST = fileURLToPath(new URL('../dist/assets', import.meta.url))

export const hasDist = () => existsSync(DIST)

export function readCss(dir = DIST) {
  return readdirSync(dir)
    .filter((f) => f.endsWith('.css'))
    .map((f) => readFileSync(join(dir, f), 'utf8'))
    .join('\n')
}

export function totalGzip(dir = DIST) {
  return measure(dir).reduce((n, r) => n + r.gzip, 0)
}

export function measure(dir = DIST) {
  return readdirSync(dir)
    .filter((f) => /\.(js|css)$/.test(f))
    .map((f) => ({ file: f, gzip: gzipSync(readFileSync(join(dir, f)), { level: 9 }).length }))
    .sort((a, b) => b.gzip - a.gzip)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const rows = measure()
  const total = totalGzip()
  for (const r of rows.slice(0, 10)) console.log(`${String(r.gzip).padStart(8)}  ${r.file}`)
  console.log(`total gzip JS+CSS: ${total} B (limit ${LIMIT}); ${rows.length} files`)
  if (total > LIMIT) {
    console.error('bundle gate FAILED')
    process.exit(1)
  }
}
