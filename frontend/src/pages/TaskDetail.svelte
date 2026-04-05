<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import type { agent, task } from '../../wailsjs/go/models.js'
  import { EventsOn } from '../../wailsjs/runtime/runtime.js'
  import { agentState } from '../lib/events.js'
  import { BrowserOpenURL } from '../../wailsjs/runtime/runtime.js'
  import { StartReview } from '../../wailsjs/go/main/App.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { STATUS_OPTIONS } from '../lib/statuses.js'
  import StatusBadge from '../components/StatusBadge.svelte'
  import StreamOutput from '../components/StreamOutput.svelte'
  import TerminalView from '../components/TerminalView.svelte'

  interface Props {
    taskId: string
    onback: () => void
    onviewagent: (agentId: string) => void
    ondelete: () => void
    onreviewplan?: (taskId: string) => void
  }

  const { taskId, onback, onviewagent, ondelete, onreviewplan }: Props = $props()

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') onback()
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  let deleting = $state(false)
  let editingBody = $state(false)
  let bodyDraft = $state('')
  let editingTitle = $state(false)
  let titleDraft = $state('')

  let t = $state<task.Task | null>(null)
  let error = $state('')
  let prompt = $state('')
  let agentMode = $state('interactive')
  let starting = $state(false)
  let runningAgent = $state<agent.Agent | null>(null)

  const statusOptions = STATUS_OPTIONS

  $effect(() => {
    loadTask()
    const existing = agentStore.byTask(taskId)
    if (existing && existing.state === 'running') {
      runningAgent = existing
    }
  })

  $effect(() => {
    if (!runningAgent) return
    const unsub = EventsOn(agentState(runningAgent.id), (data: agent.Agent) => {
      runningAgent = data
      agentStore.updateAgent(data.id, data)
    })
    return () => { unsub() }
  })

  async function loadTask() {
    try {
      t = await taskStore.get(taskId)
      agentMode = t.agentMode || 'interactive'
    } catch (e) {
      error = String(e)
    }
  }

  function startEditingTitle() {
    if (!t) return
    titleDraft = t.title
    editingTitle = true
  }

  async function saveTitle() {
    if (!t || !titleDraft.trim() || titleDraft.trim() === t.title) {
      editingTitle = false
      return
    }
    try {
      t = await taskStore.update(taskId, { title: titleDraft.trim() })
    } catch (e) {
      error = String(e)
    }
    editingTitle = false
  }

  function handleTitleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      saveTitle()
    } else if (e.key === 'Escape') {
      editingTitle = false
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

  async function startAgent() {
    if (!t || !prompt.trim()) return
    starting = true
    error = ''
    try {
      runningAgent = await agentStore.start(taskId, agentMode, prompt.trim())
      prompt = ''
    } catch (e) {
      error = String(e)
    } finally {
      starting = false
    }
  }

  function startEditingBody() {
    bodyDraft = t?.body ?? ''
    editingBody = true
  }

  async function saveBody() {
    if (!t) return
    editingBody = false
    const trimmed = bodyDraft.trim()
    if (trimmed === (t.body ?? '').trim()) return
    try {
      t = await taskStore.update(taskId, { body: trimmed })
    } catch (e) {
      error = String(e)
    }
  }

  function handleBodyKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      saveBody()
    } else if (e.key === 'Escape') {
      editingBody = false
    }
  }

  async function deleteTask() {
    if (!t) return
    deleting = true
    try {
      await taskStore.remove(taskId)
      ondelete()
    } catch (e) {
      error = String(e)
      deleting = false
    }
  }

  const triaging = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('triage:') && a.state === 'running')
  )

  const evaluating = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('eval:') && a.state === 'running')
  )

  const planningAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('plan:') && a.state === 'running')
  )

  const linkedPRs = $derived(t ? reviewStore.byTask(t) : [])

  let reviewLoading = $state(false)

  const isReviewTask = $derived(t?.tags?.includes('review') ?? false)

  const reviewingAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('review:') && a.state === 'running')
  )

  async function runReview() {
    if (!t) return
    reviewLoading = true
    error = ''
    try {
      await StartReview(taskId)
      await loadTask()
    } catch (e) {
      error = String(e)
    } finally {
      reviewLoading = false
    }
  }

  let rejectFeedback = $state('')
  let planActionLoading = $state(false)

  async function approvePlan() {
    if (!t) return
    planActionLoading = true
    try {
      t = await taskStore.approvePlan(taskId)
    } catch (e) {
      error = String(e)
    } finally {
      planActionLoading = false
    }
  }

  async function rejectPlan() {
    if (!t) return
    planActionLoading = true
    try {
      t = await taskStore.rejectPlan(taskId, rejectFeedback.trim())
      rejectFeedback = ''
    } catch (e) {
      error = String(e)
    } finally {
      planActionLoading = false
    }
  }

  let expandedRun = $state<string | null>(null)

  const pastRuns = $derived(
    (t?.agentRuns ?? []).slice().reverse()
  )

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
        {#if editingTitle}
          <input
            class="text-2xl font-bold bg-transparent border-b-2 border-primary-500 outline-none w-full"
            bind:value={titleDraft}
            onblur={saveTitle}
            onkeydown={handleTitleKeydown}
            autofocus
          />
        {:else}
          <h1
            class="text-2xl font-bold cursor-pointer hover:text-primary-500 transition-colors"
            onclick={startEditingTitle}
            title="Click to edit title"
          >{t.title}</h1>
        {/if}
        <div class="flex items-center gap-2">
          <StatusBadge status={t.status} />
          {#if triaging}
            <span class="inline-flex items-center gap-1 rounded-full bg-primary-200 px-2 py-0.5 text-xs font-medium text-primary-800 dark:bg-primary-700 dark:text-primary-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
              Triaging
            </span>
          {/if}
          {#if planningAgent}
            <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
              Planning
            </span>
          {/if}
          {#if evaluating}
            <span class="inline-flex items-center gap-1 rounded-full bg-warning-200 px-2 py-0.5 text-xs font-medium text-warning-800 dark:bg-warning-700 dark:text-warning-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
              Evaluating
            </span>
          {/if}
          {#if reviewingAgent}
            <span class="inline-flex items-center gap-1 rounded-full bg-purple-200 px-2 py-0.5 text-xs font-medium text-purple-800 dark:bg-purple-700 dark:text-purple-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-purple-500"></span>
              Reviewing
            </span>
          {/if}
          {#if t.reviewed}
            <span class="inline-flex items-center gap-1 rounded-full bg-success-200 px-2 py-0.5 text-xs font-medium text-success-800 dark:bg-success-700 dark:text-success-200" title="Review agent completed">
              ✓ Reviewed
            </span>
          {/if}
          {#if isReviewTask && t.prNumber && t.projectId}
            <button
              type="button"
              class="rounded bg-purple-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-purple-600 disabled:opacity-50"
              onclick={runReview}
              disabled={reviewLoading || reviewingAgent}
            >
              {reviewLoading ? 'Starting...' : t.reviewed ? 'Re-run Review' : 'Run Review'}
            </button>
          {/if}
          <button
            type="button"
            class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600 disabled:opacity-50"
            onclick={deleteTask}
            disabled={deleting}
          >
            {deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>

      <div class="flex flex-col gap-1">
        <span class="text-sm font-medium text-surface-500">Status</span>
        <SegmentedControl
          orientation="horizontal"
          value={t.status}
          onValueChange={(details) => { if (details.value) updateStatus(details.value) }}
        >
          <SegmentedControl.Control>
            <SegmentedControl.Indicator />
            {#each statusOptions as s}
              <SegmentedControl.Item value={s.value}>
                <SegmentedControl.ItemText>{s.label}</SegmentedControl.ItemText>
                <SegmentedControl.ItemHiddenInput />
              </SegmentedControl.Item>
            {/each}
          </SegmentedControl.Control>
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

        {#if t.projectId}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Project</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono dark:bg-surface-700">{t.projectId}</span>
          </div>
        {/if}

        {#if t.branch}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Branch</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono dark:bg-surface-700">{t.branch}</span>
          </div>
        {/if}

        {#if t.issue}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Issue</span>
            <button
              type="button"
              class="flex w-fit items-center gap-1.5 text-sm text-blue-600 hover:underline dark:text-blue-400"
              onclick={() => t && BrowserOpenURL(t.issue)}
            >
              <svg class="h-4 w-4 shrink-0" viewBox="0 0 16 16" fill="currentColor"><path d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z"/><path d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z"/></svg>
              {t.issue}
            </button>
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
                    class="inline-block h-2.5 w-2.5 shrink-0 rounded-full {pr.ciStatus === 'SUCCESS' ? 'bg-green-500' : pr.ciStatus === 'FAILURE' ? 'bg-red-500' : 'bg-yellow-500'}"
                    title="CI: {pr.ciStatus.toLowerCase()}"
                  ></span>
                {/if}
                <svg class="h-4 w-4 shrink-0 text-purple-500" viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354Z"/></svg>
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
                  <span class="rounded bg-green-500/15 px-1.5 py-0.5 text-xs font-medium text-green-600 dark:text-green-400">Approved</span>
                {:else if pr.reviewDecision === 'CHANGES_REQUESTED'}
                  <span class="rounded bg-red-500/15 px-1.5 py-0.5 text-xs font-medium text-red-600 dark:text-red-400">Changes</span>
                {:else if pr.reviewDecision === 'REVIEW_REQUIRED'}
                  <span class="rounded bg-yellow-500/15 px-1.5 py-0.5 text-xs font-medium text-yellow-600 dark:text-yellow-400">Review needed</span>
                {/if}
                {#if pr.unresolvedCount > 0}
                  <span class="rounded bg-yellow-500/15 px-1.5 py-0.5 text-xs font-medium text-yellow-600 dark:text-yellow-400"
                    title="{pr.unresolvedCount} unresolved"
                  >{pr.unresolvedCount} unresolved</span>
                {/if}
              </div>
            </button>
          {/each}
        </div>
      {:else if t.prNumber && t.projectId}
        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium text-surface-500">Pull Request</span>
          <button
            type="button"
            class="flex w-fit items-center gap-1.5 text-sm text-purple-600 hover:underline dark:text-purple-400"
            onclick={() => t && BrowserOpenURL(`https://github.com/${t.projectId}/pull/${t.prNumber}`)}
          >
            <svg class="h-4 w-4 shrink-0" viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354Z"/></svg>
            {t.projectId}#{t.prNumber}
          </button>
        </div>
      {/if}

      <div class="flex flex-col gap-1">
        <div class="flex items-center justify-between">
          <span class="text-sm font-medium text-surface-500">Description</span>
          {#if editingBody}
            <span class="text-xs text-surface-400">
              {navigator.platform.includes('Mac') ? '⌘' : 'Ctrl'}+Enter to save · Esc to cancel
            </span>
          {/if}
        </div>
        {#if editingBody}
          <!-- svelte-ignore a11y_autofocus -->
          <textarea
            class="min-h-[8rem] w-full resize-y rounded-lg border border-primary-400 bg-surface-50 p-4 font-mono text-sm dark:border-primary-500 dark:bg-surface-900"
            bind:value={bodyDraft}
            onblur={saveBody}
            onkeydown={handleBodyKeydown}
            autofocus
          ></textarea>
        {:else}
          <button
            type="button"
            class="w-full cursor-text rounded-lg border border-surface-300 bg-surface-100 p-4 text-left transition-colors hover:border-primary-400 dark:border-surface-600 dark:bg-surface-900 dark:hover:border-primary-500"
            onclick={startEditingBody}
          >
            {#if t.body}
              <pre class="whitespace-pre-wrap text-sm">{t.body}</pre>
            {:else}
              <span class="text-sm text-surface-400 italic">Click to add description...</span>
            {/if}
          </button>
        {/if}
      </div>

      <div class="flex gap-6 text-xs text-surface-400">
        <span>Created: {formatDate(t.createdAt)}</span>
        <span>Updated: {formatDate(t.updatedAt)}</span>
      </div>

      {#if t.status === 'plan-review'}
        <div class="flex flex-col gap-3 rounded-lg border border-tertiary-300 bg-tertiary-50 p-4 dark:border-tertiary-700 dark:bg-tertiary-900/30">
          <div class="flex items-center justify-between">
            <span class="text-sm font-semibold text-tertiary-700 dark:text-tertiary-300">Plan Review</span>
            {#if onreviewplan}
              <button
                type="button"
                class="text-xs text-primary-500 hover:underline"
                onclick={() => onreviewplan!(t!.id)}
              >Review Plan →</button>
            {/if}
          </div>
          <div class="flex gap-2">
            <button
              type="button"
              class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
              onclick={approvePlan}
              disabled={planActionLoading}
            >
              Approve Plan
            </button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
              onclick={rejectPlan}
              disabled={planActionLoading}
            >
              Reject Plan
            </button>
          </div>
          <textarea
            class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="2"
            placeholder="Rejection feedback (optional)..."
            bind:value={rejectFeedback}
          ></textarea>
        </div>
      {/if}

      <hr class="border-surface-300 dark:border-surface-600" />

      {#if runningAgent}
        <div class="flex flex-col gap-3">
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium text-surface-500">Agent</span>
              <button
                type="button"
                class="font-mono text-sm text-primary-500 hover:underline"
                onclick={() => onviewagent(runningAgent!.id)}
              >
                {runningAgent.id}
              </button>
              <span class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium
                {runningAgent.state === 'running' ? 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' : 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
                {#if runningAgent.state === 'running'}
                  <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-success-500"></span>
                {/if}
                {runningAgent.state}
              </span>
            </div>
            {#if runningAgent.state === 'running'}
              <button
                type="button"
                class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600"
                onclick={() => agentStore.stop(runningAgent!.id)}
              >
                Stop
              </button>
            {/if}
          </div>
          {#if runningAgent.mode === 'interactive' && runningAgent.tmuxSession}
            <TerminalView agentId={runningAgent.id} />
          {:else}
            <StreamOutput agentId={runningAgent.id} />
          {/if}
        </div>
      {:else}
        <div class="flex flex-col gap-3">
          <span class="text-sm font-medium text-surface-500">Run Agent</span>
          <label class="flex items-center gap-2">
            <input
              type="checkbox"
              checked={agentMode === 'headless'}
              onchange={(e) => { agentMode = e.currentTarget.checked ? 'headless' : 'interactive' }}
              class="rounded border-surface-300 dark:border-surface-600"
            />
            <span class="text-sm">Headless</span>
          </label>
          <textarea
            class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="3"
            placeholder="Enter prompt for the agent..."
            bind:value={prompt}
          ></textarea>
          <button
            type="button"
            class="w-fit rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
            onclick={startAgent}
            disabled={starting || !prompt.trim()}
          >
            {starting ? 'Starting...' : 'Start agent'}
          </button>
        </div>
      {/if}

      {#if pastRuns.length > 0}
        <hr class="border-surface-300 dark:border-surface-600" />
        <div class="flex flex-col gap-3">
          <span class="text-sm font-medium text-surface-500">Agent History</span>
          {#each pastRuns as run (run.agentId)}
            <div class="rounded-lg border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-800">
              <button
                type="button"
                class="flex w-full items-center justify-between px-3 py-2 text-left text-xs"
                onclick={() => { expandedRun = expandedRun === run.agentId ? null : run.agentId }}
              >
                <div class="flex items-center gap-2">
                  <span class="font-mono text-surface-400">{run.agentId}</span>
                  <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{run.mode}</span>
                  <span class="rounded px-1.5 py-0.5 {run.state === 'stopped' ? 'bg-surface-200 dark:bg-surface-700' : 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200'}">
                    {run.state || 'running'}
                  </span>
                </div>
                <div class="flex items-center gap-3 text-surface-400">
                  {#if run.costUsd > 0}
                    <span>${run.costUsd.toFixed(4)}</span>
                  {/if}
                  <span>{formatDate(run.startedAt)}</span>
                  <svg class="h-4 w-4 transition-transform {expandedRun === run.agentId ? 'rotate-180' : ''}" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
                  </svg>
                </div>
              </button>
              {#if expandedRun === run.agentId && run.result}
                <div class="border-t border-surface-300 px-3 py-2 dark:border-surface-600">
                  <pre class="whitespace-pre-wrap text-xs text-surface-300">{run.result}</pre>
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
