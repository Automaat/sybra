<script lang="ts">
  import { GetRawConfig, SaveRawConfig } from '$lib/api'

  interface Props {
    /** Called after a successful save so the parent can reload the form state. */
    onsaved?: () => void
  }

  const { onsaved }: Props = $props()

  let text = $state('')
  let original = $state('')
  let loading = $state(true)
  let saving = $state(false)
  let error = $state('')
  let saved = $state(false)

  const dirty = $derived(!loading && text !== original)

  async function load() {
    loading = true
    error = ''
    try {
      const raw = await GetRawConfig()
      text = raw
      original = raw
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function save() {
    saving = true
    error = ''
    saved = false
    try {
      await SaveRawConfig(text)
      original = text
      saved = true
      setTimeout(() => (saved = false), 3000)
      onsaved?.()
    } catch (e) {
      // Backend rejects invalid YAML / out-of-range values without touching disk.
      error = String(e).replace(/^Error:\s*/, '')
    } finally {
      saving = false
    }
  }

  function revert() {
    text = original
    error = ''
  }

  $effect(() => {
    load()
  })
</script>

<section class="flex flex-col gap-3 rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
  <div class="flex items-start justify-between gap-3">
    <div>
      <h2 class="text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Config file (YAML)</h2>
      <p class="mt-1 text-xs text-surface-500 dark:text-surface-400">
        The complete <code>config.yaml</code> — every setting, including sections without a form. Edits are validated before saving; invalid YAML is rejected without touching disk.
      </p>
    </div>
    <div class="flex shrink-0 items-center gap-2">
      {#if saved}<span class="text-sm font-medium text-success-600 dark:text-success-400">Saved</span>{/if}
      {#if dirty}
        <span class="text-xs font-medium text-warning-600 dark:text-warning-400">Unsaved</span>
        <button type="button" class="rounded-lg px-3 py-1.5 text-sm font-medium text-surface-700 hover:bg-surface-200 dark:text-surface-200 dark:hover:bg-surface-700" onclick={revert}>Revert</button>
      {/if}
      <button
        type="button"
        class="rounded-lg bg-primary-500 px-4 py-1.5 text-sm font-semibold text-primary-contrast-500 hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-40"
        onclick={save}
        disabled={!dirty || saving}
      >
        {saving ? 'Saving…' : 'Save'}
      </button>
    </div>
  </div>

  <div class="flex items-center gap-2 rounded-lg border border-warning-300/60 bg-warning-500/10 px-3 py-2 text-xs text-warning-700 dark:border-warning-800/50 dark:text-warning-300">
    <span aria-hidden="true">⚠️</span>
    This view shows secrets (e.g. API tokens) in plain text. It is your local file — take care when sharing your screen.
  </div>

  {#if error}
    <pre class="max-h-40 overflow-auto rounded-lg border border-error-300/70 bg-error-500/10 px-3 py-2 font-mono text-xs whitespace-pre-wrap text-error-700 dark:border-error-800/60 dark:text-error-300">{error}</pre>
  {/if}

  {#if loading}
    <p class="text-sm text-surface-500 dark:text-surface-400">Loading…</p>
  {:else}
    <textarea
      spellcheck="false"
      class="h-[60vh] w-full resize-y rounded-lg border border-surface-300 bg-white p-3 font-mono text-xs leading-relaxed text-surface-800 dark:border-surface-600 dark:bg-surface-900 dark:text-surface-100"
      bind:value={text}
    ></textarea>
  {/if}
</section>
