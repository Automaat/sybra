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
    bounded?: boolean
  }

  const { agentId, agentState = 'running', costUsd = 0, inputTokens = 0, outputTokens = 0, bounded = false }: Props = $props()

  let events = $state<agent.ConvoEvent[]>([])
  let container: HTMLDivElement | undefined = $state()
  let autoScroll = $state(true)

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
    if (autoScroll && container) {
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

  $effect(() => {
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

<div class="flex h-full min-h-0 flex-col">
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

    <button
      onclick={() => { autoScroll = !autoScroll }}
      title={autoScroll ? 'Disable auto-scroll' : 'Enable auto-scroll'}
      class="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium transition-colors
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

  <!-- Messages -->
  <div
    bind:this={container}
    class="flex min-h-0 flex-col gap-3 overflow-y-auto overscroll-contain px-3 py-3 md:px-4 {bounded ? 'max-h-[60dvh] md:max-h-[600px]' : 'flex-1'}"
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
    disabled={isWaitingForApproval}
    placeholder={isRunning ? 'Queue a follow-up...' : 'Type a message...'}
    onsend={handleSend}
  />
</div>
