<script lang="ts">
  import { workflow } from '../../../wailsjs/go/models.js'

  interface Props {
    condition: workflow.Condition
    onupdate: (c: workflow.Condition) => void
    onremove?: () => void
    fieldPlaceholder?: string
  }

  const { condition, onupdate, onremove, fieldPlaceholder = 'task.tags' }: Props = $props()

  function update(patch: Partial<workflow.Condition>) {
    onupdate(new workflow.Condition({ ...condition, ...patch }))
  }
</script>

<div class="flex items-center gap-1">
  <input
    type="text"
    class="w-32 rounded border border-surface-300 bg-white px-2 py-1 text-xs dark:border-surface-600 dark:bg-surface-700"
    value={condition.field ?? ''}
    placeholder={fieldPlaceholder}
    onchange={(e) => update({ field: e.currentTarget.value })}
  />
  <select
    class="rounded border border-surface-300 bg-white px-1 py-1 text-xs dark:border-surface-600 dark:bg-surface-700"
    value={condition.operator ?? 'equals'}
    onchange={(e) => update({ operator: e.currentTarget.value })}
  >
    <option value="equals">equals</option>
    <option value="not_equals">not_equals</option>
    <option value="contains">contains</option>
    <option value="not_contains">not_contains</option>
    <option value="exists">exists</option>
  </select>
  {#if condition.operator !== 'exists'}
    <input
      type="text"
      class="flex-1 min-w-0 rounded border border-surface-300 bg-white px-2 py-1 text-xs dark:border-surface-600 dark:bg-surface-700"
      value={condition.value ?? ''}
      placeholder="value"
      onchange={(e) => update({ value: e.currentTarget.value })}
    />
  {/if}
  {#if onremove}
    <button
      type="button"
      class="shrink-0 rounded p-1 text-error-500 hover:bg-error-100 dark:hover:bg-error-900/40"
      onclick={onremove}
      title="Remove"
      aria-label="Remove condition"
    >
      <svg class="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
      </svg>
    </button>
  {/if}
</div>
