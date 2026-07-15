<script lang="ts">
  import { Dialog } from '@skeletonlabs/skeleton-svelte'
  import type { Snippet } from 'svelte'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    variant?: 'bottom' | 'top' | 'center'
    title?: string
    backdropClass?: string
    children: Snippet
  }

  const { open, onOpenChange, variant = 'bottom', title, backdropClass, children }: Props = $props()

  const positionerClass = $derived(
    variant === 'bottom'
      ? 'fixed inset-0 z-50 flex items-end justify-center md:items-center md:p-4'
      : variant === 'top'
        ? 'fixed inset-0 z-50 flex items-start justify-center md:p-4 md:pt-[12vh]'
        : 'fixed inset-0 z-50 flex items-center justify-center p-4'
  )

  const contentClass = $derived(
    variant === 'bottom'
      ? 'elevation-modal flex max-h-[92dvh] w-full flex-col overflow-y-auto rounded-t-2xl pb-safe md:max-h-[85dvh] md:max-w-lg md:rounded-2xl md:pb-0'
      : variant === 'top'
        ? 'elevation-modal flex max-h-[92dvh] w-full flex-col overflow-y-auto rounded-b-2xl pt-safe md:max-h-[80dvh] md:max-w-lg md:rounded-2xl md:pt-0'
        : 'elevation-modal flex max-h-[92dvh] w-full max-w-lg flex-col overflow-y-auto rounded-2xl'
  )
</script>

<Dialog
  {open}
  onOpenChange={(d) => onOpenChange(d.open)}
>
  <Dialog.Backdrop class={backdropClass ?? 'modal-backdrop fixed inset-0 z-40'} />
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
