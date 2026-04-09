<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { convoStore } from '../stores/convo.svelte.js'
  import MessageBubble from './MessageBubble.svelte'
  import ToolApproval from './ToolApproval.svelte'
  import ChatInput from './ChatInput.svelte'

  interface Props {
    agentId: string
    agentState?: string
    costUsd?: number
    inputTokens?: number
    outputTokens?: number
  }

  const { agentId, agentState = 'running', costUsd = 0, inputTokens = 0, outputTokens = 0 }: Props = $props()

  let events = $state<agent.ConvoEvent[]>([])
  let container: HTMLDivElement | undefined = $state()

  const isRunning = $derived(agentState === 'running')
  const isPaused = $derived(agentState === 'paused')
  const approvals = $derived(
    [...convoStore.pendingApprovals.values()],
  )
  const hasApproval = $derived(approvals.length > 0)
  const isWaitingForApproval = $derived(isPaused && hasApproval)
  const isWaitingForInput = $derived(isPaused && !hasApproval)

  const contextMax = 200_000
  const totalTokens = $derived(inputTokens + outputTokens)
  const contextPct = $derived(Math.min(100, Math.round((totalTokens / contextMax) * 100)))
  const contextColor = $derived(
    contextPct > 80 ? 'bg-error-500' : contextPct > 50 ? 'bg-warning-500' : 'bg-success-500',
  )

  function scrollToBottom() {
    if (container) {
      container.scrollTop = container.scrollHeight
    }
  }

  $effect(() => {
    convoStore.getOutput(agentId).then((initial) => {
      events = initial
      requestAnimationFrame(scrollToBottom)
    })

    const unsub = convoStore.subscribe(agentId)

    return () => {
      unsub()
    }
  })

  // Watch conversation changes to update local events + auto-scroll.
  // Reading eventVersion triggers reactivity on every append without full Map copy.
  $effect(() => {
    void convoStore.eventVersion
    const current = convoStore.conversations.get(agentId)
    if (current && current.length > events.length) {
      events = current
      requestAnimationFrame(scrollToBottom)
    }
  })

  async function handleSend(text: string) {
    await convoStore.sendMessage(agentId, text)
  }

  async function handleApproval(toolUseId: string, approved: boolean) {
    await convoStore.respondApproval(toolUseId, approved)
  }
</script>

<div class="flex h-full flex-col">
  <!-- Header bar -->
  <div class="flex items-center gap-3 border-b border-surface-300 px-4 py-2 dark:border-surface-600">
    <span class="inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium
      {isRunning ? 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' :
       isWaitingForApproval ? 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200' :
       isPaused ? 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' :
       'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
      {#if isRunning}
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-success-500"></span>
      {:else if isWaitingForApproval}
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-error-500"></span>
      {:else if isPaused}
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
      {/if}
      {isWaitingForApproval ? 'Needs approval' : isWaitingForInput ? 'Waiting for input' : agentState}
    </span>

    <!-- Context window indicator -->
    {#if totalTokens > 0}
      <div class="flex items-center gap-1.5" title="{totalTokens.toLocaleString()} / {contextMax.toLocaleString()} tokens ({contextPct}%)">
        <div class="h-1.5 w-16 overflow-hidden rounded-full bg-surface-200 dark:bg-surface-700">
          <div class="h-full rounded-full transition-all {contextColor}" style="width: {contextPct}%"></div>
        </div>
        <span class="text-[10px] text-surface-500">{contextPct}%</span>
      </div>
    {/if}

    {#if costUsd > 0}
      <span class="text-xs text-surface-500">${costUsd.toFixed(2)}</span>
    {/if}
  </div>

  <!-- Messages -->
  <div
    bind:this={container}
    class="flex flex-1 flex-col gap-3 overflow-y-auto px-4 py-3"
  >
    {#if events.length === 0}
      <p class="py-12 text-center text-sm text-surface-500">Waiting for response...</p>
    {:else}
      {#each events as event, i (i)}
        <MessageBubble {event} />
      {/each}
    {/if}

    <!-- Pending approvals -->
    {#each approvals as approval (approval.toolUseId)}
      <ToolApproval
        toolUseId={approval.toolUseId}
        toolName={approval.toolName}
        input={approval.input}
        onrespond={handleApproval}
      />
    {/each}

    <!-- Streaming indicator -->
    {#if isRunning && events.length > 0}
      <div class="flex items-center gap-2 text-xs text-surface-500">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
        Agent is thinking...
      </div>
    {/if}
  </div>

  <!-- Input -->
  <ChatInput
    disabled={isRunning || isWaitingForApproval}
    onsend={handleSend}
  />
</div>
