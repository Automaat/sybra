<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { extractAutoReviewVerdict } from '../../lib/auto-review-verdict.js'
  import { isTamperFlaggedTask } from '$lib/tamper.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const verdict = $derived(extractAutoReviewVerdict(task.body ?? ''))
  // Tamper-flagged tasks should normally go through Bless first (it records the
  // bless tag so the same unchanged diff isn't re-flagged by detect_tampering) —
  // dispatching straight to testing/ready-pr without blessing would likely bounce
  // right back to human-required. Surface that as a hint, but still offer the
  // generic decision-reason + dispatch actions: the operator may have a reason to
  // act anyway (e.g. re-run implementation to produce a fresh, unflagged diff).
  const isTamperFlagged = $derived(isTamperFlaggedTask(task))

  type Target = 'in-progress' | 'testing' | 'ready-pr' | 'in-review'

  const actions: { target: Target; label: string }[] = [
    { target: 'in-progress', label: 'Re-run implementation' },
    { target: 'testing', label: 'Send to testing' },
    { target: 'ready-pr', label: 'Open PR' },
  ]

  let reason = $state('')
  let loading = $state(false)
  let error = $state('')

  const canDispatch = $derived(reason.trim().length > 0 && !loading)

  async function dispatch(target: Target) {
    if (!canDispatch) return
    loading = true
    error = ''
    try {
      await taskStore.dispatchFromHumanRequired(task.id, target, reason.trim())
      reason = ''
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }
</script>

{#if task.status === 'human-required'}
  <div class="flex flex-col gap-3 rounded-lg border border-warning-300 bg-warning-50 p-4 dark:border-warning-700 dark:bg-warning-900/30">
    <span class="text-sm font-semibold text-warning-800 dark:text-warning-200">Human Required</span>

    {#if task.statusReason}
      <p class="text-sm text-warning-800 dark:text-warning-200">{task.statusReason}</p>
    {/if}
    {#if verdict}
      <div class="whitespace-pre-wrap rounded-md border border-warning-200 bg-white/60 p-3 text-sm dark:border-warning-800 dark:bg-surface-950/30">{verdict}</div>
    {/if}

    {#if error}
      <p class="text-xs text-error-500">{error}</p>
    {/if}

    {#if isTamperFlagged}
      <p class="text-sm text-warning-800 dark:text-warning-200">
        This task is tamper-flagged. Prefer <span class="font-semibold">Bless</span> to accept the
        changes and resume review — dispatching directly may re-trigger the tamper check unless the
        diff changes first.
      </p>
    {/if}
    <textarea
      class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
      rows="2"
      placeholder="Decision reason (required)..."
      bind:value={reason}
      disabled={loading}
    ></textarea>

    <div class="flex flex-wrap gap-2">
      {#each actions as action (action.target)}
        <button
          type="button"
          class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
          onclick={() => dispatch(action.target)}
          disabled={!canDispatch}
        >
          {action.label}
        </button>
      {/each}
      {#if task.prNumber > 0}
        <button
          type="button"
          class="rounded-lg bg-secondary-500 px-4 py-2 text-sm font-medium text-white hover:bg-secondary-600 disabled:opacity-50"
          onclick={() => dispatch('in-review')}
          disabled={!canDispatch}
        >
          Link PR and review
        </button>
      {/if}
    </div>
  </div>
{/if}
