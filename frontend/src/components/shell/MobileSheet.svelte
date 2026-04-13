<script lang="ts">
  import { Dialog } from '@skeletonlabs/skeleton-svelte'
  import type { Snippet } from 'svelte'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    variant?: 'bottom' | 'top' | 'center'
    title?: string
    children: Snippet
  }

  const { open, onOpenChange, variant = 'bottom', title, children }: Props = $props()

  const positionerClass = $derived(
    variant === 'bottom'
      ? 'fixed inset-0 z-50 flex items-end justify-center md:items-center md:p-4'
      : variant === 'top'
        ? 'fixed inset-0 z-50 flex items-start justify-center md:p-4 md:pt-[12vh]'
        : 'fixed inset-0 z-50 flex items-center justify-center p-4'
  )

  const contentClass = $derived(
    variant === 'bottom'
      ? 'flex max-h-[92dvh] w-full flex-col overflow-y-auto rounded-t-2xl bg-surface-50 pb-safe shadow-2xl dark:bg-surface-950 md:max-h-[85dvh] md:max-w-lg md:rounded-2xl md:pb-0'
      : variant === 'top'
        ? 'flex max-h-[92dvh] w-full flex-col overflow-y-auto rounded-b-2xl bg-surface-50 pt-safe shadow-2xl dark:bg-surface-950 md:max-h-[80dvh] md:max-w-lg md:rounded-2xl md:pt-0'
        : 'flex max-h-[92dvh] w-full max-w-lg flex-col overflow-y-auto rounded-2xl bg-surface-50 shadow-2xl dark:bg-surface-950'
  )
</script>

<Dialog
  {open}
  onOpenChange={(d) => onOpenChange(d.open)}
>
  <Dialog.Backdrop class="fixed inset-0 z-40 bg-black/50" />
  <Dialog.Positioner class={positionerClass}>
    <Dialog.Content class={contentClass}>
      {#if open}
        {#if variant === 'bottom'}
          <div class="flex shrink-0 justify-center pt-2 md:hidden">
            <span class="h-1 w-10 rounded-full bg-surface-300 dark:bg-surface-700"></span>
          </div>
        {/if}
        {#if title}
          <Dialog.Title class="px-5 pt-3 pb-2 text-lg font-semibold md:px-6 md:pt-4">
            {title}
          </Dialog.Title>
        {/if}
        {@render children()}
      {/if}
    </Dialog.Content>
  </Dialog.Positioner>
</Dialog>
