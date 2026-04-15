<script lang="ts">
  import type { project } from '../../wailsjs/go/models.js'
  import { ListWorktrees, OpenInTerminal, OpenInEditor } from '$lib/api'
  import { Terminal, Code } from '@lucide/svelte'

  interface Props {
    projectId: string
  }

  const { projectId }: Props = $props()

  let worktrees = $state<project.Worktree[]>([])
  let loading = $state(true)
  let error = $state('')

  $effect(() => {
    load()
  })

  async function load() {
    loading = true
    error = ''
    try {
      worktrees = await ListWorktrees(projectId)
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function openTerminal(path: string) {
    try {
      await OpenInTerminal(path)
    } catch (e) {
      error = String(e)
    }
  }

  async function openEditor(path: string) {
    try {
      await OpenInEditor(path)
    } catch (e) {
      error = String(e)
    }
  }
</script>

{#if error}
  <p class="text-sm text-error-500">{error}</p>
{/if}

{#if loading}
  <p class="py-4 text-center text-sm opacity-60">Loading worktrees...</p>
{:else if worktrees.length === 0}
  <p class="py-4 text-center text-sm text-surface-400">No active worktrees</p>
{:else}
  <div class="flex flex-col gap-2">
    {#each worktrees as wt (wt.path)}
      <div class="flex items-center justify-between rounded-lg border border-surface-300 bg-surface-50 px-4 py-3 dark:border-surface-600 dark:bg-surface-800">
        <div class="flex flex-col gap-1">
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium">{wt.branch}</span>
            <span class="rounded bg-surface-200 px-1.5 py-0.5 font-mono text-xs dark:bg-surface-700">{wt.head}</span>
          </div>
          {#if wt.taskId}
            <span class="text-xs text-surface-400">Task: {wt.taskId}</span>
          {/if}
          <span class="font-mono text-xs text-surface-400">{wt.path}</span>
        </div>

        <div class="flex items-center gap-1">
          <button
            type="button"
            class="rounded p-1.5 text-surface-500 hover:bg-surface-200 hover:text-surface-800 dark:hover:bg-surface-700 dark:hover:text-surface-200"
            title="Open in Ghostty"
            onclick={() => openTerminal(wt.path)}
          >
            <Terminal size={16} />
          </button>
          <button
            type="button"
            class="rounded p-1.5 text-surface-500 hover:bg-surface-200 hover:text-surface-800 dark:hover:bg-surface-700 dark:hover:text-surface-200"
            title="Open in Zed"
            onclick={() => openEditor(wt.path)}
          >
            <Code size={16} />
          </button>
        </div>
      </div>
    {/each}
  </div>
{/if}
