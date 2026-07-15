<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import TextField from './fields/TextField.svelte'
  import TokenField from './fields/TokenField.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
    /** Persists the token via the dedicated write path (never the generic Save). */
    onsavetoken: (token: string) => Promise<void>
  }

  let { settings = $bindable(), defaults, onsavetoken }: Props = $props()
  const t = $derived(settings.todoist)
  const d = $derived(defaults.todoist)
</script>

<Section title="Todoist" description="Pull tasks from a Todoist project and close them when done.">
  <ToggleField label="Enable Todoist sync" keyPath="todoist.enabled"
    bind:checked={settings.todoist.enabled}
    modified={t.enabled !== d.enabled}
    onreset={() => (settings.todoist.enabled = d.enabled)} />

  {#if settings.todoist.enabled}
    <TokenField
      id="todoist-token"
      label="API token"
      description="Settings → Integrations → API token. Saved separately from the form and never displayed after storing."
      keyPath="todoist.api_token"
      tokenSet={settings.todoistTokenSet}
      onsave={onsavetoken}
    />
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <TextField id="todoist-project" label="Project ID" placeholder="Todoist project ID" keyPath="todoist.project_id"
        bind:value={settings.todoist.projectId}
        modified={t.projectId !== d.projectId}
        onreset={() => (settings.todoist.projectId = d.projectId)} />
      <NumberField id="todoist-poll" label="Poll interval (seconds)" keyPath="todoist.poll_seconds" min={30} max={3600}
        description="30–3600 seconds"
        bind:value={settings.todoist.pollSeconds}
        modified={t.pollSeconds !== d.pollSeconds}
        onreset={() => (settings.todoist.pollSeconds = d.pollSeconds)} />
    </div>
  {/if}
</Section>
