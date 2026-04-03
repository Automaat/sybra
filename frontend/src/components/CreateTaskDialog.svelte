<script lang="ts">
  import { Dialog, SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import { taskStore } from '../stores/tasks.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    oncreated?: (id: string) => void
  }

  const { open, onOpenChange, oncreated }: Props = $props()

  let title = $state('')
  let body = $state('')
  let mode = $state('headless')
  let submitting = $state(false)
  let error = $state('')

  function reset() {
    title = ''
    body = ''
    mode = 'headless'
    error = ''
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!title.trim()) return

    submitting = true
    error = ''
    try {
      const t = await taskStore.create(title.trim(), body, mode)
      reset()
      onOpenChange(false)
      oncreated?.(t.id)
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
      <Dialog.Title class="mb-4 text-lg font-bold">New Task</Dialog.Title>

      <form onsubmit={handleSubmit} class="flex flex-col gap-4">
        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Title</span>
          <input
            type="text"
            bind:value={title}
            placeholder="Task title..."
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            required
          />
        </label>

        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium">Agent Mode</span>
          <SegmentedControl value={mode} onValueChange={(details) => (mode = details.value ?? 'headless')}>
            <SegmentedControl.Control>
              <SegmentedControl.Indicator />
              <SegmentedControl.Item value="headless">
                <SegmentedControl.ItemText>Headless</SegmentedControl.ItemText>
                <SegmentedControl.ItemHiddenInput />
              </SegmentedControl.Item>
              <SegmentedControl.Item value="interactive">
                <SegmentedControl.ItemText>Interactive</SegmentedControl.ItemText>
                <SegmentedControl.ItemHiddenInput />
              </SegmentedControl.Item>
            </SegmentedControl.Control>
          </SegmentedControl>
        </div>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Description</span>
          <textarea
            bind:value={body}
            rows={5}
            placeholder="Task description (markdown)..."
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          ></textarea>
        </label>

        {#if error}
          <p class="text-sm text-error-500">{error}</p>
        {/if}

        <div class="flex justify-end gap-2">
          <Dialog.CloseTrigger
            class="rounded-lg px-4 py-2 text-sm font-medium hover:bg-surface-200 dark:hover:bg-surface-700"
          >
            Cancel
          </Dialog.CloseTrigger>
          <button
            type="submit"
            disabled={submitting || !title.trim()}
            class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
          >
            {submitting ? 'Creating...' : 'Create'}
          </button>
        </div>
      </form>
    </Dialog.Content>
  </Dialog.Positioner>
</Dialog>
