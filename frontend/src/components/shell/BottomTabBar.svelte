<script lang="ts">
  import { ClipboardList, MessageCircle, UserCircle, ClipboardCheck, Menu } from '@lucide/svelte'
  import { navStore, type TabKey } from '../../lib/navigation.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'

  interface Props {
    onmore: () => void
  }

  const { onmore }: Props = $props()

  const interactiveAgentCount = $derived(
    agentStore.list.filter(a => a.mode === 'interactive' && (a.state === 'running' || a.state === 'paused')).length
  )

  const runningAgentCount = $derived(
    agentStore.list.filter(a => a.state === 'running').length
  )

  const reviewCount = $derived(
    taskStore.byStatus('plan-review').length + taskStore.byStatus('test-plan-review').length
  )

  function tab(key: TabKey, action: () => void) {
    return {
      key,
      active: navStore.activeTab === key,
      action,
    }
  }
</script>

<nav
  class="fixed inset-x-0 bottom-0 z-40 flex shrink-0 items-stretch justify-between border-t border-surface-200 bg-surface-50/95 backdrop-blur pb-safe pl-safe pr-safe dark:border-surface-800 dark:bg-surface-950/95"
  aria-label="Primary"
>
  <button
    type="button"
    onclick={() => navStore.reset({ kind: 'task-list' })}
    data-active={navStore.activeTab === 'board' || undefined}
    class="tap flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'board'}
    class:dark:text-primary-400={navStore.activeTab === 'board'}
    aria-label="Board"
  >
    <ClipboardList size={24} />
    Board
  </button>

  <button
    type="button"
    onclick={() => navStore.reset({ kind: 'chats' })}
    data-active={navStore.activeTab === 'chats' || undefined}
    class="tap relative flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'chats'}
    class:dark:text-primary-400={navStore.activeTab === 'chats'}
    aria-label="Chats"
  >
    <div class="relative">
      <MessageCircle size={24} />
      {#if interactiveAgentCount > 0}
        <span class="absolute -right-1.5 -top-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-primary-500 px-1 text-[9px] font-bold text-white">{interactiveAgentCount}</span>
      {/if}
    </div>
    Chats
  </button>

  <button
    type="button"
    onclick={() => navStore.reset({ kind: 'agents' })}
    data-active={navStore.activeTab === 'agents' || undefined}
    class="tap relative flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'agents'}
    class:dark:text-primary-400={navStore.activeTab === 'agents'}
    aria-label="Agents"
  >
    <div class="relative">
      <UserCircle size={24} />
      {#if runningAgentCount > 0}
        <span class="absolute -right-1.5 -top-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-success-500 px-1 text-[9px] font-bold text-white">{runningAgentCount}</span>
      {/if}
    </div>
    Agents
  </button>

  <button
    type="button"
    onclick={() => navStore.reset({ kind: 'reviews' })}
    data-active={navStore.activeTab === 'reviews' || undefined}
    class="tap relative flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'reviews'}
    class:dark:text-primary-400={navStore.activeTab === 'reviews'}
    aria-label="Reviews"
  >
    <div class="relative">
      <ClipboardCheck size={24} />
      {#if reviewCount > 0}
        <span class="absolute -right-1.5 -top-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-warning-500 px-1 text-[9px] font-bold text-white">{reviewCount}</span>
      {/if}
    </div>
    Reviews
  </button>

  <button
    type="button"
    onclick={onmore}
    data-active={navStore.activeTab === 'more' || undefined}
    class="tap flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'more'}
    class:dark:text-primary-400={navStore.activeTab === 'more'}
    aria-label="More"
  >
    <Menu size={24} />
    More
  </button>
</nav>
