<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import TextField from './fields/TextField.svelte'
  import AdvancedDisclosure from './fields/AdvancedDisclosure.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const m = $derived(settings.monitor)
  const d = $derived(defaults.monitor)
  const sm = $derived(settings.selfMonitor)
  const sd = $derived(defaults.selfMonitor)
</script>

<Section
  title="Monitor"
  description="In-process anomaly detection — lost agents, PR gaps, dwell, failure spikes, bottlenecks — with idempotent remediation and focused agent dispatch."
>
  <ToggleField
    label="Enable monitor"
    description="Run the periodic board + audit anomaly detector"
    keyPath="monitor.enabled"
    bind:checked={settings.monitor.enabled}
    modified={m.enabled !== d.enabled}
    onreset={() => (settings.monitor.enabled = d.enabled)}
  />

  {#if settings.monitor.enabled}
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <NumberField id="mon-interval" label="Interval (seconds)" keyPath="monitor.interval_seconds" min={60}
        description="Tick cadence (floored to 300 if below 60)"
        bind:value={settings.monitor.intervalSeconds}
        modified={m.intervalSeconds !== d.intervalSeconds}
        onreset={() => (settings.monitor.intervalSeconds = d.intervalSeconds)} />
      <TextField id="mon-model" label="Model" placeholder="sonnet" keyPath="monitor.model"
        bind:value={settings.monitor.model}
        modified={m.model !== d.model}
        onreset={() => (settings.monitor.model = d.model)} />
      <NumberField id="mon-dispatch" label="Dispatch limit" keyPath="monitor.dispatch_limit" min={0}
        description="Max agents the monitor may dispatch per tick"
        bind:value={settings.monitor.dispatchLimit}
        modified={m.dispatchLimit !== d.dispatchLimit}
        onreset={() => (settings.monitor.dispatchLimit = d.dispatchLimit)} />
      <NumberField id="mon-cooldown" label="Issue cooldown (min)" keyPath="monitor.issue_cooldown_minutes" min={0}
        bind:value={settings.monitor.issueCooldownMinutes}
        modified={m.issueCooldownMinutes !== d.issueCooldownMinutes}
        onreset={() => (settings.monitor.issueCooldownMinutes = d.issueCooldownMinutes)} />
    </div>

    <AdvancedDisclosure label="Detection thresholds">
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <NumberField id="mon-stuck" label="Stuck human (hours)" keyPath="monitor.stuck_human_hours" min={0} step={0.5}
          bind:value={settings.monitor.stuckHumanHours}
          modified={m.stuckHumanHours !== d.stuckHumanHours}
          onreset={() => (settings.monitor.stuckHumanHours = d.stuckHumanHours)} />
        <NumberField id="mon-lost" label="Lost agent (minutes)" keyPath="monitor.lost_agent_minutes" min={0}
          bind:value={settings.monitor.lostAgentMinutes}
          modified={m.lostAgentMinutes !== d.lostAgentMinutes}
          onreset={() => (settings.monitor.lostAgentMinutes = d.lostAgentMinutes)} />
        <NumberField id="mon-prgap" label="PR gap grace (minutes)" keyPath="monitor.pr_gap_grace_minutes" min={0}
          bind:value={settings.monitor.prGapGraceMinutes}
          modified={m.prGapGraceMinutes !== d.prGapGraceMinutes}
          onreset={() => (settings.monitor.prGapGraceMinutes = d.prGapGraceMinutes)} />
        <NumberField id="mon-failrate" label="Failure-rate threshold" keyPath="monitor.failure_rate_threshold" min={0} max={1} step={0.05}
          bind:value={settings.monitor.failureRateThreshold}
          modified={m.failureRateThreshold !== d.failureRateThreshold}
          onreset={() => (settings.monitor.failureRateThreshold = d.failureRateThreshold)} />
      </div>
      <TextField id="mon-label" label="Issue label" keyPath="monitor.issue_label"
        bind:value={settings.monitor.issueLabel}
        modified={m.issueLabel !== d.issueLabel}
        onreset={() => (settings.monitor.issueLabel = d.issueLabel)} />
      <TextField id="mon-repo" label="Issue repo" placeholder="owner/repo" keyPath="monitor.issue_repo"
        bind:value={settings.monitor.issueRepo}
        modified={m.issueRepo !== d.issueRepo}
        onreset={() => (settings.monitor.issueRepo = d.issueRepo)} />
      <p class="text-xs text-surface-500 dark:text-surface-400">Per-stage bottleneck thresholds (<code>monitor.bottleneck_hours</code>) are editable in the Advanced YAML tab.</p>
    </AdvancedDisclosure>
  {/if}

  <ToggleField
    label="Enable self-monitor"
    description="Periodic deep analysis of agent runs and workflow health that files deduped issues"
    keyPath="self_monitor.enabled"
    bind:checked={settings.selfMonitor.enabled}
    modified={sm.enabled !== sd.enabled}
    onreset={() => (settings.selfMonitor.enabled = sd.enabled)}
  />
  {#if settings.selfMonitor.enabled}
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <NumberField id="sm-interval" label="Interval (hours)" keyPath="self_monitor.interval_hours" min={0} step={0.5}
        bind:value={settings.selfMonitor.intervalHours}
        modified={sm.intervalHours !== sd.intervalHours}
        onreset={() => (settings.selfMonitor.intervalHours = sd.intervalHours)} />
      <NumberField id="sm-maxissues" label="Max issues / run" keyPath="self_monitor.max_issues_per_run" min={0}
        bind:value={settings.selfMonitor.maxIssuesPerRun}
        modified={sm.maxIssuesPerRun !== sd.maxIssuesPerRun}
        onreset={() => (settings.selfMonitor.maxIssuesPerRun = sd.maxIssuesPerRun)} />
    </div>
    <ToggleField label="Dry run" description="Analyze and log but never file issues or take auto-actions"
      keyPath="self_monitor.dry_run"
      bind:checked={settings.selfMonitor.dryRun}
      modified={sm.dryRun !== sd.dryRun}
      onreset={() => (settings.selfMonitor.dryRun = sd.dryRun)} />
  {/if}
</Section>
