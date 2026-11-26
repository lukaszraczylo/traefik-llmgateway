import { describe, expect, it } from 'vitest'

import { promptCacheHitRate, promptCacheSavedMicros } from './prompt-cache'

describe('promptCacheHitRate', () => {
  it('returns read tokens over input tokens', () => {
    expect(promptCacheHitRate({ tokensIn: 1000, promptCacheReadTokens: 900 })).toBe(0.9)
  })

  it('returns null when no input tokens', () => {
    expect(promptCacheHitRate({ tokensIn: 0, promptCacheReadTokens: 0 })).toBeNull()
  })

  it('returns null when the backend omits the fields', () => {
    expect(promptCacheHitRate({ tokensIn: 1000 })).toBeNull()
    expect(promptCacheHitRate({ promptCacheReadTokens: 5 })).toBeNull()
  })

  it('caps at 1 when read tokens exceed input tokens', () => {
    expect(promptCacheHitRate({ tokensIn: 100, promptCacheReadTokens: 150 })).toBe(1)
  })
})

describe('promptCacheSavedMicros', () => {
  it('sums saved micro-USD, treating missing as 0', () => {
    expect(promptCacheSavedMicros([{ promptCacheSavedMicroUsd: 40 }, {}, { promptCacheSavedMicroUsd: 2 }])).toBe(42)
  })
})
