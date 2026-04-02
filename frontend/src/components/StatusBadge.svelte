<script lang="ts">
  interface Props {
    status: string
  }

  const { status }: Props = $props()

  const config: Record<string, { label: string; classes: string }> = {
    'todo': { label: 'Todo', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
    'in-progress': { label: 'In Progress', classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' },
    'done': { label: 'Done', classes: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' },
    'blocked': { label: 'Blocked', classes: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200' },
  }

  const resolved = $derived(config[status] ?? { label: status, classes: 'bg-surface-200 text-surface-800' })
</script>

<span class="inline-block rounded-full px-2.5 py-0.5 text-xs font-medium {resolved.classes}">
  {resolved.label}
</span>
