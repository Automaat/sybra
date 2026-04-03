<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'

  interface Props {
    task: task.Task
    onclick: () => void
  }

  const { task: t, onclick }: Props = $props()

  let dragging = $state(false)

  const triaging = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.name?.startsWith('triage:') && a.state === 'running')
  )

  const evaluating = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.name?.startsWith('eval:') && a.state === 'running')
  )

  function timeAgo(date: any): string {
    if (!date) return ''
    const now = Date.now()
    const then = new Date(date).getTime()
    const diff = Math.floor((now - then) / 1000)
    if (diff < 60) return 'just now'
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
    return `${Math.floor(diff / 86400)}d ago`
  }
</script>

<button
  type="button"
  draggable="true"
  class="w-full rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700 {dragging ? 'opacity-40' : ''}"
  onclick={onclick}
  ondragstart={(e) => {
    dragging = true
    e.dataTransfer!.setData('text/plain', t.id)
    e.dataTransfer!.effectAllowed = 'move'
  }}
  ondragend={() => { dragging = false }}
>
  <h3 class="mb-1.5 text-sm font-semibold leading-tight">{t.title}</h3>

  <div class="flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
      {t.agentMode}
    </span>

    {#if triaging}
      <span class="inline-flex items-center gap-1 rounded bg-primary-200 px-1.5 py-0.5 text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
        Triaging
      </span>
    {/if}

    {#if evaluating}
      <span class="inline-flex items-center gap-1 rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
        Evaluating
      </span>
    {/if}

    {#if t.tags?.length}
      {#each t.tags as tag}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
          {tag}
        </span>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(t.updatedAt)}</span>
  </div>
</button>
