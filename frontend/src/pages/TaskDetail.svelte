<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import type { task } from '../../wailsjs/go/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import StatusBadge from '../components/StatusBadge.svelte'

  interface Props {
    taskId: string
    onback: () => void
  }

  const { taskId, onback }: Props = $props()

  let t = $state<task.Task | null>(null)
  let error = $state('')

  const statusOptions = [
    { value: 'todo', label: 'Todo' },
    { value: 'in-progress', label: 'In Progress' },
    { value: 'done', label: 'Done' },
    { value: 'blocked', label: 'Blocked' },
  ]

  $effect(() => {
    loadTask()
  })

  async function loadTask() {
    try {
      t = await taskStore.get(taskId)
    } catch (e) {
      error = String(e)
    }
  }

  async function updateStatus(value: string) {
    if (!t || t.status === value) return
    try {
      t = await taskStore.update(taskId, { status: value })
    } catch (e) {
      error = String(e)
    }
  }

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-6 p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
    </svg>
    Back to tasks
  </button>

  {#if error}
    <p class="text-sm text-error-500">{error}</p>
  {/if}

  {#if t}
    <div class="flex flex-col gap-6">
      <div class="flex items-start justify-between gap-4">
        <h1 class="text-2xl font-bold">{t.title}</h1>
        <StatusBadge status={t.status} />
      </div>

      <div class="flex flex-col gap-1">
        <span class="text-sm font-medium text-surface-500">Status</span>
        <SegmentedControl
          value={t.status}
          onValueChange={(details) => { if (details.value) updateStatus(details.value) }}
        >
          <SegmentedControl.Indicator />
          {#each statusOptions as s}
            <SegmentedControl.Item value={s.value}>
              <SegmentedControl.ItemText>{s.label}</SegmentedControl.ItemText>
              <SegmentedControl.ItemHiddenInput />
            </SegmentedControl.Item>
          {/each}
        </SegmentedControl>
      </div>

      <div class="flex gap-6 text-sm">
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Agent Mode</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{t.agentMode}</span>
        </div>

        {#if t.tags?.length}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Tags</span>
            <div class="flex gap-1">
              {#each t.tags as tag}
                <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{tag}</span>
              {/each}
            </div>
          </div>
        {/if}

        {#if t.allowedTools?.length}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Allowed Tools</span>
            <div class="flex gap-1">
              {#each t.allowedTools as tool}
                <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{tool}</span>
              {/each}
            </div>
          </div>
        {/if}
      </div>

      {#if t.body}
        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium text-surface-500">Description</span>
          <pre class="whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-100 p-4 text-sm dark:border-surface-600 dark:bg-surface-900">{t.body}</pre>
        </div>
      {/if}

      <div class="flex gap-6 text-xs text-surface-400">
        <span>Created: {formatDate(t.createdAt)}</span>
        <span>Updated: {formatDate(t.updatedAt)}</span>
      </div>
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
