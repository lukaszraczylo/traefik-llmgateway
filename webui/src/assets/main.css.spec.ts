import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('main.css', () => {
  // Unlayered FontAwesome CSS sets `display` on every icon and beats Tailwind's `hidden`, so both chevrons of an accordion trigger showed.
  it('imports the FontAwesome stylesheet inside a cascade layer', () => {
    const css = readFileSync(new URL('./main.css', import.meta.url), 'utf8')
    expect(css).toMatch(/@import\s+"@fortawesome\/fontawesome-svg-core\/styles\.css"\s+layer\(/)
  })
})
