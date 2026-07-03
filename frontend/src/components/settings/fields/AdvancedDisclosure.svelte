<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    /** Label for the toggle, defaults to "Advanced". */
    label?: string
    /** Force-open (e.g. when a search or modified-filter matches a hidden field). */
    open?: boolean
    /** True to render a red-bordered danger treatment for risky settings. */
    danger?: boolean
    children: Snippet
  }

  let { label = 'Advanced', open = $bindable(false), danger = false, children }: Props = $props()
</script>

<div class="rounded-lg border {danger
    ? 'border-error-300/70 dark:border-error-800/60'
    : 'border-surface-200 dark:border-surface-700'}">
  <button
    type="button"
    class="flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-xs font-semibold uppercase tracking-wide {danger
      ? 'text-error-600 dark:text-error-400'
      : 'text-surface-500 dark:text-surface-400'}"
    aria-expanded={open}
    onclick={() => (open = !open)}
  >
    <span class="transition-transform {open ? 'rotate-90' : ''}">›</span>
    {label}
  </button>
  {#if open}
    <div class="flex flex-col gap-4 px-3 pb-4 pt-1">
      {@render children()}
    </div>
  {/if}
</div>
