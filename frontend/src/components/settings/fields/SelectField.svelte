<script lang="ts">
  import SettingRow from './SettingRow.svelte'

  interface Props {
    label: string
    value: string
    options: { value: string; label: string }[]
    description?: string
    keyPath?: string
    modified?: boolean
    onreset?: () => void
    id: string
  }

  let {
    label,
    value = $bindable(),
    options,
    description,
    keyPath,
    modified = false,
    onreset,
    id,
  }: Props = $props()
</script>

<SettingRow {label} {description} {keyPath} {modified} {onreset} for={id}>
  {#snippet control()}
    <select
      {id}
      class="w-full max-w-xs rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
      bind:value
    >
      {#each options as opt (opt.value)}
        <option value={opt.value}>{opt.label}</option>
      {/each}
    </select>
  {/snippet}
</SettingRow>
