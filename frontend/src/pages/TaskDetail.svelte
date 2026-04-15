<script lang="ts">
  import { ChevronLeft, CircleDot, GitPullRequest, ChevronDown } from '@lucide/svelte'
  import type { agent, task } from '../../wailsjs/go/models.js'
  import { EventsOn, BrowserOpenURL, StartFixReview, StartReview, GetAgentRunLog } from '$lib/api'
  import { agentState } from '../lib/events.js'
  import { renderMarkdown } from '../lib/markdown.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { connectionStore } from '../stores/connection.svelte.js'
  import { STATUS_OPTIONS, STATUS_MAP } from '../lib/statuses.js'
  import StreamOutput from '../components/StreamOutput.svelte'
  import ChatView from '../components/ChatView.svelte'
  import ProviderLogo from '../components/ProviderLogo.svelte'

  interface Props {
    taskId: string
    onback: () => void
    onviewagent: (agentId: string) => void
    ondelete: () => void
    onreviewplan?: (taskId: string) => void
  }

  const { taskId, onback, onviewagent, ondelete, onreviewplan }: Props = $props()

  let statusSelectRef = $state<HTMLSelectElement | null>(null)
  let titleInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (editingTitle && titleInputRef) titleInputRef.focus()
  })

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      if (editingTitle || editingBody || editingTags || editingDueDate) return
      onback()
      return
    }
    const target = e.target as HTMLElement
    if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable) return
    if (e.metaKey || e.ctrlKey || e.altKey) return
    if (e.key === 'e') {
      e.preventDefault()
      startEditingBody()
      return
    }
    if (e.key === 's') {
      e.preventDefault()
      statusSelectRef?.focus()
    }
    if (e.key === 'd') {
      e.preventDefault()
      deleteTask()
    }
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  let deleting = $state(false)
  let copied = $state(false)

  async function copyId() {
    if (!t) return
    await navigator.clipboard.writeText(t.id)
    copied = true
    setTimeout(() => { copied = false }, 1500)
  }
  let editingBody = $state(false)
  let bodyDraft = $state('')
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let editingTags = $state(false)
  let tagsDraft = $state<string[]>([])
  let tagInput = $state('')
  let tagInputRef = $state<HTMLInputElement | null>(null)
  let editingDueDate = $state(false)
  let dueDateDraft = $state('')
  let dueDateInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (editingTags && tagInputRef) tagInputRef.focus()
  })

  $effect(() => {
    if (editingDueDate && dueDateInputRef) dueDateInputRef.focus()
  })

  let t = $state<task.Task | null>(null)
  let error = $state('')
  let prompt = $state('')
  let agentMode = $state('interactive')
  let starting = $state(false)
  let runningAgent = $state<agent.Agent | null>(null)

  const statusOptions = STATUS_OPTIONS

  const renderedBody = $derived(renderMarkdown(t?.body))
  const renderedPlan = $derived(renderMarkdown(t?.plan))

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

  async function updateTaskType(value: string) {
    if (!t || (t.taskType ?? 'normal') === value) return
    try {
      t = await taskStore.update(taskId, { task_type: value })
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

  function startEditingTags() {
    if (!t) return
    tagsDraft = [...(t.tags ?? [])]
    tagInput = ''
    editingTags = true
  }

  function addTag() {
    const tag = tagInput.trim().replace(/,/g, '')
    if (tag && !tagsDraft.includes(tag)) tagsDraft = [...tagsDraft, tag]
    tagInput = ''
  }

  function removeTag(tag: string) {
    tagsDraft = tagsDraft.filter((x) => x !== tag)
  }

  async function saveTags() {
    editingTags = false
    if (!t) return
    const current = t.tags ?? []
    const same =
      current.length === tagsDraft.length && current.every((v, i) => v === tagsDraft[i])
    if (same) return
    try {
      t = await taskStore.update(taskId, { tags: tagsDraft })
    } catch (e) {
      error = String(e)
    }
  }

  function handleTagInputKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      if (tagInput.trim()) {
        addTag()
      } else {
        saveTags()
      }
    } else if (e.key === 'Escape') {
      editingTags = false
    } else if (e.key === 'Backspace' && !tagInput && tagsDraft.length > 0) {
      tagsDraft = tagsDraft.slice(0, -1)
    } else if (e.key === ',') {
      e.preventDefault()
      addTag()
    }
  }

  function handleTagsContainerFocusout(e: FocusEvent) {
    const related = e.relatedTarget as Node | null
    const container = e.currentTarget as HTMLElement
    if (!related || !container.contains(related)) saveTags()
  }

  function parseNaturalDate(input: string): Date | null {
    const lower = input.toLowerCase().trim()
    if (!lower || lower === 'none' || lower === 'clear') return null
    const now = new Date()
    if (lower === 'today') {
      const d = new Date(now)
      d.setHours(23, 59, 59, 0)
      return d
    }
    if (lower === 'tomorrow') {
      const d = new Date(now)
      d.setDate(d.getDate() + 1)
      d.setHours(23, 59, 59, 0)
      return d
    }
    if (lower === 'yesterday') {
      const d = new Date(now)
      d.setDate(d.getDate() - 1)
      d.setHours(23, 59, 59, 0)
      return d
    }
    const weekdays = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday']
    const nextMatch = lower.match(/^(?:next\s+)?(\w+)$/)
    if (nextMatch) {
      const day = weekdays.indexOf(nextMatch[1])
      if (day !== -1) {
        const d = new Date(now)
        const current = d.getDay()
        const diff = ((day - current + 7) % 7) || 7
        d.setDate(d.getDate() + diff)
        d.setHours(23, 59, 59, 0)
        return d
      }
    }
    const inMatch = lower.match(/^in\s+(\d+)\s+(day|days|week|weeks)$/)
    if (inMatch) {
      const n = parseInt(inMatch[1])
      const d = new Date(now)
      d.setDate(d.getDate() + (inMatch[2].startsWith('week') ? n * 7 : n))
      d.setHours(23, 59, 59, 0)
      return d
    }
    const parsed = new Date(input)
    return isNaN(parsed.getTime()) ? null : parsed
  }

  function formatDueDateDisplay(date: any): string {
    if (!date) return 'Set due date'
    const d = new Date(date)
    if (isNaN(d.getTime())) return 'Set due date'
    const now = new Date()
    const today = new Date(now.getFullYear(), now.getMonth(), now.getDate())
    const target = new Date(d.getFullYear(), d.getMonth(), d.getDate())
    const diff = Math.round((target.getTime() - today.getTime()) / 86400000)
    if (diff === 0) return 'Today'
    if (diff === 1) return 'Tomorrow'
    if (diff === -1) return 'Yesterday'
    if (diff > 1 && diff < 7) return `In ${diff} days`
    if (diff < 0) return `${Math.abs(diff)}d overdue`
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: d.getFullYear() !== now.getFullYear() ? 'numeric' : undefined })
  }

  function startEditingDueDate() {
    if (!t) return
    if (t.dueDate) {
      const d = new Date(t.dueDate)
      dueDateDraft = isNaN(d.getTime()) ? '' : d.toISOString().split('T')[0]
    } else {
      dueDateDraft = ''
    }
    editingDueDate = true
  }

  async function saveDueDate() {
    editingDueDate = false
    if (!t) return
    const input = dueDateDraft.trim()
    let newVal: string | null = null
    if (input && input.toLowerCase() !== 'none' && input.toLowerCase() !== 'clear') {
      const parsed = parseNaturalDate(input)
      if (!parsed) {
        error = `Invalid date: "${input}". Try "today", "tomorrow", "next monday", "in 3 days", or YYYY-MM-DD.`
        return
      }
      newVal = parsed.toISOString()
    }
    const currentISO = t.dueDate ? new Date(t.dueDate).toISOString() : null
    if (newVal === currentISO) return
    try {
      t = await taskStore.update(taskId, { due_date: newVal })
    } catch (e) {
      error = String(e)
    }
  }

  function handleDueDateKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      saveDueDate()
    } else if (e.key === 'Escape') {
      editingDueDate = false
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

  const hasRunningAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.state === 'running')
  )

  // Statuses that conflict with a running agent
  const AGENT_BLOCKED_STATUSES = new Set(['new', 'todo', 'done'])

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
  let fixReviewLoading = $state(false)

  const isReviewTask = $derived(t?.tags?.includes('review') ?? false)

  const reviewingAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('review:') && a.state === 'running')
  )

  const fixingReviewAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('fix-review:') && a.state === 'running')
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

  async function runFixReview() {
    if (!t) return
    fixReviewLoading = true
    error = ''
    try {
      await StartFixReview(taskId)
      await loadTask()
    } catch (e) {
      error = String(e)
    } finally {
      fixReviewLoading = false
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
  let runLogEvents = $state<Map<string, agent.StreamEvent[]>>(new Map())
  let runLogLoading = $state<Set<string>>(new Set())

  async function toggleRunLog(agentId: string) {
    if (expandedRun === agentId) {
      expandedRun = null
      return
    }
    expandedRun = agentId
    if (!runLogEvents.has(agentId) && !runLogLoading.has(agentId) && taskId) {
      runLogLoading = new Set([...runLogLoading, agentId])
      try {
        const events = await GetAgentRunLog(taskId, agentId)
        runLogEvents = new Map([...runLogEvents, [agentId, events ?? []]])
      } catch {
        runLogEvents = new Map([...runLogEvents, [agentId, []]])
      }
      const next = new Set(runLogLoading)
      next.delete(agentId)
      runLogLoading = next
    }
  }

  const pastRuns = $derived(
    (t?.agentRuns ?? []).slice().reverse()
  )

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <ChevronLeft size={16} />
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
            bind:this={titleInputRef}
            class="text-2xl font-bold bg-transparent border-b-2 border-primary-500 outline-none w-full"
            bind:value={titleDraft}
            onblur={saveTitle}
            onkeydown={handleTitleKeydown}
          />
        {:else}
          <h1 class="text-2xl font-bold">
            <button
              type="button"
              class="cursor-pointer hover:text-primary-500 transition-colors"
              onclick={startEditingTitle}
              title="Click to edit title"
            >{t.title}</button>
          </h1>
        {/if}
        <div class="flex items-center gap-2">
          <select
            bind:this={statusSelectRef}
            data-testid="task-status-select"
            class="cursor-pointer rounded-full px-2.5 py-0.5 text-xs font-semibold transition-opacity hover:opacity-80 {STATUS_MAP[t.status]?.badgeClasses ?? 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}"
            style="appearance: auto"
            value={t.status}
            onchange={(e) => updateStatus((e.target as HTMLSelectElement).value)}
            title="Click to change status"
          >
            {#each statusOptions as s}
              <option value={s.value} disabled={hasRunningAgent && AGENT_BLOCKED_STATUSES.has(s.value)}>{s.label}</option>
            {/each}
          </select>
          <select
            data-testid="task-type-select"
            class="rounded border border-surface-300 bg-surface-100 px-2 py-0.5 text-xs font-medium dark:border-surface-600 dark:bg-surface-700"
            value={t.taskType || 'normal'}
            onchange={(e) => updateTaskType((e.target as HTMLSelectElement).value)}
            title="Task type — controls execution mode and worktree behavior"
          >
            <option value="normal">normal</option>
            <option value="debug">debug</option>
            <option value="research">research</option>
          </select>
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
            <span class="inline-flex items-center gap-1 rounded-full bg-warning-200 px-2 py-0.5 text-xs font-medium text-warning-800 dark:bg-warning-700 dark:text-warning-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
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
              class="rounded bg-warning-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-warning-600 disabled:opacity-50"
              onclick={runReview}
              disabled={reviewLoading || reviewingAgent}
            >
              {reviewLoading ? 'Starting...' : t.reviewed ? 'Re-run Review' : 'Run Review'}
            </button>
          {/if}
          {#if t.status === 'in-review' && t.prNumber && t.projectId && !isReviewTask}
            <button
              type="button"
              class="rounded bg-tertiary-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-tertiary-600 disabled:opacity-50"
              onclick={runFixReview}
              disabled={fixReviewLoading || fixingReviewAgent}
              title="Run fix-review skill to apply unresolved PR review comments"
            >
              {fixReviewLoading ? 'Starting...' : 'Fix Review Comments'}
            </button>
          {/if}
          {#if fixingReviewAgent}
            <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
              Fixing review
            </span>
          {/if}
          <button
            type="button"
            class="rounded bg-surface-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-surface-600"
            onclick={copyId}
          >
            {copied ? 'Copied!' : 'Copy ID'}
          </button>
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

      <div class="flex gap-6 text-sm">
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Agent Mode</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{t.agentMode}</span>
        </div>

        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Tags</span>
          {#if editingTags}
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
              class="flex min-w-[8rem] flex-wrap items-center gap-1 rounded-lg border border-primary-400 bg-surface-50 px-2 py-1 dark:border-primary-500 dark:bg-surface-900"
              onfocusout={handleTagsContainerFocusout}
            >
              {#each tagsDraft as tag}
                <span class="inline-flex items-center gap-0.5 rounded bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">
                  {tag}
                  <button
                    type="button"
                    class="ml-0.5 text-surface-400 hover:text-error-500"
                    onclick={() => removeTag(tag)}
                    tabindex="-1"
                    aria-label="Remove tag {tag}"
                  >×</button>
                </span>
              {/each}
              <input
                bind:this={tagInputRef}
                bind:value={tagInput}
                class="min-w-[4rem] flex-1 bg-transparent text-xs outline-none"
                placeholder={tagsDraft.length ? '' : 'add tags...'}
                onkeydown={handleTagInputKeydown}
              />
            </div>
          {:else}
            <button
              type="button"
              class="flex flex-wrap items-center gap-1 rounded-lg border border-transparent px-1 py-0.5 text-left transition-colors hover:border-surface-300 hover:bg-surface-100 dark:hover:border-surface-600 dark:hover:bg-surface-800"
              onclick={startEditingTags}
              title="Click to edit tags"
            >
              {#if t.tags?.length}
                {#each t.tags as tag}
                  <span class="rounded bg-surface-200 px-2 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
                {/each}
              {:else}
                <span class="text-xs italic text-surface-400">add tags</span>
              {/if}
            </button>
          {/if}
        </div>

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
              class="flex w-fit items-center gap-1.5 text-sm text-secondary-600 hover:underline dark:text-secondary-400"
              onclick={() => t && BrowserOpenURL(t.issue)}
            >
              <CircleDot size={16} class="shrink-0" />
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
                  <span class="rounded bg-warning-500/15 px-1.5 py-0.5 text-xs font-medium text-warning-700 dark:text-warning-400"
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
            class="flex w-fit items-center gap-1.5 text-sm text-warning-700 hover:underline dark:text-warning-400"
            onclick={() => t && BrowserOpenURL(`https://github.com/${t.projectId}/pull/${t.prNumber}`)}
          >
            <GitPullRequest size={16} class="shrink-0" />
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
              <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedBody}</div>
            {:else}
              <span class="text-sm text-surface-400 italic">Click to add description...</span>
            {/if}
          </button>
        {/if}
      </div>

      {#if t.plan}
        <div class="flex flex-col gap-1">
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium text-surface-500">Plan</span>
            <span class="text-xs text-surface-400 italic">read-only · edit via synapse-cli --plan</span>
          </div>
          <div class="rounded-lg border border-surface-300 bg-surface-100 p-4 dark:border-surface-600 dark:bg-surface-900">
            <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedPlan}</div>
          </div>
        </div>
      {/if}

      <div class="flex flex-wrap items-center gap-4 text-xs text-surface-400">
        <span>Created: {formatDate(t.createdAt)}</span>
        <span>Updated: {formatDate(t.updatedAt)}</span>
        <div class="flex items-center gap-1">
          <span>Due:</span>
          {#if editingDueDate}
            <input
              bind:this={dueDateInputRef}
              bind:value={dueDateDraft}
              class="rounded border border-primary-400 bg-surface-50 px-2 py-0.5 text-xs outline-none dark:border-primary-500 dark:bg-surface-900"
              placeholder="today / tomorrow / YYYY-MM-DD"
              onblur={saveDueDate}
              onkeydown={handleDueDateKeydown}
            />
            <span class="text-surface-300 dark:text-surface-600">Esc to cancel</span>
          {:else}
            <button
              type="button"
              class="rounded px-1 py-0.5 transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300 {t.dueDate && new Date(t.dueDate) < new Date() ? 'text-error-500 dark:text-error-400' : ''}"
              onclick={startEditingDueDate}
              title="Click to set due date"
            >
              {formatDueDateDisplay(t.dueDate)}
            </button>
          {/if}
        </div>
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
              {#if runningAgent.provider}
                <ProviderLogo provider={runningAgent.provider} class="h-4 w-4 text-surface-500" />
              {/if}
              <button
                type="button"
                class="font-mono text-sm text-primary-500 hover:underline"
                onclick={() => onviewagent(runningAgent!.id)}
              >
                {runningAgent.id}
              </button>
              <span class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium
                {runningAgent.state === 'running' ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' : 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
                {#if runningAgent.state === 'running'}
                  <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
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
          {#if runningAgent.mode === 'interactive'}
            <ChatView agentId={runningAgent.id} agentState={runningAgent.state} costUsd={runningAgent.costUsd} inputTokens={runningAgent.inputTokens ?? 0} outputTokens={runningAgent.outputTokens ?? 0} bounded={true} />
          {:else}
            <StreamOutput agentId={runningAgent.id} />
          {/if}
        </div>
      {:else}
        <div class="flex flex-col gap-3">
          <span class="text-sm font-medium text-surface-500">Run Agent</span>
          <div class="flex flex-wrap items-center gap-4">
            <label class="flex items-center gap-2">
              <input
                type="checkbox"
                checked={agentMode === 'headless'}
                onchange={(e) => { agentMode = e.currentTarget.checked ? 'headless' : 'interactive' }}
                class="rounded border-surface-300 dark:border-surface-600"
              />
              <span class="text-sm">Headless</span>
            </label>
          </div>
          <textarea
            class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="3"
            placeholder="Enter prompt for the agent..."
            bind:value={prompt}
          ></textarea>
          <div class="flex items-center gap-2">
            <button
              type="button"
              class="w-fit rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
              onclick={startAgent}
              disabled={starting || !prompt.trim() || !connectionStore.online}
              title={!connectionStore.online ? 'Offline — agent cannot start until connection is restored' : undefined}
            >
              {starting ? 'Starting...' : 'Start agent'}
            </button>
            {#if !connectionStore.online}
              <span class="text-xs text-warning-600 dark:text-warning-400">Offline</span>
            {/if}
          </div>
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
                onclick={() => toggleRunLog(run.agentId)}
              >
                <div class="flex items-center gap-2">
                  {#if run.provider}
                    <ProviderLogo provider={run.provider} class="h-3.5 w-3.5 text-surface-400" />
                  {/if}
                  <span class="font-mono text-surface-400">{run.agentId}</span>
                  <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{run.mode}</span>
                  <span class="rounded px-1.5 py-0.5 {run.state === 'stopped' ? 'bg-surface-200 dark:bg-surface-700' : 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200'}">
                    {run.state || 'running'}
                  </span>
                </div>
                <div class="flex items-center gap-3 text-surface-400">
                  {#if run.costUsd > 0}
                    <span>${run.costUsd.toFixed(4)}</span>
                  {/if}
                  <span>{formatDate(run.startedAt)}</span>
                  <ChevronDown size={16} class="transition-transform {expandedRun === run.agentId ? 'rotate-180' : ''}" />
                </div>
              </button>
              {#if expandedRun === run.agentId}
                <div class="border-t border-surface-300 px-3 py-2 dark:border-surface-600">
                  {#if runLogLoading.has(run.agentId)}
                    <p class="py-4 text-center text-xs text-surface-500">Loading log...</p>
                  {:else if (runLogEvents.get(run.agentId)?.length ?? 0) > 0}
                    <StreamOutput staticEvents={runLogEvents.get(run.agentId)} />
                  {:else if run.result}
                    <pre class="max-h-[60dvh] md:max-h-[600px] overflow-y-auto whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-900 p-3 text-xs text-surface-300 dark:border-surface-600">{run.result}</pre>
                  {:else}
                    <p class="py-4 text-center text-xs text-surface-500">No output available</p>
                  {/if}
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

<style>
  :global(.markdown-body p) { margin: 0.25em 0; }
  :global(.markdown-body pre) {
    margin: 0.5em 0;
    border-radius: 0.375rem;
    overflow-x: auto;
    font-size: 0.75rem;
  }
  :global(.markdown-body pre code.hljs) {
    border-radius: 0.375rem;
    font-size: 0.75rem;
  }
  :global(.markdown-body code:not(.hljs)) {
    font-size: 0.8em;
    padding: 0.1em 0.3em;
    border-radius: 0.25rem;
    background: rgb(var(--color-surface-800) / 0.5);
  }
  :global(.markdown-body ul, .markdown-body ol) { padding-left: 1.5em; margin: 0.25em 0; }
  :global(.markdown-body h1, .markdown-body h2, .markdown-body h3) { margin: 0.5em 0 0.25em; font-weight: 600; }
  :global(.markdown-body blockquote) { border-left: 3px solid currentColor; padding-left: 0.75em; opacity: 0.8; margin: 0.25em 0; }
  :global(.markdown-body a) { text-decoration: underline; }
</style>
