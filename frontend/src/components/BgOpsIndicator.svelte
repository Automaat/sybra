<script lang="ts">
  import { bgopStore, type Operation } from '../stores/bgops.svelte.js'

  let open = $state(false)

  function typeLabel(op: Operation): string {
    return op.type === 'clone' ? 'Clone' : 'Worktree'
  }

  function elapsed(op: Operation): string {
    const start = new Date(op.startedAt).getTime()
    const end = op.completedAt ? new Date(op.completedAt).getTime() : Date.now()
    const s = Math.floor((end - start) / 1000)
    if (s < 60) return `${s}s`
    return `${Math.floor(s / 60)}m ${s % 60}s`
  }

  function statusClass(op: Operation): string {
    if (op.status === 'failed') return 'text-error-400'
    if (op.status === 'done') return 'text-success-400'
    return 'text-surface-300'
  }
</script>

{#if bgopStore.ops.length > 0}
  <div>
    <button
      type="button"
      class="relative flex items-center gap-1.5 rounded-lg px-2 py-1.5 text-sm hover:bg-surface-700 transition-colors"
      onclick={() => (open = !open)}
      aria-label="Background operations"
    >
      {#if bgopStore.hasActive}
        <span class="relative flex size-4 items-center justify-center">
          <span class="absolute inline-flex size-full animate-ping rounded-full bg-primary-400 opacity-60"></span>
          <span class="relative inline-flex size-2.5 rounded-full bg-primary-500"></span>
        </span>
        <span class="text-xs text-primary-300">{bgopStore.activeCount}</span>
      {:else}
        <span class="size-2.5 rounded-full bg-surface-500"></span>
      {/if}
    </button>

    {#if open}
      <!-- Backdrop to close -->
      <button
        type="button"
        class="fixed inset-0 z-40"
        onclick={() => (open = false)}
        aria-label="Close"
        tabindex="-1"
      ></button>

      <div class="fixed right-4 top-14 z-50 w-80 max-w-[calc(100vw-2rem)] rounded-xl border border-surface-600 bg-surface-800 shadow-xl">
        <div class="flex items-center justify-between border-b border-surface-700 px-3 py-2">
          <span class="text-xs font-medium text-surface-300">Background operations</span>
          <button
            type="button"
            class="text-xs text-surface-500 hover:text-surface-300"
            onclick={() => (open = false)}
          >✕</button>
        </div>

        <div class="max-h-64 overflow-y-auto">
          {#each bgopStore.ops as op (op.id)}
            <div class="flex flex-col gap-0.5 px-3 py-2.5 {op !== bgopStore.ops[bgopStore.ops.length - 1] ? 'border-b border-surface-700/50' : ''}">
              <div class="flex items-center justify-between gap-2">
                <span class="flex items-center gap-1.5 min-w-0">
                  {#if op.status === 'running'}
                    <span class="size-1.5 shrink-0 animate-pulse rounded-full bg-primary-400"></span>
                  {:else if op.status === 'done'}
                    <span class="size-1.5 shrink-0 rounded-full bg-success-400"></span>
                  {:else}
                    <span class="size-1.5 shrink-0 rounded-full bg-error-400"></span>
                  {/if}
                  <span class="truncate text-xs font-medium text-surface-100">{op.label}</span>
                </span>
                <span class="shrink-0 text-xs text-surface-500">{typeLabel(op)} · {elapsed(op)}</span>
              </div>

              {#if op.phase && op.status === 'running'}
                <p class="pl-3 text-xs text-surface-400">{op.phase}</p>
              {/if}

              {#if op.error}
                <p class="pl-3 text-xs {statusClass(op)}">{op.error}</p>
              {/if}
            </div>
          {/each}
        </div>
      </div>
    {/if}
  </div>
{/if}
