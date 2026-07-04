<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import TextField from './fields/TextField.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const r = $derived(settings.renovate)
  const d = $derived(defaults.renovate)
</script>

<Section title="Renovate" description="Show Renovate bot PRs for registered projects and drive the CI fixer.">
  <ToggleField label="Enable Renovate PR tracking" keyPath="renovate.enabled"
    bind:checked={settings.renovate.enabled}
    modified={r.enabled !== d.enabled}
    onreset={() => (settings.renovate.enabled = d.enabled)} />
  {#if settings.renovate.enabled}
    <TextField id="renovate-author" label="PR author" placeholder="app/renovate" keyPath="renovate.author"
      description="GitHub author filter (default: app/renovate)"
      bind:value={settings.renovate.author}
      modified={r.author !== d.author}
      onreset={() => (settings.renovate.author = d.author)} />
  {/if}
</Section>
