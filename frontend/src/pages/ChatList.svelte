<script lang="ts">
  import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import AgentListRow from '../components/AgentListRow.svelte'
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
    agentStore.list.filter((a: Agent) => a.mode === 'interactive'),
  )
</script>

<div class="flex flex-col gap-3 p-4 md:gap-4 md:p-6">
  <div class="flex items-center justify-end">
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
    <div class="flex flex-col gap-1.5">
      {#each interactiveAgents as a (a.id)}
        <AgentListRow agent={a} onclick={() => onselect(a.id)} />
      {/each}
    </div>
  {/if}
</div>

<NewChatDialog
  open={dialogOpen}
  onOpenChange={(v) => (dialogOpen = v)}
  oncreated={(id) => onselect(id)}
/>
