<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    label: string
    description?: string
    /** Dotted config key path, e.g. "agent.maxConcurrent" — shown on hover, copyable. */
    keyPath?: string
    /** True when the live value differs from the shipped default. */
    modified?: boolean
    /** Restores this field to its default. Omit to hide the reset affordance. */
    onreset?: () => void
    /** Associates the label with the control for a11y. */
    for?: string
    control: Snippet
  }

  const { label, description, keyPath, modified = false, onreset, for: htmlFor, control }: Props =
    $props()

  let copied = $state(false)
  function copyPath() {
    if (!keyPath) return
    navigator.clipboard?.writeText(keyPath).then(() => {
      copied = true
      setTimeout(() => (copied = false), 1200)
    })
  }
</script>

<div class="group relative flex flex-col gap-1 border-l-2 pl-3 {modified
    ? 'border-primary-500'
    : 'border-transparent'}">
  <div class="flex items-center gap-2">
    <label class="text-sm font-medium text-surface-800 dark:text-surface-100" for={htmlFor}>{label}</label>
    {#if modified}
      <span class="rounded bg-primary-500/12 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-primary-700 dark:text-primary-300">
        Modified
      </span>
    {/if}
    {#if keyPath}
      <button
        type="button"
        title="Copy config key: {keyPath}"
        class="font-mono text-[10px] text-surface-400 opacity-0 transition-opacity hover:text-surface-600 group-hover:opacity-100 dark:hover:text-surface-200"
        onclick={copyPath}
      >
        {copied ? 'copied ✓' : keyPath}
      </button>
    {/if}
    {#if modified && onreset}
      <button
        type="button"
        title="Reset to default"
        class="ml-auto shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium text-surface-500 opacity-0 transition-opacity hover:bg-surface-200 hover:text-surface-800 group-hover:opacity-100 dark:text-surface-400 dark:hover:bg-surface-700 dark:hover:text-surface-100"
        onclick={onreset}
      >
        ↺ Reset
      </button>
    {/if}
  </div>
  {@render control()}
  {#if description}
    <span class="text-xs text-surface-500 dark:text-surface-400">{description}</span>
  {/if}
</div>
