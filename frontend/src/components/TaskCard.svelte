<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import StatusBadge from './StatusBadge.svelte'

  interface Props {
    task: task.Task
    onclick: () => void
  }

  const { task: t, onclick }: Props = $props()

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
  class="w-full rounded-lg border border-surface-300 bg-surface-50 p-4 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={onclick}
>
  <div class="mb-2 flex items-start justify-between gap-2">
    <h3 class="text-sm font-semibold leading-tight">{t.title}</h3>
    <StatusBadge status={t.status} />
  </div>

  <div class="flex items-center gap-2 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
      {t.agentMode}
    </span>

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
