<script lang="ts">
  import { fly } from 'svelte/transition'
  import { FolderOpen, GitBranch, GitPullRequest, ChevronDown, ChevronUp } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { task } from '../../../wailsjs/go/models.js'
  import type { TimestampedStreamEvent } from '$lib/timeline.js'
  import { OpenWorktree } from '$lib/api.js'
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
  let openError = $state('')

  const summary = $derived(summarizeAgent(streamOutputs, convoEvents))

  async function handleOpenWorktree() {
    if (!linkedTask?.id) return
    openError = ''
    try {
      await OpenWorktree(linkedTask.id)
    } catch (e) {
      openError = String(e)
    }
  }
</script>

<div class="flex flex-col gap-4">
  <div in:fly={{ y: 8, duration: 150 }} class="rounded-xl border border-warning-300 bg-warning-50 p-6 dark:border-warning-700 dark:bg-warning-950/40">
    <div class="mb-4 flex items-center gap-3">
      <span class="rounded-full bg-warning-200 px-3 py-1 text-sm font-semibold text-warning-800 dark:bg-warning-800 dark:text-warning-200">
        In Review
      </span>
      <p class="text-sm text-surface-600 dark:text-surface-300">
        Agent has finished — review the output before merging
      </p>
    </div>

    {#if summary.finalMessage}
      <div class="mb-4 rounded-lg bg-surface-100 p-4 dark:bg-surface-800">
        <p class="text-xs font-medium text-surface-500 mb-1">Final message</p>
        <p class="text-sm text-surface-800 dark:text-surface-200 whitespace-pre-wrap">{summary.finalMessage}</p>
      </div>
    {/if}

    <div class="flex flex-wrap items-center gap-3">
      {#if linkedTask?.branch}
        <div class="flex items-center gap-1.5 rounded-lg bg-surface-200 px-3 py-2 dark:bg-surface-700">
          <GitBranch size={16} class="text-surface-500" />
          <span class="font-mono text-sm">{linkedTask.branch}</span>
        </div>
      {/if}

      {#if (linkedTask?.prNumber ?? 0) > 0}
        <div class="flex items-center gap-1.5 rounded-lg bg-surface-200 px-3 py-2 dark:bg-surface-700">
          <GitPullRequest size={16} class="text-surface-500" />
          <span class="text-sm font-medium">PR #{linkedTask?.prNumber}</span>
        </div>
      {/if}

      {#if linkedTask?.id}
        <button
          type="button"
          class="flex items-center gap-1.5 rounded-lg bg-primary-600 px-3 py-2 text-sm font-medium text-white hover:bg-primary-700 disabled:opacity-50"
          onclick={handleOpenWorktree}
        >
          <FolderOpen size={16} />
          Open worktree
        </button>
      {/if}
    </div>

    {#if !linkedTask?.branch && !linkedTask?.prNumber}
      <p class="mt-4 text-sm text-surface-400">No branch or PR recorded — open the linked task for context.</p>
    {/if}

    {#if openError}
      <p class="mt-2 text-xs text-error-500">{openError}</p>
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
