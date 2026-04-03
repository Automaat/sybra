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

<div class="fixed bottom-4 right-4 z-50 flex flex-col gap-2 w-80">
  {#each visible as toast (toast.id)}
    <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions a11y_no_noninteractive_element_interactions -->
    <div
      class="rounded-lg border px-4 py-3 shadow-lg text-left w-full cursor-pointer {levelClass(toast.level)}"
      onclick={() => {
        if (toast.taskId) onviewtask?.(toast.taskId)
        dismiss(toast.id)
      }}
      role="alert"
      transition:fly={{ x: 100, duration: 200 }}
      use:autoDismiss={toast.id}
    >
      <div class="flex justify-between items-start">
        <div>
          <div class="font-medium text-sm">{toast.title}</div>
          <div class="text-xs opacity-75 mt-0.5">{toast.message}</div>
        </div>
        <button
          class="ml-2 opacity-50 hover:opacity-100 text-xs"
          onclick={(e: MouseEvent) => { e.stopPropagation(); dismiss(toast.id) }}
          aria-label="Dismiss"
        >
          ✕
        </button>
      </div>
    </div>
  {/each}
</div>
