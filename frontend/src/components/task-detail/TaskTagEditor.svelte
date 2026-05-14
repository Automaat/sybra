<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'

  interface Props {
    task: Task
    onerror?: (msg: string) => void
  }

  const { task, onerror }: Props = $props()

  let editing = $state(false)
  let draft = $state<string[]>([])
  let input = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (editing && inputRef) inputRef.focus()
  })

  export function start() {
    if (editing) return
    draft = [...(task.tags ?? [])]
    input = ''
    editing = true
  }

  function addTag() {
    const tag = input.trim().replace(/,/g, '')
    if (tag && !draft.includes(tag)) draft = [...draft, tag]
    input = ''
  }

  function removeTag(tag: string) {
    draft = draft.filter((x) => x !== tag)
  }

  async function save() {
    editing = false
    const current = task.tags ?? []
    const same = current.length === draft.length && current.every((v, i) => v === draft[i])
    if (same) return
    try {
      await taskStore.update(task.id, { tags: draft })
    } catch (e) {
      onerror?.(String(e))
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      if (input.trim()) addTag()
      else save()
    } else if (e.key === 'Escape') {
      editing = false
    } else if (e.key === 'Backspace' && !input && draft.length > 0) {
      draft = draft.slice(0, -1)
    } else if (e.key === ',') {
      e.preventDefault()
      addTag()
    }
  }

  function handleFocusout(e: FocusEvent) {
    const related = e.relatedTarget as Node | null
    const container = e.currentTarget as HTMLElement
    if (!related || !container.contains(related)) save()
  }
</script>

{#if editing}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="flex min-w-[8rem] flex-wrap items-center gap-1 rounded-lg border border-primary-400 bg-surface-50 px-2 py-1 dark:border-primary-500 dark:bg-surface-900"
    onfocusout={handleFocusout}
  >
    {#each draft as tag}
      <span class="inline-flex items-center gap-0.5 rounded bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">
        {tag}
        <button
          type="button"
          class="ml-0.5 text-surface-400 hover:text-error-500"
          onclick={() => removeTag(tag)}
          tabindex="-1"
          aria-label="Remove tag {tag}"
        >×</button>
      </span>
    {/each}
    <input
      bind:this={inputRef}
      bind:value={input}
      class="min-w-[4rem] flex-1 bg-transparent text-xs outline-none"
      placeholder={draft.length ? '' : 'add tags...'}
      onkeydown={handleKeydown}
    />
  </div>
{:else}
  <button
    type="button"
    class="flex flex-wrap items-center gap-1 rounded-lg border border-transparent px-1 py-0.5 text-left transition-colors hover:border-surface-300 hover:bg-surface-100 dark:hover:border-surface-600 dark:hover:bg-surface-800"
    onclick={start}
    title="Click to edit tags"
  >
    {#if task.tags?.length}
      {#each task.tags as tag}
        <span class="rounded bg-surface-200 px-2 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
      {/each}
    {:else}
      <span class="text-xs italic text-surface-400">add tags</span>
    {/if}
  </button>
{/if}
