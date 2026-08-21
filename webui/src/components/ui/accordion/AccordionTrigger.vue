<script setup lang="ts">
import type { AccordionTriggerProps } from 'reka-ui'

import type { HTMLAttributes } from 'vue'
import { faChevronDown, faChevronUp } from '@fortawesome/free-solid-svg-icons'
import { reactiveOmit } from '@vueuse/core'
import {
  AccordionHeader,
  AccordionTrigger,
} from 'reka-ui'
import { cn } from '@/lib/utils'

const props = defineProps<AccordionTriggerProps & { class?: HTMLAttributes['class'] }>()

const delegatedProps = reactiveOmit(props, 'class')
</script>

<template>
  <AccordionHeader class="flex">
    <AccordionTrigger
      data-slot="accordion-trigger"
      v-bind="delegatedProps"
      :class="
        cn(
          'focus-visible:ring-ring/50 focus-visible:border-ring focus-visible:after:border-ring **:data-[slot=accordion-trigger-icon]:text-muted-foreground rounded-lg py-2.5 text-left text-sm font-medium hover:underline focus-visible:ring-3 **:data-[slot=accordion-trigger-icon]:ml-auto **:data-[slot=accordion-trigger-icon]:size-4 group/accordion-trigger relative flex flex-1 items-start justify-between border border-transparent transition-all outline-none disabled:pointer-events-none disabled:opacity-50',
          props.class,
        )
      "
    >
      <slot />
      <slot name="icon">
        <FontAwesomeIcon
          :icon="faChevronDown"
          data-slot="accordion-trigger-icon"
          aria-hidden="true"
          class="pointer-events-none shrink-0 group-aria-expanded/accordion-trigger:hidden"
        />
        <FontAwesomeIcon
          :icon="faChevronUp"
          data-slot="accordion-trigger-icon"
          aria-hidden="true"
          class="pointer-events-none hidden shrink-0 group-aria-expanded/accordion-trigger:inline"
        />
      </slot>
    </AccordionTrigger>
  </AccordionHeader>
</template>
