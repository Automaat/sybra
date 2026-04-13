<script lang="ts">
  import MobileSheet from './shell/MobileSheet.svelte'
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    oncreated?: (id: string) => void
  }

  const { open, onOpenChange, oncreated }: Props = $props()

  let url = $state('')
  let projectType = $state<'pet' | 'work'>('pet')
  let submitting = $state(false)
  let error = $state('')

  function reset() {
    url = ''
    projectType = 'pet'
    error = ''
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!url.trim()) return

    submitting = true
    error = ''
    try {
      const p = await projectStore.create(url.trim(), projectType)
      reset()
      onOpenChange(false)
      oncreated?.(p.id)
    } catch (e) {
      error = String(e)
    } finally {
      submitting = false
    }
  }
</script>

<MobileSheet
  {open}
  onOpenChange={(o) => {
    onOpenChange(o)
    if (!o) reset()
  }}
  variant="bottom"
  title="Add Project"
>
  <div class="flex flex-col px-5 pb-5 md:px-6 md:pb-6">
      <form onsubmit={handleSubmit} class="flex flex-col gap-4">
        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">GitHub URL</span>
          <input
            type="text"
            bind:value={url}
            placeholder="https://github.com/owner/repo"
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            required
          />
        </label>

        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium">Project Type</span>
          <div class="flex gap-2">
            <button
              type="button"
              class="tap flex-1 rounded-lg border px-3 py-3 text-sm font-medium transition-colors {projectType === 'pet' ? 'border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'border-surface-300 bg-surface-100 active:bg-surface-200 dark:border-surface-600 dark:bg-surface-700 dark:active:bg-surface-600'}"
              onclick={() => (projectType = 'pet')}
            >
              Pet Project
            </button>
            <button
              type="button"
              class="tap flex-1 rounded-lg border px-3 py-3 text-sm font-medium transition-colors {projectType === 'work' ? 'border-warning-500 bg-warning-50 text-warning-700 dark:bg-warning-900/30 dark:text-warning-300' : 'border-surface-300 bg-surface-100 active:bg-surface-200 dark:border-surface-600 dark:bg-surface-700 dark:active:bg-surface-600'}"
              onclick={() => (projectType = 'work')}
            >
              Work Project
            </button>
          </div>
        </div>

        {#if error}
          <p class="text-sm text-error-500">{error}</p>
        {/if}

        {#if submitting}
          <p class="text-sm text-surface-500">Cloning repository...</p>
        {/if}

        <div class="sticky bottom-0 -mx-5 -mb-5 flex justify-end gap-2 border-t border-surface-200 bg-surface-50/95 px-5 pt-3 pb-safe backdrop-blur dark:border-surface-800 dark:bg-surface-950/95 md:-mx-6 md:-mb-6 md:px-6 md:pb-4">
          <button
            type="button"
            onclick={() => { onOpenChange(false); reset() }}
            class="tap rounded-lg px-4 py-2.5 text-sm font-medium active:bg-surface-200 dark:active:bg-surface-700"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={submitting || !url.trim()}
            class="tap rounded-lg bg-primary-500 px-5 py-2.5 text-sm font-medium text-white active:bg-primary-700 disabled:opacity-50"
          >
            {submitting ? 'Cloning...' : 'Add Project'}
          </button>
        </div>
      </form>
  </div>
</MobileSheet>
