<script setup lang="ts">
import { faCheck, faCopy, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed, onUnmounted, ref, useTemplateRef } from 'vue'

import { Badge } from '@/components/ui/badge'
import { copyText } from '@/lib/clipboard'
import { formatContextWindow, formatModelCostHover } from '@/lib/format'

/**
 * ModelChip is one clickable, copyable model-id chip, used anywhere a
 * resolvable model or alias id renders: a provider's expanded model list
 * (ProvidersView.vue's accordion), the model-alias table, and a group's
 * usage-detail model list (UsageView.vue — passed with no metadata props
 * there, since a raw group-config glob entry has no resolvable provider/
 * model pair to look metadata up against). id is the full routable id
 * (lib/format.ts's routableModelId) or an alias name — what actually
 * gets copied, so a pasted value works straight into a `model: "..."`
 * request field.
 *
 * contextTokens/inputPerMTokUsd/outputPerMTokUsd are this model's
 * resolved metadata (feature v0.23, hover-detail refinement:
 * admin.go's adminModelMetaView / adminAliasView.ModelMeta) — all
 * optional and independently omittable, exactly mirroring
 * AdminModelMetaView's own "undefined means unknown" convention. When
 * known, they render as an appended hover-detail phrase (metaDetail
 * below) on top of whichever copy-state message title already carries;
 * when nothing is known at all, the hover is unchanged from before this
 * feature existed. A caller resolving an ALIAS's chip passes the
 * alias's own INHERITED metadata (its target's resolved values, unless
 * the alias itself has a modelMeta override) — the same values GET
 * /v1/models and the admin API already resolve that way.
 *
 * The label span (labelRef below) is copyText's select-text fallback
 * target — not a rare edge case here: navigator.clipboard is undefined
 * outright on any plain-HTTP admin deployment (the Clipboard API
 * requires a secure context — https, or localhost), so the fallback is
 * the ONLY path a click ever takes there, not a backstop for an unlikely
 * failure. labelRef is a template ref on an element this component
 * always renders (below), so it is populated by the time onClick can
 * ever run — a click handler cannot fire before mount.
 */
const props = defineProps<{
  id: string
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
}>()

const labelRef = useTemplateRef<HTMLElement>('label')
const state = ref<'idle' | 'copied' | 'selected' | 'failed'>('idle')
let resetTimer: ReturnType<typeof setTimeout> | undefined

function scheduleReset(): void {
  clearTimeout(resetTimer)
  resetTimer = setTimeout(() => {
    state.value = 'idle'
  }, 1500)
}

async function onClick(): Promise<void> {
  const result = await copyText(props.id, labelRef.value ?? undefined)
  // 'failed' (neither the Clipboard API nor the selection fallback
  // worked) gets its own visible state rather than a silent no-op — a
  // click that visibly does nothing reads as a broken button, not as
  // "nothing to report".
  state.value = result
  scheduleReset()
}

const icon = {
  idle: faCopy,
  copied: faCheck,
  selected: faCheck,
  failed: faTriangleExclamation,
}
const baseTitle = computed<Record<typeof state.value, string>>(() => ({
  idle: `Copy ${props.id}`,
  copied: 'Copied',
  selected: 'Selected — press Ctrl/Cmd+C',
  failed: 'Copy failed',
}))

/**
 * metaDetail is the resolved-metadata hover phrase (feature v0.23):
 * cost first ("in $0.19 / out $0.51 per MTok", or "free"), then context
 * ("256k context"), joined with " · " — either half is independently
 * omitted when its props are undefined, and the whole phrase is empty
 * (never appended to title below) when nothing at all is known, per
 * this feature's "fully-unknown models get no metadata hover" rule.
 */
const metaDetail = computed(() => {
  const parts: string[] = []
  if (props.inputPerMTokUsd !== undefined && props.outputPerMTokUsd !== undefined) {
    parts.push(formatModelCostHover(props.inputPerMTokUsd, props.outputPerMTokUsd))
  }
  if (props.contextTokens !== undefined) {
    parts.push(`${formatContextWindow(props.contextTokens)} context`)
  }
  return parts.join(' · ')
})

/**
 * title carries BOTH the copy-state message (unchanged behavior) and,
 * when known, the resolved-metadata detail appended after " · " — bound
 * to both the mouse-only `title` attribute and `aria-label` (same
 * mouse-vs-keyboard/screen-reader parity fix ProviderRateBadge.vue's
 * own `detail` computed already applies).
 */
const title = computed(() => {
  const base = baseTitle.value[state.value]
  return metaDetail.value ? `${base} · ${metaDetail.value}` : base
})

onUnmounted(() => {
  clearTimeout(resetTimer)
})
</script>

<template>
  <Badge
    as="button"
    type="button"
    variant="secondary"
    class="cursor-pointer gap-1 font-mono text-xs font-normal transition-colors hover:bg-accent hover:text-accent-foreground"
    :class="state === 'failed' ? 'text-destructive' : ''"
    :title="title"
    :aria-label="title"
    @click="onClick"
  >
    <span ref="label" class="select-text">{{ id }}</span>
    <FontAwesomeIcon :icon="icon[state]" class="size-2.5 shrink-0" aria-hidden="true" />
  </Badge>
</template>
