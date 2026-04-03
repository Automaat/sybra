<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import TaskCard from '../components/TaskCard.svelte'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  let dragOverStatus = $state<string | null>(null)
  let addingToColumn = $state<string | null>(null)
  let newTaskTitle = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

  async function handleDrop(e: DragEvent, targetStatus: string) {
    e.preventDefault()
    dragOverStatus = null
    const taskId = e.dataTransfer?.getData('text/plain')
    if (!taskId) return
    const existing = taskStore.tasks.get(taskId)
    if (!existing || existing.status === targetStatus) return
    await taskStore.update(taskId, { status: targetStatus })
  }

  function openInlineAdd(status: string) {
    addingToColumn = status
    newTaskTitle = ''
    // Focus after Svelte renders the input
    requestAnimationFrame(() => inputRef?.focus())
  }

  function dismissInlineAdd() {
    addingToColumn = null
    newTaskTitle = ''
  }

  async function submitInlineAdd(status: string) {
    const title = newTaskTitle.trim()
    if (!title) return
    newTaskTitle = ''
    const created = await taskStore.create(title, '', 'headless')
    if (status !== 'new') {
      await taskStore.update(created.id, { status })
    }
    // Keep input open for rapid entry, re-focus
    requestAnimationFrame(() => inputRef?.focus())
  }

  function handleInputKeydown(e: KeyboardEvent, status: string) {
    if (e.key === 'Enter') {
      e.preventDefault()
      submitInlineAdd(status)
    } else if (e.key === 'Escape') {
      dismissInlineAdd()
    }
  }

  const columns = [
    { status: 'new', label: 'Inbox', border: 'border-t-tertiary-500 dark:border-t-tertiary-400' },
    { status: 'todo', label: 'Todo', border: 'border-t-surface-400 dark:border-t-surface-500' },
    { status: 'in-progress', label: 'In Progress', border: 'border-t-primary-500 dark:border-t-primary-400' },
    { status: 'in-review', label: 'In Review', border: 'border-t-warning-500 dark:border-t-warning-400' },
    { status: 'human-required', label: 'Human Required', border: 'border-t-error-500 dark:border-t-error-400' },
    { status: 'done', label: 'Done', border: 'border-t-success-500 dark:border-t-success-400' },
  ]
</script>

<div class="flex h-full gap-4 overflow-x-auto p-6">
  {#if taskStore.loading}
    <p class="m-auto text-sm opacity-60">Loading tasks...</p>
  {:else if taskStore.error}
    <p class="m-auto text-sm text-error-500">{taskStore.error}</p>
  {:else}
    {#each columns as col}
      {@const tasks = taskStore.byStatus(col.status)}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div
        class="flex min-w-[250px] flex-1 flex-col rounded-lg border-t-4 bg-surface-100 transition-shadow dark:bg-surface-900 {col.border} {dragOverStatus === col.status ? 'ring-2 ring-primary-400 dark:ring-primary-500' : ''}"
        ondragover={(e) => { e.preventDefault(); dragOverStatus = col.status }}
        ondragleave={() => { dragOverStatus = null }}
        ondrop={(e) => handleDrop(e, col.status)}
      >
        <div class="flex items-center justify-between px-3 py-2">
          <h2 class="text-sm font-semibold">{col.label}</h2>
          <span class="rounded-full bg-surface-200 px-2 py-0.5 text-xs font-medium dark:bg-surface-700">
            {tasks.length}
          </span>
        </div>
        <div class="flex flex-1 flex-col gap-2 overflow-y-auto px-2 pb-2">
          {#each tasks as t (t.id)}
            <TaskCard task={t} onclick={() => onselect(t.id)} />
          {/each}
        </div>
        <div class="px-2 pb-2">
          {#if addingToColumn === col.status}
            <input
              bind:this={inputRef}
              bind:value={newTaskTitle}
              type="text"
              placeholder="Task title"
              class="w-full rounded-md border border-surface-300 bg-surface-50 px-2 py-1.5 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-600 dark:bg-surface-800"
              onkeydown={(e) => handleInputKeydown(e, col.status)}
              onblur={() => dismissInlineAdd()}
            />
          {:else}
            <button
              type="button"
              class="flex w-full items-center gap-1 rounded-md px-2 py-1.5 text-sm opacity-50 transition-opacity hover:bg-surface-200 hover:opacity-100 dark:hover:bg-surface-800"
              onclick={() => openInlineAdd(col.status)}
            >
              <span class="text-base leading-none">+</span> Add task
            </button>
          {/if}
        </div>
      </div>
    {/each}
  {/if}
</div>
