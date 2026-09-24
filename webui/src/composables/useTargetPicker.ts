import { computed, ref, type ComputedRef, type Ref } from 'vue'

import { isValidName, isValidUserName } from '@/lib/snippets'

/** GrantTargetKind — named to avoid colliding with lib/target-columns.ts's own unrelated TargetKind ('mcp' | 'agent'). */
export type GrantTargetKind = 'group' | 'user'

export interface UseTargetPicker {
  targetKind: Ref<GrantTargetKind>
  targetName: Ref<string>
  /** Every configured name for the CURRENT targetKind — groupNames() while 'group', userNames() while 'user'. */
  targetNames: ComputedRef<string[]>
  /** A group name follows NAME_PATTERN (isValidName); a user name only needs to be non-empty (isValidUserName) — auth.go's own two different name rules. */
  nameValid: ComputedRef<boolean>
  /** The SnippetBlock label every ChangeHelper form using this picker shows once its snippet is ready. */
  snippetLabel: ComputedRef<string>
}

/**
 * useTargetPicker (reuse-audit.md F12) is the ONE "Target kind, then Group
 * or User name" picker GrantForm.vue and LimitsForm.vue used to each
 * hand-roll — the same targetKind/targetName refs, targetNames/nameValid
 * computeds (down to the identical doc comments), and SnippetBlock label
 * ternary. `groupNames`/`userNames` are getters, not plain arrays, so a
 * caller can pass `() => props.groupNames` and keep this reactive to a
 * prop that changes after mount (destructuring `props.groupNames`
 * directly would freeze it at the value read at call time).
 */
export function useTargetPicker(groupNames: () => string[], userNames: () => string[]): UseTargetPicker {
  const targetKind = ref<GrantTargetKind>('group')
  const targetName = ref('')

  const targetNames = computed(() => (targetKind.value === 'group' ? groupNames() : userNames()))

  const nameValid = computed(() => (targetKind.value === 'group' ? isValidName(targetName.value.trim()) : isValidUserName(targetName.value)))

  const snippetLabel = computed(() =>
    targetKind.value === 'group' ? 'Paste into middleware.yaml' : `Merge into ${targetName.value}’s users.json line`,
  )

  return { targetKind, targetName, targetNames, nameValid, snippetLabel }
}
