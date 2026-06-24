<script lang="ts">
  import { STATUS_MAP, statusLabel } from '../lib/statuses.js'

  interface Props {
    status: string
  }

  const { status }: Props = $props()

  const badgeClasses = $derived(
    STATUS_MAP[status]?.badgeClasses ??
      'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200',
  )
  const label = $derived(statusLabel(status))
</script>

<span class="inline-block whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors duration-100 {badgeClasses}">
  {label}
</span>
