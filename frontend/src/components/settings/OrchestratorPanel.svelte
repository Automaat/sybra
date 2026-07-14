<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import NumberField from './fields/NumberField.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import AdvancedDisclosure from './fields/AdvancedDisclosure.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const o = $derived(settings.orchestrator)
  const d = $derived(defaults.orchestrator)
</script>

<Section title="Orchestrator" description="Dispatch and recovery cadence for the in-process monitor loop.">
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
  <AdvancedDisclosure label="Plan review">
    <ToggleField
      label="Auto-approve plans without decisions"
      description="Validated plans skip plan-review only when the planner explicitly recorded no open decisions."
      keyPath="orchestrator.auto_approve_plans_without_decisions"
      bind:checked={settings.orchestrator.autoApprovePlansWithoutDecisions}
      modified={o.autoApprovePlansWithoutDecisions !== d.autoApprovePlansWithoutDecisions}
      onreset={() => (settings.orchestrator.autoApprovePlansWithoutDecisions = d.autoApprovePlansWithoutDecisions)}
    />
  </AdvancedDisclosure>
</Section>
