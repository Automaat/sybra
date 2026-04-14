<script lang="ts">
  import { EventsOn } from '$lib/api'
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { agentOutput } from '../lib/events.js'

  interface Props {
    agentId?: string
    staticEvents?: agent.StreamEvent[]
  }

  const { agentId, staticEvents }: Props = $props()

  let events = $state<agent.StreamEvent[]>([])
  let container: HTMLDivElement | undefined = $state()
  let autoScroll = $state(true)

  const typeStyles: Record<string, { label: string; classes: string }> = {
    init: { label: 'INIT', classes: 'bg-surface-300 text-surface-800 dark:bg-surface-600 dark:text-surface-200' },
    assistant: { label: 'ASST', classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' },
    tool_use: { label: 'TOOL', classes: 'bg-blue-200 text-blue-800 dark:bg-blue-700 dark:text-blue-200' },
    tool_result: { label: 'RSLT', classes: 'bg-green-200 text-green-800 dark:bg-green-700 dark:text-green-200' },
    result: { label: 'DONE', classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' },
  }

  function scrollToBottom() {
    if (container) {
      container.scrollTop = container.scrollHeight
    }
  }

  function onScroll() {
    if (!container) return
    const atBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 10
    autoScroll = atBottom
  }

  $effect(() => {
    if (staticEvents) {
      events = staticEvents
      requestAnimationFrame(scrollToBottom)
      return
    }

    if (!agentId) return

    agentStore.getOutput(agentId).then((initial) => {
      events = initial
      requestAnimationFrame(scrollToBottom)
    })

    const unsub = EventsOn(agentOutput(agentId), (event: agent.StreamEvent) => {
      events = [...events, event]
      agentStore.appendEvent(agentId, event)
      if (autoScroll) requestAnimationFrame(scrollToBottom)
    })

    return () => {
      unsub()
    }
  })
</script>

<div class="flex max-h-[60dvh] md:max-h-[600px] flex-col rounded-lg border border-surface-300 dark:border-surface-600">
  {#if !staticEvents}
    <div class="flex items-center justify-end border-b border-surface-300 px-2 py-1 dark:border-surface-600">
      <button
        type="button"
        onclick={() => { autoScroll = !autoScroll }}
        title={autoScroll ? 'Auto-scroll on' : 'Auto-scroll off'}
        class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium transition-colors
          {autoScroll ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' : 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400'}"
      >
        <svg class="h-3 w-3" aria-hidden="true" viewBox="0 0 16 16" fill="currentColor">
          <path d="M8 11L3 6h10z"/>
        </svg>
        Auto-scroll
      </button>
    </div>
  {/if}
  <div
    bind:this={container}
    onscroll={onScroll}
    class="flex flex-1 flex-col gap-1 overflow-y-auto bg-surface-900 p-3 font-mono text-xs {staticEvents ? 'rounded-lg' : 'rounded-b-lg'}"
  >
    {#if events.length === 0}
      <p class="py-8 text-center text-surface-500">Waiting for output...</p>
    {:else}
      {#each events as event, i (i)}
        {@const style = typeStyles[event.type] ?? { label: event.type.toUpperCase(), classes: 'bg-surface-300 text-surface-800' }}
        <div class="flex items-start gap-2">
          <span class="mt-0.5 inline-block shrink-0 rounded px-1.5 py-0.5 text-[10px] font-bold {style.classes}">
            {style.label}
          </span>
          <pre class="min-w-0 flex-1 whitespace-pre-wrap break-words text-surface-200">{event.content ?? ''}</pre>
        </div>
      {/each}
    {/if}
  </div>
</div>
