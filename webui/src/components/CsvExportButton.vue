<script setup lang="ts">
import { faCircleCheck, faCircleXmark, faFileCsv } from '@fortawesome/free-solid-svg-icons'
import { computed, onBeforeUnmount, ref } from 'vue'

import { Button } from '@/components/ui/button'
import { downloadText, type DownloadOutcome } from '@/lib/download'

/**
 * CsvExportButton is the one "Export CSV" control every table export in
 * this panel uses — F8. Owned by WP-B1 (usage-csv.ts, this component) but
 * consumed unmodified by WP-B2's Models tab, which builds its own CSV text
 * via lib/usage-csv.ts's modelDetailCsv and passes it through the same
 * `build` prop — one export affordance, not a copy per tab (vue.md: "if
 * you've written it twice, you owe an abstraction").
 *
 * `build` is called fresh on every click, never memoized — an export
 * always reflects whatever rows are CURRENTLY visible (filtered + sorted)
 * at click time, matching the coordinator brief ("exports filtered+sorted
 * rows"), never a stale snapshot captured at mount.
 */
const props = defineProps<{
  /** The downloaded file's name, e.g. "usage-users.csv". */
  filename: string
  /** Builds the CSV text (lib/csv.ts's toCsv, via usage-csv.ts) on demand. */
  build: () => string
}>()

/**
 * exporting guards a rapid double-click from firing two downloads (or two
 * copyText fallback prompts) back to back — downloadText (lib/download.ts)
 * is async only because its clipboard fallback is, so this window is
 * normally imperceptibly short, but a real one regardless.
 */
const exporting = ref(false)

/** P5 review fix: downloadText's own return value used to be discarded entirely — a failed export (or a silent clipboard/selection fallback) looked identical to a successful one. `feedback` holds the most recent outcome; the label/icon below reflect it for FEEDBACK_MS, then the button reverts to its normal resting state. */
const feedback = ref<DownloadOutcome | null>(null)
const FEEDBACK_MS = 2500
let feedbackTimer: ReturnType<typeof setTimeout> | undefined

const FEEDBACK_LABEL: Record<DownloadOutcome, string> = {
  downloaded: 'Downloaded',
  copied: 'Copied',
  selected: 'Selected — press ⌘/Ctrl+C',
  failed: 'Export failed',
}

const label = computed(() => (feedback.value ? FEEDBACK_LABEL[feedback.value] : 'Export CSV'))
const isFailure = computed(() => feedback.value === 'failed')

function clearFeedbackAfterDelay(): void {
  if (feedbackTimer !== undefined) clearTimeout(feedbackTimer)
  feedbackTimer = setTimeout(() => {
    feedback.value = null
    feedbackTimer = undefined
  }, FEEDBACK_MS)
}

async function onClick(): Promise<void> {
  if (exporting.value) return
  exporting.value = true
  try {
    feedback.value = await downloadText(props.filename, props.build())
    clearFeedbackAfterDelay()
  } finally {
    exporting.value = false
  }
}

onBeforeUnmount(() => {
  if (feedbackTimer !== undefined) clearTimeout(feedbackTimer)
})
</script>

<template>
  <Button type="button" variant="outline" size="sm" :disabled="exporting" @click="onClick">
    <FontAwesomeIcon v-if="feedback === 'failed'" :icon="faCircleXmark" class="size-3.5 text-destructive" aria-hidden="true" />
    <FontAwesomeIcon
      v-else-if="feedback"
      :icon="faCircleCheck"
      class="size-3.5 text-chart-requests"
      aria-hidden="true"
    />
    <FontAwesomeIcon v-else :icon="faFileCsv" class="size-3.5" aria-hidden="true" />
    <span :class="isFailure ? 'text-destructive' : undefined">{{ label }}</span>
  </Button>
</template>
