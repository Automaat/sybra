<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const testing = $derived(settings.testing)
  const td = $derived(defaults.testing)
  const exp = $derived(settings.experience)
  const ed = $derived(defaults.experience)

  // Project types are edited as a comma-separated list bound to the string[] field.
  let projectTypesText = $state('')
  let lastPushed = ''
  $effect(() => {
    const joined = (settings.projectTypes ?? []).join(', ')
    if (joined !== lastPushed) {
      projectTypesText = joined
      lastPushed = joined
    }
  })
  function commitProjectTypes() {
    const arr = projectTypesText
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s.length > 0)
    settings.projectTypes = arr
    lastPushed = arr.join(', ')
  }
  const projectTypesModified = $derived(
    (settings.projectTypes ?? []).join(',') !== (defaults.projectTypes ?? []).join(',')
  )
</script>

<Section
  title="Machine routing"
  description="Which project types this instance handles. Empty = all types. Lets you run Sybra on multiple machines without duplicating work."
>
  <div class="group flex flex-col gap-1 border-l-2 pl-3 {projectTypesModified ? 'border-primary-500' : 'border-transparent'}">
    <div class="flex items-center gap-2">
      <label class="text-sm font-medium text-surface-800 dark:text-surface-100" for="project-types">Project types</label>
      {#if projectTypesModified}
        <span class="rounded bg-primary-500/12 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-primary-700 dark:text-primary-300">Modified</span>
      {/if}
      <span class="font-mono text-[10px] text-surface-400 opacity-0 group-hover:opacity-100">project_types</span>
    </div>
    <input
      id="project-types"
      type="text"
      placeholder="pet, work  (empty = all)"
      class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
      bind:value={projectTypesText}
      onblur={commitProjectTypes}
      onchange={commitProjectTypes}
    />
    <span class="text-xs text-surface-500 dark:text-surface-400">Comma-separated. Commits on blur.</span>
  </div>
</Section>

<Section title="Testing" description="Autonomous manual-testing phase — one adversarial test-runner per task in an isolated sandbox.">
  <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
    <NumberField id="testing-concurrent" label="Max concurrent test-runners" keyPath="testing.max_concurrent" min={0}
      description="0 uses the built-in default (3)"
      bind:value={settings.testing.maxConcurrent}
      modified={testing.maxConcurrent !== td.maxConcurrent}
      onreset={() => (settings.testing.maxConcurrent = td.maxConcurrent)} />
    <NumberField id="testing-attempts" label="Max attempts" keyPath="testing.max_attempts" min={0}
      description="Test failures before escalating to human-required (0 = default)"
      bind:value={settings.testing.maxAttempts}
      modified={testing.maxAttempts !== td.maxAttempts}
      onreset={() => (settings.testing.maxAttempts = td.maxAttempts)} />
  </div>
</Section>

<Section title="Experience memory & metrics" description="Advisory experience records and the Prometheus metrics endpoint.">
  <ToggleField label="Enable experience memory" description="Advisory-only context for triage and planning"
    keyPath="experience.enabled"
    bind:checked={settings.experience.enabled}
    modified={exp.enabled !== ed.enabled}
    onreset={() => (settings.experience.enabled = ed.enabled)} />
  {#if settings.experience.enabled}
    <NumberField id="exp-max" label="Max records" keyPath="experience.max_records" min={0}
      bind:value={settings.experience.maxRecords}
      modified={exp.maxRecords !== ed.maxRecords}
      onreset={() => (settings.experience.maxRecords = ed.maxRecords)} />
  {/if}
  <ToggleField label="Enable Prometheus metrics" description="Mounts /metrics on the server (requires restart)"
    keyPath="metrics.enabled"
    bind:checked={settings.metrics.enabled}
    modified={settings.metrics.enabled !== defaults.metrics.enabled}
    onreset={() => (settings.metrics.enabled = defaults.metrics.enabled)} />
</Section>
