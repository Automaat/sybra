<script lang="ts">
  import MobileSheet from '../components/shell/MobileSheet.svelte'
  import { loopStore } from '../stores/loops.svelte.js'
  import { loopagent } from '../../wailsjs/go/models.js'
  import { EventsOn } from '$lib/api'
  import { LoopAgentUpdated } from '../lib/events.js'

  let showForm = $state(false)
  let editing = $state<loopagent.LoopAgent | null>(null)

  // Form state
  let formName = $state('')
  let formPrompt = $state('')
  let formInterval = $state(3600)
  let formModel = $state('')
  let formEnabled = $state(true)
  let formAllowedTools = $state('')
  let formError = $state('')
  let saving = $state(false)

  const intervalPresets = [
    { label: '1h', value: 3600 },
    { label: '3h', value: 10800 },
    { label: '6h', value: 21600 },
    { label: '12h', value: 43200 },
    { label: '24h', value: 86400 },
  ]

  function formatInterval(sec: number): string {
    if (sec >= 86400) return `${Math.round(sec / 86400)}d`
    if (sec >= 3600) return `${Math.round(sec / 3600)}h`
    if (sec >= 60) return `${Math.round(sec / 60)}m`
    return `${sec}s`
  }

  function formatRelative(dateStr: string | any): string {
    if (!dateStr) return 'never'
    const d = new Date(dateStr)
    if (isNaN(d.getTime()) || d.getTime() === 0) return 'never'
    const diff = Date.now() - d.getTime()
    if (diff < 60_000) return 'just now'
    if (diff < 3_600_000) return `${Math.round(diff / 60_000)}m ago`
    if (diff < 86_400_000) return `${Math.round(diff / 3_600_000)}h ago`
    return `${Math.round(diff / 86_400_000)}d ago`
  }

  function formatCost(cost: number): string {
    if (!cost) return '-'
    return `$${cost.toFixed(2)}`
  }

  function openCreate() {
    editing = null
    formName = ''
    formPrompt = ''
    formInterval = 3600
    formModel = 'sonnet'
    formEnabled = true
    formAllowedTools = 'Bash, Read, Grep, Glob'
    formError = ''
    showForm = true
  }

  function openEdit(la: loopagent.LoopAgent) {
    editing = la
    formName = la.name
    formPrompt = la.prompt
    formInterval = la.intervalSec
    formModel = la.model
    formEnabled = la.enabled
    formAllowedTools = (la.allowedTools ?? []).join(', ')
    formError = ''
    showForm = true
  }

  async function save() {
    saving = true
    formError = ''
    try {
      const tools = formAllowedTools
        .split(',')
        .map((t) => t.trim())
        .filter(Boolean)

      if (editing) {
        const updated = new loopagent.LoopAgent({
          ...editing,
          name: formName,
          prompt: formPrompt,
          intervalSec: formInterval,
          model: formModel,
          enabled: formEnabled,
          allowedTools: tools,
        })
        await loopStore.update(updated)
      } else {
        await loopStore.create({
          name: formName,
          prompt: formPrompt,
          intervalSec: formInterval,
          model: formModel,
          enabled: formEnabled,
          allowedTools: tools,
          provider: 'claude',
        })
      }
      showForm = false
    } catch (e) {
      formError = String(e)
    } finally {
      saving = false
    }
  }

  async function toggleEnabled(la: loopagent.LoopAgent) {
    const updated = new loopagent.LoopAgent({ ...la, enabled: !la.enabled })
    await loopStore.update(updated)
  }

  async function runNow(la: loopagent.LoopAgent) {
    try {
      await loopStore.runNow(la.id)
    } catch (e) {
      loopStore.error = String(e)
    }
  }

  async function remove(la: loopagent.LoopAgent) {
    try {
      await loopStore.remove(la.id)
    } catch (e) {
      loopStore.error = String(e)
    }
  }

  $effect(() => {
    loopStore.load()
    const interval = setInterval(() => loopStore.load(), 10000)
    const unsub = EventsOn(LoopAgentUpdated, () => loopStore.load())
    return () => {
      clearInterval(interval)
      unsub()
    }
  })
</script>

<div class="flex flex-col gap-3 p-4 md:gap-4 md:p-6">
  <div class="flex items-center justify-between">
    <p class="text-sm opacity-60">
      {loopStore.list.length} loop{loopStore.list.length !== 1 ? 's' : ''}
    </p>
    <button
      type="button"
      class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
      onclick={openCreate}
    >
      + New Loop
    </button>
  </div>

  {#if loopStore.loading && loopStore.list.length === 0}
    <p class="text-center text-sm opacity-60">Loading loops...</p>
  {:else if loopStore.error}
    <p class="text-center text-sm text-error-500">{loopStore.error}</p>
  {:else if loopStore.list.length === 0}
    <div class="flex flex-col items-center gap-2 py-16 opacity-50">
      <p class="text-lg">No loop agents</p>
      <p class="text-sm">Create a recurring headless agent to run a skill on a schedule</p>
    </div>
  {:else}
    <div class="flex flex-col gap-2">
      {#each loopStore.list as la (la.id)}
        <div
          class="flex items-center justify-between rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800"
        >
          <div class="flex flex-col gap-1">
            <div class="flex items-center gap-2">
              <span class="font-mono text-sm font-semibold">{la.name}</span>
              <span
                class="rounded px-1.5 py-0.5 text-xs {la.enabled
                  ? 'bg-success-200 text-success-800 dark:bg-success-900 dark:text-success-200'
                  : 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-400'}"
              >
                {la.enabled ? 'active' : 'paused'}
              </span>
            </div>
            <div class="flex gap-4 text-xs opacity-50">
              <span>every {formatInterval(la.intervalSec)}</span>
              <span>last: {formatRelative(la.lastRunAt)}</span>
              <span>cost: {formatCost(la.lastRunCost)}</span>
              {#if la.model}
                <span>{la.model}</span>
              {/if}
            </div>
            <span class="text-xs font-mono opacity-40">{la.prompt}</span>
          </div>
          <div class="flex gap-2">
            <button
              type="button"
              class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
              onclick={() => runNow(la)}
            >
              Run now
            </button>
            <button
              type="button"
              class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
              onclick={() => toggleEnabled(la)}
            >
              {la.enabled ? 'Pause' : 'Enable'}
            </button>
            <button
              type="button"
              class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
              onclick={() => openEdit(la)}
            >
              Edit
            </button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-600"
              onclick={() => remove(la)}
            >
              Delete
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

<!-- Create / Edit modal -->
<MobileSheet open={showForm} onOpenChange={(o) => (showForm = o)} variant="bottom" title={editing ? 'Edit Loop Agent' : 'New Loop Agent'}>
  <div class="flex flex-col px-5 pb-5 md:px-6 md:pb-6">
      {#if formError}
        <p class="mb-3 text-sm text-error-500">{formError}</p>
      {/if}

      <div class="flex flex-col gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Name</span>
          <input
            type="text"
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            placeholder="sybra-self-monitor"
            bind:value={formName}
          />
        </label>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Prompt</span>
          <textarea
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm font-mono dark:border-surface-600 dark:bg-surface-700"
            rows="3"
            placeholder="/sybra-self-monitor"
            bind:value={formPrompt}
          ></textarea>
        </label>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Interval</span>
          <div class="flex gap-2">
            {#each intervalPresets as preset}
              <button
                type="button"
                class="rounded-lg px-3 py-1.5 text-sm {formInterval === preset.value
                  ? 'bg-primary-500 text-white'
                  : 'bg-surface-200 hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600'}"
                onclick={() => (formInterval = preset.value)}
              >
                {preset.label}
              </button>
            {/each}
            <input
              type="number"
              class="w-24 rounded-lg border border-surface-300 bg-surface-100 px-3 py-1.5 text-sm dark:border-surface-600 dark:bg-surface-700"
              placeholder="sec"
              min="60"
              bind:value={formInterval}
            />
          </div>
        </label>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Model</span>
          <select
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={formModel}
          >
            <option value="">default</option>
            <option value="sonnet">sonnet</option>
            <option value="opus">opus</option>
            <option value="haiku">haiku</option>
          </select>
        </label>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Allowed tools (comma-separated)</span>
          <input
            type="text"
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm font-mono dark:border-surface-600 dark:bg-surface-700"
            placeholder="Bash, Read, Grep, Glob"
            bind:value={formAllowedTools}
          />
        </label>

        <label class="flex items-center gap-2">
          <input type="checkbox" bind:checked={formEnabled} />
          <span class="text-sm">Enabled (start running on schedule)</span>
        </label>
      </div>

      <div class="sticky bottom-0 -mx-5 -mb-5 mt-5 flex justify-end gap-2 border-t border-surface-200 bg-surface-50/95 px-5 pt-3 pb-safe backdrop-blur dark:border-surface-800 dark:bg-surface-950/95 md:-mx-6 md:-mb-6 md:px-6 md:pb-4">
        <button
          type="button"
          class="tap rounded-lg bg-surface-200 px-4 py-2.5 text-sm font-medium active:bg-surface-300 dark:bg-surface-700 dark:active:bg-surface-600"
          onclick={() => (showForm = false)}
        >
          Cancel
        </button>
        <button
          type="button"
          class="tap rounded-lg bg-primary-500 px-5 py-2.5 text-sm font-medium text-white active:bg-primary-700 disabled:opacity-50"
          disabled={saving || !formName || !formPrompt}
          onclick={save}
        >
          {saving ? 'Saving...' : editing ? 'Update' : 'Create'}
        </button>
      </div>
  </div>
</MobileSheet>
