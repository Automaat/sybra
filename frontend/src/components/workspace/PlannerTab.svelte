<script lang="ts">
  import type { PlanStep } from '$lib/plan-steps.js'
  import PlanSteps from '../PlanSteps.svelte'

  interface Props {
    planSteps: PlanStep[]
  }

  const { planSteps }: Props = $props()
</script>

{#if planSteps.length > 0}
  <PlanSteps steps={planSteps} collapsed={false} />
{:else}
  <div class="flex items-center justify-center py-12 text-sm text-surface-400">
    No plan yet
  </div>
{/if}
