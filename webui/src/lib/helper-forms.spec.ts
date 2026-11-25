import { describe, expect, it } from 'vitest'

import { HELPER_IDS, HELPER_LABEL, formatHelperSel, parseHelperSel } from '@/lib/helper-forms'

describe('helper selection', () => {
  it('round-trips every helper id', () => {
    for (const id of HELPER_IDS) expect(parseHelperSel(formatHelperSel(id))).toBe(id)
  })
  it('formats as helper:<id>', () => {
    expect(formatHelperSel('router')).toBe('helper:router')
  })
  it('rejects absent, unknown and non-helper values', () => {
    expect(parseHelperSel(undefined)).toBeNull()
    expect(parseHelperSel('helper:nope')).toBeNull()
    expect(parseHelperSel('helper:')).toBeNull()
    expect(parseHelperSel('user:router')).toBeNull()
  })
  it('opens the first form for a bare "helper" selection', () => {
    expect(parseHelperSel('helper')).toBe(HELPER_IDS[0])
  })
  it('has a label for every id', () => {
    for (const id of HELPER_IDS) expect(HELPER_LABEL[id]).toBeTruthy()
  })
})
