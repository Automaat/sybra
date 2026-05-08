<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { renderMarkdown } from '../../lib/markdown.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let editing = $state(false)
  let draft = $state('')
  let error = $state('')

  const renderedBody = $derived(renderMarkdown(task.body))
  const renderedPlan = $derived(renderMarkdown(task.plan))
  const renderedCodeReview = $derived(renderMarkdown(task.codeReview))

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
    <span class="text-sm font-medium text-surface-500">Description</span>
    {#if editing}
      <span class="text-xs text-surface-400">
        {navigator.platform.includes('Mac') ? '⌘' : 'Ctrl'}+Enter to save · Esc to cancel
      </span>
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
    <button
      type="button"
      class="w-full cursor-text rounded-lg border border-surface-300 bg-surface-100 p-4 text-left transition-colors hover:border-primary-400 dark:border-surface-600 dark:bg-surface-900 dark:hover:border-primary-500"
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

{#if task.plan}
  <div class="flex flex-col gap-1">
    <div class="flex items-center gap-2">
      <span class="text-sm font-medium text-surface-500">Plan</span>
      <span class="text-xs text-surface-400 italic">read-only · edit via sybra-cli --plan</span>
    </div>
    <div class="rounded-lg border border-surface-300 bg-surface-100 p-4 dark:border-surface-600 dark:bg-surface-900">
      <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedPlan}</div>
    </div>
  </div>
{/if}

{#if task.codeReview}
  <details open class="rounded-lg border border-warning-300 bg-warning-50 dark:border-warning-700 dark:bg-warning-900/20">
    <summary class="cursor-pointer px-4 py-2 text-sm font-semibold text-warning-800 dark:text-warning-200">
      Code Review (auto-generated)
    </summary>
    <div class="markdown-body px-4 pb-4 text-sm text-surface-900 dark:text-surface-100">
      {@html renderedCodeReview}
    </div>
  </details>
{/if}
