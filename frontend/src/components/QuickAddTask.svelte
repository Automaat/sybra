<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { detectProject } from '../lib/detectProject.js'

  interface Props {
    open: boolean
    onclose: () => void
    oncreated?: (id: string) => void
  }

  const { open, onclose, oncreated }: Props = $props()

  let value = $state('')
  let submitting = $state(false)
  let inputEl = $state<HTMLInputElement | null>(null)

  // Project state
  let selectedProject = $state('')
  let userOverrode = $state(false)
  let projectSearch = $state('')
  let projectDropdownOpen = $state(false)

  const autoDetected = $derived(detectProject(value, projectStore.list))

  // Split input into [before, match, after] for highlight overlay
  const highlightParts = $derived.by(() => {
    if (!autoDetected || userOverrode) return null
    const { matchStart, matchEnd } = autoDetected
    return {
      before: value.slice(0, matchStart),
      match: value.slice(matchStart, matchEnd),
      after: value.slice(matchEnd),
    }
  })

  $effect(() => {
    if (!userOverrode && autoDetected) {
      selectedProject = autoDetected.project.id
    } else if (!userOverrode && !autoDetected) {
      selectedProject = ''
    }
  })

  const filteredProjects = $derived(
    projectStore.list.filter((p) => {
      if (!projectSearch) return true
      const q = projectSearch.toLowerCase()
      return p.id.toLowerCase().includes(q) || p.name.toLowerCase().includes(q)
    }),
  )

  $effect(() => {
    if (open) {
      value = ''
      selectedProject = ''
      userOverrode = false
      projectSearch = ''
      projectDropdownOpen = false
      requestAnimationFrame(() => inputEl?.focus())
    }
  })

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault()
      if (projectDropdownOpen) {
        projectDropdownOpen = false
        inputEl?.focus()
      } else {
        onclose()
      }
    }
  }

  function handleProjectKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      handleSubmit(e)
    } else if (e.key === 'Escape') {
      e.preventDefault()
      projectDropdownOpen = false
      inputEl?.focus()
    }
  }

  async function handleSubmit(e: Event) {
    e.preventDefault()
    if (!value.trim() || submitting) return

    submitting = true
    try {
      let t = await taskStore.create(value.trim(), '', 'interactive')
      if (selectedProject) {
        t = await taskStore.update(t.id, { project_id: selectedProject })
      }
      value = ''
      onclose()
      oncreated?.(t.id)
    } finally {
      submitting = false
    }
  }

  function dismissDetection() {
    selectedProject = ''
    userOverrode = true
  }

  function selectProjectManual(id: string) {
    selectedProject = id
    userOverrode = true
    projectDropdownOpen = false
    projectSearch = ''
  }

  function clearManualProject() {
    selectedProject = ''
    userOverrode = false
  }
</script>

{#if open}
  <div class="fixed inset-0 z-40 bg-black/40" onclick={onclose} onkeydown={handleKeydown} role="none"></div>
  <div class="fixed left-1/2 top-1/4 z-50 w-full max-w-xl -translate-x-1/2">
    <div class="rounded-xl border border-surface-300 bg-surface-50 shadow-2xl dark:border-surface-600 dark:bg-surface-900">
      <!-- Title input with highlight overlay -->
      <form onsubmit={handleSubmit} class="relative">
        {#if highlightParts}
          <div
            aria-hidden="true"
            class="pointer-events-none absolute inset-0 overflow-hidden whitespace-pre px-5 py-3.5 text-base text-transparent"
          >{highlightParts.before}<mark class="rounded bg-primary-200/60 text-transparent dark:bg-primary-700/40">{highlightParts.match}</mark>{highlightParts.after}</div>
        {/if}
        <input
          bind:this={inputEl}
          bind:value
          type="text"
          placeholder="Task title, link, or note..."
          disabled={submitting}
          onkeydown={handleKeydown}
          class="relative w-full rounded-t-xl border-none bg-transparent px-5 py-3.5 text-base outline-none ring-0 placeholder:text-surface-400 focus:ring-0 dark:placeholder:text-surface-500"
        />
      </form>

      <!-- Project row: detected chip OR manual selected chip OR search input -->
      {#if projectStore.list.length > 0}
        <div class="relative border-t border-surface-200 dark:border-surface-700">
          {#if autoDetected && !userOverrode}
            <!-- Auto-detected project chip -->
            <div class="flex items-center gap-2 px-5 py-2 text-xs text-surface-500">
              <svg class="h-3.5 w-3.5 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
              </svg>
              <span class="inline-flex items-center gap-1 rounded-md bg-primary-100 px-2 py-0.5 font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
                {autoDetected.project.owner}/{autoDetected.project.repo}
                <button
                  type="button"
                  aria-label="Dismiss detected project"
                  class="ml-0.5 hover:text-primary-900 dark:hover:text-primary-100"
                  onclick={dismissDetection}
                >
                  <svg class="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                  </svg>
                </button>
              </span>
              <span class="text-surface-400">from {autoDetected.matchType === 'url' ? 'link' : 'title'}</span>
            </div>
          {:else if selectedProject && userOverrode}
            <!-- Manually selected project chip -->
            <div class="flex items-center gap-2 px-5 py-2 text-xs">
              <svg class="h-3.5 w-3.5 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
              </svg>
              <span class="text-sm">{projectStore.list.find((p) => p.id === selectedProject)?.id}</span>
              <button
                type="button"
                aria-label="Clear selected project"
                class="text-surface-400 hover:text-surface-600 dark:hover:text-surface-300"
                onclick={clearManualProject}
              >
                <svg class="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
          {:else}
            <!-- Project search dropdown -->
            <div class="relative">
              <input
                type="text"
                bind:value={projectSearch}
                placeholder="Project (optional)..."
                class="w-full rounded-b-xl border-none bg-transparent px-5 py-2 text-xs outline-none ring-0 placeholder:text-surface-400 focus:ring-0"
                onfocus={() => (projectDropdownOpen = true)}
                onblur={() => setTimeout(() => { projectDropdownOpen = false }, 150)}
                onkeydown={handleProjectKeydown}
              />
              {#if projectDropdownOpen}
                <div class="absolute bottom-full left-0 mb-1 max-h-48 w-full overflow-y-auto rounded-lg border border-surface-300 bg-surface-50 shadow-lg dark:border-surface-600 dark:bg-surface-800">
                  {#each filteredProjects as p (p.id)}
                    <button
                      type="button"
                      class="flex w-full items-center gap-2 px-4 py-1.5 text-left text-xs hover:bg-surface-100 dark:hover:bg-surface-700"
                      onmousedown={() => selectProjectManual(p.id)}
                    >
                      <svg class="h-3.5 w-3.5 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
                      </svg>
                      {p.owner}/{p.repo}
                    </button>
                  {/each}
                  {#if filteredProjects.length === 0}
                    <div class="px-4 py-1.5 text-xs text-surface-400">No matches</div>
                  {/if}
                </div>
              {/if}
            </div>
          {/if}
        </div>
      {/if}
    </div>
  </div>
{/if}
