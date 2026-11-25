import { describe, expect, it } from 'vitest'

import { splitModelId } from './model-id'

describe('splitModelId', () => {
  it('splits a provider-prefixed id at the first slash', () => {
    expect(splitModelId('uni/glm-5.3-flash')).toEqual({ provider: 'uni', model: 'glm-5.3-flash' })
  })

  it('keeps further slashes in the model part', () => {
    expect(splitModelId('nvidia-nim/nvidia/nemotron-3-super')).toEqual({ provider: 'nvidia-nim', model: 'nvidia/nemotron-3-super' })
  })

  it('returns a null provider for a bare id', () => {
    expect(splitModelId('gpt-5')).toEqual({ provider: null, model: 'gpt-5' })
  })

  it('treats a leading or trailing slash as part of a bare id', () => {
    expect(splitModelId('/x')).toEqual({ provider: null, model: '/x' })
    expect(splitModelId('x/')).toEqual({ provider: null, model: 'x/' })
  })

  it('returns an empty model for an empty id', () => {
    expect(splitModelId('')).toEqual({ provider: null, model: '' })
  })
})
