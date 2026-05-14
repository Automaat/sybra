<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'

  interface Props {
    task: Task
    onerror?: (msg: string) => void
  }

  const { task, onerror }: Props = $props()

  let editing = $state(false)
  let draft = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (editing && inputRef) inputRef.focus()
  })

  export function start() {
    if (editing) return
    draft = task.maxTurns ? String(task.maxTurns) : ''
    editing = true
  }

  async function save() {
    editing = false
    const raw = String(draft ?? '').trim()
    const n = raw === '' ? 0 : parseInt(raw, 10)
    if (raw !== '' && (isNaN(n) || n < 0)) {
      onerror?.('Max turns must be a non-negative integer.')
      return
    }
    const current = task.maxTurns ?? 0
    if (n === current) return
    try {
      await taskStore.update(task.id, { max_turns: n })
      onerror?.('')
    } catch (e) {
      onerror?.(String(e))
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      save()
    } else if (e.key === 'Escape') {
      editing = false
    }
  }
</script>

{#if editing}
  <input
    bind:this={inputRef}
    bind:value={draft}
    type="number"
    min="0"
    class="w-24 rounded border border-primary-400 bg-surface-50 px-2 py-0.5 text-xs outline-none dark:border-primary-500 dark:bg-surface-900"
    placeholder="global default"
    onblur={save}
    onkeydown={handleKeydown}
  />
{:else}
  <button
    type="button"
    class="w-fit rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
    onclick={start}
    title="Click to set per-task max turns (0 = use global default)"
  >
    {#if task.maxTurns}
      {task.maxTurns}
    {:else}
      <span class="italic text-surface-400">global default</span>
    {/if}
  </button>
{/if}
