<script lang="ts">
  import { STATUS_MAP } from '../lib/statuses.js'

  interface Props {
    status: string
  }

  const { status }: Props = $props()

  const resolved = $derived(
    STATUS_MAP[status] ?? {
      label: status,
      badgeClasses: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200',
    },
  )
</script>

<span class="inline-block whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors duration-100 {resolved.badgeClasses}">
  {resolved.label}
</span>
