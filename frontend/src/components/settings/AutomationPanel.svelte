<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import TextField from './fields/TextField.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const t = $derived(settings.triage)
  const td = $derived(defaults.triage)
  const u = $derived(settings.umbrella)
  const ud = $derived(defaults.umbrella)
</script>

<Section title="Triage" description="Background worker that classifies status=new tasks and atomically applies the verdict.">
  <ToggleField label="Enable auto-triage" description="Periodically classify new tasks via the triage model"
    keyPath="triage.enabled"
    bind:checked={settings.triage.enabled}
    modified={t.enabled !== td.enabled}
    onreset={() => (settings.triage.enabled = td.enabled)} />
  {#if settings.triage.enabled}
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <NumberField id="triage-poll" label="Poll interval (seconds)" keyPath="triage.poll_seconds" min={10}
        bind:value={settings.triage.pollSeconds}
        modified={t.pollSeconds !== td.pollSeconds}
        onreset={() => (settings.triage.pollSeconds = td.pollSeconds)} />
      <TextField id="triage-model" label="Model" placeholder="sonnet" keyPath="triage.model"
        bind:value={settings.triage.model}
        modified={t.model !== td.model}
        onreset={() => (settings.triage.model = td.model)} />
    </div>
  {/if}
</Section>

<Section title="Umbrella issues" description="Auto-expand ☂️ umbrella issues into a gated task DAG (project-scoped via project types).">
  <ToggleField label="Enable umbrella expansion" keyPath="umbrella.enabled"
    bind:checked={settings.umbrella.enabled}
    modified={u.enabled !== ud.enabled}
    onreset={() => (settings.umbrella.enabled = ud.enabled)} />
  {#if settings.umbrella.enabled}
    <TextField id="umbrella-model" label="Planner model" placeholder="claude default" keyPath="umbrella.model"
      bind:value={settings.umbrella.model}
      modified={u.model !== ud.model}
      onreset={() => (settings.umbrella.model = ud.model)} />
    <ToggleField label="Ground sub-issues against repo files"
      description="Confirm each sub-issue's touches against the real repo (extra git/tool calls)"
      keyPath="umbrella.ground"
      bind:checked={settings.umbrella.ground}
      modified={u.ground !== ud.ground}
      onreset={() => (settings.umbrella.ground = ud.ground)} />
    {#if settings.umbrella.ground}
      <NumberField id="umbrella-min" label="Grounding min sub-issues" keyPath="umbrella.ground_min_sub_issues" min={0}
        description="Only ground umbrellas with at least this many sub-issues (0 = always)"
        bind:value={settings.umbrella.groundMinSubIssues}
        modified={u.groundMinSubIssues !== ud.groundMinSubIssues}
        onreset={() => (settings.umbrella.groundMinSubIssues = ud.groundMinSubIssues)} />
    {/if}
  {/if}
</Section>
