<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import SelectField from './fields/SelectField.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const l = $derived(settings.logging)
  const ld = $derived(defaults.logging)
  const au = $derived(settings.audit)
  const ad = $derived(defaults.audit)
</script>

<Section title="Logging & audit" description="Log verbosity, rotation, and audit-trail retention.">
  <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
    <SelectField id="log-level" label="Log level" keyPath="logging.level"
      options={[
        { value: 'debug', label: 'Debug' },
        { value: 'info', label: 'Info' },
        { value: 'warn', label: 'Warn' },
        { value: 'error', label: 'Error' },
      ]}
      bind:value={settings.logging.level}
      modified={l.level !== ld.level}
      onreset={() => (settings.logging.level = ld.level)} />
    <NumberField id="log-max-size" label="Max log size (MB)" keyPath="logging.max_size_mb" min={1} max={500}
      description="1–500 MB"
      bind:value={settings.logging.maxSizeMB}
      modified={l.maxSizeMB !== ld.maxSizeMB}
      onreset={() => (settings.logging.maxSizeMB = ld.maxSizeMB)} />
    <NumberField id="log-max-files" label="Max log files" keyPath="logging.max_files" min={1} max={50}
      description="1–50 files"
      bind:value={settings.logging.maxFiles}
      modified={l.maxFiles !== ld.maxFiles}
      onreset={() => (settings.logging.maxFiles = ld.maxFiles)} />
    <NumberField id="audit-retention" label="Audit retention (days)" keyPath="audit.retention_days" min={1} max={365}
      description="1–365 days"
      bind:value={settings.audit.retentionDays}
      modified={au.retentionDays !== ad.retentionDays}
      onreset={() => (settings.audit.retentionDays = ad.retentionDays)} />
  </div>
  <ToggleField label="Enable audit logging" keyPath="audit.enabled"
    bind:checked={settings.audit.enabled}
    modified={au.enabled !== ad.enabled}
    onreset={() => (settings.audit.enabled = ad.enabled)} />
</Section>
