import type { VariantProps } from 'class-variance-authority'
import { cva } from 'class-variance-authority'

export { default as Tabs } from './Tabs.vue'
export { default as TabsContent } from './TabsContent.vue'
export { default as TabsList } from './TabsList.vue'
export { default as TabsTrigger } from './TabsTrigger.vue'

export const tabsListVariants = cva(
  // Horizontal orientation (the only one this app actually uses, in every
  // tab strip switching between pages/kinds/forms) is capped to the
  // available width and made horizontally scrollable instead of letting
  // `w-fit` push it wider than its container — every Card (ui/card)
  // clips overflow with `overflow-hidden` by design (rounded corners), so
  // an uncapped TabsList wider than its Card doesn't spill onto the page
  // (confirmed via live audit: zero page-level scrollWidth overflow
  // anywhere) — it just gets silently CLIPPED with no scrollbar and no
  // way to reach the hidden triggers. That silent clipping, not page
  // overflow, is what broke "switching tabs on mobile" (measured live:
  // the 5-trigger Config change-helper strip loses its last ~1.5 triggers
  // under its Card at 360-390px). `justify-start` overrides the default
  // centering only for this case: centering an overflowing flex row can
  // otherwise leave its first item unreachable by scroll in some browsers
  // (the well-known justify-content/overflow interaction) — reka-ui's own
  // roving-tabindex keyboard handling on TabsTrigger is untouched by any
  // of this, so arrow-key/Home/End navigation still works.
  //
  // `overflow-x-auto` alone (verify-ui-states.md #2) makes the computed
  // `overflow-y` resolve to `auto` too (the CSS `overflow` shorthand
  // behavior when only one axis is set explicitly) — combined with
  // TabsTrigger's own active-indicator pseudo-element extending 5px below
  // the 32px-tall list (`after:bottom-[-5px]`), that produced a real,
  // always-visible vertical scrollbar on every TabsList on any platform
  // that shows classic (non-overlay) scrollbars. `overflow-y-hidden`
  // clips that harmless 5px indicator overflow without touching the
  // horizontal scroll this rule exists for.
  'rounded-lg p-0.75 group-data-horizontal/tabs:h-8 group-data-horizontal/tabs:max-w-full group-data-horizontal/tabs:justify-start group-data-horizontal/tabs:overflow-x-auto group-data-horizontal/tabs:overflow-y-hidden data-[variant=line]:rounded-none group/tabs-list inline-flex w-fit items-center justify-center text-muted-foreground group-data-vertical/tabs:h-fit group-data-vertical/tabs:flex-col',
  {
    variants: {
      variant: {
        default: 'bg-muted',
        line: 'gap-1 bg-transparent',
      },
    },
    defaultVariants: {
      variant: 'default',
    },
  },
)

export type TabsListVariants = VariantProps<typeof tabsListVariants>
