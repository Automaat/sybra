<script lang="ts">
  interface Props {
    status: string
  }

  const { status }: Props = $props()

  const config: Record<string, { label: string; classes: string }> = {
    'new': { label: 'New', classes: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200' },
    'todo': { label: 'Todo', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
    'planning': { label: 'Planning', classes: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200' },
    'plan-review': { label: 'Plan Review', classes: 'bg-tertiary-100 text-tertiary-700 dark:bg-tertiary-800 dark:text-tertiary-300' },
    'in-progress': { label: 'In Progress', classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' },
    'in-review': { label: 'In Review', classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' },
    'human-required': { label: 'Human Required', classes: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200' },
    'done': { label: 'Done', classes: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' },
  }

  const resolved = $derived(config[status] ?? { label: status, classes: 'bg-surface-200 text-surface-800' })
</script>

<span class="inline-block rounded-full px-2.5 py-0.5 text-xs font-medium {resolved.classes}">
  {resolved.label}
</span>
