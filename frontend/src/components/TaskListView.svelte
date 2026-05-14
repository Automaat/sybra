<script lang="ts">
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'

  interface Props {
    tasks: Task[]
    focusedTaskId: string | null
    onselect: (id: string) => void
    onhover: (rowIdx: number) => void
  }

  const { tasks, focusedTaskId, onselect, onhover }: Props = $props()

  function priorityIcon(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.icon ?? '–'
  }

  function priorityLabel(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.label ?? 'None'
  }

  function priorityClasses(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.classes ?? 'text-surface-400'
  }
</script>

<div class="min-h-0 flex-1 overflow-y-auto">
  <table class="w-full text-sm">
    <thead class="sticky top-0 z-10 border-b border-surface-200 bg-surface-100 dark:border-surface-700 dark:bg-surface-900">
      <tr>
        <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider w-8">P</th>
        <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider">Title</th>
        <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider">Status</th>
        <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider hidden md:table-cell">Project</th>
        <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider hidden lg:table-cell">Updated</th>
      </tr>
    </thead>
    <tbody>
      {#each tasks as t, rowIdx (t.id)}
        {@const isFocused = focusedTaskId === t.id}
        <tr
          data-focused-task={isFocused ? '' : undefined}
          class="cursor-pointer border-b border-surface-100 transition-colors dark:border-surface-800 {isFocused ? 'bg-primary-50 dark:bg-primary-900/20' : 'hover:bg-surface-100 dark:hover:bg-surface-800'}"
          onclick={() => onselect(t.id)}
          onmouseenter={() => onhover(rowIdx)}
        >
          <td class="px-4 py-2">
            <span class="font-mono text-sm {priorityClasses(t.priority)}" title="Priority: {priorityLabel(t.priority)}">{priorityIcon(t.priority)}</span>
          </td>
          <td class="px-4 py-2 font-medium">{t.title}</td>
          <td class="px-4 py-2">
            <span class="rounded-full px-2 py-0.5 text-xs font-semibold bg-surface-200 dark:bg-surface-700">{t.status}</span>
          </td>
          <td class="hidden px-4 py-2 text-surface-500 md:table-cell">{t.projectId || '—'}</td>
          <td class="hidden px-4 py-2 text-surface-400 text-xs lg:table-cell">
            {t.updatedAt ? new Date(t.updatedAt).toLocaleDateString() : '—'}
          </td>
        </tr>
      {/each}
      {#if tasks.length === 0}
        <tr>
          <td colspan="5" class="px-4 py-8 text-center text-surface-400">No tasks match your filters</td>
        </tr>
      {/if}
    </tbody>
  </table>
</div>
