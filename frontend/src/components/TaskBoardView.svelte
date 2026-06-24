<script lang="ts">
  import { ChevronDown } from '@lucide/svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { BoardColumn } from '../lib/statuses.js'
  import TaskCard from './TaskCard.svelte'
  import InlineTaskAdd from './InlineTaskAdd.svelte'
  import { agentStore } from '../stores/agents.svelte.js'
  import { viewport } from '../lib/viewport.svelte.js'
  import { fly } from 'svelte/transition'
  import { flip } from 'svelte/animate'
  import { cubicOut } from 'svelte/easing'

  interface Props {
    visibleColumns: BoardColumn[]
    /** Tasks for a given column's statuses. Caller filters; we render. */
    columnTasks: (col: BoardColumn) => Task[]
    focusedTaskId: string | null
    collapsedColumns: Set<string>
    onselect: (id: string) => void
    onmove: (taskId: string, status: string) => void
    ontogglecolumn: (status: string) => void
  }

  const {
    visibleColumns,
    columnTasks,
    focusedTaskId,
    collapsedColumns,
    onselect,
    onmove,
    ontogglecolumn,
  }: Props = $props()

  let dragOverStatus = $state<string | null>(null)
  // Empty desktop columns collapse to a thin rail; clicking one expands it so
  // tasks can still be added there.
  let expandedEmpty = $state<Set<string>>(new Set())

  function expandEmpty(status: string) {
    expandedEmpty = new Set(expandedEmpty).add(status)
  }

  // Drop the expand override once a column has tasks, so it re-collapses to a
  // thin rail if it later empties out again (rather than staying expanded).
  $effect(() => {
    let changed = false
    const next = new Set(expandedEmpty)
    for (const status of expandedEmpty) {
      const col = visibleColumns.find((c) => c.status === status)
      if (col && columnTasks(col).length > 0) {
        next.delete(status)
        changed = true
      }
    }
    if (changed) expandedEmpty = next
  })

  // A static "agents working now" count per column — the live activity cue the
  // board otherwise lacks, without an animated pulse keeping a swarm in motion.
  function runningCount(tasks: Task[]): number {
    const list = agentStore.list ?? []
    return tasks.filter((t) => list.some((a) => a.taskId === t.id && a.state === 'running')).length
  }

  async function handleDrop(e: DragEvent, targetStatus: string) {
    e.preventDefault()
    dragOverStatus = null
    const taskId = e.dataTransfer?.getData('text/plain')
    if (!taskId) return
    onmove(taskId, targetStatus)
  }
</script>

<div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 md:flex-row md:gap-4 md:overflow-x-auto md:overflow-y-hidden md:p-6">
  {#each visibleColumns as col}
    {@const tasks = columnTasks(col)}
    {@const rc = runningCount(tasks)}
    {@const isCollapsed = !viewport.isDesktop && collapsedColumns.has(col.status)}
    {@const isThin = viewport.isDesktop && tasks.length === 0 && !expandedEmpty.has(col.status)}
    {#if isThin}
      <!-- Thin rail for an empty desktop column — stays a drop target; click to expand. -->
      <button
        type="button"
        data-col-status={col.status}
        onclick={() => expandEmpty(col.status)}
        ondragover={(e) => { e.preventDefault(); dragOverStatus = col.status }}
        ondragleave={() => { dragOverStatus = null }}
        ondrop={(e) => handleDrop(e, col.status)}
        title="{col.label} (empty) — click to expand"
        class="hidden shrink-0 flex-col items-center gap-2 rounded-lg border-t-4 bg-surface-100 py-3 transition-colors hover:bg-surface-200 dark:bg-surface-900 dark:hover:bg-surface-800 md:flex md:w-10 {col.border} {dragOverStatus === col.status ? 'ring-2 ring-primary-400 dark:ring-primary-500' : ''}"
      >
        <span class="rounded-full bg-surface-200 px-1.5 py-0.5 text-[10px] font-medium text-surface-400 dark:bg-surface-700">0</span>
        <span role="heading" aria-level="2" class="text-xs font-medium text-surface-400 [writing-mode:vertical-rl]">{col.label}</span>
      </button>
    {:else}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      data-col-status={col.status}
      class="flex w-full shrink-0 flex-col rounded-lg border-t-4 bg-surface-100 transition-shadow dark:bg-surface-900 md:min-w-[260px] md:flex-1 md:shrink {col.border} {dragOverStatus === col.status ? 'ring-2 ring-primary-400 dark:ring-primary-500' : ''}"
      ondragover={(e) => { e.preventDefault(); dragOverStatus = col.status }}
      ondragleave={() => { dragOverStatus = null }}
      ondrop={(e) => handleDrop(e, col.status)}
    >
      <button
        type="button"
        onclick={() => ontogglecolumn(col.status)}
        class="tap flex w-full items-center justify-between gap-2 px-3 py-2 text-left active:bg-surface-200 dark:active:bg-surface-800 md:cursor-default md:active:bg-transparent dark:md:active:bg-transparent"
      >
        <span class="flex items-center gap-2">
          <ChevronDown size={16} class="transition-transform md:hidden {isCollapsed ? '-rotate-90' : ''}" aria-hidden="true" />
          <h2 class="text-sm font-semibold">{col.label}</h2>
        </span>
        <span class="flex items-center gap-1.5">
          {#if rc > 0}
            <span
              class="inline-flex items-center gap-1 text-xs font-medium text-success-700 dark:text-success-400"
              title="{rc} agent(s) working in this column"
            >
              <span class="h-1.5 w-1.5 rounded-full bg-success-500"></span>
              {rc}
            </span>
          {/if}
          <span class="rounded-full bg-surface-200 px-2 py-0.5 text-xs font-medium dark:bg-surface-700">
            {tasks.length}
          </span>
        </span>
      </button>
      {#if !isCollapsed}
        <div class="flex flex-col gap-2 px-2 pb-2 md:flex-1 md:overflow-y-auto">
          {#each tasks as t (t.id)}
            <div
              in:fly={{ y: -12, duration: 150, easing: cubicOut }}
              out:fly={{ y: 12, duration: 200, easing: cubicOut }}
              animate:flip={{ duration: 200, easing: cubicOut }}
            >
              <TaskCard
                task={t}
                onclick={() => onselect(t.id)}
                focused={focusedTaskId === t.id}
                onstatuschange={(s) => onmove(t.id, s)}
              />
            </div>
          {/each}
        </div>
        <div class="px-2 pb-2">
          <InlineTaskAdd status={col.status} />
        </div>
      {/if}
    </div>
    {/if}
  {/each}
</div>
