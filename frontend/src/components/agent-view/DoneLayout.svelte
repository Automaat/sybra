<script lang="ts">
  import { fly } from 'svelte/transition'
  import { FileEdit, Terminal, GitBranch, GitPullRequest, Clock, DollarSign, ChevronDown, ChevronUp } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { task } from '../../../wailsjs/go/models.js'
  import type { TimestampedStreamEvent } from '$lib/timeline.js'
  import { summarizeAgent } from '$lib/agent-summary.js'
  import StreamOutput from '../StreamOutput.svelte'
  import ChatView from '../ChatView.svelte'

  interface Props {
    a: agent.Agent
    linkedTask?: task.Task | null
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: agent.ConvoEvent[]
  }

  const { a, linkedTask, streamOutputs, convoEvents }: Props = $props()

  let activityOpen = $state(false)

  const summary = $derived(summarizeAgent(streamOutputs, convoEvents))

  function formatDuration(start: any, end: any): string {
    if (!start) return '—'
    const s = new Date(start).getTime()
    const e = end ? new Date(end).getTime() : Date.now()
    const diff = Math.round((e - s) / 1000)
    if (diff < 60) return `${diff}s`
    const m = Math.floor(diff / 60)
    const rem = diff % 60
    return rem > 0 ? `${m}m ${rem}s` : `${m}m`
  }
</script>

<div class="flex flex-col gap-4">
  <div in:fly={{ y: 8, duration: 150 }} class="rounded-xl border border-success-300 bg-success-50 p-6 dark:border-success-700 dark:bg-success-950/40">
    <div class="mb-4 flex items-center gap-3">
      <span class="rounded-full bg-success-200 px-3 py-1 text-sm font-semibold text-success-800 dark:bg-success-800 dark:text-success-200">
        Done
      </span>
      {#if summary.finalMessage}
        <p class="min-w-0 flex-1 truncate text-sm text-surface-700 dark:text-surface-300" title={summary.finalMessage}>
          {summary.finalMessage}
        </p>
      {/if}
    </div>

    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-4">
      <div class="flex items-center gap-2">
        <Clock size={16} class="shrink-0 text-surface-400" />
        <div>
          <p class="text-xs text-surface-500">Duration</p>
          <p class="text-sm font-medium">{formatDuration(a.startedAt, a.lastEventAt)}</p>
        </div>
      </div>

      {#if a.costUsd > 0}
        <div class="flex items-center gap-2">
          <DollarSign size={16} class="shrink-0 text-surface-400" />
          <div>
            <p class="text-xs text-surface-500">Cost</p>
            <p class="text-sm font-medium">${a.costUsd.toFixed(3)}</p>
          </div>
        </div>
      {/if}

      {#if a.turnCount && a.turnCount > 0}
        <div class="flex flex-col">
          <p class="text-xs text-surface-500">Turns</p>
          <p class="text-sm font-medium">{a.turnCount}</p>
        </div>
      {/if}

      {#if summary.filesEdited.length > 0}
        <div class="flex items-center gap-2">
          <FileEdit size={16} class="shrink-0 text-surface-400" />
          <div>
            <p class="text-xs text-surface-500">Files edited</p>
            <p class="text-sm font-medium">{summary.filesEdited.length}</p>
          </div>
        </div>
      {/if}

      {#if summary.commandsRun > 0}
        <div class="flex items-center gap-2">
          <Terminal size={16} class="shrink-0 text-surface-400" />
          <div>
            <p class="text-xs text-surface-500">Commands run</p>
            <p class="text-sm font-medium">{summary.commandsRun}</p>
          </div>
        </div>
      {/if}

      {#if linkedTask?.branch}
        <div class="flex items-center gap-2">
          <GitBranch size={16} class="shrink-0 text-surface-400" />
          <div>
            <p class="text-xs text-surface-500">Branch</p>
            <p class="truncate font-mono text-xs font-medium">{linkedTask.branch}</p>
          </div>
        </div>
      {/if}

      {#if (linkedTask?.prNumber ?? 0) > 0}
        <div class="flex items-center gap-2">
          <GitPullRequest size={16} class="shrink-0 text-surface-400" />
          <div>
            <p class="text-xs text-surface-500">PR</p>
            <p class="text-sm font-medium">#{linkedTask?.prNumber}</p>
          </div>
        </div>
      {/if}
    </div>

    {#if summary.filesEdited.length > 0}
      <div class="mt-4">
        <p class="mb-1.5 text-xs font-medium text-surface-500">Files changed</p>
        <ul class="flex flex-wrap gap-1.5">
          {#each summary.filesEdited as f}
            <li class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{f}</li>
          {/each}
        </ul>
      </div>
    {:else}
      <p class="mt-4 text-sm text-surface-400">Agent completed without modifying files.</p>
    {/if}
  </div>

  <div>
    <button
      type="button"
      class="flex items-center gap-1.5 text-sm text-surface-500 hover:text-surface-700 dark:hover:text-surface-300"
      onclick={() => { activityOpen = !activityOpen }}
    >
      {#if activityOpen}
        <ChevronUp size={16} />
      {:else}
        <ChevronDown size={16} />
      {/if}
      {activityOpen ? 'Hide activity' : 'Show full activity'}
    </button>

    {#if activityOpen}
      <div class="mt-3" in:fly={{ y: 4, duration: 120 }}>
        {#if a.mode === 'interactive'}
          <ChatView agentId={a.id} agentState={a.state} costUsd={a.costUsd} inputTokens={a.inputTokens ?? 0} outputTokens={a.outputTokens ?? 0} />
        {:else}
          <StreamOutput agentId={a.id} />
        {/if}
      </div>
    {/if}
  </div>
</div>
