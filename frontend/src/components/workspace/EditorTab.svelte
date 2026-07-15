<script lang="ts">
  import { RefreshCw } from '@lucide/svelte'
  import { fetchAgentDiff, invalidateDiffCache, type FileDiff } from '$lib/agent-diff.js'

  interface Props {
    taskId: string
    latestEditToolUseId: string
  }

  const { taskId, latestEditToolUseId }: Props = $props()

  let files = $state<FileDiff[]>([])
  let loading = $state(false)
  let error = $state('')
  let selectedFile = $state<string | null>(null)
  let showAll = $state(false)

  const PAGE_SIZE = 20

  async function loadDiff(editId: string) {
    loading = true
    error = ''
    try {
      files = await fetchAgentDiff(taskId, editId)
      if (files.length > 0 && selectedFile === null) {
        selectedFile = files[0].path
      }
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function refresh() {
    invalidateDiffCache()
    await loadDiff(latestEditToolUseId)
  }

  $effect(() => {
    void loadDiff(latestEditToolUseId)
  })

  const visibleFiles = $derived(showAll ? files : files.slice(0, PAGE_SIZE))
  const selectedDiff = $derived(files.find((f) => f.path === selectedFile))

  function statBadgeClasses(f: FileDiff): string {
    if (f.isNew) return 'bg-success-100 text-success-700 dark:bg-success-900 dark:text-success-300'
    if (f.isDeleted) return 'bg-error-100 text-error-700 dark:bg-error-900 dark:text-error-300'
    return 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-400'
  }
</script>

<div class="flex h-full min-h-0 flex-col">
  <!-- Toolbar -->
  <div class="flex items-center justify-between border-b border-surface-300 px-3 py-1.5 dark:border-surface-700">
    <span class="text-xs text-surface-500">{files.length} file{files.length !== 1 ? 's' : ''} changed</span>
    <button
      type="button"
      onclick={refresh}
      disabled={loading}
      class="flex items-center gap-1 rounded px-1.5 py-0.5 text-xs text-surface-500 hover:text-surface-800 disabled:opacity-50 dark:hover:text-surface-200"
      title="Refresh diff"
    >
      <RefreshCw size={12} class={loading ? 'animate-spin' : ''} />
      Refresh
    </button>
  </div>

  {#if error}
    <p class="px-3 py-2 text-xs text-error-500">{error}</p>
  {:else if files.length === 0 && !loading}
    <div class="flex items-center justify-center py-12 text-sm text-surface-400">
      No changes yet
    </div>
  {:else}
    <div class="flex min-h-0 flex-1">
      <!-- File list -->
      <div class="flex w-48 shrink-0 flex-col overflow-y-auto border-r border-surface-300 dark:border-surface-700">
        {#each visibleFiles as f (f.path)}
          <button
            type="button"
            onclick={() => { selectedFile = f.path }}
            class="flex w-full items-start gap-1.5 px-2 py-1.5 text-left text-[11px] transition-colors
              {selectedFile === f.path
                ? 'bg-primary-100 text-primary-800 dark:bg-primary-900/40 dark:text-primary-200'
                : 'hover:bg-surface-100 dark:hover:bg-surface-800'}"
          >
            <span class="min-w-0 flex-1 truncate font-mono leading-snug" title={f.path}>
              {f.path.split('/').pop()}
            </span>
            <span class="shrink-0 rounded px-1 py-0.5 text-[9px] font-semibold {statBadgeClasses(f)}">
              {#if f.isBinary}
                bin
              {:else if f.isRenamed}
                mv
              {:else}
                +{f.additions} -{f.deletions}
              {/if}
            </span>
          </button>
        {/each}
        {#if !showAll && files.length > PAGE_SIZE}
          <button
            type="button"
            onclick={() => { showAll = true }}
            class="px-2 py-1.5 text-center text-[11px] text-primary-600 hover:underline dark:text-primary-400"
          >
            Show all {files.length} files
          </button>
        {/if}
      </div>

      <!-- Diff content -->
      <div class="min-w-0 flex-1 overflow-y-auto">
        {#if selectedDiff}
          {#if selectedDiff.isBinary}
            <div class="p-4 text-sm text-surface-400">Binary file — no diff available</div>
          {:else if selectedDiff.hunks.length === 0}
            <div class="p-4 text-sm text-surface-400">
              {selectedDiff.isNew ? 'New empty file' : 'No hunks'}
            </div>
          {:else}
            <div class="font-mono text-[11px]">
              {#each selectedDiff.hunks as hunk (hunk.header)}
                <div class="border-b border-surface-200 bg-surface-100 px-3 py-1 text-surface-500 dark:border-surface-700 dark:bg-surface-800">
                  {hunk.header}
                </div>
                {#each hunk.lines as line}
                  <div
                    class="whitespace-pre-wrap break-all px-3 py-0.5 leading-relaxed
                      {line.type === 'add'
                        ? 'bg-success-50 text-success-700 dark:bg-success-950/30 dark:text-success-300'
                        : line.type === 'del'
                          ? 'bg-error-50 text-error-700 dark:bg-error-950/30 dark:text-error-300'
                          : 'text-surface-600 dark:text-surface-400'}"
                  >{line.type === 'add' ? '+' : line.type === 'del' ? '-' : ' '}{line.content}</div>
                {/each}
              {/each}
            </div>
          {/if}
        {/if}
      </div>
    </div>
  {/if}
</div>
