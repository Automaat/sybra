<script lang="ts">
  import { ChevronDown, ChevronRight, Circle, CircleDot, CircleCheck } from '@lucide/svelte'
  import type { PlanStep } from '$lib/plan-steps.js'

  interface Props {
    steps: PlanStep[]
    collapsed?: boolean
  }

  let { steps, collapsed = $bindable(false) }: Props = $props()

  const completedCount = $derived(steps.filter((s) => s.status === 'completed').length)

  function statusIcon(status: string) {
    switch (status) {
      case 'completed':
        return CircleCheck
      case 'in_progress':
        return CircleDot
      default:
        return Circle
    }
  }

  function statusClasses(status: string): string {
    switch (status) {
      case 'completed':
        return 'text-success-500 dark:text-success-400'
      case 'in_progress':
        return 'text-primary-500 dark:text-primary-400 animate-pulse-subtle'
      default:
        return 'text-surface-400 dark:text-surface-500'
    }
  }

  function textClasses(status: string): string {
    return status === 'completed'
      ? 'line-through text-surface-400 dark:text-surface-500'
      : 'text-surface-800 dark:text-surface-200'
  }
</script>

<div class="rounded-lg border border-surface-300 dark:border-surface-700 overflow-hidden">
  <button
    type="button"
    class="flex w-full items-center gap-2 px-3 py-2 text-left text-sm font-medium
      bg-surface-100 dark:bg-surface-800 hover:bg-surface-200 dark:hover:bg-surface-700
      transition-colors"
    onclick={() => { collapsed = !collapsed }}
  >
    {#if collapsed}
      <ChevronRight size={14} class="shrink-0 text-surface-400" />
    {:else}
      <ChevronDown size={14} class="shrink-0 text-surface-400" />
    {/if}
    <span class="text-surface-600 dark:text-surface-300">Plan</span>
    <span class="ml-auto rounded-full bg-surface-200 dark:bg-surface-700 px-1.5 py-0.5 text-[10px] font-semibold text-surface-500 dark:text-surface-400">
      {completedCount}/{steps.length}
    </span>
  </button>

  {#if !collapsed}
    <ul class="divide-y divide-surface-200 dark:divide-surface-700">
      {#each steps as step (step.content)}
        <li class="flex items-start gap-2 px-3 py-2">
          <span class="mt-0.5 shrink-0 {statusClasses(step.status)}">
            {#if step.status === 'completed'}
              <CircleCheck size={14} />
            {:else if step.status === 'in_progress'}
              <CircleDot size={14} />
            {:else}
              <Circle size={14} />
            {/if}
          </span>
          <span class="text-xs leading-relaxed {textClasses(step.status)}" title={step.content}>
            {step.content}
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</div>
