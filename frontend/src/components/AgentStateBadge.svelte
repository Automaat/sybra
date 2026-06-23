<script lang="ts">
  import { fade } from 'svelte/transition'
  import { PHASE_CONFIG, type AgentPhase } from '$lib/agent-phases.js'

  interface Props {
    phase: AgentPhase
    /** sm = list/card pill, md = detail header pill */
    size?: 'sm' | 'md'
  }

  const { phase, size = 'sm' }: Props = $props()

  const config = $derived(PHASE_CONFIG[phase])
  const pillSize = $derived(size === 'md' ? 'px-3 py-1 text-sm' : 'px-2.5 py-0.5 text-xs')
  const dotSize = $derived(size === 'md' ? 'h-2 w-2' : 'h-1.5 w-1.5')
  // Queued and done are resting states — no leading dot, just the label.
  const showDot = $derived(phase !== 'queued' && phase !== 'done')
</script>

<span class="inline-flex items-center gap-1 rounded-full font-medium transition-all duration-150 {pillSize} {config.badgeClasses}">
  {#if config.animate}
    <span transition:fade={{ duration: 150 }} class="animate-pulse-subtle rounded-full {dotSize} {config.dotClasses}"></span>
  {:else if showDot}
    <span transition:fade={{ duration: 150 }} class="rounded-full {dotSize} {config.dotClasses}"></span>
  {/if}
  {config.label}
</span>
