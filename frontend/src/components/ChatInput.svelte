<script lang="ts">
  interface Props {
    disabled?: boolean
    placeholder?: string
    onsend: (text: string) => void
  }

  const { disabled = false, placeholder = 'Type a message...', onsend }: Props = $props()

  let text = $state('')
  let textarea: HTMLTextAreaElement | undefined = $state()

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  function submit() {
    const trimmed = text.trim()
    if (!trimmed || disabled) return
    onsend(trimmed)
    text = ''
    if (textarea) {
      textarea.style.height = 'auto'
    }
  }

  function autoResize() {
    if (!textarea) return
    textarea.style.height = 'auto'
    const max = typeof window !== 'undefined' ? Math.min(textarea.scrollHeight, window.innerHeight * 0.4) : Math.min(textarea.scrollHeight, 200)
    textarea.style.height = max + 'px'
  }
</script>

<div class="sticky bottom-0 z-10 shrink-0 border-t border-surface-300 bg-surface-50/95 px-3 pb-safe pt-3 backdrop-blur dark:border-surface-600 dark:bg-surface-950/95 md:px-4 md:pb-3">
  <div class="flex items-end gap-2">
    <textarea
      bind:this={textarea}
      bind:value={text}
      {disabled}
      placeholder={disabled ? 'Waiting for approval...' : placeholder}
      rows="1"
      class="flex-1 resize-none rounded-lg border border-surface-300 bg-surface-50 px-3 py-2 text-base
        text-surface-900 placeholder:text-surface-400
        focus:border-primary-500 focus:outline-none
        disabled:cursor-not-allowed disabled:opacity-50
        dark:border-surface-600 dark:bg-surface-800 dark:text-surface-100 dark:placeholder:text-surface-500
        md:text-sm"
      onkeydown={handleKeydown}
      oninput={autoResize}
    ></textarea>
    <button
      type="button"
      disabled={disabled || !text.trim()}
      title="Send message"
      class="tap shrink-0 rounded-lg bg-primary-600 p-3 text-white active:bg-primary-700
        disabled:cursor-not-allowed disabled:opacity-50"
      onclick={submit}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 19V5m0 0l-7 7m7-7l7 7" />
      </svg>
    </button>
  </div>
</div>
