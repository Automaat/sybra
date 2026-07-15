<script lang="ts">
  import { STATUS_MAP, statusLabel } from '../lib/statuses.js'
  import { pillClass } from '../lib/pills.js'

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

<!-- The canonical "status" pill role: shape from the role, colour from the status. -->
<span class={pillClass('status', `transition-colors duration-100 ${badgeClasses}`)}>
  {label}
</span>
