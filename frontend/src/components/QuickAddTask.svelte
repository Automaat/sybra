<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'

  interface Props {
    open: boolean
    onclose: () => void
    oncreated?: (id: string) => void
  }

  const { open, onclose, oncreated }: Props = $props()

  let value = $state('')
  let submitting = $state(false)
  let inputEl = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (open) {
      value = ''
      requestAnimationFrame(() => inputEl?.focus())
    }
  })

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault()
      onclose()
    }
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!value.trim() || submitting) return

    submitting = true
    try {
      const t = await taskStore.create(value.trim(), '', 'interactive')
      value = ''
      onclose()
      oncreated?.(t.id)
    } finally {
      submitting = false
    }
  }
</script>

{#if open}
  <div class="fixed inset-0 z-40 bg-black/40" onclick={onclose} onkeydown={handleKeydown} role="none"></div>
  <div class="fixed left-1/2 top-1/4 z-50 w-full max-w-xl -translate-x-1/2">
    <form onsubmit={handleSubmit}>
      <input
        bind:this={inputEl}
        bind:value
        type="text"
        placeholder="Task title, link, or note..."
        disabled={submitting}
        onkeydown={handleKeydown}
        class="w-full rounded-xl border border-surface-300 bg-surface-50 px-5 py-3.5 text-base shadow-2xl outline-none ring-2 ring-primary-500/50 placeholder:text-surface-400 focus:ring-primary-500 dark:border-surface-600 dark:bg-surface-900 dark:placeholder:text-surface-500"
      />
    </form>
  </div>
{/if}
