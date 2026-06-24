<script lang="ts">
  import { ChevronLeft } from '@lucide/svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import TaskHeaderBar from '../components/task-detail/TaskHeaderBar.svelte'
  import TaskStatusBanner from '../components/task-detail/TaskStatusBanner.svelte'
  import TaskMetadataRow from '../components/task-detail/TaskMetadataRow.svelte'
  import TaskPullRequestsPanel from '../components/task-detail/TaskPullRequestsPanel.svelte'
  import TaskDescriptionEditor from '../components/task-detail/TaskDescriptionEditor.svelte'
  import PlanReviewPanel from '../components/task-detail/PlanReviewPanel.svelte'
  import AgentLauncher from '../components/task-detail/AgentLauncher.svelte'
  import AgentHistoryList from '../components/task-detail/AgentHistoryList.svelte'

  interface Props {
    taskId: string
    onback: () => void
    onviewagent: (agentId: string) => void
    ondelete: () => void
    onreviewplan?: (taskId: string) => void
  }

  const { taskId, onback, onviewagent, ondelete, onreviewplan }: Props = $props()

  let t = $state<Task | null>(null)
  let error = $state('')

  $effect(() => {
    loadTask()
  })

  async function loadTask() {
    try {
      t = await taskStore.get(taskId)
    } catch (e) {
      error = String(e)
    }
  }

  // Translate page-level keyboard shortcuts into CustomEvents that the
  // sub-components listen for. Mirrors the existing `open-due-date` pattern.
  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      const active = document.activeElement as HTMLElement | null
      if (active && (active.tagName === 'INPUT' || active.tagName === 'TEXTAREA' || active.isContentEditable)) return
      onback()
      return
    }
    if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key === '.') {
      e.preventDefault()
      window.dispatchEvent(new CustomEvent(e.shiftKey ? 'task-detail:copy-branch' : 'task-detail:copy-id'))
      return
    }
    if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key === 'd') {
      e.preventDefault()
      window.dispatchEvent(new CustomEvent('open-due-date'))
      return
    }
    const target = e.target as HTMLElement
    if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable) return
    if (e.metaKey || e.ctrlKey || e.altKey) return
    if (e.key === 'e') {
      e.preventDefault()
      window.dispatchEvent(new CustomEvent('task-detail:edit-body'))
      return
    }
    if (e.key === 's') {
      e.preventDefault()
      window.dispatchEvent(new CustomEvent('task-detail:focus-status'))
      return
    }
    if (e.key === 'd') {
      e.preventDefault()
      window.dispatchEvent(new CustomEvent('task-detail:delete'))
    }
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })
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
      <TaskHeaderBar task={t} {ondelete} />
      <TaskStatusBanner task={t} />
      <!-- Plan approve/reject is the only decision for a plan-review task, so it
           sits directly under the banner — above the fold, not below it. -->
      <PlanReviewPanel task={t} {onreviewplan} />
      <TaskMetadataRow task={t} />
      <TaskPullRequestsPanel task={t} />
      <TaskDescriptionEditor task={t} />
      <hr class="border-surface-300 dark:border-surface-600" />
      <!-- AgentLauncher always renders: it also hosts a live running agent's
           output + Stop. It collapses only its generic "new run" FORM for a
           plan-review task (whose job is approve/reject). -->
      <AgentLauncher task={t} {onviewagent} />
      <AgentHistoryList task={t} />
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
  /* Non-interactive checklist glyphs (replacing GFM's disabled checkboxes). */
  :global(.markdown-body .task-check) {
    display: inline-block; width: 1.1em; margin-right: 0.15em;
    text-align: center; opacity: 0.55;
  }
  :global(.markdown-body .task-check--done) { color: var(--color-success-600); opacity: 1; }
  :global(.markdown-body li:has(.task-check)) { list-style: none; }
</style>
