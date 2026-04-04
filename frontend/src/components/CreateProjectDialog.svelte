<script lang="ts">
  import { Dialog } from '@skeletonlabs/skeleton-svelte'
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

<Dialog
  {open}
  onOpenChange={(details) => {
    onOpenChange(details.open)
    if (!details.open) reset()
  }}
>
  <Dialog.Backdrop class="fixed inset-0 z-40 bg-black/50" />
  <Dialog.Positioner class="fixed inset-0 z-50 flex items-center justify-center p-4">
    <Dialog.Content class="w-full max-w-lg rounded-xl bg-surface-50 p-6 shadow-2xl dark:bg-surface-950">
      <Dialog.Title class="mb-4 text-lg font-bold">Add Project</Dialog.Title>

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
              class="flex-1 rounded-lg border px-3 py-2 text-sm font-medium transition-colors {projectType === 'pet' ? 'border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'border-surface-300 bg-surface-100 hover:bg-surface-200 dark:border-surface-600 dark:bg-surface-700 dark:hover:bg-surface-600'}"
              onclick={() => (projectType = 'pet')}
            >
              Pet Project
            </button>
            <button
              type="button"
              class="flex-1 rounded-lg border px-3 py-2 text-sm font-medium transition-colors {projectType === 'work' ? 'border-warning-500 bg-warning-50 text-warning-700 dark:bg-warning-900/30 dark:text-warning-300' : 'border-surface-300 bg-surface-100 hover:bg-surface-200 dark:border-surface-600 dark:bg-surface-700 dark:hover:bg-surface-600'}"
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

        <div class="flex justify-end gap-2">
          <Dialog.CloseTrigger
            class="rounded-lg px-4 py-2 text-sm font-medium hover:bg-surface-200 dark:hover:bg-surface-700"
          >
            Cancel
          </Dialog.CloseTrigger>
          <button
            type="submit"
            disabled={submitting || !url.trim()}
            class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
          >
            {submitting ? 'Cloning...' : 'Add Project'}
          </button>
        </div>
      </form>
    </Dialog.Content>
  </Dialog.Positioner>
</Dialog>
