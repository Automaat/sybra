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
    tool_use: { label: 'TOOL', classes: 'bg-secondary-200 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200' },
    tool_result: { label: 'RSLT', classes: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200' },
    result: { label: 'DONE', classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' },
  }

  function onScroll() {
    if (!container) return
    autoScroll = container.scrollHeight - container.scrollTop - container.clientHeight < 10
  }

  function scrollToBottom() {
    if (autoScroll && container) {
      container.scrollTop = container.scrollHeight
    }
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
      scrollToBottom()
    })

    const unsub = EventsOn(agentOutput(agentId), (event: agent.StreamEvent) => {
      events = [...events, event]
      agentStore.appendEvent(agentId, event)
      requestAnimationFrame(scrollToBottom)
    })

    return () => {
      unsub()
    }
  })
</script>

<div class="flex flex-col gap-1">
  <div class="flex items-center justify-end">
    <button
      onclick={() => { autoScroll = !autoScroll }}
      title={autoScroll ? 'Disable auto-scroll' : 'Enable auto-scroll'}
      class="flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium transition-colors
        {autoScroll
          ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200'
          : 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400'}"
    >
      <svg class="h-3 w-3" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M8 3v8M5 8l3 3 3-3" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
      {autoScroll ? 'Auto-scroll on' : 'Auto-scroll off'}
    </button>
  </div>
  <div
    bind:this={container}
    onscroll={onScroll}
    class="flex max-h-[60dvh] md:max-h-[600px] flex-col gap-1 overflow-y-auto rounded-lg border border-surface-700 bg-surface-950 p-3 font-mono text-xs text-surface-50"
  >
    {#if events.length === 0}
      <p class="py-8 text-center text-surface-500">Waiting for output...</p>
    {:else}
      {#each events as event, i (i)}
        {@const style = typeStyles[event.type] ?? { label: event.type.toUpperCase(), classes: 'bg-surface-700 text-surface-200' }}
        <div class="flex items-start gap-2">
          <span class="mt-0.5 inline-block shrink-0 rounded px-1.5 py-0.5 text-[10px] font-bold {style.classes}">
            {style.label}
          </span>
          <pre class="min-w-0 flex-1 whitespace-pre-wrap break-words">{event.content ?? ''}</pre>
        </div>
      {/each}
    {/if}
  </div>
</div>
