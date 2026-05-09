<script lang="ts">
  import { GitPullRequest, RotateCcw } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { reviewStore } from '../../stores/reviews.svelte.js'
  import { BrowserOpenURL, ResumeInClaudeCode } from '$lib/api'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const linkedPRs = $derived(reviewStore.byTask(task))

  const hasPR = $derived(linkedPRs.length > 0 || (task.prNumber > 0 && !!task.projectId))

  let resuming = $state(false)

  async function resumeInCC() {
    resuming = true
    try {
      await ResumeInClaudeCode(task.id)
    } finally {
      resuming = false
    }
  }
</script>

{#if linkedPRs.length > 0}
  <div class="flex flex-col gap-2">
    <span class="text-sm font-medium text-surface-500">Pull Requests</span>
    {#each linkedPRs as pr (pr.number)}
      <button
        type="button"
        class="flex w-full items-start justify-between gap-3 rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
        onclick={() => BrowserOpenURL(pr.url)}
      >
        <div class="flex items-center gap-2">
          {#if pr.ciStatus}
            <span
              class="inline-block h-2.5 w-2.5 shrink-0 rounded-full {pr.ciStatus === 'SUCCESS' ? 'bg-success-500' : pr.ciStatus === 'FAILURE' ? 'bg-error-500' : 'bg-warning-500'}"
              title="CI: {pr.ciStatus.toLowerCase()}"
            ></span>
          {/if}
          <GitPullRequest size={16} class="shrink-0 text-warning-500" />
          <div class="flex flex-col">
            <span class="text-sm font-semibold">{pr.title}</span>
            <span class="text-xs text-surface-500">{pr.repository}#{pr.number} by {pr.author}</span>
          </div>
        </div>
        <div class="flex shrink-0 items-center gap-1.5">
          {#if pr.isDraft}
            <span class="rounded bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">Draft</span>
          {/if}
          {#if pr.reviewDecision === 'APPROVED'}
            <span class="rounded bg-success-500/15 px-1.5 py-0.5 text-xs font-medium text-success-700 dark:text-success-400">Approved</span>
          {:else if pr.reviewDecision === 'CHANGES_REQUESTED'}
            <span class="rounded bg-error-500/15 px-1.5 py-0.5 text-xs font-medium text-error-700 dark:text-error-400">Changes</span>
          {:else if pr.reviewDecision === 'REVIEW_REQUIRED'}
            <span class="rounded bg-warning-500/15 px-1.5 py-0.5 text-xs font-medium text-warning-700 dark:text-warning-400">Review needed</span>
          {/if}
          {#if pr.unresolvedCount > 0}
            <span
              class="rounded bg-warning-500/15 px-1.5 py-0.5 text-xs font-medium text-warning-700 dark:text-warning-400"
              title="{pr.unresolvedCount} unresolved"
            >{pr.unresolvedCount} unresolved</span>
          {/if}
        </div>
      </button>
    {/each}
  </div>
{:else if task.prNumber && task.projectId}
  <div class="flex flex-col gap-1">
    <span class="text-sm font-medium text-surface-500">Pull Request</span>
    <button
      type="button"
      class="flex w-fit items-center gap-1.5 text-sm text-warning-700 hover:underline dark:text-warning-400"
      onclick={() => BrowserOpenURL(`https://github.com/${task.projectId}/pull/${task.prNumber}`)}
    >
      <GitPullRequest size={16} class="shrink-0" />
      {task.projectId}#{task.prNumber}
    </button>
  </div>
{/if}

{#if hasPR}
  <button
    type="button"
    class="flex w-fit items-center gap-1.5 rounded-md border border-surface-300 bg-surface-50 px-2.5 py-1.5 text-sm transition-colors hover:bg-surface-100 disabled:opacity-50 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
    onclick={resumeInCC}
    disabled={resuming}
    title="Resume the Claude Code session that produced this PR"
  >
    <RotateCcw size={14} class="shrink-0" />
    {resuming ? 'Resuming…' : 'Resume in Claude Code'}
  </button>
{/if}
