<script lang="ts">
  interface Props {
    disabled?: boolean
    onsend: (text: string) => void
  }

  const { disabled = false, onsend }: Props = $props()

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
    textarea.style.height = Math.min(textarea.scrollHeight, 200) + 'px'
  }
</script>

<div class="border-t border-surface-300 px-4 py-3 dark:border-surface-600">
  <div class="flex items-end gap-2">
    <textarea
      bind:this={textarea}
      bind:value={text}
      {disabled}
      placeholder={disabled ? 'Agent is thinking...' : 'Type a message...'}
      rows="1"
      class="flex-1 resize-none rounded-lg border border-surface-300 bg-surface-50 px-3 py-2 text-sm
        text-surface-900 placeholder:text-surface-400
        focus:border-primary-500 focus:outline-none
        disabled:cursor-not-allowed disabled:opacity-50
        dark:border-surface-600 dark:bg-surface-800 dark:text-surface-100 dark:placeholder:text-surface-500"
      onkeydown={handleKeydown}
      oninput={autoResize}
    ></textarea>
    <button
      type="button"
      disabled={disabled || !text.trim()}
      title="Send message"
      class="shrink-0 rounded-lg bg-primary-600 p-2 text-white hover:bg-primary-700
        disabled:cursor-not-allowed disabled:opacity-50"
      onclick={submit}
    >
      <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 19V5m0 0l-7 7m7-7l7 7" />
      </svg>
    </button>
  </div>
</div>
