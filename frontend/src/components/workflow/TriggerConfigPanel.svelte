<script lang="ts">
  import { workflow } from '../../../wailsjs/go/models.js'
  import ConditionRow from './ConditionRow.svelte'

  interface Props {
    trigger: workflow.Trigger | null
    onupdate: (t: workflow.Trigger) => void
  }

  const { trigger, onupdate }: Props = $props()

  const triggerEvents = [
    { value: 'task.created', label: 'Task created' },
    { value: 'task.status_changed', label: 'Task status changed' },
    { value: 'pr.event', label: 'PR event' },
  ]

  function emit(patch: Partial<workflow.Trigger>) {
    const base = trigger ?? new workflow.Trigger({ on: '', conditions: [] })
    const next = new workflow.Trigger({
      on: base.on ?? '',
      conditions: base.conditions ?? [],
      position: base.position,
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

<aside class="w-80 flex-shrink-0 overflow-y-auto border-l border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800/50">
  <div class="mb-3 flex items-center gap-2">
    <svg
      class="h-4 w-4 text-amber-600 dark:text-amber-400"
      fill="currentColor"
      viewBox="0 0 20 20"
    >
      <path
        fill-rule="evenodd"
        d="M11.3 1.046A1 1 0 0112 2v5h4a1 1 0 01.82 1.573l-7 10A1 1 0 018 18v-5H4a1 1 0 01-.82-1.573l7-10a1 1 0 011.12-.38z"
        clip-rule="evenodd"
      />
    </svg>
    <h3 class="text-sm font-semibold">Trigger</h3>
  </div>

  <div class="flex flex-col gap-4">
    <label class="flex flex-col gap-1">
      <span class="text-xs font-medium text-surface-500">Event</span>
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
      <div class="flex items-center justify-between">
        <span class="text-xs font-medium text-surface-500">Conditions</span>
        <button
          type="button"
          class="rounded bg-surface-200 px-2 py-0.5 text-xs hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
          onclick={addCondition}
        >
          + Add
        </button>
      </div>
      {#if trigger?.conditions?.length}
        <div class="flex flex-col gap-1">
          {#each trigger.conditions as cond, i (i)}
            <ConditionRow
              condition={cond}
              onupdate={(c) => updateCondition(i, c)}
              onremove={() => removeCondition(i)}
            />
          {/each}
        </div>
      {:else}
        <p class="text-xs text-surface-400 italic">No conditions — triggers on every matching event</p>
      {/if}
    </div>
  </div>
</aside>
