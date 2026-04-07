<script lang="ts">
  import type { workflow } from '../../../wailsjs/go/models.js'

  interface Props {
    step: workflow.Step | null
    onupdate: (step: workflow.Step) => void
    ondelete: (stepId: string) => void
  }

  const { step, onupdate, ondelete }: Props = $props()

  function updateField(field: string, value: any) {
    if (!step) return
    const updated = structuredClone(step)
    const parts = field.split('.')
    let obj: any = updated
    for (let i = 0; i < parts.length - 1; i++) {
      if (!obj[parts[i]]) obj[parts[i]] = {}
      obj = obj[parts[i]]
    }
    obj[parts[parts.length - 1]] = value
    onupdate(updated)
  }
</script>

{#if step}
  <div class="flex flex-col gap-4 border-l border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800" style="width: 320px;">
    <div class="flex items-center justify-between">
      <h3 class="text-sm font-semibold">Step Config</h3>
      <button
        type="button"
        class="rounded p-1 text-error-500 hover:bg-error-100 dark:hover:bg-error-900/40"
        onclick={() => ondelete(step.id)}
        title="Delete step"
      >
        <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
        </svg>
      </button>
    </div>

    <label class="flex flex-col gap-1">
      <span class="text-xs font-medium text-surface-500">ID</span>
      <input
        type="text"
        class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
        value={step.id}
        onchange={(e) => updateField('id', e.currentTarget.value)}
      />
    </label>

    <label class="flex flex-col gap-1">
      <span class="text-xs font-medium text-surface-500">Name</span>
      <input
        type="text"
        class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
        value={step.name}
        onchange={(e) => updateField('name', e.currentTarget.value)}
      />
    </label>

    <label class="flex flex-col gap-1">
      <span class="text-xs font-medium text-surface-500">Type</span>
      <select
        class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
        value={step.type}
        onchange={(e) => updateField('type', e.currentTarget.value)}
      >
        <option value="run_agent">Run Agent</option>
        <option value="wait_human">Wait Human</option>
        <option value="set_status">Set Status</option>
        <option value="condition">Condition</option>
        <option value="shell">Shell</option>
        <option value="parallel">Parallel</option>
      </select>
    </label>

    {#if step.type === 'run_agent'}
      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Role</span>
        <select
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.role ?? ''}
          onchange={(e) => updateField('config.role', e.currentTarget.value)}
        >
          <option value="">implementation</option>
          <option value="triage">triage</option>
          <option value="plan">plan</option>
          <option value="eval">eval</option>
          <option value="pr-fix">pr-fix</option>
          <option value="review">review</option>
        </select>
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Mode</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.mode ?? ''}
          onchange={(e) => updateField('config.mode', e.currentTarget.value)}
          placeholder="headless"
        />
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Model</span>
        <select
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.model ?? 'sonnet'}
          onchange={(e) => updateField('config.model', e.currentTarget.value)}
        >
          <option value="sonnet">sonnet</option>
          <option value="opus">opus</option>
          <option value="haiku">haiku</option>
        </select>
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Prompt</span>
        <textarea
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          rows="6"
          value={step.config?.prompt ?? ''}
          onchange={(e) => updateField('config.prompt', e.currentTarget.value)}
        ></textarea>
      </label>
    {/if}

    {#if step.type === 'wait_human'}
      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Status to set</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.status ?? ''}
          onchange={(e) => updateField('config.status', e.currentTarget.value)}
          placeholder="plan-review"
        />
      </label>
    {/if}

    {#if step.type === 'set_status'}
      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Status</span>
        <select
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.status ?? ''}
          onchange={(e) => updateField('config.status', e.currentTarget.value)}
        >
          <option value="new">new</option>
          <option value="todo">todo</option>
          <option value="planning">planning</option>
          <option value="plan-review">plan-review</option>
          <option value="in-progress">in-progress</option>
          <option value="in-review">in-review</option>
          <option value="human-required">human-required</option>
          <option value="done">done</option>
        </select>
      </label>
    {/if}

    {#if step.type === 'shell'}
      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Command</span>
        <textarea
          class="rounded border border-surface-300 bg-white px-2 py-1 font-mono text-sm dark:border-surface-600 dark:bg-surface-700"
          rows="4"
          value={step.config?.command ?? ''}
          onchange={(e) => updateField('config.command', e.currentTarget.value)}
        ></textarea>
      </label>
    {/if}
  </div>
{/if}
