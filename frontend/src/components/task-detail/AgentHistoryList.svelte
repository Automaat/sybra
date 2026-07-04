<script lang="ts">
  import { ChevronDown, RefreshCw } from '@lucide/svelte'
  import type { ConvoEvent, StreamEvent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { GetAgentRunLog, GetAgentRunConvoLog } from '$lib/api'
  import { formatDateTime } from '../../lib/dates.js'
  import { formatCostShort } from '../../lib/cost.js'
  import { runStateClasses, runRoleLabel, runRoleClasses } from '../../lib/agent-run.js'
  import StreamOutput from '../StreamOutput.svelte'
  import MessageBubble from '../MessageBubble.svelte'
  import ProviderLogo from '../ProviderLogo.svelte'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let expandedRun = $state<string | null>(null)
  // Headless agents persist StreamEvents (flat); interactive agents persist
  // ConvoEvents (structured tool_use/tool_result blocks). Keep both keyed by
  // agent ID — fetched only when the history row expands so we don't pay the
  // parse cost for rows the user never opens.
  let runLogStreamEvents = $state<Map<string, StreamEvent[]>>(new Map())
  let runLogConvoEvents = $state<Map<string, ConvoEvent[]>>(new Map())
  let runLogLoading = $state<Set<string>>(new Set())
  let runLogError = $state<Map<string, string>>(new Map())

  const pastRuns = $derived(
    (task.agentRuns ?? []).filter((r) => r.state !== 'running').reverse(),
  )

  async function loadRunLog(agentId: string) {
    const run = (task.agentRuns ?? []).find((r) => r.agentId === agentId)
    const isInteractive = run?.mode === 'interactive'
    if (runLogLoading.has(agentId) || !task.id) return
    runLogLoading = new Set([...runLogLoading, agentId])
    try {
      if (isInteractive) {
        const events = await GetAgentRunConvoLog(task.id, agentId)
        runLogConvoEvents = new Map([...runLogConvoEvents, [agentId, events ?? []]])
      } else {
        const events = await GetAgentRunLog(task.id, agentId)
        runLogStreamEvents = new Map([...runLogStreamEvents, [agentId, events ?? []]])
      }
      const nextErr = new Map(runLogError)
      nextErr.delete(agentId)
      runLogError = nextErr
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      console.warn('[task] failed to load run log', { agentId, isInteractive, err: msg })
      runLogError = new Map([...runLogError, [agentId, msg]])
      if (isInteractive) {
        runLogConvoEvents = new Map([...runLogConvoEvents, [agentId, []]])
      } else {
        runLogStreamEvents = new Map([...runLogStreamEvents, [agentId, []]])
      }
    }
    const next = new Set(runLogLoading)
    next.delete(agentId)
    runLogLoading = next
  }

  async function toggleRunLog(agentId: string) {
    if (expandedRun === agentId) {
      expandedRun = null
      return
    }
    expandedRun = agentId
    const run = (task.agentRuns ?? []).find((r) => r.agentId === agentId)
    const isInteractive = run?.mode === 'interactive'
    const alreadyLoaded = isInteractive
      ? runLogConvoEvents.has(agentId)
      : runLogStreamEvents.has(agentId)
    if (alreadyLoaded) return
    await loadRunLog(agentId)
  }

  // Drop the cached empty result + error so the next load re-fetches.
  async function retryRunLog(agentId: string) {
    const run = (task.agentRuns ?? []).find((r) => r.agentId === agentId)
    if (run?.mode === 'interactive') {
      const m = new Map(runLogConvoEvents); m.delete(agentId); runLogConvoEvents = m
    } else {
      const m = new Map(runLogStreamEvents); m.delete(agentId); runLogStreamEvents = m
    }
    const e = new Map(runLogError); e.delete(agentId); runLogError = e
    await loadRunLog(agentId)
  }
</script>

{#if pastRuns.length > 0}
  <hr class="border-surface-300 dark:border-surface-600" />
  <div class="flex flex-col gap-3">
    <span class="text-sm font-medium text-surface-500">Agent History</span>
    {#each pastRuns as run (run.agentId)}
      <div class="rounded-lg border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-800">
        <button
          type="button"
          class="flex w-full items-center justify-between px-3 py-2 text-left text-xs"
          onclick={() => toggleRunLog(run.agentId)}
        >
          <div class="flex items-center gap-2">
            {#if run.provider}
              <ProviderLogo provider={run.provider} class="h-3.5 w-3.5 text-surface-400" />
            {/if}
            {#if runRoleLabel(run.role)}
              <!-- Role is the signal ("which run is the plan/review/eval"); the
                   opaque agentId is demoted to a quiet mono id beside it. -->
              <span class="rounded px-1.5 py-0.5 font-medium {runRoleClasses(run.role)}">{runRoleLabel(run.role)}</span>
            {/if}
            <span class="font-mono text-[11px] text-surface-400">{run.agentId}</span>
            <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{run.mode}</span>
            <span class="rounded px-1.5 py-0.5 {runStateClasses(run.state || 'running')}">
              {run.state || 'running'}
            </span>
          </div>
          <div class="flex items-center gap-3 text-surface-400">
            {#if run.costUsd > 0}
              <span class="tabular-nums">{formatCostShort(run.costUsd)}</span>
            {/if}
            <span>{formatDateTime(run.startedAt)}</span>
            <ChevronDown size={16} class="transition-transform {expandedRun === run.agentId ? 'rotate-180' : ''}" />
          </div>
        </button>
        {#if expandedRun === run.agentId}
          <div class="border-t border-surface-300 px-3 py-2 dark:border-surface-600">
            {#if run.prompt}
              <details class="mb-3 rounded-lg border border-surface-300 bg-surface-100 dark:border-surface-600 dark:bg-surface-900">
                <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Prompt</summary>
                <pre class="max-h-64 overflow-y-auto whitespace-pre-wrap border-t border-surface-300 px-3 py-2 text-xs text-surface-700 dark:border-surface-600 dark:text-surface-300">{run.prompt}</pre>
              </details>
            {/if}
            {#if runLogLoading.has(run.agentId)}
              <p class="py-4 text-center text-xs text-surface-500">Loading log...</p>
            {:else if run.mode === 'interactive' && (runLogConvoEvents.get(run.agentId)?.length ?? 0) > 0}
              <div class="flex max-h-[60dvh] md:max-h-[600px] flex-col gap-3 overflow-y-auto px-1 py-1">
                {#each runLogConvoEvents.get(run.agentId) ?? [] as event, i (i)}
                  <MessageBubble {event} />
                {/each}
              </div>
            {:else if run.mode !== 'interactive' && (runLogStreamEvents.get(run.agentId)?.length ?? 0) > 0}
              <StreamOutput staticEvents={runLogStreamEvents.get(run.agentId)} />
            {:else if runLogError.has(run.agentId)}
              <div class="flex flex-col items-center gap-2 py-4 text-center text-xs text-surface-500">
                <p>Couldn't load this run's log — it may not have been written yet.</p>
                <div class="flex items-center gap-2">
                  <button
                    type="button"
                    class="inline-flex items-center gap-1 rounded border border-surface-300 px-2 py-1 font-medium text-surface-600 transition-colors hover:bg-surface-200 dark:border-surface-600 dark:text-surface-300 dark:hover:bg-surface-700"
                    onclick={() => retryRunLog(run.agentId)}
                  >
                    <RefreshCw size={12} /> Retry
                  </button>
                  {#if run.logFile}
                    <span class="font-mono text-surface-400" title={run.logFile}>{run.logFile}</span>
                  {/if}
                </div>
                <details class="w-full">
                  <summary class="cursor-pointer text-surface-400">Details</summary>
                  <p class="mt-1 break-words font-mono text-surface-400">{runLogError.get(run.agentId)}</p>
                </details>
              </div>
            {:else if run.result}
              <pre class="max-h-[60dvh] md:max-h-[600px] overflow-y-auto whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-100 p-3 text-xs text-surface-700 dark:border-surface-600 dark:bg-surface-900 dark:text-surface-300">{run.result}</pre>
            {:else}
              <p class="py-4 text-center text-xs text-surface-500">No output available</p>
            {/if}
          </div>
        {/if}
      </div>
    {/each}
  </div>
{/if}
