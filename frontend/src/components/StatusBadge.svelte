<script lang="ts">
  import { STATUS_MAP } from '../lib/statuses.js'

  interface Props {
    status: string
  }

  const { status }: Props = $props()

  const resolved = $derived(STATUS_MAP[status] ?? { label: status, badgeClasses: 'bg-surface-200 text-surface-800' })
</script>

<span class="inline-block rounded-full px-2.5 py-0.5 text-xs font-medium {resolved.badgeClasses}">
  {resolved.label}
</span>
