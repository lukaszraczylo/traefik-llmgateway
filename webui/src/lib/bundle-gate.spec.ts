import { describe, expect, it } from 'vitest'

import { hasDist, LIMIT, totalGzip } from '../../scripts/bundle-size.mjs'

// Needs a prior `npm run build`; skipped when dist/ is absent.
describe.skipIf(!hasDist())('bundle gate', () => {
  it('keeps gzip JS+CSS within the size limit', () => {
    expect(totalGzip()).toBeLessThanOrEqual(LIMIT)
  })
})
