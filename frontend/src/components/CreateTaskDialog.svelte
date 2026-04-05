<script lang="ts">
  import { Dialog } from '@skeletonlabs/skeleton-svelte'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    oncreated?: (id: string) => void
  }

  const { open, onOpenChange, oncreated }: Props = $props()

  let title = $state('')
  let body = $state('')
  let headless = $state(false)
  let taskType = $state('normal')
  let selectedProject = $state('')
  let projectSearch = $state('')
  let projectDropdownOpen = $state(false)
  let submitting = $state(false)
  let error = $state('')

  const filteredProjects = $derived(
    projectStore.list.filter((p) => {
      if (!projectSearch) return true
      const q = projectSearch.toLowerCase()
      return p.id.toLowerCase().includes(q) || p.name.toLowerCase().includes(q)
    })
  )

  const selectedProjectName = $derived(
    selectedProject ? projectStore.list.find((p) => p.id === selectedProject)?.id ?? '' : ''
  )

  function selectProject(id: string) {
    selectedProject = id
    projectSearch = ''
    projectDropdownOpen = false
  }

  function clearProject() {
    selectedProject = ''
    projectSearch = ''
    projectDropdownOpen = false
  }

  function reset() {
    title = ''
    body = ''
    headless = false
    taskType = 'normal'
    selectedProject = ''
    projectSearch = ''
    projectDropdownOpen = false
    error = ''
  }

  function handleProjectBlur() {
    setTimeout(() => { projectDropdownOpen = false }, 150)
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!title.trim()) return

    submitting = true
    error = ''
    try {
      // debug/research task types force the agent mode; ignore checkbox.
      const effectiveMode =
        taskType === 'debug' ? 'interactive'
        : taskType === 'research' ? 'headless'
        : (headless ? 'headless' : 'interactive')
      let t = await taskStore.create(title.trim(), body, effectiveMode)
      const updates: Record<string, unknown> = {}
      if (taskType !== 'normal') updates.task_type = taskType
      if (selectedProject) updates.project_id = selectedProject
      if (Object.keys(updates).length > 0) {
        t = await taskStore.update(t.id, updates)
      }
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

        {#if projectStore.list.length > 0}
          <div class="flex flex-col gap-1">
            <span class="text-sm font-medium">Project</span>
            {#if selectedProject}
              <div class="flex items-center gap-2 rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700">
                <svg class="h-4 w-4 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
                </svg>
                <span class="flex-1">{selectedProjectName}</span>
                <button
                  type="button"
                  class="text-surface-400 hover:text-surface-600 dark:hover:text-surface-300"
                  onclick={clearProject}
                >
                  <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                  </svg>
                </button>
              </div>
            {:else}
              <div class="relative">
                <input
                  type="text"
                  bind:value={projectSearch}
                  placeholder="Search projects..."
                  class="w-full rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
                  onfocus={() => (projectDropdownOpen = true)}
                  onblur={handleProjectBlur}
                />
                {#if projectDropdownOpen}
                  <div class="absolute z-10 mt-1 max-h-48 w-full overflow-y-auto rounded-lg border border-surface-300 bg-surface-50 shadow-lg dark:border-surface-600 dark:bg-surface-800">
                    <button
                      type="button"
                      class="w-full px-3 py-2 text-left text-sm text-surface-400 hover:bg-surface-100 dark:hover:bg-surface-700"
                      onclick={() => { selectedProject = ''; projectDropdownOpen = false }}
                    >
                      None
                    </button>
                    {#each filteredProjects as p (p.id)}
                      <button
                        type="button"
                        class="flex w-full items-center gap-2 px-3 py-2 text-left text-sm hover:bg-surface-100 dark:hover:bg-surface-700"
                        onclick={() => selectProject(p.id)}
                      >
                        <svg class="h-4 w-4 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
                        </svg>
                        {p.owner}/{p.repo}
                      </button>
                    {/each}
                    {#if filteredProjects.length === 0}
                      <div class="px-3 py-2 text-sm text-surface-400">No matches</div>
                    {/if}
                  </div>
                {/if}
              </div>
            {/if}
          </div>
        {/if}

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Type</span>
          <select
            bind:value={taskType}
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          >
            <option value="normal">normal</option>
            <option value="debug">debug (interactive, per-tool perms)</option>
            <option value="research">research (headless, research-machine)</option>
          </select>
        </label>

        {#if taskType === 'normal'}
          <label class="flex items-center gap-2">
            <input
              type="checkbox"
              bind:checked={headless}
              class="rounded border-surface-300 dark:border-surface-600"
            />
            <span class="text-sm font-medium">Headless</span>
          </label>
        {/if}

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
