<script lang="ts">
  import type { github } from '../../wailsjs/go/models.js'
  import {
    MergeRenovatePR,
    ApproveRenovatePR,
    RerunRenovateChecks,
  } from '../../wailsjs/go/main/App.js'
  import { renovateStore } from '../stores/renovate.svelte.js'

  interface Props {
    pr: github.RenovatePR
    onselect: () => void
  }

  const { pr, onselect }: Props = $props()
  let busy = $state('')

  function timeAgo(date: string): string {
    if (!date) return ''
    const now = Date.now()
    const then = new Date(date).getTime()
    const diff = Math.floor((now - then) / 1000)
    if (diff < 60) return 'just now'
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
    return `${Math.floor(diff / 86400)}d ago`
  }

  const isEligible = $derived(
    !pr.isDraft &&
    pr.mergeable === 'MERGEABLE' &&
    (pr.ciStatus === 'SUCCESS' || pr.ciStatus === '')
  )

  async function approve(e: Event) {
    e.stopPropagation()
    busy = 'approve'
    try {
      await ApproveRenovatePR(pr.repository, pr.number)
      await renovateStore.load()
    } finally {
      busy = ''
    }
  }

  async function merge(e: Event) {
    e.stopPropagation()
    busy = 'merge'
    try {
      await MergeRenovatePR(pr.repository, pr.number)
      await renovateStore.load()
    } finally {
      busy = ''
    }
  }

  async function rerun(e: Event) {
    e.stopPropagation()
    busy = 'rerun'
    try {
      await RerunRenovateChecks(pr.repository, pr.number)
      await renovateStore.load()
    } finally {
      busy = ''
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="link"
  tabindex="0"
  class="w-full cursor-pointer rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={onselect}
  onkeydown={(e) => { if (e.key === 'Enter') onselect() }}
>
  <div class="flex items-start justify-between gap-2">
    <div class="flex items-center gap-2">
      {#if pr.ciStatus}
        <span
          class="inline-block h-2.5 w-2.5 shrink-0 rounded-full {pr.ciStatus === 'SUCCESS' ? 'bg-green-500' : pr.ciStatus === 'FAILURE' ? 'bg-red-500' : 'bg-yellow-500'}"
          title="CI: {pr.ciStatus.toLowerCase()}"
        ></span>
      {/if}
      <h3 class="text-sm font-semibold leading-tight">{pr.title}</h3>
    </div>
    <div class="flex shrink-0 items-center gap-1.5">
      {#if pr.reviewDecision === 'APPROVED'}
        <span class="rounded bg-green-500/15 px-1.5 py-0.5 text-xs font-medium text-green-600 dark:text-green-400">Approved</span>
      {/if}
      {#if pr.mergeable === 'CONFLICTING'}
        <span class="rounded bg-red-500/15 px-1.5 py-0.5 text-xs font-medium text-red-600 dark:text-red-400">Conflicts</span>
      {/if}
    </div>
  </div>

  <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="font-mono">{pr.repository}#{pr.number}</span>

    {#if pr.labels?.length}
      {#each pr.labels as label}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{label}</span>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(pr.updatedAt)}</span>
  </div>

  <div class="mt-2 flex gap-1.5">
    {#if pr.reviewDecision !== 'APPROVED'}
      <button
        type="button"
        class="rounded bg-green-600 px-2 py-0.5 text-xs font-medium text-white transition-colors hover:bg-green-700 disabled:opacity-50"
        onclick={approve}
        disabled={busy !== ''}
      >
        {busy === 'approve' ? '...' : 'Approve'}
      </button>
    {/if}

    {#if isEligible}
      <button
        type="button"
        class="rounded bg-primary-600 px-2 py-0.5 text-xs font-medium text-white transition-colors hover:bg-primary-700 disabled:opacity-50"
        onclick={merge}
        disabled={busy !== ''}
      >
        {busy === 'merge' ? '...' : 'Merge'}
      </button>
    {/if}

    {#if pr.ciStatus === 'FAILURE'}
      <button
        type="button"
        class="rounded bg-yellow-600 px-2 py-0.5 text-xs font-medium text-white transition-colors hover:bg-yellow-700 disabled:opacity-50"
        onclick={rerun}
        disabled={busy !== ''}
      >
        {busy === 'rerun' ? '...' : 'Rerun'}
      </button>
    {/if}
  </div>
</div>
