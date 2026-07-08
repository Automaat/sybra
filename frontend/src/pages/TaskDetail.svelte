<script lang="ts">
  import { ChevronLeft } from '@lucide/svelte'
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import TaskHeaderBar from '../components/task-detail/TaskHeaderBar.svelte'
  import TaskStatusBanner from '../components/task-detail/TaskStatusBanner.svelte'
  import HumanRequiredPanel from '../components/task-detail/HumanRequiredPanel.svelte'
  import TaskMetadataRow from '../components/task-detail/TaskMetadataRow.svelte'
  import TaskPullRequestsPanel from '../components/task-detail/TaskPullRequestsPanel.svelte'
  import TaskDescriptionEditor from '../components/task-detail/TaskDescriptionEditor.svelte'
  import PlanReviewPanel from '../components/task-detail/PlanReviewPanel.svelte'
  import TaskPlanPanel from '../components/task-detail/TaskPlanPanel.svelte'
  import TaskReviewPanel from '../components/task-detail/TaskReviewPanel.svelte'
  import LiveAgentPanel from '../components/task-detail/LiveAgentPanel.svelte'
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
  // The detail body is split into tabs so a big task (plan + review + many runs)
  // is scannable instead of a single long scroll. Default is always Overview.
  let activeTab = $state('overview')

  const REVIEW_ROLES = new Set(['review', 'fix-review', 'test-runner'])
  const hasPlan = $derived(
    !!(t && (t.plan || t.planBrief || t.planDecisions || t.planCritique || t.planResearch)),
  )
  // A plan-review task always needs a Plan tab to host the approve/reject
  // decision, even when its plan lives in the body rather than a sidecar.
  const showPlanTab = $derived(hasPlan || t?.status === 'plan-review')
  const hasReview = $derived(
    !!(t && (t.codeReview || (t.agentRuns ?? []).some((r) => REVIEW_ROLES.has(r.role)))),
  )
  const runsCount = $derived((t?.agentRuns ?? []).filter((r) => r.state !== 'running').length)

  const tabs = $derived([
    { value: 'overview', label: 'Overview' },
    ...(showPlanTab ? [{ value: 'plan', label: t?.status === 'plan-review' ? 'Plan ●' : 'Plan' }] : []),
    ...(hasReview ? [{ value: 'review', label: 'Review' }] : []),
    { value: 'runs', label: runsCount > 0 ? `Runs · ${runsCount}` : 'Runs' },
  ])

  // If the active tab disappears (e.g. its data was cleared), fall back to Overview.
  $effect(() => {
    if (!tabs.some((tb) => tb.value === activeTab)) activeTab = 'overview'
  })

  function panelClass(tab: string, layout = 'flex flex-col gap-6'): string {
    return activeTab === tab ? layout : 'hidden'
  }

  function cycleTab(dir: number) {
    const vals = tabs.map((tb) => tb.value)
    const i = vals.indexOf(activeTab)
    if (i < 0) return
    activeTab = vals[(i + dir + vals.length) % vals.length]
  }

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
    if (e.key === '[') {
      e.preventDefault()
      cycleTab(-1)
      return
    }
    if (e.key === ']') {
      e.preventDefault()
      cycleTab(1)
      return
    }
    if (e.key === 'e') {
      e.preventDefault()
      // The body editor lives on Overview; make sure it's visible before editing.
      activeTab = 'overview'
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

<div class="mx-auto flex w-full max-w-[1600px] flex-col gap-4 p-4 md:gap-6 md:p-6">
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
      <!-- Persistent header: always visible above the tabs. -->
      <TaskHeaderBar task={t} {ondelete} />
      <TaskStatusBanner task={t} />
      <HumanRequiredPanel task={t} />
      {#if t.status === 'plan-review' && activeTab !== 'plan'}
        <!-- Default is Overview, so nudge the pending approve/reject to the fore. -->
        <button
          type="button"
          class="w-fit text-sm text-primary-500 hover:underline"
          onclick={() => (activeTab = 'plan')}
        >Review the plan →</button>
      {/if}
      <!-- Pinned outside the tabs so a live SSE stream never unmounts on a switch. -->
      <LiveAgentPanel task={t} {onviewagent} />

      <SegmentedControl orientation="horizontal" value={activeTab} onValueChange={(details) => (activeTab = details.value ?? 'overview')}>
        <SegmentedControl.Control>
          <SegmentedControl.Indicator />
          {#each tabs as tab}
            <SegmentedControl.Item value={tab.value}>
              <SegmentedControl.ItemText>{tab.label}</SegmentedControl.ItemText>
              <SegmentedControl.ItemHiddenInput />
            </SegmentedControl.Item>
          {/each}
        </SegmentedControl.Control>
      </SegmentedControl>

      <!-- Two-column: a wide main content column + a persistent properties rail,
           so the page uses its width instead of stranding a narrow column on the
           left. The rail (task properties + PR) stays visible on every tab. -->
      <div class="flex flex-col gap-6 lg:flex-row lg:items-start lg:gap-8">
        <div class="flex min-w-0 flex-1 flex-col gap-6">
          <section class={panelClass('overview', 'flex flex-col gap-6')}>
            <TaskDescriptionEditor task={t} />
          </section>

          {#if showPlanTab}
            <section class={panelClass('plan', 'flex flex-col gap-4')}>
              {#if t.status === 'plan-review'}
                <PlanReviewPanel task={t} {onreviewplan} />
              {:else}
                <TaskPlanPanel task={t} />
              {/if}
            </section>
          {/if}

          {#if hasReview}
            <section class={panelClass('review', 'flex flex-col gap-4')}>
              <TaskReviewPanel task={t} />
            </section>
          {/if}

          <section class={panelClass('runs')}>
            <AgentLauncher task={t} />
            <AgentHistoryList task={t} />
          </section>
        </div>

        <aside class="flex w-full flex-col gap-6 lg:w-80 lg:shrink-0">
          <TaskMetadataRow task={t} />
          <TaskPullRequestsPanel task={t} />
        </aside>
      </div>
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
  /* Tailwind's preflight resets list-style to none, which strips the markers
     from ordered/unordered lists in rendered plan/review markdown. Restore them. */
  :global(.markdown-body ul) { list-style: disc; padding-left: 1.5em; margin: 0.25em 0; }
  :global(.markdown-body ol) { list-style: decimal; padding-left: 1.5em; margin: 0.25em 0; }
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
