<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'

  // Mirrors github.ParsePRURL (internal/github/client.go) — a fast client-side
  // check; the backend remains the source of truth for what actually enriches.
  const PR_URL_RE = /^https:\/\/github\.com\/[^/\s]+\/[^/\s]+\/pull\/\d+/

  let active = $state(false)
  let link = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

  function open() {
    active = true
    link = ''
    requestAnimationFrame(() => inputRef?.focus())
  }

  function dismiss() {
    active = false
    link = ''
  }

  async function submit() {
    const url = link.trim()
    if (!url) return
    if (!PR_URL_RE.test(url)) {
      notificationStore.pushLocal(
        'error',
        'Invalid link',
        'Paste a GitHub PR URL, e.g. https://github.com/owner/repo/pull/123',
      )
      return
    }
    link = ''
    try {
      await taskStore.create(url, '', 'headless')
    } catch (err) {
      notificationStore.pushLocal('error', 'Create failed', String(err))
      return
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
    bind:value={link}
    type="text"
    placeholder="Paste a GitHub PR link…"
    class="w-full rounded-md border border-surface-300 bg-surface-50 px-2 py-2.5 text-base outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-600 dark:bg-surface-800 md:py-1.5 md:text-sm"
    onkeydown={handleKeydown}
    onblur={dismiss}
  />
{:else}
  <button
    type="button"
    class="tap flex w-full items-center gap-1 rounded-md px-2 py-2.5 text-sm opacity-60 transition-opacity active:bg-surface-200 active:opacity-100 dark:active:bg-surface-800 md:py-1.5 md:hover:bg-surface-200 md:hover:opacity-100 dark:md:hover:bg-surface-800"
    onclick={open}
    title="Add a task with a link to review"
  >
    <span class="text-base leading-none">+</span> Add review link
  </button>
{/if}
