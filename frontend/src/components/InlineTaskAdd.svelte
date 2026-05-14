<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'

  interface Props {
    status: string
  }

  const { status }: Props = $props()

  let active = $state(false)
  let title = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

  function open() {
    active = true
    title = ''
    requestAnimationFrame(() => inputRef?.focus())
  }

  function dismiss() {
    active = false
    title = ''
  }

  async function submit() {
    const t = title.trim()
    if (!t) return
    title = ''
    let created
    try {
      created = await taskStore.create(t, '', 'headless')
    } catch (err) {
      notificationStore.pushLocal('error', 'Create failed', String(err))
      return
    }
    if (status !== 'new') {
      try {
        await taskStore.update(created.id, { status })
      } catch (err) {
        notificationStore.pushLocal('error', 'Failed to set status', String(err))
      }
    }
    requestAnimationFrame(() => inputRef?.focus())
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      submit()
    } else if (e.key === 'Escape') {
      dismiss()
    }
  }
</script>

{#if active}
  <input
    bind:this={inputRef}
    bind:value={title}
    type="text"
    placeholder="Task title"
    class="w-full rounded-md border border-surface-300 bg-surface-50 px-2 py-2.5 text-base outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-600 dark:bg-surface-800 md:py-1.5 md:text-sm"
    onkeydown={handleKeydown}
    onblur={dismiss}
  />
{:else}
  <button
    type="button"
    class="tap flex w-full items-center gap-1 rounded-md px-2 py-2.5 text-sm opacity-60 transition-opacity active:bg-surface-200 active:opacity-100 dark:active:bg-surface-800 md:py-1.5 md:hover:bg-surface-200 md:hover:opacity-100 dark:md:hover:bg-surface-800"
    onclick={open}
    title="Add task (C)"
  >
    <span class="text-base leading-none">+</span> Add task
  </button>
{/if}
