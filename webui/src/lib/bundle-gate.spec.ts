import { describe, expect, it } from 'vitest'

import { hasDist, LIMIT, readCss, totalGzip } from '../../scripts/bundle-size.mjs'

// Needs a prior `npm run build`; skipped when dist/ is absent.
describe.skipIf(!hasDist())('bundle gate', () => {
  it('keeps gzip JS+CSS within the size limit', () => {
    expect(totalGzip()).toBeLessThanOrEqual(LIMIT)
  })

  // Utilities that only one perfect-design component directory emits; a missing @source entry drops them from the built CSS.
  it.each([
    ['NativeSelect', '.\\[\\&_option\\]\\:bg-popover'],
    ['Button', '.hover\\:bg-destructive\\/90'],
    ['Badge', '.border-primary\\/50'],
    ['Card', '.\\@container\\/card-header'],
    ['Input', '.end-11'],
  ])('emits the %s utility in the built CSS', (_name, selector) => {
    expect(readCss()).toContain(selector)
  })
})
