<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'
  import NewChatDialog from '../components/NewChatDialog.svelte'
  import { MessageCircle } from '@lucide/svelte'

  interface Props {
    onselect: (agentId: string) => void
  }

  const { onselect }: Props = $props()

  let dialogOpen = $state(false)

  $effect(() => {
    if (projectStore.list.length === 0) {
      projectStore.load().catch(() => {})
    }
  })

  function openDialog() {
    dialogOpen = true
  }

  const interactiveAgents = $derived(
    agentStore.list.filter((a: agent.Agent) => a.mode === 'interactive'),
  )

  function agentPhaseConfig(a: agent.Agent) {
    return PHASE_CONFIG[getAgentPhase(a.state, a.escalationReason, undefined, a.awaitingApproval)]
  }

  function timeAgo(date: string | undefined): string {
    if (!date) return ''
    const ms = Date.now() - new Date(date).getTime()
    const mins = Math.floor(ms / 60000)
    if (mins < 1) return 'just now'
    if (mins < 60) return `${mins}m ago`
    const hrs = Math.floor(mins / 60)
    if (hrs < 24) return `${hrs}h ago`
    return `${Math.floor(hrs / 24)}d ago`
  }
</script>

<div class="flex flex-col gap-3 p-4 md:gap-4 md:p-6">
  <div class="flex items-center justify-between">
    <h2 class="text-lg font-semibold text-surface-900 dark:text-surface-100">Chats</h2>
    <button
      type="button"
      class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
      onclick={openDialog}
    >
      + New Chat
    </button>
  </div>

  {#if interactiveAgents.length === 0}
    <div class="flex flex-col items-center gap-3 py-16 text-center">
      <MessageCircle size={48} class="text-surface-400" />
      <p class="text-sm text-surface-500">No interactive chats yet</p>
      <button
        type="button"
        class="mt-2 rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600"
        onclick={openDialog}
      >
        Start a new chat
      </button>
    </div>
  {:else}
    <div class="flex flex-col gap-2">
      {#each interactiveAgents as a (a.id)}
        <button
          type="button"
          class="flex items-center gap-3 rounded-lg border border-surface-200 bg-white p-4 text-left transition-colors
            hover:bg-surface-50 dark:border-surface-700 dark:bg-surface-800 dark:hover:bg-surface-750"
          onclick={() => onselect(a.id)}
        >
          <!-- Status dot -->
          <span class="h-2.5 w-2.5 shrink-0 rounded-full {agentPhaseConfig(a).dotClasses}
            {agentPhaseConfig(a).animate ? 'animate-pulse' : ''}"></span>

          <!-- Content -->
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="truncate text-sm font-medium text-surface-900 dark:text-surface-100">
                {a.name || a.taskId || a.id}
              </span>
              {#if a.project}
                <span class="shrink-0 rounded bg-surface-100 px-1.5 py-0.5 text-[10px] text-surface-500 dark:bg-surface-700">
                  {a.project}
                </span>
              {/if}
            </div>
            <p class="mt-0.5 text-xs text-surface-500">
              {agentPhaseConfig(a).label}
            </p>
          </div>

          <!-- Meta -->
          <div class="flex shrink-0 flex-col items-end gap-1">
            {#if a.costUsd > 0}
              <span class="text-xs text-surface-500">${a.costUsd.toFixed(2)}</span>
            {/if}
            <span class="text-[10px] text-surface-400">{timeAgo(a.startedAt)}</span>
          </div>
        </button>
      {/each}
    </div>
  {/if}
</div>

<NewChatDialog
  open={dialogOpen}
  onOpenChange={(v) => (dialogOpen = v)}
  oncreated={(id) => onselect(id)}
/>
