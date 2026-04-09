<script lang="ts">
  import { workflow } from '../../../wailsjs/go/models.js'
  import ConditionRow from './ConditionRow.svelte'

  interface Props {
    step: workflow.Step | null
    allStepIds: string[]
    onupdate: (step: workflow.Step) => void
    ondelete: (stepId: string) => void
  }

  const { step, allStepIds, onupdate, ondelete }: Props = $props()

  function emit(updated: workflow.Step) {
    onupdate(updated)
  }

  function updateField(field: string, value: unknown) {
    if (!step) return
    const updated = structuredClone($state.snapshot(step)) as workflow.Step
    const parts = field.split('.')
    let obj: Record<string, unknown> = updated as unknown as Record<string, unknown>
    for (let i = 0; i < parts.length - 1; i++) {
      if (!obj[parts[i]]) obj[parts[i]] = {}
      obj = obj[parts[i]] as Record<string, unknown>
    }
    obj[parts[parts.length - 1]] = value
    emit(updated)
  }

  function updateConfig(patch: Partial<workflow.StepConfig>) {
    if (!step) return
    const snap = $state.snapshot(step) as workflow.Step
    const updated = new workflow.Step({
      ...snap,
      config: new workflow.StepConfig({ ...snap.config, ...patch }),
    })
    emit(updated)
  }

  function updateTransition(idx: number, t: workflow.Transition) {
    if (!step) return
    const next = [...(step.next ?? [])]
    next[idx] = t
    emit(new workflow.Step({ ...($state.snapshot(step) as workflow.Step), next }))
  }

  function removeTransition(idx: number) {
    if (!step) return
    const next = [...(step.next ?? [])]
    next.splice(idx, 1)
    emit(new workflow.Step({ ...($state.snapshot(step) as workflow.Step), next }))
  }

  function addTransition() {
    if (!step) return
    const next = [...(step.next ?? [])]
    next.push(new workflow.Transition({ goto: '' }))
    emit(new workflow.Step({ ...($state.snapshot(step) as workflow.Step), next }))
  }

  function setTransitionGoto(idx: number, goto: string) {
    const t = step?.next?.[idx]
    if (!t) return
    updateTransition(idx, new workflow.Transition({ when: t.when, goto }))
  }

  function toggleTransitionWhen(idx: number, enabled: boolean) {
    const t = step?.next?.[idx]
    if (!t) return
    updateTransition(
      idx,
      new workflow.Transition({
        when: enabled ? new workflow.Condition({ field: '', operator: 'equals', value: '' }) : undefined,
        goto: t.goto,
      }),
    )
  }

  function updateTransitionWhen(idx: number, c: workflow.Condition) {
    const t = step?.next?.[idx]
    if (!t) return
    updateTransition(idx, new workflow.Transition({ when: c, goto: t.goto }))
  }

  function joinTools(tools?: string[]): string {
    return (tools ?? []).join(', ')
  }

  function splitTools(value: string): string[] {
    return value
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s.length > 0)
  }

  function updateCheck(c: workflow.Condition) {
    updateConfig({ check: c })
  }
</script>

{#if step}
  <div class="flex flex-col gap-4 overflow-y-auto border-l border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800" style="width: 340px;">
    <div class="flex items-center justify-between">
      <h3 class="text-sm font-semibold">Step Config</h3>
      <button
        type="button"
        class="rounded p-1 text-error-500 hover:bg-error-100 dark:hover:bg-error-900/40"
        onclick={() => ondelete(step.id)}
        title="Delete step"
        aria-label="Delete step"
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

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Allowed tools (comma-separated)</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 font-mono text-xs dark:border-surface-600 dark:bg-surface-700"
          value={joinTools(step.config?.allowedTools)}
          placeholder="Read, Grep, Bash"
          onchange={(e) => updateConfig({ allowedTools: splitTools(e.currentTarget.value) })}
        />
      </label>

      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={step.config?.needsWorktree ?? false}
          onchange={(e) => updateField('config.needsWorktree', e.currentTarget.checked)}
        />
        <span class="text-xs font-medium text-surface-500">Needs worktree</span>
      </label>

      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={step.config?.reuseAgent ?? false}
          onchange={(e) => updateField('config.reuseAgent', e.currentTarget.checked)}
        />
        <span class="text-xs font-medium text-surface-500">Reuse agent</span>
      </label>

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Max retries (0-10)</span>
        <input
          type="number"
          min="0"
          max="10"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.maxRetries ?? 0}
          onchange={(e) => {
            const v = Math.max(0, Math.min(10, Number(e.currentTarget.value) || 0))
            updateField('config.maxRetries', v)
          }}
        />
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

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Human actions (comma-separated)</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={joinTools(step.config?.humanActions)}
          placeholder="approve, reject"
          onchange={(e) => updateConfig({ humanActions: splitTools(e.currentTarget.value) })}
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

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Status reason</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.statusReason ?? ''}
          onchange={(e) => updateField('config.statusReason', e.currentTarget.value)}
        />
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

      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Working directory</span>
        <input
          type="text"
          class="rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={step.config?.dir ?? ''}
          onchange={(e) => updateField('config.dir', e.currentTarget.value)}
          placeholder="optional"
        />
      </label>
    {/if}

    {#if step.type === 'condition'}
      <div class="flex flex-col gap-1">
        <span class="text-xs font-medium text-surface-500">Check</span>
        <ConditionRow
          condition={step.config?.check ?? new workflow.Condition({ field: '', operator: 'equals', value: '' })}
          onupdate={updateCheck}
        />
      </div>
    {/if}

    <!-- Transitions section -->
    <div class="flex flex-col gap-2 border-t border-surface-300 pt-3 dark:border-surface-600">
      <div class="flex items-center justify-between">
        <span class="text-xs font-semibold uppercase tracking-wide text-surface-500">Transitions</span>
        <button
          type="button"
          class="rounded bg-surface-200 px-2 py-0.5 text-xs hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
          onclick={addTransition}
        >
          + Add
        </button>
      </div>
      {#if step.next?.length}
        {#each step.next as t, i (i)}
          <div class="flex flex-col gap-1 rounded border border-surface-300 bg-white p-2 dark:border-surface-600 dark:bg-surface-700/50">
            <div class="flex items-center gap-1">
              <span class="text-xs text-surface-500">&rarr;</span>
              <select
                class="flex-1 rounded border border-surface-300 bg-white px-1 py-1 text-xs dark:border-surface-600 dark:bg-surface-700"
                value={t.goto ?? ''}
                onchange={(e) => setTransitionGoto(i, e.currentTarget.value)}
              >
                <option value="">&lt;end workflow&gt;</option>
                {#each allStepIds.filter((id) => id !== step.id) as id (id)}
                  <option value={id}>{id}</option>
                {/each}
              </select>
              <button
                type="button"
                class="shrink-0 rounded p-1 text-error-500 hover:bg-error-100 dark:hover:bg-error-900/40"
                onclick={() => removeTransition(i)}
                title="Remove transition"
                aria-label="Remove transition"
              >
                <svg class="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <label class="flex items-center gap-1 text-xs">
              <input
                type="checkbox"
                checked={!!t.when}
                onchange={(e) => toggleTransitionWhen(i, e.currentTarget.checked)}
              />
              <span class="text-surface-500">conditional (when)</span>
            </label>
            {#if t.when}
              <ConditionRow
                condition={t.when}
                onupdate={(c) => updateTransitionWhen(i, c)}
                fieldPlaceholder="task.status"
              />
            {/if}
          </div>
        {/each}
      {:else}
        <p class="text-xs italic text-surface-400">No transitions &mdash; step ends the workflow</p>
      {/if}
    </div>
  </div>
{/if}
