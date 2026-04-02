<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import { taskStore } from '../stores/tasks.svelte.js'
  import TaskCard from '../components/TaskCard.svelte'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  let filter = $state('all')

  const statuses = [
    { value: 'all', label: 'All' },
    { value: 'todo', label: 'Todo' },
    { value: 'in-progress', label: 'In Progress' },
    { value: 'done', label: 'Done' },
    { value: 'blocked', label: 'Blocked' },
  ]

  const filtered = $derived(taskStore.byStatus(filter))
</script>

<div class="flex flex-col gap-4 p-6">
  <SegmentedControl value={filter} onValueChange={(details) => (filter = details.value ?? 'all')}>
    <SegmentedControl.Indicator />
    {#each statuses as s}
      <SegmentedControl.Item value={s.value}>
        <SegmentedControl.ItemText>{s.label}</SegmentedControl.ItemText>
        <SegmentedControl.ItemHiddenInput />
      </SegmentedControl.Item>
    {/each}
  </SegmentedControl>

  {#if taskStore.loading}
    <p class="text-center text-sm opacity-60">Loading tasks...</p>
  {:else if taskStore.error}
    <p class="text-center text-sm text-error-500">{taskStore.error}</p>
  {:else if filtered.length === 0}
    <div class="flex flex-col items-center gap-2 py-16 opacity-50">
      <p class="text-lg">No tasks</p>
      <p class="text-sm">Create a task to get started</p>
    </div>
  {:else}
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {#each filtered as t (t.id)}
        <TaskCard task={t} onclick={() => onselect(t.id)} />
      {/each}
    </div>
  {/if}
</div>
