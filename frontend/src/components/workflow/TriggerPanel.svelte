<script lang="ts">
  import { workflow } from '../../../wailsjs/go/models.js'
  import ConditionRow from './ConditionRow.svelte'

  interface Props {
    trigger: workflow.Trigger
    onupdate: (t: workflow.Trigger) => void
  }

  const { trigger, onupdate }: Props = $props()

  let expanded = $state(true)

  const triggerEvents = [
    { value: 'task.created', label: 'Task created' },
    { value: 'task.status_changed', label: 'Task status changed' },
    { value: 'pr.event', label: 'PR event' },
  ]

  function emit(patch: Partial<workflow.Trigger>) {
    const next = new workflow.Trigger({
      on: trigger?.on ?? '',
      conditions: trigger?.conditions ?? [],
      ...patch,
    })
    onupdate(next)
  }

  function updateOn(value: string) {
    emit({ on: value })
  }

  function updateCondition(idx: number, c: workflow.Condition) {
    const conditions = [...(trigger?.conditions ?? [])]
    conditions[idx] = c
    emit({ conditions })
  }

  function removeCondition(idx: number) {
    const conditions = [...(trigger?.conditions ?? [])]
    conditions.splice(idx, 1)
    emit({ conditions })
  }

  function addCondition() {
    const conditions = [...(trigger?.conditions ?? [])]
    conditions.push(new workflow.Condition({ field: '', operator: 'equals', value: '' }))
    emit({ conditions })
  }
</script>

<div class="border-b border-surface-300 bg-surface-50 px-4 py-2 dark:border-surface-600 dark:bg-surface-800/50">
  <button
    type="button"
    class="flex w-full items-center gap-2 text-left"
    onclick={() => (expanded = !expanded)}
  >
    <svg
      class="h-4 w-4 transition-transform {expanded ? 'rotate-90' : ''}"
      fill="none"
      stroke="currentColor"
      viewBox="0 0 24 24"
    >
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
    </svg>
    <span class="text-xs font-semibold uppercase tracking-wide text-surface-500">Trigger</span>
    <span class="text-xs text-surface-500">
      {trigger?.on || '(none)'}
      {#if trigger?.conditions?.length}
        · {trigger.conditions.length} condition{trigger.conditions.length === 1 ? '' : 's'}
      {/if}
    </span>
  </button>

  {#if expanded}
    <div class="mt-3 flex flex-col gap-3 pl-6">
      <label class="flex items-center gap-2">
        <span class="w-20 text-xs font-medium text-surface-500">Event</span>
        <select
          class="rounded border border-surface-300 bg-white px-2 py-1 text-xs dark:border-surface-600 dark:bg-surface-700"
          value={trigger?.on ?? ''}
          onchange={(e) => updateOn(e.currentTarget.value)}
        >
          <option value="">(none)</option>
          {#each triggerEvents as ev (ev.value)}
            <option value={ev.value}>{ev.label} — {ev.value}</option>
          {/each}
        </select>
      </label>

      <div class="flex flex-col gap-2">
        <div class="flex items-center gap-2">
          <span class="w-20 text-xs font-medium text-surface-500">Conditions</span>
          <button
            type="button"
            class="rounded bg-surface-200 px-2 py-0.5 text-xs hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
            onclick={addCondition}
          >
            + Add condition
          </button>
        </div>
        {#if trigger?.conditions?.length}
          <div class="flex flex-col gap-1 pl-20">
            {#each trigger.conditions as cond, i (i)}
              <ConditionRow
                condition={cond}
                onupdate={(c) => updateCondition(i, c)}
                onremove={() => removeCondition(i)}
              />
            {/each}
          </div>
        {:else}
          <p class="pl-20 text-xs text-surface-400 italic">No conditions — triggers on every matching event</p>
        {/if}
      </div>
    </div>
  {/if}
</div>
