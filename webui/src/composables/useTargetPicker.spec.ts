import { describe, expect, it } from 'vitest'

import { useTargetPicker } from './useTargetPicker'

describe('useTargetPicker', () => {
  it('defaults to targetKind "group" with an empty targetName', () => {
    const picker = useTargetPicker(() => ['eng'], () => ['alice'])
    expect(picker.targetKind.value).toBe('group')
    expect(picker.targetName.value).toBe('')
  })

  it('targetNames reads groupNames() while targetKind is "group", userNames() while "user"', () => {
    const picker = useTargetPicker(() => ['eng', 'ops'], () => ['alice', 'bob'])
    expect(picker.targetNames.value).toEqual(['eng', 'ops'])
    picker.targetKind.value = 'user'
    expect(picker.targetNames.value).toEqual(['alice', 'bob'])
  })

  it('nameValid applies isValidName for a group target', () => {
    const picker = useTargetPicker(() => [], () => [])
    picker.targetName.value = 'valid-name.1'
    expect(picker.nameValid.value).toBe(true)
    picker.targetName.value = 'has spaces'
    expect(picker.nameValid.value).toBe(false)
  })

  it('nameValid applies isValidUserName (non-empty only) for a user target', () => {
    const picker = useTargetPicker(() => [], () => [])
    picker.targetKind.value = 'user'
    picker.targetName.value = 'has spaces are fine for a user'
    expect(picker.nameValid.value).toBe(true)
    picker.targetName.value = '   '
    expect(picker.nameValid.value).toBe(false)
  })

  it('snippetLabel names middleware.yaml for a group target', () => {
    const picker = useTargetPicker(() => [], () => [])
    expect(picker.snippetLabel.value).toBe('Paste into middleware.yaml')
  })

  it('snippetLabel names the user\'s users.json line, with the current target name, for a user target', () => {
    const picker = useTargetPicker(() => [], () => [])
    picker.targetKind.value = 'user'
    picker.targetName.value = 'alice'
    expect(picker.snippetLabel.value).toBe('Merge into alice’s users.json line')
  })
})
