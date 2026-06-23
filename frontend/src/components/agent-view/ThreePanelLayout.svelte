<script lang="ts">
  import type { Snippet } from 'svelte'
  import { ChevronLeft, ChevronRight } from '@lucide/svelte'

  interface Props {
    sidebar: Snippet
    workspace: Snippet
    children: Snippet
  }

  const { sidebar, workspace, children }: Props = $props()

  function getStorageBool(key: string, fallback: boolean): boolean {
    if (typeof localStorage === 'undefined') return fallback
    const val = localStorage.getItem(key)
    return val === null ? fallback : val === 'true'
  }

  function setStorageBool(key: string, val: boolean): void {
    try {
      localStorage.setItem(key, String(val))
    } catch {
      // storage disabled / quota exceeded — preference just won't persist
    }
  }

  // Default to two panes (the center step-list + output); the agents sidebar and
  // the session workspace are collapsed behind toggles to avoid cramping.
  let sidebarCollapsed = $state(getStorageBool('sybra.workspace.leftCollapsed', true))
  let workspaceCollapsed = $state(getStorageBool('sybra.workspace.rightCollapsed', true))

  $effect(() => {
    setStorageBool('sybra.workspace.leftCollapsed', sidebarCollapsed)
  })
  $effect(() => {
    setStorageBool('sybra.workspace.rightCollapsed', workspaceCollapsed)
  })
</script>

<div class="flex min-h-0 items-start gap-3">
  <!-- Left sidebar toggle — only at md+ -->
  <button
    type="button"
    onclick={() => { sidebarCollapsed = !sidebarCollapsed }}
    title={sidebarCollapsed ? 'Show agents' : 'Hide agents'}
    aria-label={sidebarCollapsed ? 'Show agents' : 'Hide agents'}
    class="hidden shrink-0 rounded p-1 text-surface-400 hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-200 md:flex"
  >
    {#if sidebarCollapsed}
      <ChevronRight size={14} />
    {:else}
      <ChevronLeft size={14} />
    {/if}
  </button>

  <!-- Left sidebar: hidden below md, collapsible -->
  {#if !sidebarCollapsed}
    <div class="hidden w-48 shrink-0 flex-col md:flex">
      {@render sidebar()}
    </div>
  {/if}

  <!-- Center: always visible -->
  <div class="min-w-0 flex-1">
    {@render children()}
  </div>

  <!-- Right workspace: hidden below lg, collapsible -->
  {#if !workspaceCollapsed}
    <div class="hidden w-[400px] shrink-0 flex-col rounded-lg border border-surface-300 dark:border-surface-700 lg:flex" style="min-height: 400px; max-height: 80vh;">
      {@render workspace()}
    </div>
  {/if}

  <!-- Workspace toggle — only visible at lg+ -->
  <button
    type="button"
    onclick={() => { workspaceCollapsed = !workspaceCollapsed }}
    title={workspaceCollapsed ? 'Show workspace' : 'Hide workspace'}
    aria-label={workspaceCollapsed ? 'Show workspace' : 'Hide workspace'}
    class="hidden shrink-0 rounded p-1 text-surface-400 hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-200 lg:flex"
  >
    {#if workspaceCollapsed}
      <ChevronLeft size={14} />
    {:else}
      <ChevronRight size={14} />
    {/if}
  </button>
</div>
