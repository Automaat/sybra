<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'

  // Mirrors github.ParsePRURL (internal/github/client.go) — a fast client-side
  // check; the backend remains the source of truth for what actually enriches.
  // Anchored so a PR number followed by anything other than "/" (e.g. a
  // "?diff=split" or "#discussion_r123" glued onto the number from a copied
  // browser URL) fails here instead of silently becoming a plain task with a
  // garbage title — ParsePRURL's own strconv.Atoi on that path segment would
  // reject it the same way. The number is captured (not just \d+-matched) so
  // isValidPRURL can also reject "pull/0", matching ParsePRURL's n==0 check.
  const PR_URL_RE = /^https:\/\/github\.com\/[^/\s]+\/[^/\s]+\/pull\/(\d+)(?:\/.*)?$/

  function stripQueryAndFragment(url: string): string {
    const i = url.search(/[?#]/)
    return i === -1 ? url : url.slice(0, i)
  }

  function isValidPRURL(url: string): boolean {
    const m = PR_URL_RE.exec(url)
    return m !== null && Number(m[1]) !== 0
  }

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
    const url = stripQueryAndFragment(link.trim())
    if (!url) return
    if (!isValidPRURL(url)) {
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
