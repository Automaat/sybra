<script lang="ts">
  import { Pencil } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { renderChecklistMarkdown } from '../../lib/markdown.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let editing = $state(false)
  let draft = $state('')
  let error = $state('')

  // The read-only Plan and Code Review artifacts moved to the Plan/Review tabs
  // (TaskPlanPanel / TaskReviewPanel); this editor is now only the task body.
  const renderedBody = $derived(renderChecklistMarkdown(task.body))

  function startEditing() {
    draft = task.body ?? ''
    editing = true
  }

  async function save() {
    editing = false
    const trimmed = draft.trim()
    if (trimmed === (task.body ?? '').trim()) return
    try {
      await taskStore.update(task.id, { body: trimmed })
    } catch (e) {
      error = String(e)
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      save()
    } else if (e.key === 'Escape') {
      editing = false
    }
  }

  $effect(() => {
    function onEditBody() {
      if (!editing) startEditing()
    }
    window.addEventListener('task-detail:edit-body', onEditBody)
    return () => window.removeEventListener('task-detail:edit-body', onEditBody)
  })
</script>

<div class="flex flex-col gap-1">
  <div class="flex items-center justify-between">
    <span class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Description</span>
    {#if editing}
      <span class="text-xs text-surface-400">
        {navigator.platform.includes('Mac') ? '⌘' : 'Ctrl'}+Enter to save · Esc to cancel
      </span>
    {:else}
      <button
        type="button"
        class="text-surface-400 transition-colors hover:text-surface-600 dark:hover:text-surface-300"
        onclick={startEditing}
        title="Edit description (e)"
        aria-label="Edit description"
      >
        <Pencil size={14} />
      </button>
    {/if}
  </div>
  {#if error}
    <p class="text-xs text-error-500">{error}</p>
  {/if}
  {#if editing}
    <!-- svelte-ignore a11y_autofocus -->
    <textarea
      class="min-h-[8rem] w-full resize-y rounded-lg border border-primary-400 bg-surface-50 p-4 font-mono text-sm dark:border-primary-500 dark:bg-surface-900"
      bind:value={draft}
      onblur={save}
      onkeydown={handleKeydown}
      autofocus
    ></textarea>
  {:else}
    <!-- Low-chrome, content-first: borderless until hover signals it's editable. -->
    <button
      type="button"
      class="w-full cursor-text rounded-lg p-3 text-left transition-colors hover:bg-surface-100 dark:hover:bg-surface-900"
      onclick={startEditing}
    >
      {#if task.body}
        <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedBody}</div>
      {:else}
        <span class="text-sm text-surface-400 italic">Click to add description...</span>
      {/if}
    </button>
  {/if}
</div>
