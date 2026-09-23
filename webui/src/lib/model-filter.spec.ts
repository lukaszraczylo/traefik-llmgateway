import { describe, expect, it } from 'vitest'

import { filterModelsByPrefix } from './model-filter'
import type { AdminUsageModelEntry } from '@/types/api'

const models: AdminUsageModelEntry[] = [
  { id: 'openai/gpt-5', value: 10 },
  { id: 'openai/gpt-5-mini', value: 5 },
  { id: 'anthropic/claude-opus', value: 8 },
]

describe('filterModelsByPrefix', () => {
  it('returns every entry unchanged when the prefix is empty', () => {
    expect(filterModelsByPrefix(models, '')).toBe(models)
  })

  it('keeps only ids starting with the given provider prefix', () => {
    const result = filterModelsByPrefix(models, 'openai/')
    expect(result.map((m) => m.id)).toEqual(['openai/gpt-5', 'openai/gpt-5-mini'])
  })

  it('matches a full id exactly (a single-model prefix)', () => {
    const result = filterModelsByPrefix(models, 'anthropic/claude-opus')
    expect(result).toEqual([{ id: 'anthropic/claude-opus', value: 8 }])
  })

  it('returns an empty array when nothing matches the prefix', () => {
    expect(filterModelsByPrefix(models, 'mistral/')).toEqual([])
  })

  it('is case-sensitive (ids are canonical, lowercase provider/model strings)', () => {
    expect(filterModelsByPrefix(models, 'OpenAI/')).toEqual([])
  })

  it('does not match a substring that is not a prefix', () => {
    expect(filterModelsByPrefix(models, 'gpt-5')).toEqual([])
  })
})
