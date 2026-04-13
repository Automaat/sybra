<script lang="ts">
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { fly } from 'svelte/transition'

  type Props = {
    onviewtask?: (id: string) => void
  }
  let { onviewtask }: Props = $props()

  const visible = $derived(notificationStore.notifications.slice(0, 3))

  function levelClass(level: string): string {
    switch (level) {
      case 'success':
        return 'bg-success-800/90 border-success-500 text-success-100'
      case 'warning':
        return 'bg-warning-800/90 border-warning-500 text-warning-100'
      case 'error':
        return 'bg-error-800/90 border-error-500 text-error-100'
      default:
        return 'bg-surface-700 border-surface-500 text-surface-100'
    }
  }

  function dismiss(id: string) {
    notificationStore.dismiss(id)
  }

  function autoDismiss(_node: HTMLElement, id: string) {
    const timer = setTimeout(() => dismiss(id), 5000)
    return {
      destroy() {
        clearTimeout(timer)
      },
    }
  }
</script>

<div class="fixed inset-x-3 bottom-[calc(env(safe-area-inset-bottom)+5rem)] z-[60] flex flex-col gap-2 md:inset-x-auto md:bottom-4 md:right-4 md:w-80">
  {#each visible as toast (toast.id)}
    <div
      class="rounded-lg border shadow-lg w-full {levelClass(toast.level)}"
      role="alert"
      transition:fly={{ x: 100, duration: 200 }}
      use:autoDismiss={toast.id}
    >
      <div class="flex justify-between items-start">
        <button
          type="button"
          class="flex-1 px-4 py-3 text-left cursor-pointer"
          onclick={() => {
            if (toast.taskId) onviewtask?.(toast.taskId)
            dismiss(toast.id)
          }}
        >
          <div class="font-medium text-sm">{toast.title}</div>
          <div class="text-xs opacity-75 mt-0.5">{toast.message}</div>
        </button>
        <button
          type="button"
          class="px-3 py-3 opacity-50 hover:opacity-100 text-xs"
          onclick={() => dismiss(toast.id)}
          aria-label="Dismiss"
        >
          ✕
        </button>
      </div>
    </div>
  {/each}
</div>
