<script lang="ts">
  import { fly } from 'svelte/transition'
  import { cubicOut } from 'svelte/easing'
  import { X } from '@lucide/svelte'
  import TaskDetail from '../pages/TaskDetail.svelte'

  interface Props {
    taskId: string | null
    onclose: () => void
    onviewagent: (agentId: string) => void
    onviewtask: (taskId: string) => void
  }

  const { taskId, onclose, onviewagent, onviewtask }: Props = $props()

  $effect(() => {
    if (!taskId) return
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        const target = e.target as HTMLElement
        const inInput = target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable
        if (!inInput) { e.preventDefault(); onclose() }
      }
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })
</script>

{#if taskId}
  <div
    class="flex h-full w-[480px] shrink-0 flex-col border-l border-surface-200 bg-surface-50 dark:border-surface-700 dark:bg-surface-900"
    transition:fly={{ x: 480, duration: 200, easing: cubicOut }}
  >
    <div class="flex shrink-0 items-center justify-between border-b border-surface-200 px-4 py-2 dark:border-surface-700">
      <p class="text-xs text-surface-400">
        Task detail <kbd class="ml-1 rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">⌘I</kbd>
      </p>
      <div class="flex items-center gap-2">
        <button
          type="button"
          class="text-xs text-primary-500 hover:underline"
          onclick={() => onviewtask(taskId)}
        >Open full</button>
        <button
          type="button"
          onclick={onclose}
          class="rounded p-1 text-surface-400 hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
          aria-label="Close sidebar"
        >
          <X size={16} />
        </button>
      </div>
    </div>
    <div class="min-h-0 flex-1 overflow-y-auto">
      <TaskDetail
        {taskId}
        onback={onclose}
        {onviewagent}
        ondelete={onclose}
      />
    </div>
  </div>
{/if}
