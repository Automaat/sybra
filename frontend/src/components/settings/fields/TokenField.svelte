<script lang="ts">
  import SettingRow from './SettingRow.svelte'

  interface Props {
    label: string
    description?: string
    keyPath?: string
    /** Whether a token is already stored on the backend. */
    tokenSet: boolean
    /** Persists a new token (or "" to clear) via the dedicated write path. */
    onsave: (token: string) => Promise<void>
    id: string
  }

  const { label, description, keyPath, tokenSet, onsave, id }: Props = $props()

  let editing = $state(false)
  let reveal = $state(false)
  let draft = $state('')
  let busy = $state(false)
  let localSet = $state(false)

  $effect(() => {
    localSet = tokenSet
  })

  async function commit(token: string) {
    busy = true
    try {
      await onsave(token)
      localSet = token !== ''
      editing = false
      draft = ''
    } finally {
      busy = false
    }
  }
</script>

<SettingRow {label} {description} {keyPath} for={id}>
  {#snippet control()}
    {#if localSet && !editing}
      <div class="flex items-center gap-2">
        <span class="inline-flex items-center gap-1.5 rounded-lg border border-success-300/60 bg-success-500/10 px-3 py-2 text-sm text-success-700 dark:border-success-800/60 dark:text-success-300">
          <span aria-hidden="true">✓</span> Token stored
        </span>
        <button
          type="button"
          class="rounded-lg px-3 py-2 text-sm font-medium text-surface-700 hover:bg-surface-200 dark:text-surface-200 dark:hover:bg-surface-700"
          onclick={() => { editing = true; draft = '' }}
        >
          Replace
        </button>
        <button
          type="button"
          disabled={busy}
          class="rounded-lg px-3 py-2 text-sm font-medium text-error-600 hover:bg-error-500/10 disabled:opacity-40 dark:text-error-400"
          onclick={() => commit('')}
        >
          Clear
        </button>
      </div>
    {:else}
      <div class="flex items-center gap-2">
        <input
          {id}
          type={reveal ? 'text' : 'password'}
          placeholder="Paste API token"
          autocomplete="off"
          class="flex-1 rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          bind:value={draft}
        />
        <button
          type="button"
          title={reveal ? 'Hide token' : 'Reveal token'}
          aria-label={reveal ? 'Hide token' : 'Reveal token'}
          aria-pressed={reveal}
          class="rounded-lg px-2.5 py-2 text-sm text-surface-500 hover:bg-surface-200 dark:hover:bg-surface-700"
          onclick={() => (reveal = !reveal)}
        >
          <span aria-hidden="true">{reveal ? '🙈' : '👁'}</span>
        </button>
        <button
          type="button"
          disabled={busy || draft === ''}
          class="rounded-lg bg-primary-500 px-3 py-2 text-sm font-semibold text-primary-contrast-500 hover:bg-primary-600 disabled:opacity-40"
          onclick={() => commit(draft)}
        >
          {busy ? 'Saving…' : 'Save'}
        </button>
        {#if localSet}
          <button
            type="button"
            class="rounded-lg px-3 py-2 text-sm font-medium text-surface-600 hover:bg-surface-200 dark:text-surface-300 dark:hover:bg-surface-700"
            onclick={() => { editing = false; draft = '' }}
          >
            Cancel
          </button>
        {/if}
      </div>
    {/if}
  {/snippet}
</SettingRow>
