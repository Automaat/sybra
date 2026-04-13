<script lang="ts">
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
    <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
    </svg>
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
      <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
      </svg>
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
    class="tap flex flex-1 flex-col items-center justify-center gap-0.5 py-2 text-[10px] font-medium text-surface-500 transition-colors active:bg-surface-100 dark:active:bg-surface-800"
    class:text-primary-600={navStore.activeTab === 'agents'}
    class:dark:text-primary-400={navStore.activeTab === 'agents'}
    aria-label="Agents"
  >
    <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
    </svg>
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
      <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4" />
      </svg>
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
    <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" d="M4 6h16M4 12h16M4 18h16" />
    </svg>
    More
  </button>
</nav>
