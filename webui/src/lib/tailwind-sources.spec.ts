/// <reference types="node" />
import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

// vitest replaces CSS imports (even ?raw) with an empty string, so read the file directly.
const mainCss = readFileSync(new URL('../assets/main.css', import.meta.url), 'utf8')

// Tailwind only emits utilities for the perfect-design component directories listed as @source in main.css.
// An imported component whose directory is missing renders unstyled in the production build.

const PD_DIST = '/node_modules/@perfect-design/ui/dist/components/'

const sources = import.meta.glob<string>('/src/**/*.{vue,ts}', { query: '?raw', import: 'default', eager: true })
const indexes = import.meta.glob<string>('/node_modules/@perfect-design/ui/dist/components/*/*/index.d.ts', {
  query: '?raw',
  import: 'default',
  eager: true,
})
const scripts = import.meta.glob<string>('/node_modules/@perfect-design/ui/dist/components/**/*.js', { query: '?raw', import: 'default' })

const importRe = /import\s+(type\s+)?\{([^}]*)\}\s*from\s*'@perfect-design\/ui'/g

function usedNames(): string[] {
  const names = new Set<string>()
  for (const [path, text] of Object.entries(sources)) {
    if (path.endsWith('.spec.ts')) continue
    for (const m of text.matchAll(importRe)) {
      if (m[1]) continue
      for (const part of m[2].split(',')) {
        const name = part.trim().split(/\s+as\s+/)[0]
        if (name && !name.startsWith('type ')) names.add(name)
      }
    }
  }
  return [...names]
}

/** export name -> "<group>/<Dir>", read from each component directory's index.d.ts. */
function exportOwners(): Map<string, string> {
  const owners = new Map<string, string>()
  for (const [path, text] of Object.entries(indexes)) {
    const dir = path.slice(PD_DIST.length).replace(/\/index\.d\.ts$/, '')
    for (const m of text.matchAll(/export\s*\{([^}]*)\}\s*from/g)) {
      for (const part of m[1].split(',')) {
        const name = part.trim().split(/\s+as\s+/).pop()
        if (name) owners.set(name, dir)
      }
    }
  }
  return owners
}

function coveredDirs(): { dirs: Set<string>; groups: Set<string> } {
  const dirs = new Set<string>()
  const groups = new Set<string>()
  for (const m of mainCss.matchAll(/^@source\s+"[^"]*\/dist\/components\/([^"]+)";/gm)) {
    const target = m[1].replace(/\/\*\*\/\*\.js$/, '')
    if (target.endsWith('/*.js')) groups.add(target.slice(0, -'/*.js'.length))
    else dirs.add(target)
  }
  return { dirs, groups }
}

/** Directories (and group roots) a component directory pulls in through its relative imports. */
async function dependencyDirs(start: string, seen: Set<string>, roots: Set<string>): Promise<void> {
  if (seen.has(start)) return
  seen.add(start)
  const prefix = `${PD_DIST}${start}/`
  for (const [path, load] of Object.entries(scripts)) {
    if (!path.startsWith(prefix) || path.endsWith('.map')) continue
    const text = await load()
    for (const m of text.matchAll(/from\s*"(\.[^"]+)"/g)) {
      const resolved = resolve(path, m[1])
      if (!resolved.startsWith(PD_DIST)) continue
      const parts = resolved.slice(PD_DIST.length).split('/')
      if (parts.length === 2) roots.add(parts[0])
      else await dependencyDirs(`${parts[0]}/${parts[1]}`, seen, roots)
    }
  }
}

function resolve(from: string, rel: string): string {
  const out = from.split('/').slice(0, -1)
  for (const seg of rel.split('/')) {
    if (seg === '..') out.pop()
    else if (seg !== '.') out.push(seg)
  }
  return out.join('/')
}

describe('main.css @source coverage', () => {
  const { dirs, groups } = coveredDirs()

  it('lists component directories at all', () => {
    expect(dirs.size).toBeGreaterThan(10)
  })

  it('covers the directory of every component imported from @perfect-design/ui', () => {
    const owners = exportOwners()
    const missing = usedNames()
      .map((name) => ({ name, dir: owners.get(name) }))
      .filter((e): e is { name: string; dir: string } => e.dir !== undefined && !dirs.has(e.dir))
    expect(missing).toEqual([])
  })

  it('covers every directory those components import', async () => {
    const owners = exportOwners()
    const seen = new Set<string>()
    const roots = new Set<string>()
    for (const name of usedNames()) {
      const dir = owners.get(name)
      if (dir) await dependencyDirs(dir, seen, roots)
    }
    expect([...seen].filter((d) => !dirs.has(d))).toEqual([])
    expect([...roots].filter((g) => !groups.has(g))).toEqual([])
  })
})
