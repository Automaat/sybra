<script lang="ts">
  import SettingRow from './SettingRow.svelte'

  interface Props {
    label: string
    value: string
    description?: string
    keyPath?: string
    placeholder?: string
    modified?: boolean
    onreset?: () => void
    id: string
  }

  let {
    label,
    value = $bindable(),
    description,
    keyPath,
    placeholder,
    modified = false,
    onreset,
    id,
  }: Props = $props()
</script>

<SettingRow {label} {description} {keyPath} {modified} {onreset} for={id}>
  {#snippet control()}
    <input
      {id}
      type="text"
      {placeholder}
      class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
      bind:value
    />
  {/snippet}
</SettingRow>
