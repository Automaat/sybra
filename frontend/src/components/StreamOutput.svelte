<script lang="ts">
  import { EventsOn } from '../../wailsjs/runtime/runtime.js'
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'

  interface Props {
    agentId: string
  }

  const { agentId }: Props = $props()

  let events = $state<agent.StreamEvent[]>([])
  let container: HTMLDivElement | undefined = $state()

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

  $effect(() => {
    agentStore.getOutput(agentId).then((initial) => {
      events = initial
      scrollToBottom()
    })

    const unsub = EventsOn(`agent:output:${agentId}`, (event: agent.StreamEvent) => {
      events = [...events, event]
      agentStore.appendEvent(agentId, event)
      requestAnimationFrame(scrollToBottom)
    })

    return () => {
      unsub()
    }
  })
</script>

<div
  bind:this={container}
  class="flex max-h-[600px] flex-col gap-1 overflow-y-auto rounded-lg border border-surface-300 bg-surface-900 p-3 font-mono text-xs dark:border-surface-600"
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
