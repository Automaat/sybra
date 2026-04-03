<script lang="ts">
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    onselect: (id: string) => void
    onadd: () => void
  }

  const { onselect, onadd }: Props = $props()

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleDateString()
  }
</script>

<div class="flex flex-col gap-4 p-6">
  <div class="flex items-center justify-between">
    <h2 class="text-lg font-semibold">Projects</h2>
    <button
      type="button"
      class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
      onclick={onadd}
    >
      + Add Project
    </button>
  </div>

  {#if projectStore.loading && projectStore.list.length === 0}
    <p class="text-sm opacity-60">Loading projects...</p>
  {:else if projectStore.error}
    <p class="text-sm text-error-500">{projectStore.error}</p>
  {:else if projectStore.list.length === 0}
    <div class="flex flex-col items-center gap-3 py-16 text-center">
      <svg class="h-12 w-12 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
      </svg>
      <p class="text-sm text-surface-500">No projects yet</p>
      <button
        type="button"
        class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600"
        onclick={onadd}
      >
        Add your first project
      </button>
    </div>
  {:else}
    <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {#each projectStore.list as p (p.id)}
        <button
          type="button"
          class="flex flex-col gap-2 rounded-lg border border-surface-300 bg-surface-50 p-4 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
          onclick={() => onselect(p.id)}
        >
          <div class="flex items-center gap-2">
            <svg class="h-5 w-5 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
            </svg>
            <span class="text-sm font-semibold">{p.owner}/{p.repo}</span>
          </div>
          <div class="flex items-center gap-2 text-xs text-surface-500">
            <span>Added {formatDate(p.createdAt)}</span>
          </div>
        </button>
      {/each}
    </div>
  {/if}
</div>
