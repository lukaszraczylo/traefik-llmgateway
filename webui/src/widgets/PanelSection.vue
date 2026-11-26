<script setup lang="ts">
defineProps<{ title: string }>()
</script>

<template>
  <section class="flex flex-col gap-2 py-3" :aria-label="title">
    <h3 class="text-muted-foreground m-0 text-xs font-medium tracking-wide uppercase">{{ title }}</h3>
    <slot />
  </section>
</template>
