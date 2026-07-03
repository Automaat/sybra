<script lang="ts">
  import SettingRow from './SettingRow.svelte'

  interface Props {
    label: string
    value: number
    description?: string
    keyPath?: string
    min?: number
    max?: number
    step?: number
    modified?: boolean
    onreset?: () => void
    id: string
  }

  let {
    label,
    value = $bindable(),
    description,
    keyPath,
    min,
    max,
    step,
    modified = false,
    onreset,
    id,
  }: Props = $props()
</script>

<SettingRow {label} {description} {keyPath} {modified} {onreset} for={id}>
  {#snippet control()}
    <input
      {id}
      type="number"
      {min}
      {max}
      {step}
      class="w-40 rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
      bind:value
    />
  {/snippet}
</SettingRow>
