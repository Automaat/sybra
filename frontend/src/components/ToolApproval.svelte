<script lang="ts">
  import DiffViewer from './DiffViewer.svelte'

  interface Props {
    toolUseId: string
    toolName: string
    input: Record<string, unknown>
    onrespond: (toolUseId: string, approved: boolean) => void
  }

  const { toolUseId, toolName, input, onrespond }: Props = $props()

  let responding = $state(false)

  function isEditTool(): boolean {
    return toolName === 'Edit' || toolName === 'Write'
  }

  async function handleApprove() {
    responding = true
    onrespond(toolUseId, true)
  }

  async function handleReject() {
    responding = true
    onrespond(toolUseId, false)
  }
</script>

<div class="rounded-lg border-2 border-warning-400 bg-warning-50 p-3 dark:border-warning-600 dark:bg-warning-950">
  <div class="mb-2 flex items-center gap-2">
    <span class="rounded bg-warning-200 px-2 py-0.5 text-xs font-bold text-warning-800 dark:bg-warning-700 dark:text-warning-200">
      APPROVAL
    </span>
    <span class="text-sm font-medium text-surface-800 dark:text-surface-200">{toolName}</span>
    {#if input.file_path}
      <span class="text-xs text-surface-500">{input.file_path}</span>
    {/if}
  </div>

  <!-- Tool-specific preview -->
  <div class="mb-3">
    {#if isEditTool() && input.old_string !== undefined}
      <DiffViewer
        oldText={String(input.old_string ?? '')}
        newText={String(input.new_string ?? '')}
        filePath={String(input.file_path ?? '')}
      />
    {:else if toolName === 'Bash' && input.command}
      <div class="rounded bg-surface-900 px-3 py-2 font-mono text-xs text-success-400 dark:bg-surface-950">
        $ {input.command}
      </div>
      {#if input.description}
        <p class="mt-1 text-xs text-surface-500">{input.description}</p>
      {/if}
    {:else if toolName === 'Write' && input.content}
      <div class="max-h-40 overflow-y-auto rounded bg-surface-900 px-3 py-2 font-mono text-xs text-surface-300 dark:bg-surface-950">
        {String(input.content).slice(0, 500)}{String(input.content).length > 500 ? '...' : ''}
      </div>
    {:else}
      <pre class="max-h-32 overflow-y-auto rounded bg-surface-100 px-3 py-2 text-xs dark:bg-surface-800">{JSON.stringify(input, null, 2)}</pre>
    {/if}
  </div>

  <!-- Action buttons -->
  <div class="flex items-center gap-2">
    <button
      type="button"
      disabled={responding}
      class="flex items-center gap-1.5 rounded-lg bg-success-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-success-700 disabled:opacity-50"
      onclick={handleApprove}
    >
      <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
      </svg>
      Approve
    </button>
    <button
      type="button"
      disabled={responding}
      class="flex items-center gap-1.5 rounded-lg bg-error-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-700 disabled:opacity-50"
      onclick={handleReject}
    >
      <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
      </svg>
      Reject
    </button>
  </div>
</div>
