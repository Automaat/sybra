<script lang="ts">
  interface Props {
    label: string
    checked: boolean
    description?: string
    keyPath?: string
    modified?: boolean
    onreset?: () => void
  }

  let {
    label,
    checked = $bindable(),
    description,
    keyPath,
    modified = false,
    onreset,
  }: Props = $props()
</script>

<div class="group flex items-start gap-3 border-l-2 pl-3 {modified
    ? 'border-primary-500'
    : 'border-transparent'}">
  <input
    type="checkbox"
    aria-label={label}
    class="mt-0.5 h-4 w-4 shrink-0 cursor-pointer rounded border-surface-300 accent-primary-500"
    bind:checked
  />
  <div class="flex min-w-0 flex-1 flex-col">
    <div class="flex items-center gap-2">
      <span class="text-sm font-medium text-surface-800 dark:text-surface-100">{label}</span>
      {#if modified}
        <span class="rounded bg-primary-500/12 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-primary-700 dark:text-primary-300">
          Modified
        </span>
      {/if}
      {#if keyPath}
        <span class="hidden font-mono text-[10px] text-surface-400 group-hover:inline">{keyPath}</span>
      {/if}
      {#if modified && onreset}
        <button
          type="button"
          class="ml-auto shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium text-surface-500 opacity-0 transition-opacity hover:bg-surface-200 hover:text-surface-800 group-hover:opacity-100 dark:text-surface-400 dark:hover:bg-surface-700 dark:hover:text-surface-100"
          onclick={onreset}
        >
          ↺ Reset
        </button>
      {/if}
    </div>
    {#if description}
      <span class="text-xs text-surface-500 dark:text-surface-400">{description}</span>
    {/if}
  </div>
</div>
