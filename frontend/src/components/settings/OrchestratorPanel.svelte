<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import AdvancedDisclosure from './fields/AdvancedDisclosure.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const o = $derived(settings.orchestrator)
  const d = $derived(defaults.orchestrator)
</script>

<Section title="Orchestrator" description="Automatic dispatch of triage and planning agents, plus recovery cadence.">
  <ToggleField label="Auto-triage" description="Automatically dispatch triage agents on task creation"
    keyPath="orchestrator.auto_triage"
    bind:checked={settings.orchestrator.autoTriage}
    modified={o.autoTriage !== d.autoTriage}
    onreset={() => (settings.orchestrator.autoTriage = d.autoTriage)} />
  <ToggleField label="Auto-plan" description="Automatically dispatch planning agents on complex tasks"
    keyPath="orchestrator.auto_plan"
    bind:checked={settings.orchestrator.autoPlan}
    modified={o.autoPlan !== d.autoPlan}
    onreset={() => (settings.orchestrator.autoPlan = d.autoPlan)} />

  <AdvancedDisclosure label="Loop cadence">
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <NumberField id="orch-dispatch" label="Dispatch interval (seconds)" keyPath="orchestrator.dispatch_interval_seconds" min={1}
        description="Cheap dispatch pass — release unblocked children (default 10)"
        bind:value={settings.orchestrator.dispatchIntervalSeconds}
        modified={o.dispatchIntervalSeconds !== d.dispatchIntervalSeconds}
        onreset={() => (settings.orchestrator.dispatchIntervalSeconds = d.dispatchIntervalSeconds)} />
      <NumberField id="orch-maint" label="Maintenance interval (seconds)" keyPath="orchestrator.maintenance_interval_seconds" min={1}
        description="Expensive recovery pass — resume stalled, prune worktrees (default 60)"
        bind:value={settings.orchestrator.maintenanceIntervalSeconds}
        modified={o.maintenanceIntervalSeconds !== d.maintenanceIntervalSeconds}
        onreset={() => (settings.orchestrator.maintenanceIntervalSeconds = d.maintenanceIntervalSeconds)} />
    </div>
  </AdvancedDisclosure>
</Section>
