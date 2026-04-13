<script lang="ts">
  import MobileSheet from './shell/MobileSheet.svelte'
  import { agentStore } from '../stores/agents.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    oncreated?: (agentId: string) => void
  }

  const { open, onOpenChange, oncreated }: Props = $props()

  let selectedProject = $state('')
  let projectSearch = $state('')
  let projectDropdownOpen = $state(false)
  let provider = $state('claude')
  let prompt = $state('')
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
    selectedProject = ''
    projectSearch = ''
    projectDropdownOpen = false
    provider = 'claude'
    prompt = ''
    error = ''
  }

  function handleProjectBlur() {
    setTimeout(() => { projectDropdownOpen = false }, 150)
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!selectedProject) {
      error = 'Select a project'
      return
    }
    submitting = true
    error = ''
    try {
      const ag = await agentStore.startChat(selectedProject, provider, prompt.trim())
      const newId = ag.id
      reset()
      onOpenChange(false)
      oncreated?.(newId)
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
  title="New Chat"
>
  <div class="flex flex-col px-5 pb-5 md:px-6 md:pb-6">
      <form onsubmit={handleSubmit} class="flex flex-col gap-4">
        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium">Project</span>
          {#if projectStore.list.length === 0}
            <p class="text-sm text-surface-500">No projects registered. Create one first.</p>
          {:else if selectedProject}
            <div class="flex items-center gap-2 rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700">
              <svg class="h-4 w-4 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
              </svg>
              <span class="flex-1">{selectedProjectName}</span>
              <button
                type="button"
                class="text-surface-400 hover:text-surface-600 dark:hover:text-surface-300"
                aria-label="Clear project"
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
                <div class="absolute z-10 mt-1 max-h-64 w-full overflow-y-auto rounded-lg border border-surface-300 bg-surface-50 shadow-lg dark:border-surface-600 dark:bg-surface-800">
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

        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium">Provider</span>
          <div class="grid grid-cols-2 gap-2 md:flex md:gap-4">
            <label class="tap flex cursor-pointer items-center gap-2 rounded-lg border border-surface-300 bg-surface-100 px-3 py-2.5 dark:border-surface-600 dark:bg-surface-700 has-[:checked]:border-primary-500 has-[:checked]:bg-primary-50 dark:has-[:checked]:bg-primary-900/30">
              <input
                type="radio"
                name="chat-provider"
                value="claude"
                bind:group={provider}
                class="border-surface-300 dark:border-surface-600"
              />
              <span class="text-sm">Claude</span>
            </label>
            <label class="tap flex cursor-pointer items-center gap-2 rounded-lg border border-surface-300 bg-surface-100 px-3 py-2.5 dark:border-surface-600 dark:bg-surface-700 has-[:checked]:border-primary-500 has-[:checked]:bg-primary-50 dark:has-[:checked]:bg-primary-900/30">
              <input
                type="radio"
                name="chat-provider"
                value="codex"
                bind:group={provider}
                class="border-surface-300 dark:border-surface-600"
              />
              <span class="text-sm">Codex</span>
            </label>
          </div>
        </div>

        <label class="flex flex-col gap-1">
          <span class="text-sm font-medium">Prompt <span class="text-surface-400">(optional)</span></span>
          <textarea
            bind:value={prompt}
            rows={4}
            placeholder="Leave empty to land in an idle chat..."
            class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          ></textarea>
        </label>

        {#if error}
          <p class="text-sm text-error-500">{error}</p>
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
            disabled={submitting || !selectedProject || projectStore.list.length === 0}
            class="tap rounded-lg bg-primary-500 px-5 py-2.5 text-sm font-medium text-white active:bg-primary-700 disabled:opacity-50"
          >
            {submitting ? 'Starting...' : 'Start chat'}
          </button>
        </div>
      </form>
  </div>
</MobileSheet>
