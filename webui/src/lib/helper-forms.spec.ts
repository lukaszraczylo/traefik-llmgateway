import { describe, expect, it } from 'vitest'

import { HELPER_IDS, HELPER_LABEL, formatHelperSel, isStaleHelperSel, parseHelperSel } from '@/lib/helper-forms'

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

describe('isStaleHelperSel', () => {
  it('is false when absent or a valid helper', () => {
    expect(isStaleHelperSel(undefined)).toBe(false)
    expect(isStaleHelperSel('helper:router')).toBe(false)
  })
  it('is true for an unknown helper id or a foreign sel the Config page cannot open', () => {
    expect(isStaleHelperSel('helper:bogus')).toBe(true)
    expect(isStaleHelperSel('user:alice')).toBe(true)
  })
})
