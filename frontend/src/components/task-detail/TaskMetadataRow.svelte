<script lang="ts">
  import { CircleDot, Copy } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { BrowserOpenURL } from '$lib/api'
  import { formatDate, formatDueDateDisplay, parseNaturalDate } from '../../lib/dates.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let editingTags = $state(false)
  let tagsDraft = $state<string[]>([])
  let tagInput = $state('')
  let tagInputRef = $state<HTMLInputElement | null>(null)

  let editingDueDate = $state(false)
  let dueDateDraft = $state('')
  let dueDateInputRef = $state<HTMLInputElement | null>(null)

  let copiedBranch = $state(false)
  let error = $state('')

  let editingMaxTurns = $state(false)
  let maxTurnsDraft = $state('')
  let maxTurnsInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (editingMaxTurns && maxTurnsInputRef) maxTurnsInputRef.focus()
  })

  const taskBranchName = $derived(
    task ? 'sybra/' + (task.slug ? task.slug + '-' + task.id : task.id) : '',
  )

  $effect(() => {
    if (editingTags && tagInputRef) tagInputRef.focus()
  })

  $effect(() => {
    if (editingDueDate && dueDateInputRef) dueDateInputRef.focus()
  })

  $effect(() => {
    function onEditTags() {
      if (!editingTags) startEditingTags()
    }
    function onOpenDueDate() {
      if (!editingDueDate) startEditingDueDate()
    }
    window.addEventListener('task-detail:edit-tags', onEditTags)
    window.addEventListener('open-due-date', onOpenDueDate)
    return () => {
      window.removeEventListener('task-detail:edit-tags', onEditTags)
      window.removeEventListener('open-due-date', onOpenDueDate)
    }
  })

  function startEditingTags() {
    tagsDraft = [...(task.tags ?? [])]
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
    const current = task.tags ?? []
    const same =
      current.length === tagsDraft.length && current.every((v, i) => v === tagsDraft[i])
    if (same) return
    try {
      await taskStore.update(task.id, { tags: tagsDraft })
    } catch (e) {
      error = String(e)
    }
  }

  function handleTagInputKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      if (tagInput.trim()) addTag()
      else saveTags()
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

  function startEditingDueDate() {
    if (task.dueDate) {
      const d = new Date(task.dueDate as unknown as string)
      dueDateDraft = isNaN(d.getTime()) ? '' : d.toISOString().split('T')[0]
    } else {
      dueDateDraft = ''
    }
    editingDueDate = true
  }

  async function saveDueDate() {
    editingDueDate = false
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
    const currentISO = task.dueDate ? new Date(task.dueDate as unknown as string).toISOString() : null
    if (newVal === currentISO) return
    try {
      await taskStore.update(task.id, { due_date: newVal })
      error = ''
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

  function startEditingMaxTurns() {
    maxTurnsDraft = task.maxTurns ? String(task.maxTurns) : ''
    editingMaxTurns = true
  }

  async function saveMaxTurns() {
    editingMaxTurns = false
    const raw = maxTurnsDraft.trim()
    const n = raw === '' ? 0 : parseInt(raw, 10)
    if (raw !== '' && (isNaN(n) || n < 0)) {
      error = 'Max turns must be a non-negative integer.'
      return
    }
    const current = task.maxTurns ?? 0
    if (n === current) return
    try {
      await taskStore.update(task.id, { max_turns: n })
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  function handleMaxTurnsKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      saveMaxTurns()
    } else if (e.key === 'Escape') {
      editingMaxTurns = false
    }
  }

  async function copyBranch() {
    if (!task.projectId) return
    try {
      await navigator.clipboard.writeText(taskBranchName)
      copiedBranch = true
      setTimeout(() => { copiedBranch = false }, 1500)
    } catch (e) {
      notificationStore.pushLocal('error', 'Copy failed', String(e))
    }
  }
</script>

{#if error}
  <p class="text-xs text-error-500">{error}</p>
{/if}

<div class="flex gap-6 text-sm">
  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Agent Mode</span>
    <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{task.agentMode}</span>
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
        {#if task.tags?.length}
          {#each task.tags as tag}
            <span class="rounded bg-surface-200 px-2 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
          {/each}
        {:else}
          <span class="text-xs italic text-surface-400">add tags</span>
        {/if}
      </button>
    {/if}
  </div>

  {#if task.projectId}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Project</span>
      <span class="rounded bg-surface-200 px-2 py-0.5 font-mono dark:bg-surface-700">{task.projectId}</span>
    </div>

    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Branch</span>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 rounded bg-surface-200 px-2 py-0.5 font-mono text-left transition-colors hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
        onclick={copyBranch}
        title="Copy branch name (⇧⌘.)"
      >
        {copiedBranch ? 'Copied!' : taskBranchName}
        <Copy size={12} class="shrink-0 text-surface-400" />
      </button>
    </div>
  {/if}

  {#if task.issue}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Issue</span>
      <button
        type="button"
        class="flex w-fit items-center gap-1.5 text-sm text-secondary-600 hover:underline dark:text-secondary-400"
        onclick={() => BrowserOpenURL(task.issue)}
      >
        <CircleDot size={16} class="shrink-0" />
        {task.issue}
      </button>
    </div>
  {/if}

  {#if task.allowedTools?.length}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Allowed Tools</span>
      <div class="flex gap-1">
        {#each task.allowedTools as tool}
          <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{tool}</span>
        {/each}
      </div>
    </div>
  {/if}

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Fork Subagents</span>
    <button
      type="button"
      class="w-fit rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
      onclick={async () => {
        try {
          await taskStore.update(task.id, { fork_subagent: !task.forkSubagent })
        } catch (e) {
          error = String(e)
        }
      }}
      title="Enable CLAUDE_CODE_FORK_SUBAGENT=1 — parallel subagents, higher token cost"
    >
      {#if task.forkSubagent}
        <span class="text-primary-600 dark:text-primary-400">enabled</span>
      {:else}
        <span class="italic text-surface-400">disabled</span>
      {/if}
    </button>
  </div>

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Max Turns</span>
    {#if editingMaxTurns}
      <input
        bind:this={maxTurnsInputRef}
        bind:value={maxTurnsDraft}
        type="number"
        min="0"
        class="w-24 rounded border border-primary-400 bg-surface-50 px-2 py-0.5 text-xs outline-none dark:border-primary-500 dark:bg-surface-900"
        placeholder="global default"
        onblur={saveMaxTurns}
        onkeydown={handleMaxTurnsKeydown}
      />
    {:else}
      <button
        type="button"
        class="w-fit rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
        onclick={startEditingMaxTurns}
        title="Click to set per-task max turns (0 = use global default)"
      >
        {#if task.maxTurns}
          {task.maxTurns}
        {:else}
          <span class="italic text-surface-400">global default</span>
        {/if}
      </button>
    {/if}
  </div>
</div>

<div class="flex flex-wrap items-center gap-4 text-xs text-surface-400">
  <span>Created: {formatDate(task.createdAt)}</span>
  <span>Updated: {formatDate(task.updatedAt)}</span>
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
        class="rounded px-1 py-0.5 transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300 {task.dueDate && new Date(task.dueDate as unknown as string) < new Date() ? 'text-error-500 dark:text-error-400' : ''}"
        onclick={startEditingDueDate}
        title="Click to set due date"
      >
        {formatDueDateDisplay(task.dueDate)}
      </button>
    {/if}
  </div>
</div>
