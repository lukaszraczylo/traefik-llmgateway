<script setup lang="ts">
import { PAGE_NAV } from '@/lib/pages'
import { useNavStore } from '@/stores/nav'

/**
 * SidebarNav (redesign-plan.md section 3.1) is the shell's primary
 * navigation: a vertical list of pages on a normal-width viewport,
 * collapsing to a horizontally-scrollable bar on a narrow one (the
 * `md:` breakpoint below) rather than a hamburger/drawer — every page is
 * still one tap away, and the panel is an internal admin tool with no
 * phone-first audience to justify hiding the list behind an extra tap.
 *
 * Real `<button>` elements, not `<a href>` (mirrors EntityLink.vue's own
 * structural note): activating one is a programmatic Pinia navigation
 * (nav.goTo), not a traditional link follow, and there is no separate
 * URL to navigate to server-side — the hash is derived FROM this state
 * (composables/useHashState.ts), not the other way around.
 */
const nav = useNavStore()
</script>

<template>
  <nav
    aria-label="Primary"
    class="flex gap-1 overflow-x-auto border-b border-border pb-2 md:w-52 md:shrink-0 md:flex-col md:border-r md:border-b-0 md:pr-3 md:pb-0"
  >
    <button
      v-for="entry in PAGE_NAV"
      :key="entry.id"
      type="button"
      :aria-current="nav.page === entry.id ? 'page' : undefined"
      class="flex shrink-0 items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium whitespace-nowrap transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 md:w-full"
      :class="nav.page === entry.id ? 'bg-accent text-accent-foreground' : 'text-muted-foreground hover:bg-muted hover:text-foreground'"
      @click="nav.goTo(entry.id)"
    >
      <FontAwesomeIcon :icon="entry.icon" class="size-4" aria-hidden="true" />
      <span>{{ entry.label }}</span>
    </button>
  </nav>
</template>
