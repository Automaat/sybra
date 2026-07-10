<script lang="ts">
  import { Bell, Trash2 } from '@lucide/svelte'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { timeAgo, formatDateTime } from '../lib/dates.js'

  interface Props {
    onviewtask?: (id: string) => void
    onviewagent?: (id: string) => void
  }

  const { onviewtask, onviewagent }: Props = $props()

  // Store is already newest-first (load/listen/pushLocal all prepend).
  const notifications = $derived(notificationStore.notifications)

  function levelClasses(level: string): string {
    switch (level) {
      case 'success':
        return 'bg-success-100 text-success-700 dark:bg-success-900/40 dark:text-success-300'
      case 'warning':
        return 'bg-warning-100 text-warning-700 dark:bg-warning-900/40 dark:text-warning-300'
      case 'error':
        return 'bg-error-100 text-error-700 dark:bg-error-900/40 dark:text-error-300'
      default:
        return 'bg-surface-200 text-surface-700 dark:bg-surface-700 dark:text-surface-300'
    }
  }
</script>

<div class="flex h-full min-h-0 flex-col overflow-hidden">
  <div class="flex items-center justify-between gap-2 border-b border-surface-200 px-4 py-3 dark:border-surface-700">
    <span class="text-sm text-surface-400">{notifications.length} notification{notifications.length === 1 ? '' : 's'}</span>
    {#if notifications.length > 0}
      <button
        type="button"
        class="tap flex items-center gap-1 rounded-md border border-surface-300 bg-surface-50 px-2 py-1 text-xs font-medium hover:bg-surface-200 dark:border-surface-700 dark:bg-surface-800 dark:hover:bg-surface-700"
        onclick={() => notificationStore.clear()}
      >
        <Trash2 size={14} />
        Clear all
      </button>
    {/if}
  </div>

  <div class="min-h-0 flex-1 overflow-y-auto">
    {#if notifications.length === 0}
      <div class="flex flex-col items-center gap-2 p-12 text-center text-surface-400">
        <Bell size={32} class="opacity-40" />
        <p class="text-sm font-medium">No notifications yet</p>
        <p class="text-xs">Agent and workflow events will show up here.</p>
      </div>
    {:else}
      <ul class="flex flex-col divide-y divide-surface-200 dark:divide-surface-700">
        {#each notifications as n (n.id)}
          <li class="flex items-start gap-3 px-4 py-3">
            <span class="mt-0.5 shrink-0 rounded-full px-2 py-0.5 text-xs font-medium {levelClasses(n.level)}">{n.level}</span>
            <div class="min-w-0 flex-1">
              <div class="flex items-baseline justify-between gap-2">
                <span class="truncate text-sm font-medium">{n.title}</span>
                <span class="shrink-0 text-xs text-surface-400" title={formatDateTime(n.createdAt)}>{timeAgo(n.createdAt)}</span>
              </div>
              <p class="mt-0.5 text-sm text-surface-500 dark:text-surface-400">{n.message}</p>
              <div class="mt-1.5 flex items-center gap-3">
                {#if n.taskId && onviewtask}
                  <button type="button" class="text-xs text-primary-500 hover:underline" onclick={() => onviewtask!(n.taskId!)}>View task →</button>
                {/if}
                {#if n.agentId && onviewagent}
                  <button type="button" class="text-xs text-primary-500 hover:underline" onclick={() => onviewagent!(n.agentId!)}>View agent →</button>
                {/if}
                <button type="button" class="text-xs text-surface-400 hover:underline" onclick={() => notificationStore.dismiss(n.id)}>Dismiss</button>
              </div>
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</div>
