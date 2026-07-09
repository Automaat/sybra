<script lang="ts">
  import { untrack } from 'svelte'
  import { CircleDot, Lightbulb, OctagonAlert, CircleX } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { ProgressEntry } from '../../../bindings/github.com/Automaat/sybra/internal/artifact/models.js'
  import { ListTaskProgress } from '$lib/api'
  import { formatDateTime } from '../../lib/dates.js'
  import { renderMarkdown } from '../../lib/markdown.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let entries = $state<ProgressEntry[]>([])
  let loading = $state(false)
  let loadInFlight = false
  let error = $state('')

  const kindMeta: Record<string, { label: string; icon: typeof CircleDot; classes: string }> = {
    progress: { label: 'Progress', icon: CircleDot, classes: 'text-surface-600 dark:text-surface-300' },
    decision: { label: 'Decision', icon: Lightbulb, classes: 'text-primary-500' },
    blocker: { label: 'Blocker', icon: OctagonAlert, classes: 'text-warning-500' },
    failure: { label: 'Failure', icon: CircleX, classes: 'text-error-500' },
  }

  function meta(kind: string) {
    return kindMeta[kind] ?? kindMeta.progress
  }

  async function load(taskID: string) {
    if (!taskID || loadInFlight) return
    loadInFlight = true
    loading = true
    try {
      entries = (await ListTaskProgress(taskID)) ?? []
      error = ''
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      loadInFlight = false
      loading = false
    }
  }

  $effect(() => {
    const id = task.id
    void task.updatedAt
    untrack(() => load(id))
  })
</script>

<div class="flex flex-col gap-3">
  {#if error}
    <p class="text-sm text-error-500">Failed to load progress: {error}</p>
  {:else if entries.length === 0}
    <p class="text-sm opacity-60">No progress recorded yet. Agents post decisions, blockers, and updates here as they work.</p>
  {:else}
    {#each entries as e (e.ts + e.message)}
      {@const m = meta(e.kind)}
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-3 dark:border-surface-600 dark:bg-surface-800">
        <div class="mb-1 flex items-center gap-2 text-xs">
          <span class={`inline-flex items-center gap-1 font-medium ${m.classes}`}>
            <m.icon size={14} />
            {m.label}
          </span>
          {#if e.role}
            <span class="opacity-50">· {e.role}</span>
          {/if}
          <span class="ml-auto opacity-50">{formatDateTime(e.ts)}</span>
        </div>
        <div class="markdown-body text-sm">{@html renderMarkdown(e.message)}</div>
      </div>
    {/each}
  {/if}
</div>
