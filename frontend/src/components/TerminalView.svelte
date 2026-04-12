<script lang="ts">
  import { CaptureAgentPane, AttachAgent } from '$lib/api'
  import { agentStore } from '../stores/agents.svelte.js'

  interface Props {
    agentId: string
  }

  const { agentId }: Props = $props()

  let output = $state('')
  let error = $state('')
  let container: HTMLPreElement | undefined = $state()

  const agent = $derived(agentStore.agents.get(agentId))
  const stopped = $derived(agent?.state === 'stopped')

  function scrollToBottom() {
    if (container) {
      container.scrollTop = container.scrollHeight
    }
  }

  async function poll() {
    try {
      const text = await CaptureAgentPane(agentId)
      if (text !== output) {
        output = text
        requestAnimationFrame(scrollToBottom)
      }
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function handleAttach() {
    try {
      await AttachAgent(agentId)
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    poll()
    if (stopped) return

    const timer = setInterval(poll, 1000)
    return () => clearInterval(timer)
  })
</script>

<div class="flex flex-col gap-2">
  <div class="flex items-center justify-between">
    <span class="text-xs text-surface-400">tmux capture-pane (live)</span>
    {#if !stopped}
      <button
        type="button"
        class="rounded bg-primary-500 px-3 py-1 text-xs font-medium text-white hover:bg-primary-600"
        onclick={handleAttach}
      >
        Attach in Terminal
      </button>
    {/if}
  </div>

  {#if error}
    <p class="text-xs text-error-500">{error}</p>
  {/if}

  <pre
    bind:this={container}
    class="max-h-[600px] overflow-y-auto whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-900 p-3 font-mono text-xs text-surface-200 dark:border-surface-600"
  >{output || 'Waiting for output...'}</pre>
</div>
