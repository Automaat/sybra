<script lang="ts">
  import { ArrowDown } from '@lucide/svelte'
  import { EventsOn } from '$lib/api'
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { agentOutput } from '../lib/events.js'

  interface Props {
    agentId?: string
    staticEvents?: agent.StreamEvent[]
    highlightIndex?: number | null
    onvisibleindex?: (index: number) => void
  }

  const { agentId, staticEvents, highlightIndex = null, onvisibleindex }: Props = $props()

  let events = $state<agent.StreamEvent[]>([])
  let container: HTMLDivElement | undefined = $state()
  let autoScroll = $state(true)
  let flashIndex = $state<number | null>(null)

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
    if (onvisibleindex) {
      const els = container.querySelectorAll<HTMLElement>('[data-event-index]')
      for (const el of els) {
        const rect = el.getBoundingClientRect()
        const parentRect = container.getBoundingClientRect()
        if (rect.top >= parentRect.top && rect.bottom <= parentRect.bottom) {
          onvisibleindex(Number(el.dataset.eventIndex))
          break
        }
      }
    }
  }

  function scrollToBottom() {
    if (autoScroll && container) {
      container.scrollTop = container.scrollHeight
    }
  }

  // Scroll to highlighted event
  $effect(() => {
    if (highlightIndex === null || !container) return
    const el = container.querySelector<HTMLElement>(`[data-event-index="${highlightIndex}"]`)
    if (!el) return
    autoScroll = false
    el.scrollIntoView({ behavior: 'smooth', block: 'center' })
    flashIndex = highlightIndex
    const t = setTimeout(() => { flashIndex = null }, 600)
    return () => clearTimeout(t)
  })

  $effect(() => {
    if (staticEvents) {
      events = staticEvents
      requestAnimationFrame(scrollToBottom)
      return
    }

    if (!agentId) return

    agentStore.getOutput(agentId).then((initial) => {
      events = initial.map((tse) => tse.event)
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
      <ArrowDown size={12} />
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
        <div
          data-event-index={i}
          class="flex items-start gap-2 rounded transition-colors duration-300
            {flashIndex === i ? 'bg-warning-900/40' : ''}"
        >
          <span class="mt-0.5 inline-block shrink-0 rounded px-1.5 py-0.5 text-[10px] font-bold {style.classes}">
            {style.label}
          </span>
          <pre class="min-w-0 flex-1 whitespace-pre-wrap break-words">{event.content ?? ''}</pre>
        </div>
      {/each}
    {/if}
  </div>
</div>
