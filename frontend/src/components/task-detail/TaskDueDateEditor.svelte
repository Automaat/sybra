<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { formatDueDateDisplay, parseNaturalDate } from '../../lib/dates.js'

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
    if (task.dueDate) {
      const d = new Date(task.dueDate as unknown as string)
      draft = isNaN(d.getTime()) ? '' : d.toISOString().split('T')[0]
    } else {
      draft = ''
    }
    editing = true
  }

  async function save() {
    editing = false
    const value = draft.trim()
    let newVal: string | null = null
    if (value && value.toLowerCase() !== 'none' && value.toLowerCase() !== 'clear') {
      const parsed = parseNaturalDate(value)
      if (!parsed) {
        onerror?.(`Invalid date: "${value}". Try "today", "tomorrow", "next monday", "in 3 days", or YYYY-MM-DD.`)
        return
      }
      newVal = parsed.toISOString()
    }
    const currentISO = task.dueDate ? new Date(task.dueDate as unknown as string).toISOString() : null
    if (newVal === currentISO) return
    try {
      await taskStore.update(task.id, { due_date: newVal })
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

  const overdue = $derived(
    task.dueDate ? new Date(task.dueDate as unknown as string) < new Date() : false,
  )
</script>

{#if editing}
  <input
    bind:this={inputRef}
    bind:value={draft}
    class="rounded border border-primary-400 bg-surface-50 px-2 py-0.5 text-xs outline-none dark:border-primary-500 dark:bg-surface-900"
    placeholder="today / tomorrow / YYYY-MM-DD"
    onblur={save}
    onkeydown={handleKeydown}
  />
  <span class="text-surface-300 dark:text-surface-600">Esc to cancel</span>
{:else}
  <button
    type="button"
    class="rounded px-1 py-0.5 transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300 {overdue ? 'text-error-500 dark:text-error-400' : ''}"
    onclick={start}
    title="Click to set due date"
  >
    {formatDueDateDisplay(task.dueDate)}
  </button>
{/if}
