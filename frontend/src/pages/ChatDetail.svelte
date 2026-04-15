<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { EventsOn } from '$lib/api'
  import { agentStore } from '../stores/agents.svelte.js'
  import { agentState } from '../lib/events.js'
  import ChatView from '../components/ChatView.svelte'
  import { ChevronLeft } from '@lucide/svelte'

  interface Props {
    agentId: string
    onback: () => void
    onviewtask: (taskId: string) => void
  }

  const { agentId, onback, onviewtask }: Props = $props()

  let a = $state<agent.Agent | null>(null)
  let error = $state('')

  $effect(() => {
    const cached = agentStore.agents.get(agentId)
    if (cached) a = cached

    const unsub = EventsOn(agentState(agentId), (data: agent.Agent) => {
      a = data
      agentStore.updateAgent(agentId, data)
    })

    return () => {
      unsub()
    }
  })

  async function handleStop() {
    try {
      await agentStore.stop(agentId)
    } catch (e) {
      error = String(e)
    }
  }

  async function handleEndChat() {
    try {
      await agentStore.stopChat(agentId)
      onback()
    } catch (e) {
      error = String(e)
    }
  }
</script>

<div class="flex h-full flex-col">
  <!-- Header -->
  <div class="flex items-center gap-3 border-b border-surface-200 px-4 py-2 dark:border-surface-700">
    <button
      type="button"
      class="flex items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
      onclick={onback}
    >
      <ChevronLeft size={16} />
      Chats
    </button>

    {#if a}
      <span class="text-sm font-medium text-surface-900 dark:text-surface-100">{a.name || a.id}</span>

      {#if a.taskId}
        <button
          type="button"
          class="text-xs text-primary-500 hover:underline"
          onclick={() => onviewtask(a!.taskId)}
        >
          {a.taskId}
        </button>
      {/if}

      {#if a.project}
        <span class="rounded bg-surface-100 px-1.5 py-0.5 text-[10px] text-surface-500 dark:bg-surface-700">{a.project}</span>
      {/if}

      <div class="flex-1"></div>

      {#if a.state === 'running' || a.state === 'paused'}
        <button
          type="button"
          class="rounded bg-surface-300 px-2.5 py-1 text-xs font-medium text-surface-900 hover:bg-surface-400 dark:bg-surface-700 dark:text-surface-100 dark:hover:bg-surface-600"
          onclick={handleStop}
        >
          Stop
        </button>
      {/if}
      <button
        type="button"
        class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600"
        onclick={handleEndChat}
        title="Stop chat and delete worktree"
      >
        End chat
      </button>
    {/if}
  </div>

  {#if error}
    <p class="px-4 py-2 text-sm text-error-500">{error}</p>
  {/if}

  {#if a}
    <div class="min-h-0 flex-1">
      <ChatView agentId={agentId} agentState={a.state} costUsd={a.costUsd} inputTokens={a.inputTokens ?? 0} outputTokens={a.outputTokens ?? 0} />
    </div>
  {:else}
    <p class="p-6 text-sm text-surface-500">Loading...</p>
  {/if}
</div>
